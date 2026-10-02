// Package wg manages a single kernel WireGuard interface: creating or
// adopting it, and reconciling its peers against a desired state.
//
// A Node owns exactly one interface. stagbru runs one Node per configured
// config.Network (see cmd/stagbru) — this package itself has no notion of
// "multiple networks"; that composition happens one level up.
//
// Peer validation (base64 key decoding, CIDR parsing, endpoint resolution)
// happens upstream in pkg/peers, not here: by the time a Peer reaches
// Reconcile, it's assumed already valid. See docs/plan.md's "Peer
// reconcile" section for the full spec this implements.
package wg

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// persistentKeepalive is the interval used for any peer with a non-nil
// Endpoint, per docs/current-state.md (matches today's peers.conf template
// exactly).
const persistentKeepalive = 25 * time.Second

// routeProtocol tags every route stagbru installs for a peer's AllowedIPs,
// so a resync can identify and only touch routes it manages itself (never
// touching routes installed by anything else, e.g. the kernel's own
// connected-route entries or routes owned by other daemons). 99 is in the
// range Linux leaves open for userspace-assigned route protocols (see
// rtnetlink.h: RTPROT_STATIC is 4, with low numbers reserved for the kernel
// and boot-time protocols; values from roughly 16 up, and certainly in the
// 100s, are unassigned convention-wise) — chosen as an arbitrary, documented
// constant rather than a well-known protocol number, matching the
// "dedicated proto number shared across all networks" requirement in
// docs/plan.md's "Peer reconcile" section.
const routeProtocol = netlink.RouteProtocol(99)

// familyAll and scopeLink mirror netlink.FAMILY_ALL and netlink.SCOPE_LINK.
// Those constants are only defined in the vishvananda/netlink package's
// Linux-specific build files (this whole package is Linux-only in practice
// — it manages a real kernel WireGuard device — but defining the values
// locally keeps `go build`/`go vet`/`go test` working on any GOOS, since
// the rest of the vishvananda/netlink API surface we use has portable
// (ErrNotImplemented) stubs for non-Linux already).
const (
	familyAll               = 0   // unix.AF_UNSPEC, same value as netlink.FAMILY_ALL
	scopeLink netlink.Scope = 253 // unix.RT_SCOPE_LINK, same value as netlink.SCOPE_LINK
)

// Peer is one desired WireGuard peer.
type Peer struct {
	// Name is informational only (used in logs/metrics), not sent to the
	// kernel.
	Name string

	PublicKey  wgtypes.Key
	AllowedIPs []netip.Prefix

	// Endpoint is nil for a peer with no known dial-out address (it can
	// still dial in). PersistentKeepalive (25s, per docs/current-state.md)
	// is only set when Endpoint is non-nil.
	Endpoint *net.UDPAddr
}

// Config is everything needed to bring up and own one network's interface.
type Config struct {
	Interface  string
	ListenPort int
	PrivateKey wgtypes.Key

	// Address is this node's own address on this network (e.g.
	// 100.192.8.5/24). AllowedIPs overlapping Address.Addr() are skipped
	// when installing routes (see Reconcile).
	Address netip.Prefix
}

// deviceSource abstracts "read the device's actual state" so Reconcile's
// diff logic can be unit tested without a real kernel WireGuard device.
type deviceSource interface {
	Device() (*wgtypes.Device, error)
}

// deviceApplier abstracts "apply a peer configuration change" similarly.
type deviceApplier interface {
	ConfigureDevice(cfg wgtypes.Config) error
}

// routeManager abstracts route install/removal for the same reason.
type routeManager interface {
	// Sync is given the full set of routes that should exist (one per
	// AllowedIP of every desired peer, already filtered to exclude
	// anything overlapping the node's own address) and ensures that's
	// exactly the set of stagbru-managed (routeProtocol-tagged) routes
	// present on this link, adding/replacing and removing as needed.
	Sync(wanted []netip.Prefix) error
}

