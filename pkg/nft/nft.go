//go:build linux

// Package nft owns the one nftables table stagbru manages, `table inet
// stagbru` (docs/plan.md's "Shared plumbing: one nftables table for both"
// section): forward-accept between every configured WireGuard interface
// and the Tailscale interface, and a postrouting masquerade rule per
// network that needs it. See docs/plan.md's hard constraints: this
// package must never create, touch, or flush any table other than its
// own.
//
// Linux-only (unlike pkg/wg, which stays buildable on any GOOS by
// redefining the couple of Linux-only vishvananda/netlink constants it
// needs locally): github.com/google/nftables's expr package unconditionally
// imports its own xt subpackage, which references golang.org/x/sys/unix
// netfilter constants (e.g. NFPROTO_IPV4) that don't exist outside Linux,
// in files with no build constraint of their own -- a whole-package compile
// failure on any other GOOS, not a missing symbol this package can work
// around locally. CI (.github/workflows/ci.yml) builds/vets/tests on
// ubuntu-latest, so this still gets full coverage there; it simply can't
// be built or tested from a non-Linux workstation.
package nft

import (
	"errors"
	"fmt"
	"sort"
	"syscall"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

// TableName is the one nftables table this package owns exclusively.
const TableName = "stagbru"

// TailscaleInterface is the interface name the forward chain always
// includes, regardless of whether tailscaled is actually running yet
// (docs/plan.md's startup order step 3: the set is "the full configured
// interface set plus tailscale0", a fixed name, not derived from live
// process state).
const TailscaleInterface = "tailscale0"

const (
	forwardChainName     = "forward"
	postroutingChainName = "postrouting"
)

// Network is the nftables-relevant shape of one configured WireGuard
// network. Kept decoupled from config.Network so this package has no
// import-time dependency on pkg/config.
type Network struct {
	Interface  string
	Masquerade bool
}

// conn is the subset of *nftables.Conn Reconcile/Delete need.
// *nftables.Conn satisfies it directly (every method below has an
// identical signature) -- this interface exists purely so tests can
// substitute a fake, the same pattern pkg/wg uses for its
// deviceSource/deviceApplier/routeManager abstractions over wgctrl/netlink.
type conn interface {
	AddTable(t *nftables.Table) *nftables.Table
	DelTable(t *nftables.Table)
	FlushTable(t *nftables.Table)
	AddChain(c *nftables.Chain) *nftables.Chain
	AddRule(r *nftables.Rule) *nftables.Rule
	Flush() error
}

// Reconcile rebuilds TableName from scratch and applies it as one atomic
// replace: forward-accept for traffic to/from every configured interface
// plus TailscaleInterface, and one postrouting masquerade rule per network
// with Masquerade set (replacing today's per-interface `iptables ...
// MASQUERADE`, see docs/current-state.md). Safe to call repeatedly --
// every call fully re-derives the table's contents from networks, never
// incrementally patching, so there is no window where only some of the
// expected rules are in place.
func Reconcile(c conn, networks []Network, tailscaleInterface string) error {
	table := c.AddTable(&nftables.Table{
		Name:   TableName,
		Family: nftables.TableFamilyINet,
	})
	// Idempotent full rebuild: empty the table's existing chains/rules (if
	// any -- e.g. a prior Reconcile call, or a stale table surviving a
	// restart) before re-adding everything below, rather than
	// accumulating rules across calls.
	c.FlushTable(table)

	forward := c.AddChain(&nftables.Chain{
		Name:     forwardChainName,
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
		Policy:   policyRef(nftables.ChainPolicyAccept),
	})
	for _, iface := range interfaceSet(networks, tailscaleInterface) {
		c.AddRule(directionAcceptRule(table, forward, expr.MetaKeyIIFNAME, iface))
		c.AddRule(directionAcceptRule(table, forward, expr.MetaKeyOIFNAME, iface))
	}

	if masqueradeInterfaces := masqueradeSet(networks); len(masqueradeInterfaces) > 0 {
		postrouting := c.AddChain(&nftables.Chain{
			Name:     postroutingChainName,
			Table:    table,
			Type:     nftables.ChainTypeNAT,
			Hooknum:  nftables.ChainHookPostrouting,
			Priority: nftables.ChainPriorityNATSource,
			Policy:   policyRef(nftables.ChainPolicyAccept),
		})
		for _, iface := range masqueradeInterfaces {
			c.AddRule(masqueradeRule(table, postrouting, iface))
		}
	}

	if err := c.Flush(); err != nil {
		return fmt.Errorf("nft: apply table %q: %w", TableName, err)
	}
	return nil
}

// Delete removes TableName (docs/plan.md's Shutdown step 3). A table that
// doesn't exist -- Reconcile was never called, or shutdown runs twice --
// is not an error.
func Delete(c conn) error {
	c.DelTable(&nftables.Table{Name: TableName, Family: nftables.TableFamilyINet})
	if err := c.Flush(); err != nil && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("nft: delete table %q: %w", TableName, err)
	}
	return nil
}

// interfaceSet returns the deduplicated, sorted set of interfaces the
// forward chain accepts traffic to/from: every network's own interface,
// plus tailscaleInterface.
func interfaceSet(networks []Network, tailscaleInterface string) []string {
	seen := make(map[string]bool, len(networks)+1)
	var out []string
	add := func(iface string) {
		if iface == "" || seen[iface] {
			return
		}
		seen[iface] = true
		out = append(out, iface)
	}
	for _, n := range networks {
		add(n.Interface)
	}
	add(tailscaleInterface)
	sort.Strings(out)
	return out
}

// masqueradeSet returns the sorted set of interfaces needing a postrouting
// masquerade rule.
func masqueradeSet(networks []Network) []string {
	var out []string
	for _, n := range networks {
		if n.Masquerade {
			out = append(out, n.Interface)
		}
	}
	sort.Strings(out)
	return out
}

func directionAcceptRule(table *nftables.Table, chain *nftables.Chain, key expr.MetaKey, iface string) *nftables.Rule {
	return &nftables.Rule{
		Table: table,
		Chain: chain,
		Exprs: []expr.Any{
			&expr.Meta{Key: key, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(iface)},
			&expr.Verdict{Kind: expr.VerdictAccept},
		},
	}
}

func masqueradeRule(table *nftables.Table, chain *nftables.Chain, iface string) *nftables.Rule {
	return &nftables.Rule{
		Table: table,
		Chain: chain,
		Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(iface)},
			&expr.Masq{},
		},
	}
}

// ifname encodes an interface name the way nftables matches it against
// meta iifname/oifname: a zero-padded IFNAMSIZ (16-byte) buffer.
func ifname(n string) []byte {
	b := make([]byte, 16)
	copy(b, n)
	return b
}

func policyRef(p nftables.ChainPolicy) *nftables.ChainPolicy {
	return &p
}
