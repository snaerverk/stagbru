// Command stagbru runs one or more WireGuard nodes inside a Kubernetes
// pod. See docs/plan.md for the full design; this file currently wires up
// only what's built so far (pkg/config, pkg/wg) — see the TODOs below for
// what's still missing before this matches the full spec.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/snaerverk/stagbru/pkg/config"
	"github.com/snaerverk/stagbru/pkg/wg"
)

// version is overridden at build time via -ldflags "-X main.version=...";
// see docs/plan.md's Phase 3 Dockerfile/CI section.
var version = "dev"

// envVar documents one recognized environment variable for the -h/--help
// output. Keep this in sync with pkg/config's `defaults` map and
// `validate` — this list exists purely to make `stagbru --help` useful;
// pkg/config is still the single source of truth for actual behavior.
type envVar struct {
	name    string
	example string // default shown verbatim, or "(required)"
	help    string
}

var envVars = []envVar{
	{"WG_INTERFACE", "wg0", "interface name for the default network"},
	{"WG_LISTEN_PORT", "51820", "UDP listen port for the default network"},
	{"WG_SELF_SECRET", "(required)", "Secret name holding PRIVATE_KEY/IP (envFrom also acceptable; see below)"},
	{"WG_SELF_NAME", "(required)", "this node's own name, excluded from its peer list"},
	{"WG_PEERS_SECRET", "(required)", "Secret name holding the peer list, key peers.json"},
	{"WG_MASQUERADE", "true", "install `oifname <interface> masquerade` for this network"},
	{"WG_ADVERTISE_TO_TAILSCALE", "true", "union this network's peer AllowedIPs into the tailnet's advertised routes"},
	{"PRIVATE_KEY", "(required)", "this node's WireGuard private key (base64) — set via WG_SELF_SECRET's envFrom"},
	{"IP", "(required)", "this node's own WireGuard address — set via WG_SELF_SECRET's envFrom"},
	{"POD_NAMESPACE", "", "this pod's namespace (downward API)"},
	{"TS_LOGIN_SERVER", "", "headscale/Tailscale control server URL"},
	{"TS_AUTHKEY", "", "Tailscale auth key"},
	{"TS_STATE_DIR", "/var/lib/tailscale", "tailscaled state directory (PVC mount)"},
	{"TS_HOSTNAME", "", "this node's tailnet hostname"},
	{"TS_ROUTES", "172.16.6.0/24,100.192.8.0/24", "extra advertised CIDRs not already covered by a network's own peers"},
	{"HTTP_ADDR", ":9090", "probes + metrics listen address"},
	{"STAGBRU_CONFIG", "", "comma-separated YAML/TOML file path(s), later overrides earlier (see configs/config.yaml)"},
}

func usage() {
	w := flag.CommandLine.Output()
	fmt.Fprintf(w, "stagbru runs one or more WireGuard nodes inside a Kubernetes pod\nand (eventually) bridges them onto a tailnet. See docs/plan.md for the\nfull design — this build only implements interface bring-up so far.\n\n")
	fmt.Fprintf(w, "Usage:\n  stagbru [flags]\n\n")
	fmt.Fprintf(w, "Flags:\n")
	flag.PrintDefaults()
	fmt.Fprintf(w, "\nConfiguration is read from environment variables, plus an optional\nYAML/TOML file layer (STAGBRU_CONFIG — see configs/config.yaml for an\nannotated example). Recognized variables:\n\n")
	for _, e := range envVars {
		fmt.Fprintf(w, "  %-28s %-14s %s\n", e.name, e.example, e.help)
	}
	fmt.Fprintf(w, "\nWithout a wg.networks file-based override, exactly one network named\n\"default\" is synthesized from the WG_* variables above.\n")
}

