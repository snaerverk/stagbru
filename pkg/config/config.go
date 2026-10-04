// Package config loads and validates stagbru's runtime configuration.
//
// See docs/plan.md's "Configuration" and "Generalize `internal/wg` to manage
// multiple WireGuard networks" sections for the full spec this implements.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// defaults are stagbru's compiled-in configuration defaults, keyed by the
// same bare env-var-style names the env provider below reads (no
// STAGBRU_ prefix, no case-folding — see docs/plan.md's "Configuration"
// section). Fields with no sensible cluster-wide default (secrets, names,
// tailnet identity) are deliberately absent here; Load's validation step
// rejects a Config that's still missing them.
var defaults = map[string]any{
	"WG_INTERFACE":              "wg0",
	"WG_LISTEN_PORT":            51820,
	"WG_MASQUERADE":             true,
	"WG_ADVERTISE_TO_TAILSCALE": true,
	"TS_STATE_DIR":              "/var/lib/tailscale",
	"TS_ROUTES":                 "172.16.6.0/24,100.192.8.0/24",
	"HTTP_ADDR":                 ":9090",
}

// Network describes one WireGuard network stagbru manages. Today's real
// deployment has exactly one; the zero-config fallback (no config file, no
// `networks` list) synthesizes a single Network named "default" from flat
// env vars (WG_INTERFACE, WG_LISTEN_PORT, WG_SELF_SECRET, WG_PEERS_SECRETS,
// WG_SELF_NAME, WG_MASQUERADE, WG_ADVERTISE_TO_TAILSCALE).
type Network struct {
	// Name is this network's logical key: used as a metrics/log label.
	Name string `koanf:"name"`

	// Interface is the kernel interface name. Must be unique across all
	// configured networks and at most 15 bytes (Linux IFNAMSIZ - 1).
	Interface string `koanf:"interface"`

	// ListenPort is this network's UDP listen port. Must be unique across
	// all configured networks.
	ListenPort int `koanf:"listen_port"`

	// SelfSecret is the Kubernetes Secret name holding this node's own
	// identity on this network: keys PRIVATE_KEY and IP (envFrom-shaped,
	// matching today's wireguard-gateway-self Secret — see
	// docs/current-state.md).
	SelfSecret string `koanf:"self_secret"`

	// SelfName excludes this node from its own peer list (defensive;
	// peers.json is expected to already exclude self upstream).
	SelfName string `koanf:"self_name"`

	// PeersSources is this network's desired peer list, as one or more
	// typed sources whose contents are unioned into one desired peer set
	// (see pkg/peers). Two networks may list an identical source (e.g. the
	// same Kubernetes Secret) — pkg/peers runs it once and shares the
	// result. A public key appearing in more than one of a network's own
	// sources is a validation error at reconcile time (pkg/peers), not
	// silently resolved by precedence.
	PeersSources []PeerSource `koanf:"peers_sources"`

	// Masquerade installs `oifname Interface masquerade` in the shared
	// nftables table for this network. See docs/plan.md's "Shared
	// plumbing" section for why this is needed at all.
	Masquerade bool `koanf:"masquerade"`

	// AdvertiseToTailscale unions this network's live peer AllowedIPs into
	// the tailnet's advertised routes (see docs/plan.md's "Tailscale
	// supervision" section).
	AdvertiseToTailscale bool `koanf:"advertise_to_tailscale"`
}

// PeerSourceKubernetesSecret and PeerSourceOpenBao are the recognized
// PeerSource.Type values.
const (
	// PeerSourceKubernetesSecret reads a Kubernetes Secret (key
	// "peers.json") via an informer. Implemented in pkg/peers
	// (KubernetesSource). Requires SecretName.
	PeerSourceKubernetesSecret = "kubernetes_secret"

	// PeerSourceOpenBao polls OpenBao's KV v2 API directly for the same
	// peers.json-shaped payload, skipping the Kubernetes
	// Secret/ExternalSecret hop. Accepted and validated here so manifests
	// can adopt the shape ahead of time, but not yet runnable — see
	// pkg/peers/doc.go for the planned implementation (OpenBao's
	// kubernetes auth method, no static credential). Requires OpenBao.
	PeerSourceOpenBao = "openbao"
)

// PeerSource is one source of peer data for a network. Which fields are
// required depends on Type — see PeerSourceKubernetesSecret and
// PeerSourceOpenBao.
type PeerSource struct {
	Type string `koanf:"type"`

	// SecretName is required when Type == PeerSourceKubernetesSecret: the
	// Kubernetes Secret name (in this pod's own namespace) holding the
	// peer list, key "peers.json".
	SecretName string `koanf:"secret_name"`

	// OpenBao is required when Type == PeerSourceOpenBao.
	OpenBao *OpenBaoPeerSource `koanf:"openbao"`
}