// wgctrlDevice adapts *wgctrl.Client to deviceSource/deviceApplier for one
// named interface.
type wgctrlDevice struct {
	client *wgctrl.Client
	iface  string
}

func (d *wgctrlDevice) Device() (*wgtypes.Device, error) {
	return d.client.Device(d.iface)
}

func (d *wgctrlDevice) ConfigureDevice(cfg wgtypes.Config) error {
	return d.client.ConfigureDevice(d.iface, cfg)
}

// netlinkRoutes adapts netlink route syscalls to routeManager for one link.
type netlinkRoutes struct {
	linkIndex int
	ownAddr   netip.Prefix
}

func (r *netlinkRoutes) Sync(wanted []netip.Prefix) error {
	wantedSet := make(map[string]netip.Prefix, len(wanted))
	for _, p := range wanted {
		wantedSet[p.String()] = p
	}

	existing, err := netlink.RouteList(&netlink.GenericLink{
		LinkAttrs: netlink.LinkAttrs{Index: r.linkIndex},
	}, familyAll)
	if err != nil {
		return fmt.Errorf("wg: list routes: %w", err)
	}

	have := make(map[string]bool)
	var errs []error
	for _, rt := range existing {
		if rt.Protocol != routeProtocol || rt.Dst == nil {
			continue
		}
		prefix, ok := ipNetToPrefix(rt.Dst)
		if !ok {
			continue
		}
		key := prefix.String()
		have[key] = true
		if _, wantIt := wantedSet[key]; !wantIt {
			del := rt
			if err := netlink.RouteDel(&del); err != nil {
				errs = append(errs, fmt.Errorf("wg: delete route %s: %w", key, err))
			}
		}
	}

	for key, prefix := range wantedSet {
		if have[key] {
			continue
		}
		route := &netlink.Route{
			LinkIndex: r.linkIndex,
			Dst:       prefixToIPNet(prefix),
			Scope:     scopeLink,
			Protocol:  routeProtocol,
		}
		if err := netlink.RouteReplace(route); err != nil {
			errs = append(errs, fmt.Errorf("wg: add route %s: %w", key, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("wg: route sync: %w", joinErrors(errs))
	}
	return nil
}

func joinErrors(errs []error) error {
	var b strings.Builder
	b.WriteString(errs[0].Error())
	for _, e := range errs[1:] {
		b.WriteString("; ")
		b.WriteString(e.Error())
	}
	return fmt.Errorf("%s", b.String())
}

func ipNetToPrefix(n *net.IPNet) (netip.Prefix, bool) {
	addr, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	addr = addr.Unmap()
	ones, _ := n.Mask.Size()
	return netip.PrefixFrom(addr, ones), true
}

func prefixToIPNet(p netip.Prefix) *net.IPNet {
	addr := p.Addr()
	return &net.IPNet{
		IP:   addr.AsSlice(),
		Mask: net.CIDRMask(p.Bits(), addr.BitLen()),
	}
}

// Node owns one kernel WireGuard interface: its creation/adoption, address,
// and peer reconciliation against repeated calls to Reconcile.
type Node struct {
	cfg Config

	link      netlink.Link
	wgClient  *wgctrl.Client
	device    deviceSource
	apply     deviceApplier
	routes    routeManager
	lastPeers map[wgtypes.Key]Peer
}

// NewNode creates the interface described by cfg if it doesn't exist, or
// adopts it if a stale one with the same name already exists (rather than
// failing), sets its private key/listen port/address, and brings it up.
func NewNode(cfg Config) (*Node, error) {
	link, err := netlink.LinkByName(cfg.Interface)
	if err != nil {
		if _, notFound := err.(netlink.LinkNotFoundError); !notFound {
			return nil, fmt.Errorf("wg: look up interface %q: %w", cfg.Interface, err)
		}

		wgLink := &netlink.Wireguard{
			LinkAttrs: netlink.LinkAttrs{Name: cfg.Interface},
		}
		if err := netlink.LinkAdd(wgLink); err != nil {
			return nil, fmt.Errorf("wg: create interface %q: %w", cfg.Interface, err)
		}
		link, err = netlink.LinkByName(cfg.Interface)
		if err != nil {
			return nil, fmt.Errorf("wg: look up newly created interface %q: %w", cfg.Interface, err)
		}
	}

	addr := &netlink.Addr{IPNet: prefixToIPNet(cfg.Address)}
	if err := netlink.AddrAdd(link, addr); err != nil {
		// Address may already be assigned from a prior adopted link; treat
		// "file exists" as success, anything else is a real failure.
		if !isExistsErr(err) {
			return nil, fmt.Errorf("wg: assign address %s to %q: %w", cfg.Address, cfg.Interface, err)
		}
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return nil, fmt.Errorf("wg: bring up interface %q: %w", cfg.Interface, err)
	}

	client, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("wg: open wgctrl client: %w", err)
	}

	listenPort := cfg.ListenPort
	if err := client.ConfigureDevice(cfg.Interface, wgtypes.Config{
		PrivateKey: &cfg.PrivateKey,
		ListenPort: &listenPort,
	}); err != nil {
		client.Close()
		return nil, fmt.Errorf("wg: configure device %q: %w", cfg.Interface, err)
	}

	dev := &wgctrlDevice{client: client, iface: cfg.Interface}

	return &Node{
		cfg:      cfg,
		link:     link,
		wgClient: client,
		device:   dev,
		apply:    dev,
		routes: &netlinkRoutes{
			linkIndex: link.Attrs().Index,
			ownAddr:   cfg.Address,
		},
		lastPeers: make(map[wgtypes.Key]Peer),
	}, nil
}

func isExistsErr(err error) bool {
	return errors.Is(err, syscall.EEXIST)
}

// Reconcile diffs the desired peer set against the interface's actual
// state (via wgctrl) and applies only the delta: new peers are added,
// changed peers (AllowedIPs or endpoint differ) are updated with
// ReplaceAllowedIPs=true, and peers no longer desired are removed. It is a
// no-op — no syscalls at all — when nothing has changed, so that unchanged
// peers keep their live handshake state.
//
// For every AllowedIP of every peer, Reconcile also installs a route via
// this Node's interface (scope link), tagged with a dedicated route
// `proto` so only stagbru-managed routes are ever touched, and removes
// stagbru-managed routes that are no longer wanted. Any AllowedIP
// overlapping this Node's own Config.Address is skipped.
func (n *Node) Reconcile(peers []Peer) error {
	dev, err := n.device.Device()
	if err != nil {
		return fmt.Errorf("wg: read device state: %w", err)
	}

	peerConfigs := diffPeers(dev.Peers, peers)
	if len(peerConfigs) > 0 {
		if err := n.apply.ConfigureDevice(wgtypes.Config{
			ReplacePeers: false,
			Peers:        peerConfigs,
		}); err != nil {
			return fmt.Errorf("wg: configure peers: %w", err)
		}
	}

	var wantedRoutes []netip.Prefix
	for _, p := range peers {
		for _, allowed := range p.AllowedIPs {
			if overlapsOwnAddress(allowed, n.cfg.Address) {
				continue
			}
			wantedRoutes = append(wantedRoutes, allowed)
		}
	}
	if err := n.routes.Sync(wantedRoutes); err != nil {
		return err
	}

	n.lastPeers = make(map[wgtypes.Key]Peer, len(peers))
	for _, p := range peers {
		n.lastPeers[p.PublicKey] = p
	}

	return nil
}

// overlapsOwnAddress reports whether allowed contains ownAddr's address
// (this node's own address on this network).
func overlapsOwnAddress(allowed, ownAddr netip.Prefix) bool {
	if !ownAddr.IsValid() || !allowed.IsValid() {
		return false
	}
	return allowed.Contains(ownAddr.Addr())
}

// diffPeers computes the wgtypes.PeerConfig list needed to turn actual into
// desired: new peers are added, changed peers (AllowedIPs or Endpoint
// differ from actual) are updated with ReplaceAllowedIPs=true, and peers
// present in actual but absent from desired are removed. Returns nil/empty
// when there is nothing to change, which is the signal Reconcile uses to
// skip the ConfigureDevice call entirely (the no-op rule).
//
// This is a pure function so it can be unit tested without a real kernel
// device.
func diffPeers(actual []wgtypes.Peer, desired []Peer) []wgtypes.PeerConfig {
	actualByKey := make(map[wgtypes.Key]wgtypes.Peer, len(actual))
	for _, p := range actual {
		actualByKey[p.PublicKey] = p
	}

	desiredByKey := make(map[wgtypes.Key]Peer, len(desired))
	for _, p := range desired {
		desiredByKey[p.PublicKey] = p
	}

	var configs []wgtypes.PeerConfig

	for _, want := range desired {
		have, exists := actualByKey[want.PublicKey]
		wantAllowed := toIPNets(want.AllowedIPs)

		switch {
		case !exists:
			configs = append(configs, newPeerConfig(want, wantAllowed))
		case peerChanged(have, want, wantAllowed):
			cfg := newPeerConfig(want, wantAllowed)
			cfg.UpdateOnly = true
			configs = append(configs, cfg)
		}
	}

	for _, have := range actual {
		if _, stillWanted := desiredByKey[have.PublicKey]; !stillWanted {
			configs = append(configs, wgtypes.PeerConfig{
				PublicKey: have.PublicKey,
				Remove:    true,
			})
		}
	}

	return configs
}

func newPeerConfig(want Peer, wantAllowed []net.IPNet) wgtypes.PeerConfig {
	cfg := wgtypes.PeerConfig{
		PublicKey:         want.PublicKey,
		Endpoint:          want.Endpoint,
		ReplaceAllowedIPs: true,
		AllowedIPs:        wantAllowed,
	}
	if want.Endpoint != nil {
		keepalive := persistentKeepalive
		cfg.PersistentKeepaliveInterval = &keepalive
	}
	return cfg
}

// peerChanged reports whether want's AllowedIPs or Endpoint differ from
// have's actual, live state.
func peerChanged(have wgtypes.Peer, want Peer, wantAllowed []net.IPNet) bool {
	if !endpointsEqual(have.Endpoint, want.Endpoint) {
		return true
	}
	return !allowedIPsEqual(have.AllowedIPs, wantAllowed)
}

func endpointsEqual(a, b *net.UDPAddr) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return a.IP.Equal(b.IP) && a.Port == b.Port && a.Zone == b.Zone
}

func allowedIPsEqual(a, b []net.IPNet) bool {
	if len(a) != len(b) {
		return false
	}
	aSet := make(map[string]bool, len(a))
	for _, n := range a {
		aSet[n.String()] = true
	}
	for _, n := range b {
		if !aSet[n.String()] {
			return false
		}
	}
	return true
}

func toIPNets(prefixes []netip.Prefix) []net.IPNet {
	out := make([]net.IPNet, 0, len(prefixes))
	for _, p := range prefixes {
		out = append(out, *prefixToIPNet(p))
	}
	return out
}

// Close deletes the interface.
func (n *Node) Close() error {
	var errs []error
	if n.wgClient != nil {
		if err := n.wgClient.Close(); err != nil {
			errs = append(errs, fmt.Errorf("wg: close wgctrl client: %w", err))
		}
	}
	if n.link != nil {
		if err := netlink.LinkDel(n.link); err != nil {
			errs = append(errs, fmt.Errorf("wg: delete interface %q: %w", n.cfg.Interface, err))
		}
	}
	if len(errs) > 0 {
		return joinErrors(errs)
	}
	return nil
}
