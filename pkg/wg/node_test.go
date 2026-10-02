package wg

import (
	"net"
	"net/netip"
	"reflect"
	"sort"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func mustKey(t *testing.T, seed byte) wgtypes.Key {
	t.Helper()
	var k wgtypes.Key
	k[0] = seed
	return k
}

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("parse prefix %q: %v", s, err)
	}
	return p
}

func udpAddr(ip string, port int) *net.UDPAddr {
	return &net.UDPAddr{IP: net.ParseIP(ip), Port: port}
}

// --- diffPeers: add/update/remove/no-op -----------------------------------

func TestDiffPeers_AddNewPeer(t *testing.T) {
	keyA := mustKey(t, 1)
	desired := []Peer{
		{PublicKey: keyA, AllowedIPs: []netip.Prefix{mustPrefix(t, "10.0.0.1/32")}},
	}

	got := diffPeers(nil, desired)
	if len(got) != 1 {
		t.Fatalf("expected 1 peer config, got %d: %+v", len(got), got)
	}
	if got[0].PublicKey != keyA {
		t.Errorf("wrong public key: %v", got[0].PublicKey)
	}
	if got[0].Remove {
		t.Errorf("new peer should not be marked Remove")
	}
	if !got[0].ReplaceAllowedIPs {
		t.Errorf("new peer should have ReplaceAllowedIPs=true")
	}
	if len(got[0].AllowedIPs) != 1 || got[0].AllowedIPs[0].String() != "10.0.0.1/32" {
		t.Errorf("unexpected AllowedIPs: %+v", got[0].AllowedIPs)
	}
}

func TestDiffPeers_UpdateChangedAllowedIPs(t *testing.T) {
	keyA := mustKey(t, 1)
	actual := []wgtypes.Peer{
		{
			PublicKey:  keyA,
			AllowedIPs: toIPNets([]netip.Prefix{mustPrefix(t, "10.0.0.1/32")}),
		},
	}
	desired := []Peer{
		{PublicKey: keyA, AllowedIPs: []netip.Prefix{mustPrefix(t, "10.0.0.2/32")}},
	}

	got := diffPeers(actual, desired)
	if len(got) != 1 {
		t.Fatalf("expected 1 peer config, got %d: %+v", len(got), got)
	}
	if !got[0].ReplaceAllowedIPs {
		t.Errorf("changed peer should have ReplaceAllowedIPs=true")
	}
	if len(got[0].AllowedIPs) != 1 || got[0].AllowedIPs[0].String() != "10.0.0.2/32" {
		t.Errorf("unexpected AllowedIPs: %+v", got[0].AllowedIPs)
	}
}

func TestDiffPeers_UpdateChangedEndpoint(t *testing.T) {
	keyA := mustKey(t, 1)
	allowed := []netip.Prefix{mustPrefix(t, "10.0.0.1/32")}
	actual := []wgtypes.Peer{
		{
			PublicKey:  keyA,
			Endpoint:   udpAddr("1.2.3.4", 51820),
			AllowedIPs: toIPNets(allowed),
		},
	}
	desired := []Peer{
		{PublicKey: keyA, AllowedIPs: allowed, Endpoint: udpAddr("5.6.7.8", 51820)},
	}

	got := diffPeers(actual, desired)
	if len(got) != 1 {
		t.Fatalf("expected 1 peer config (endpoint changed), got %d", len(got))
	}
	if got[0].Endpoint == nil || got[0].Endpoint.IP.String() != "5.6.7.8" {
		t.Errorf("expected updated endpoint, got %+v", got[0].Endpoint)
	}
}

func TestDiffPeers_RemoveGonePeer(t *testing.T) {
	keyA := mustKey(t, 1)
	actual := []wgtypes.Peer{
		{PublicKey: keyA, AllowedIPs: toIPNets([]netip.Prefix{mustPrefix(t, "10.0.0.1/32")})},
	}

	got := diffPeers(actual, nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 peer config, got %d: %+v", len(got), got)
	}
	if !got[0].Remove {
		t.Errorf("gone peer should be marked Remove")
	}
	if got[0].PublicKey != keyA {
		t.Errorf("wrong public key for removal: %v", got[0].PublicKey)
	}
}