// OpenBaoPeerSource configures a PeerSourceOpenBao source: where to read
// the peer list from OpenBao, and how to authenticate.
type OpenBaoPeerSource struct {
	// Address is the OpenBao server's API address, e.g.
	// "https://openbao.internal:8200".
	Address string `koanf:"address"`

	// Mount is the KV v2 secrets engine mount point, e.g. "infra".
	Mount string `koanf:"mount"`

	// Path is the secret path under Mount holding this network's
	// peers.json-shaped payload, e.g. "network/<mesh>/peers".
	Path string `koanf:"path"`

	// AuthRole is the OpenBao kubernetes auth method role stagbru
	// authenticates as, exchanging this pod's projected ServiceAccount
	// token for a short-lived OpenBao token.
	AuthRole string `koanf:"auth_role"`

	// AuthMountPath is the kubernetes auth method's mount path. Defaults
	// to "kubernetes" if empty.
	AuthMountPath string `koanf:"auth_mount_path"`
}

// ID returns a stable identity for this source: used to dedupe an
// identical source referenced by more than one network (so pkg/peers runs
// it once and shares the result, rather than watching/polling it twice)
// and as its log label.
func (p PeerSource) ID() string {
	switch p.Type {
	case PeerSourceKubernetesSecret:
		return PeerSourceKubernetesSecret + ":" + p.SecretName
	case PeerSourceOpenBao:
		if p.OpenBao == nil {
			return PeerSourceOpenBao + ":<unconfigured>"
		}
		return fmt.Sprintf("%s:%s/%s/%s", PeerSourceOpenBao, p.OpenBao.Address, p.OpenBao.Mount, p.OpenBao.Path)
	default:
		return "unknown:" + p.Type
	}
}

// Tailscale holds the supervised tailscaled's configuration.
type Tailscale struct {
	LoginServer string
	AuthKey     string
	StateDir    string
	Hostname    string

	// Routes are *extra* advertised CIDRs not already covered by any
	// network's own peer AllowedIPs (see docs/plan.md: today's hardcoded
	// local-only routes, e.g. the DMZ subnet). Union'd in regardless of any
	// network's AdvertiseToTailscale.
	Routes []netip.Prefix
}

// Config is stagbru's fully resolved, validated runtime configuration.
type Config struct {
	Networks     []Network
	PodNamespace string
	HTTPAddr     string
	Tailscale    Tailscale
}

