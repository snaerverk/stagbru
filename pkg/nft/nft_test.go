//go:build linux

package nft

import (
	"bytes"
	"errors"
	"syscall"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

// fakeConn records every call Reconcile/Delete make, so tests can assert
// on the sequence and content without a real netlink socket.
type fakeConn struct {
	addTableCalls   int
	flushTableCalls int
	delTableCalls   int
	chains          []*nftables.Chain
	rules           []*nftables.Rule
	flushCalls      int
	flushErr        error
}

func (f *fakeConn) AddTable(t *nftables.Table) *nftables.Table {
	f.addTableCalls++
	return t
}

func (f *fakeConn) DelTable(t *nftables.Table) { f.delTableCalls++ }

func (f *fakeConn) FlushTable(t *nftables.Table) { f.flushTableCalls++ }

func (f *fakeConn) AddChain(c *nftables.Chain) *nftables.Chain {
	f.chains = append(f.chains, c)
	return c
}

func (f *fakeConn) AddRule(r *nftables.Rule) *nftables.Rule {
	f.rules = append(f.rules, r)
	return r
}

func (f *fakeConn) Flush() error {
	f.flushCalls++
	return f.flushErr
}

// ruleDirection decodes one directionAcceptRule/masqueradeRule back into
// (metaKey, interface name, isMasquerade) for assertions.
func ruleDirection(t *testing.T, r *nftables.Rule) (key expr.MetaKey, iface string, masq bool) {
	t.Helper()
	if len(r.Exprs) != 3 {
		t.Fatalf("rule has %d exprs, want 3: %+v", len(r.Exprs), r.Exprs)
	}
	meta, ok := r.Exprs[0].(*expr.Meta)
	if !ok {
		t.Fatalf("Exprs[0] = %T, want *expr.Meta", r.Exprs[0])
	}
	cmp, ok := r.Exprs[1].(*expr.Cmp)
	if !ok {
		t.Fatalf("Exprs[1] = %T, want *expr.Cmp", r.Exprs[1])
	}
	name := bytes.TrimRight(cmp.Data, "\x00")
	switch r.Exprs[2].(type) {
	case *expr.Verdict:
		masq = false
	case *expr.Masq:
		masq = true
	default:
		t.Fatalf("Exprs[2] = %T, want *expr.Verdict or *expr.Masq", r.Exprs[2])
	}
	return meta.Key, string(name), masq
}

func TestReconcile_BuildsForwardAndPostroutingChains(t *testing.T) {
	networks := []Network{
		{Interface: "wg0", Masquerade: true},
		{Interface: "wg1", Masquerade: false},
	}
	fc := &fakeConn{}

	if err := Reconcile(fc, networks, "tailscale0"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if fc.addTableCalls != 1 || fc.flushTableCalls != 1 || fc.flushCalls != 1 {
		t.Errorf("addTableCalls=%d flushTableCalls=%d flushCalls=%d, want 1/1/1", fc.addTableCalls, fc.flushTableCalls, fc.flushCalls)
	}
	if len(fc.chains) != 2 {
		t.Fatalf("chains = %d, want 2 (forward, postrouting)", len(fc.chains))
	}
	if fc.chains[0].Name != forwardChainName {
		t.Errorf("chains[0].Name = %q, want %q", fc.chains[0].Name, forwardChainName)
	}
	if fc.chains[1].Name != postroutingChainName {
		t.Errorf("chains[1].Name = %q, want %q", fc.chains[1].Name, postroutingChainName)
	}

	// forward: one iifname + one oifname accept rule per interface in
	// {wg0, wg1, tailscale0} (sorted) = 6 rules, then one masquerade rule
	// for wg0 only = 7 total.
	if len(fc.rules) != 7 {
		t.Fatalf("rules = %d, want 7: %+v", len(fc.rules), fc.rules)
	}

	wantForward := []struct {
		key   expr.MetaKey
		iface string
	}{
		{expr.MetaKeyIIFNAME, "tailscale0"},
		{expr.MetaKeyOIFNAME, "tailscale0"},
		{expr.MetaKeyIIFNAME, "wg0"},
		{expr.MetaKeyOIFNAME, "wg0"},
		{expr.MetaKeyIIFNAME, "wg1"},
		{expr.MetaKeyOIFNAME, "wg1"},
	}
	for i, want := range wantForward {
		if fc.rules[i].Chain.Name != forwardChainName {
			t.Errorf("rules[%d].Chain = %q, want %q", i, fc.rules[i].Chain.Name, forwardChainName)
		}
		key, iface, masq := ruleDirection(t, fc.rules[i])
		if key != want.key || iface != want.iface || masq {
			t.Errorf("rules[%d] = (key=%v iface=%q masq=%v), want (key=%v iface=%q masq=false)", i, key, iface, masq, want.key, want.iface)
		}
	}

	masqRule := fc.rules[6]
	if masqRule.Chain.Name != postroutingChainName {
		t.Errorf("rules[6].Chain = %q, want %q", masqRule.Chain.Name, postroutingChainName)
	}
	key, iface, masq := ruleDirection(t, masqRule)
	if key != expr.MetaKeyOIFNAME || iface != "wg0" || !masq {
		t.Errorf("rules[6] = (key=%v iface=%q masq=%v), want (key=OIFNAME iface=wg0 masq=true)", key, iface, masq)
	}
}

func TestReconcile_NoMasqueradeNetworksSkipsPostroutingChain(t *testing.T) {
	networks := []Network{{Interface: "wg0", Masquerade: false}}
	fc := &fakeConn{}

	if err := Reconcile(fc, networks, "tailscale0"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if len(fc.chains) != 1 {
		t.Fatalf("chains = %d, want 1 (forward only, no network needs masquerade)", len(fc.chains))
	}
	for _, r := range fc.rules {
		if r.Chain.Name == postroutingChainName {
			t.Errorf("unexpected postrouting rule: %+v", r)
		}
	}
}

func TestReconcile_DedupesInterfaceAppearingAsTailscaleAndNetwork(t *testing.T) {
	// A degenerate but valid config: nothing stops tailscaleInterface from
	// colliding with a configured network's own interface name. The
	// forward set must still only list it once.
	networks := []Network{{Interface: "wg0"}}
	fc := &fakeConn{}

	if err := Reconcile(fc, networks, "wg0"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(fc.rules) != 2 {
		t.Errorf("rules = %d, want 2 (one iifname + one oifname for the single deduped interface)", len(fc.rules))
	}
}

func TestReconcile_PropagatesFlushError(t *testing.T) {
	fc := &fakeConn{flushErr: errors.New("netlink: boom")}
	if err := Reconcile(fc, []Network{{Interface: "wg0"}}, "tailscale0"); err == nil {
		t.Fatal("Reconcile() error = nil, want the Flush error surfaced")
	}
}

func TestDelete_RemovesTableAndFlushes(t *testing.T) {
	fc := &fakeConn{}
	if err := Delete(fc); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if fc.delTableCalls != 1 || fc.flushCalls != 1 {
		t.Errorf("delTableCalls=%d flushCalls=%d, want 1/1", fc.delTableCalls, fc.flushCalls)
	}
}

func TestDelete_MissingTableIsNotAnError(t *testing.T) {
	fc := &fakeConn{flushErr: syscall.ENOENT}
	if err := Delete(fc); err != nil {
		t.Errorf("Delete() error = %v, want nil for a table that doesn't exist", err)
	}
}

func TestDelete_PropagatesOtherErrors(t *testing.T) {
	fc := &fakeConn{flushErr: errors.New("netlink: permission denied")}
	if err := Delete(fc); err == nil {
		t.Fatal("Delete() error = nil, want the Flush error surfaced")
	}
}