// TestDiffPeers_NoOp is the hardest-to-get-wrong correctness property: when
// desired exactly matches actual, diffPeers must return no changes at all,
// so Reconcile makes zero ConfigureDevice calls and unchanged peers keep
// their live handshake state.
func TestDiffPeers_NoOp(t *testing.T) {
	keyA := mustKey(t, 1)
	keyB := mustKey(t, 2)
	allowedA := []netip.Prefix{mustPrefix(t, "10.0.0.1/32"), mustPrefix(t, "10.0.1.0/24")}
	allowedB := []netip.Prefix{mustPrefix(t, "10.0.2.1/32")}

	actual := []wgtypes.Peer{
		{
			PublicKey:  keyA,
			Endpoint:   udpAddr("1.2.3.4", 51820),
			AllowedIPs: toIPNets(allowedA),
		},
		{
			PublicKey:  keyB,
			AllowedIPs: toIPNets(allowedB),
			// no endpoint
		},
	}
	desired := []Peer{
		{PublicKey: keyA, AllowedIPs: allowedA, Endpoint: udpAddr("1.2.3.4", 51820)},
		{PublicKey: keyB, AllowedIPs: allowedB},
	}

	got := diffPeers(actual, desired)
	if len(got) != 0 {
		t.Fatalf("expected no changes, got %d: %+v", len(got), got)
	}
}

// TestDiffPeers_NoOpOrderIndependent makes sure the no-op comparison isn't
// accidentally order-sensitive on AllowedIPs.
func TestDiffPeers_NoOpOrderIndependent(t *testing.T) {
	keyA := mustKey(t, 1)
	actual := []wgtypes.Peer{
		{
			PublicKey: keyA,
			AllowedIPs: toIPNets([]netip.Prefix{
				mustPrefix(t, "10.0.0.1/32"),
				mustPrefix(t, "10.0.1.0/24"),
			}),
		},
	}
	desired := []Peer{
		{
			PublicKey: keyA,
			AllowedIPs: []netip.Prefix{
				mustPrefix(t, "10.0.1.0/24"),
				mustPrefix(t, "10.0.0.1/32"),
			},
		},
	}

	got := diffPeers(actual, desired)
	if len(got) != 0 {
		t.Fatalf("expected no changes (order should not matter), got %d: %+v", len(got), got)
	}
}

func TestDiffPeers_MixedAddUpdateRemove(t *testing.T) {
	keyAdd := mustKey(t, 1)
	keyUpdate := mustKey(t, 2)
	keyRemove := mustKey(t, 3)
	keyUnchanged := mustKey(t, 4)

	unchangedAllowed := []netip.Prefix{mustPrefix(t, "10.0.9.0/24")}

	actual := []wgtypes.Peer{
		{PublicKey: keyUpdate, AllowedIPs: toIPNets([]netip.Prefix{mustPrefix(t, "10.0.2.1/32")})},
		{PublicKey: keyRemove, AllowedIPs: toIPNets([]netip.Prefix{mustPrefix(t, "10.0.3.1/32")})},
		{PublicKey: keyUnchanged, AllowedIPs: toIPNets(unchangedAllowed)},
	}
	desired := []Peer{
		{PublicKey: keyAdd, AllowedIPs: []netip.Prefix{mustPrefix(t, "10.0.1.1/32")}},
		{PublicKey: keyUpdate, AllowedIPs: []netip.Prefix{mustPrefix(t, "10.0.2.2/32")}},
		{PublicKey: keyUnchanged, AllowedIPs: unchangedAllowed},
	}

	got := diffPeers(actual, desired)
	if len(got) != 3 {
		t.Fatalf("expected 3 changes (add, update, remove), got %d: %+v", len(got), got)
	}

	byKey := make(map[wgtypes.Key]wgtypes.PeerConfig, len(got))
	for _, c := range got {
		byKey[c.PublicKey] = c
	}

	if _, ok := byKey[keyUnchanged]; ok {
		t.Errorf("unchanged peer should not appear in the diff")
	}
	if c, ok := byKey[keyAdd]; !ok || c.Remove {
		t.Errorf("expected keyAdd to be added, got %+v (present=%v)", c, ok)
	}
	if c, ok := byKey[keyUpdate]; !ok || c.Remove {
		t.Errorf("expected keyUpdate to be updated, got %+v (present=%v)", c, ok)
	}
	if c, ok := byKey[keyRemove]; !ok || !c.Remove {
		t.Errorf("expected keyRemove to be removed, got %+v (present=%v)", c, ok)
	}
}