// Load builds a Config from (in increasing priority order) compiled-in
// defaults, environment variables, and CLI flags, and validates it.
//
// Validation performed here is limited to what's knowable from config
// alone: every Network's Name/Interface/ListenPort is unique across the
// list, every Interface is <= 15 bytes, and every Network has a non-empty
// SelfSecret/SelfName and at least one well-formed PeersSources entry. The
// cross-network "no two networks'
// peer AllowedIPs overlap" check from docs/plan.md happens later, once
// peer data is actually available (it is NOT a config-time check — peers
// come from a separate, dynamically-updated source, not this Config).
func Load(args []string) (*Config, error) {
	// args is reserved for a future koanf posflag (CLI flag) layer, the
	// highest-priority provider in docs/plan.md's ordering. No flag
	// parsing is implemented yet, so it's unused today.
	_ = args

	k := koanf.New(".")

	if err := k.Load(confmap.Provider(defaults, "."), nil); err != nil {
		return nil, fmt.Errorf("config: loading defaults: %w", err)
	}

	// Optional file layer, sourced from STAGBRU_CONFIG (comma-separated,
	// later files override earlier ones) -- this is the only way to
	// express more than one WireGuard network (see "Generalize
	// internal/wg" in docs/plan.md: wg.networks is a list of structs,
	// which a flat env-var scheme can't spell). A path here is read
	// directly from the process environment, not through koanf, because
	// it controls which files koanf itself goes on to load. Unset/empty
	// means no file layer at all -- today's real deployment (flat env
	// vars only, see docs/current-state.md) works unchanged.
	for _, path := range configPaths() {
		parser, err := parserFor(path)
		if err != nil {
			return nil, err
		}
		if err := k.Load(file.Provider(path), parser); err != nil {
			return nil, fmt.Errorf("config: loading file %q: %w", path, err)
		}
	}

	// Bare env var names (e.g. WG_INTERFACE), no STAGBRU_ prefix (other
	// than STAGBRU_CONFIG itself, read above): this is the actual
	// in-cluster configuration surface today (see docs/current-state.md's
	// Deployment env tables). Loaded after any config file so an operator
	// can still override a file's values from the pod's env without
	// editing the file.
	if err := k.Load(env.Provider("", ".", nil), nil); err != nil {
		return nil, fmt.Errorf("config: loading environment: %w", err)
	}

	// []Network: "wg.networks" is a dotted key path a future koanf `file`
	// provider (YAML/TOML) can populate directly (see docs/plan.md's
	// "Generalize internal/wg" section) — koanf's plain env.Provider has
	// no way to express a list of structs from flat env vars, so in
	// practice this branch is only ever reachable once a file provider is
	// wired in. Until then, every real deployment falls through to the
	// flat-env-var synthesis below.
	var networks []Network
	if k.Exists("wg.networks") {
		if err := k.Unmarshal("wg.networks", &networks); err != nil {
			return nil, fmt.Errorf("config: decoding wg.networks: %w", err)
		}
	}
	if len(networks) == 0 {
		networks = []Network{{
			Name:                 "default",
			Interface:            k.String("WG_INTERFACE"),
			ListenPort:           k.Int("WG_LISTEN_PORT"),
			SelfSecret:           k.String("WG_SELF_SECRET"),
			SelfName:             k.String("WG_SELF_NAME"),
			PeersSources:         peerSourcesFromEnv(k),
			Masquerade:           k.Bool("WG_MASQUERADE"),
			AdvertiseToTailscale: k.Bool("WG_ADVERTISE_TO_TAILSCALE"),
		}}
	}

	routes, err := parseRoutes(k.String("TS_ROUTES"))
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Networks:     networks,
		PodNamespace: k.String("POD_NAMESPACE"),
		HTTPAddr:     k.String("HTTP_ADDR"),
		Tailscale: Tailscale{
			LoginServer: k.String("TS_LOGIN_SERVER"),
			AuthKey:     k.String("TS_AUTHKEY"),
			StateDir:    k.String("TS_STATE_DIR"),
			Hostname:    k.String("TS_HOSTNAME"),
			Routes:      routes,
		},
	}

	if err := validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// peerSourcesFromEnv reads the default (zero-config) network's peer
// sources. The flat env-var scheme only ever expresses
// PeerSourceKubernetesSecret entries — anything else (e.g. PeerSourceOpenBao)
// requires the wg.networks file layer, since it needs structured
// per-source fields a flat scheme can't spell (same reasoning as
// docs/plan.md's "Generalize internal/wg" section). WG_PEERS_SECRETS
// (comma-separated) is preferred; WG_PEERS_SECRET (singular) is kept as a
// fallback so today's real deployment (one secret, see
// docs/current-state.md) needs no manifest change. Both set, or neither,
// falls through to validate's "empty peers_sources" error.
func peerSourcesFromEnv(k *koanf.Koanf) []PeerSource {
	var names []string
	if raw := k.String("WG_PEERS_SECRETS"); raw != "" {
		names = splitCSV(raw)
	} else if single := k.String("WG_PEERS_SECRET"); single != "" {
		names = []string{single}
	}
	if len(names) == 0 {
		return nil
	}
	sources := make([]PeerSource, len(names))
	for i, name := range names {
		sources[i] = PeerSource{Type: PeerSourceKubernetesSecret, SecretName: name}
	}
	return sources
}

// splitCSV splits a comma-separated string, trimming whitespace and
// dropping empty fields.
func splitCSV(s string) []string {
	var out []string
	for field := range strings.SplitSeq(s, ",") {
		field = strings.TrimSpace(field)
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}

// configPaths returns the ordered list of config file paths to load, from
// the STAGBRU_CONFIG env var: comma-separated, later paths override
// earlier ones (same precedence a later koanf.Load call always has). An
// unset or blank STAGBRU_CONFIG returns nil -- no file layer at all.
func configPaths() []string {
	raw := os.Getenv("STAGBRU_CONFIG")
	if raw == "" {
		return nil
	}
	var paths []string
	for field := range strings.SplitSeq(raw, ",") {
		field = strings.TrimSpace(field)
		if field != "" {
			paths = append(paths, field)
		}
	}
	return paths
}

// parserFor picks a koanf parser by file extension: .yaml/.yml -> YAML,
// .toml -> TOML. Any other extension is a hard error -- an explicitly
// listed config file with an extension we don't understand is almost
// certainly a typo, not something to silently ignore.
func parserFor(path string) (koanf.Parser, error) {
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".yaml", ".yml":
		return yaml.Parser(), nil
	case ".toml":
		return toml.Parser(), nil
	default:
		return nil, fmt.Errorf("config: file %q has unsupported extension %q (expected .yaml, .yml, or .toml)", path, ext)
	}
}