func main() {
	flag.Usage = usage
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("stagbru", version)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := run(logger); err != nil {
		logger.Error("stagbru exiting", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load(flag.Args())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Startup order step 1 (docs/plan.md).
	if err := checkIPForward(); err != nil {
		return err
	}

	// Startup order step 2: create/adopt every configured network's
	// interface. All must succeed before continuing.
	nodes := make(map[string]*wg.Node, len(cfg.Networks))
	for _, netCfg := range cfg.Networks {
		nodeCfg, err := loadNodeConfig(cfg.Networks, netCfg)
		if err != nil {
			return fmt.Errorf("network %q: %w", netCfg.Name, err)
		}

		node, err := wg.NewNode(nodeCfg)
		if err != nil {
			return fmt.Errorf("network %q: bring up interface: %w", netCfg.Name, err)
		}
		nodes[netCfg.Name] = node
		logger.Info("wireguard interface up", "network", netCfg.Name, "interface", netCfg.Interface, "listen_port", netCfg.ListenPort)
	}

	// TODO(pkg/nft): startup step 3, `table inet stagbru`, not built yet.
	// TODO(pkg/peers): startup step 4, peer informer + first reconcile —
	// interfaces above are up but have no peers configured yet.
	// TODO(pkg/tailscale): startup steps 5-6, tailscaled supervision.
	// TODO(pkg/health): /healthz, /readyz, /metrics; "mark the pod ready"
	// (step 7) has nothing to report readiness through yet.

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	<-ctx.Done()
	logger.Info("shutting down")

	// Shutdown: best-effort close every network's interface even if one
	// fails (docs/plan.md's "Shutdown" section — steps 1-3 are TODOs
	// above; this is step 4).
	var errs []error
	for name, node := range nodes {
		if err := node.Close(); err != nil {
			errs = append(errs, fmt.Errorf("network %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// checkIPForward verifies net.ipv4.ip_forward=1, which an initContainer is
// expected to have already set (docs/plan.md's manifest changes).
func checkIPForward() error {
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return fmt.Errorf("check net.ipv4.ip_forward: %w", err)
	}
	if strings.TrimSpace(string(data)) != "1" {
		return errors.New("net.ipv4.ip_forward is not enabled (expected an initContainer to set it)")
	}
	return nil
}

// loadNodeConfig builds a wg.Config for one network.
//
// For the single, default network (today's real deployment — see
// docs/current-state.md), identity comes straight from this process's own
// environment: PRIVATE_KEY and IP, matching the wireguard-gateway-self
// Secret's envFrom-projected keys exactly. No Kubernetes API call is
// needed for self-identity in this case.
//
// This does NOT yet generalize to more than one simultaneously configured
// network: two networks' SelfSecrets can't both be envFrom-projected into
// the same bare PRIVATE_KEY/IP names without colliding. The fix is reading
// each network's named Secret (netCfg.SelfSecret) via the Kubernetes API,
// which needs a Kubernetes client dependency not yet added (pkg/peers,
// which will need the same kind of client for the peers Secret informer,
// doesn't exist yet either). Until then, a config with more than one
// network fails loudly here instead of silently misconfiguring one of
// them.
func loadNodeConfig(all []config.Network, netCfg config.Network) (wg.Config, error) {
	if len(all) > 1 {
		return wg.Config{}, errors.New("self-identity for more than one configured network requires reading each network's SelfSecret via the Kubernetes API, not yet implemented")
	}

	rawKey := os.Getenv("PRIVATE_KEY")
	if rawKey == "" {
		return wg.Config{}, fmt.Errorf("PRIVATE_KEY env var not set (expected via envFrom from Secret %q)", netCfg.SelfSecret)
	}
	privateKey, err := wgtypes.ParseKey(rawKey)
	if err != nil {
		return wg.Config{}, fmt.Errorf("parse PRIVATE_KEY: %w", err)
	}

	rawIP := os.Getenv("IP")
	if rawIP == "" {
		return wg.Config{}, fmt.Errorf("IP env var not set (expected via envFrom from Secret %q)", netCfg.SelfSecret)
	}
	addr, err := netip.ParseAddr(rawIP)
	if err != nil {
		return wg.Config{}, fmt.Errorf("parse IP %q: %w", rawIP, err)
	}

	// The real Secret stores a bare address with no CIDR (see
	// docs/current-state.md); historically the wg-quick template hardcoded
	// a /10 prefix for it. stagbru's own route management is entirely
	// peer-AllowedIP-driven (see pkg/wg's Reconcile), so it doesn't rely on
	// the interface's own connected route the way wg-quick's /10 implicitly
	// did — using /32 here is the safe, non-committal choice rather than
	// guessing at reproducing /10's side effects. Flagging this as a
	// deliberate behavior difference from today's deployment, not an
	// oversight: worth confirming before Phase 5's cutover (it has no
	// effect until then).
	address := netip.PrefixFrom(addr, addr.BitLen())

	return wg.Config{
		Interface:  netCfg.Interface,
		ListenPort: netCfg.ListenPort,
		PrivateKey: privateKey,
		Address:    address,
	}, nil
}