// --- PersistentKeepalive ----------------------------------------------------

func TestNewPeerConfig_KeepaliveOnlyWithEndpoint(t *testing.T) {
	keyA := mustKey(t, 1)

	withEndpoint := Peer{PublicKey: keyA, Endpoint: udpAddr("1.2.3.4", 51820)}
	cfg := newPeerConfig(withEndpoint, nil)
	if cfg.PersistentKeepaliveInterval == nil {
		t.Fatalf("expected PersistentKeepaliveInterval to be set when Endpoint is non-nil")
	}
	if *cfg.PersistentKeepaliveInterval != persistentKeepalive {
		t.Errorf("expected keepalive %v, got %v", persistentKeepalive, *cfg.PersistentKeepaliveInterval)
	}

	withoutEndpoint := Peer{PublicKey: keyA}
	cfg2 := newPeerConfig(withoutEndpoint, nil)
	if cfg2.PersistentKeepaliveInterval != nil {
		t.Errorf("expected PersistentKeepaliveInterval unset when Endpoint is nil, got %v", *cfg2.PersistentKeepaliveInterval)
	}
}

func TestDiffPeers_NewPeerKeepaliveViaDiff(t *testing.T) {
	keyA := mustKey(t, 1)
	keyB := mustKey(t, 2)
	desired := []Peer{
		{PublicKey: keyA, Endpoint: udpAddr("1.2.3.4", 51820)},
		{PublicKey: keyB},
	}

	got := diffPeers(nil, desired)
	byKey := make(map[wgtypes.Key]wgtypes.PeerConfig, len(got))
	for _, c := range got {
		byKey[c.PublicKey] = c
	}

	if byKey[keyA].PersistentKeepaliveInterval == nil {
		t.Errorf("peer with endpoint should get a keepalive interval")
	}
	if byKey[keyB].PersistentKeepaliveInterval != nil {
		t.Errorf("peer without endpoint should not get a keepalive interval")
	}
}

// --- overlapsOwnAddress / route skip rule -----------------------------------

func TestOverlapsOwnAddress(t *testing.T) {
	ownAddr := mustPrefix(t, "100.192.8.5/24")

	tests := []struct {
		name    string
		allowed netip.Prefix
		want    bool
	}{
		{"exact own address as /32", mustPrefix(t, "100.192.8.5/32"), true},
		{"supernet containing own address", mustPrefix(t, "100.192.8.0/24"), true},
		{"disjoint network", mustPrefix(t, "172.16.6.0/32"), false},
		{"different peer address on same subnet", mustPrefix(t, "100.192.8.6/32"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := overlapsOwnAddress(tc.allowed, ownAddr)
			if got != tc.want {
				t.Errorf("overlapsOwnAddress(%v, %v) = %v, want %v", tc.allowed, ownAddr, got, tc.want)
			}
		})
	}
}

// fakeRoutes is a routeManager fake used to assert Reconcile computes the
// right wanted-route set (in particular, that it skips AllowedIPs
// overlapping the node's own address) without touching the kernel.
type fakeRoutes struct {
	calls   int
	wanted  []netip.Prefix
	syncErr error
}

func (f *fakeRoutes) Sync(wanted []netip.Prefix) error {
	f.calls++
	f.wanted = wanted
	return f.syncErr
}