// parseRoutes parses a comma-separated CIDR list (TS_ROUTES) into
// netip.Prefix values. Empty entries (from a blank or trailing-comma
// string) are skipped; any entry that fails to parse is a hard error.
func parseRoutes(s string) ([]netip.Prefix, error) {
	var routes []netip.Prefix
	for field := range strings.SplitSeq(s, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		p, err := netip.ParsePrefix(field)
		if err != nil {
			return nil, fmt.Errorf("config: invalid TS_ROUTES entry %q: %w", field, err)
		}
		routes = append(routes, p)
	}
	return routes, nil
}

// validate checks everything knowable from config alone. See Load's doc
// comment for what's deliberately out of scope here (cross-network peer
// AllowedIPs overlap).
func validate(cfg *Config) error {
	if len(cfg.Networks) == 0 {
		return fmt.Errorf("config: no WireGuard networks configured")
	}

	names := make(map[string]bool, len(cfg.Networks))
	interfaces := make(map[string]bool, len(cfg.Networks))
	ports := make(map[int]bool, len(cfg.Networks))

	for _, n := range cfg.Networks {
		if n.Name == "" {
			return fmt.Errorf("config: network with empty name")
		}
		if names[n.Name] {
			return fmt.Errorf("config: duplicate network name %q", n.Name)
		}
		names[n.Name] = true

		if n.Interface == "" {
			return fmt.Errorf("config: network %q: empty interface", n.Name)
		}
		if len(n.Interface) > 15 {
			return fmt.Errorf("config: network %q: interface %q exceeds 15 bytes (Linux IFNAMSIZ-1)", n.Name, n.Interface)
		}
		if interfaces[n.Interface] {
			return fmt.Errorf("config: duplicate interface %q (network %q)", n.Interface, n.Name)
		}
		interfaces[n.Interface] = true

		if ports[n.ListenPort] {
			return fmt.Errorf("config: duplicate listen_port %d (network %q)", n.ListenPort, n.Name)
		}
		ports[n.ListenPort] = true

		if n.SelfSecret == "" {
			return fmt.Errorf("config: network %q: empty self_secret", n.Name)
		}
		if n.SelfName == "" {
			return fmt.Errorf("config: network %q: empty self_name", n.Name)
		}
		if len(n.PeersSources) == 0 {
			return fmt.Errorf("config: network %q: empty peers_sources", n.Name)
		}
		seen := make(map[string]bool, len(n.PeersSources))
		for i := range n.PeersSources {
			if err := validatePeerSource(&n.PeersSources[i]); err != nil {
				return fmt.Errorf("config: network %q: peers_sources[%d]: %w", n.Name, i, err)
			}
			id := n.PeersSources[i].ID()
			if seen[id] {
				return fmt.Errorf("config: network %q: duplicate peers_sources entry %q", n.Name, id)
			}
			seen[id] = true
		}
	}

	return nil
}

// validatePeerSource checks one PeerSource against its Type's required
// fields, and fills AuthMountPath's default in place.
func validatePeerSource(p *PeerSource) error {
	switch p.Type {
	case PeerSourceKubernetesSecret:
		if p.SecretName == "" {
			return fmt.Errorf("type %q requires secret_name", p.Type)
		}
	case PeerSourceOpenBao:
		if p.OpenBao == nil {
			return fmt.Errorf("type %q requires an openbao block", p.Type)
		}
		if p.OpenBao.Address == "" || p.OpenBao.Mount == "" || p.OpenBao.Path == "" || p.OpenBao.AuthRole == "" {
			return fmt.Errorf("type %q requires openbao.address, openbao.mount, openbao.path, and openbao.auth_role", p.Type)
		}
		if p.OpenBao.AuthMountPath == "" {
			p.OpenBao.AuthMountPath = "kubernetes"
		}
	case "":
		return fmt.Errorf("empty type (expected %q or %q)", PeerSourceKubernetesSecret, PeerSourceOpenBao)
	default:
		return fmt.Errorf("unknown type %q (expected %q or %q)", p.Type, PeerSourceKubernetesSecret, PeerSourceOpenBao)
	}
	return nil
}
