// Package peers resolves the desired peer list for every configured
// WireGuard network (config.Network.PeersSources) from its external
// source(s), validates it, and drives pkg/wg's Node.Reconcile whenever it
// changes. See docs/plan.md's "Peer reconcile" section for the full spec
// this implements.
//
// # Source
//
// Source (source.go) is the seam between "where one peer list lives" and
// everything else in this package: each config.PeerSource entry becomes
// one Source instance, built by NewSource from that entry's Type, and
// Manager only ever asks it to watch and report that one source's raw
// peers.json-shaped bytes — it never otherwise cares what kind of source
// it's talking to.
//
// KubernetesSource (kubernetes.go) implements config.PeerSourceKubernetesSecret:
// one informer per distinct Secret name, matching docs/plan.md's
// "`internal/peers` runs one informer per distinct `peers_secret`" and the
// single-Secret `resourceNames` RBAC grant described in its "Manifest
// changes" section.
//
// config.PeerSourceOpenBao is accepted and validated by pkg/config already
// (so manifests/configs can adopt the shape ahead of time) but has no
// Source implementation yet — NewSource returns an error for it. The
// planned OpenBaoSource would poll OpenBao's KV v2 API
// (OpenBaoPeerSource.Address/Mount/Path) for the same payload instead of
// going through the ExternalSecret-synced Kubernetes Secret, authenticating
// via OpenBao's kubernetes auth method: exchanging this pod's projected
// ServiceAccount token for a short-lived OpenBao token at
// OpenBaoPeerSource.AuthMountPath/AuthRole — no static credential to
// provision, matching how the cluster already trusts pod identity for
// everything else here. Because Source's contract is just "watch, call
// onUpdate with bytes when they change", OpenBaoSource slots in beside
// KubernetesSource with no change to Manager, Parse, or cmd/stagbru's
// wiring once it's built.
package peers