// fakeDevice is a deviceSource+deviceApplier fake for exercising Reconcile
// end-to-end, in particular the no-op rule (zero ConfigureDevice calls).
type fakeDevice struct {
	device       *wgtypes.Device
	configureErr error
	configureN   int
	lastCfg      wgtypes.Config
}

func (f *fakeDevice) Device() (*wgtypes.Device, error) {
	return f.device, nil
}

func (f *fakeDevice) ConfigureDevice(cfg wgtypes.Config) error {
	f.configureN++
	f.lastCfg = cfg
	return f.configureErr
}

func TestReconcile_NoOpMakesNoConfigureDeviceCall(t *testing.T) {
	keyA := mustKey(t, 1)
	allowed := []netip.Prefix{mustPrefix(t, "10.0.0.1/32")}

	dev := &fakeDevice{
		device: &wgtypes.Device{
			Peers: []wgtypes.Peer{
				{PublicKey: keyA, AllowedIPs: toIPNets(allowed)},
			},
		},
	}
	routes := &fakeRoutes{}

	n := &Node{
		cfg:    Config{Address: mustPrefix(t, "192.168.1.1/24")},
		device: dev,
		apply:  dev,
		routes: routes,
	}

	if err := n.Reconcile([]Peer{{PublicKey: keyA, AllowedIPs: allowed}}); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	if dev.configureN != 0 {
		t.Errorf("expected zero ConfigureDevice calls for a no-op reconcile, got %d", dev.configureN)
	}
	if routes.calls != 1 {
		t.Errorf("expected route sync to still run once, got %d", routes.calls)
	}
}

func TestReconcile_ChangesTriggerOneConfigureDeviceCall(t *testing.T) {
	keyA := mustKey(t, 1)
	keyB := mustKey(t, 2)

	dev := &fakeDevice{
		device: &wgtypes.Device{
			Peers: []wgtypes.Peer{
				{PublicKey: keyA, AllowedIPs: toIPNets([]netip.Prefix{mustPrefix(t, "10.0.0.1/32")})},
			},
		},
	}
	routes := &fakeRoutes{}

	n := &Node{
		cfg:    Config{Address: mustPrefix(t, "192.168.1.1/24")},
		device: dev,
		apply:  dev,
		routes: routes,
	}

	desired := []Peer{
		{PublicKey: keyB, AllowedIPs: []netip.Prefix{mustPrefix(t, "10.0.0.2/32")}},
	}
	if err := n.Reconcile(desired); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	if dev.configureN != 1 {
		t.Fatalf("expected exactly one ConfigureDevice call, got %d", dev.configureN)
	}
	if dev.lastCfg.ReplacePeers {
		t.Errorf("ReplacePeers must be false per the peer-reconcile spec")
	}
	if len(dev.lastCfg.Peers) != 2 {
		t.Fatalf("expected 2 peer configs (add keyB, remove keyA), got %d", len(dev.lastCfg.Peers))
	}
}

func TestReconcile_SkipsRouteForAllowedIPOverlappingOwnAddress(t *testing.T) {
	keyA := mustKey(t, 1)
	ownAddr := mustPrefix(t, "100.192.8.5/24")

	dev := &fakeDevice{device: &wgtypes.Device{}}
	routes := &fakeRoutes{}

	n := &Node{
		cfg:    Config{Address: ownAddr},
		device: dev,
		apply:  dev,
		routes: routes,
	}

	desired := []Peer{
		{
			PublicKey: keyA,
			AllowedIPs: []netip.Prefix{
				mustPrefix(t, "100.192.8.0/24"), // overlaps own address -> skip
				mustPrefix(t, "172.16.6.0/32"),  // does not overlap -> keep
			},
		},
	}

	if err := n.Reconcile(desired); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	if routes.calls != 1 {
		t.Fatalf("expected exactly one route sync call, got %d", routes.calls)
	}

	got := make([]string, 0, len(routes.wanted))
	for _, p := range routes.wanted {
		got = append(got, p.String())
	}
	sort.Strings(got)

	want := []string{"172.16.6.0/32"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("wanted routes = %v, want %v (own-address-overlapping prefix must be skipped)", got, want)
	}
}
