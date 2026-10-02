// Command e2e-peer is test-only scaffolding for the Phase 1 integration
// test described in docs/plan.md ("Integration test: ... Bring up two
// stagbru-like peers, check: they handshake; a peer update doesn't reset
// the other peer's handshake; a peer removal removes its routes.").
//
// It is NOT part of the production stagbru binary (see cmd/stagbru). It is
// a thin HTTP-controlled wrapper around pkg/wg.Node: it brings up one
// WireGuard interface at startup (with zero peers) and lets a test driver
// (test/e2e/run.sh) drive Reconcile calls over HTTP, simulating the peer
// updates/removals stagbru itself would apply from its Kubernetes-sourced
// peer list.
//
// Kept intentionally simple: no graceful shutdown beyond basic signal
// handling, no production config layering (koanf etc.) — just the env vars
// this test needs.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/snaerverk/stagbru/pkg/wg"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// wirePeer is the wire format POSTed to /peers by the test driver: a JSON
// array of these, one per desired peer.
type wirePeer struct {
	Name       string   `json:"name"`
	PublicKey  string   `json:"public_key"`
	AllowedIPs []string `json:"allowed_ips"`
	Endpoint   *string  `json:"endpoint"`
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("e2e-peer: %v", err)
	}
}

func run() error {
	iface := getEnvDefault("WG_INTERFACE", "wg0")
	httpAddr := getEnvDefault("HTTP_ADDR", ":8080")

	listenPortStr, err := requireEnv("WG_LISTEN_PORT")
	if err != nil {
		return err
	}
	listenPort, err := strconv.Atoi(listenPortStr)
	if err != nil {
		return fmt.Errorf("parse WG_LISTEN_PORT %q: %w", listenPortStr, err)
	}

	privateKeyStr, err := requireEnv("WG_PRIVATE_KEY")
	if err != nil {
		return err
	}
	privateKey, err := wgtypes.ParseKey(privateKeyStr)
	if err != nil {
		return fmt.Errorf("parse WG_PRIVATE_KEY: %w", err)
	}

	addressStr, err := requireEnv("WG_ADDRESS")
	if err != nil {
		return err
	}
	address, err := netip.ParsePrefix(addressStr)
	if err != nil {
		return fmt.Errorf("parse WG_ADDRESS %q: %w", addressStr, err)
	}

	node, err := wg.NewNode(wg.Config{
		Interface:  iface,
		ListenPort: listenPort,
		PrivateKey: privateKey,
		Address:    address,
	})
	if err != nil {
		return fmt.Errorf("create wg node: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /peers", func(w http.ResponseWriter, r *http.Request) {
		handlePeers(w, r, node)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		handleStatus(w, r, iface)
	})

	srv := &http.Server{Addr: httpAddr, Handler: mux}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("e2e-peer: listening on %s (interface %s)", httpAddr, iface)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-errCh:
		return err
	case <-sigCh:
		log.Printf("e2e-peer: signal received, shutting down")
	}

	if err := node.Close(); err != nil {
		log.Printf("e2e-peer: close node: %v", err)
	}
	return nil
}

func handlePeers(w http.ResponseWriter, r *http.Request, node *wg.Node) {
	var wire []wirePeer
	if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
		http.Error(w, fmt.Sprintf("decode body: %v", err), http.StatusBadRequest)
		return
	}

	peers := make([]wg.Peer, 0, len(wire))
	for _, wp := range wire {
		peer, err := wirePeerToPeer(wp)
		if err != nil {
			http.Error(w, fmt.Sprintf("peer %q: %v", wp.Name, err), http.StatusBadRequest)
			return
		}
		peers = append(peers, peer)
	}

	if err := node.Reconcile(peers); err != nil {
		http.Error(w, fmt.Sprintf("reconcile: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func wirePeerToPeer(wp wirePeer) (wg.Peer, error) {
	pubKey, err := wgtypes.ParseKey(wp.PublicKey)
	if err != nil {
		return wg.Peer{}, fmt.Errorf("parse public_key: %w", err)
	}

	allowed := make([]netip.Prefix, 0, len(wp.AllowedIPs))
	for _, a := range wp.AllowedIPs {
		prefix, err := netip.ParsePrefix(a)
		if err != nil {
			return wg.Peer{}, fmt.Errorf("parse allowed_ip %q: %w", a, err)
		}
		allowed = append(allowed, prefix)
	}

	var endpoint *net.UDPAddr
	if wp.Endpoint != nil && *wp.Endpoint != "" {
		udpAddr, err := net.ResolveUDPAddr("udp", *wp.Endpoint)
		if err != nil {
			return wg.Peer{}, fmt.Errorf("resolve endpoint %q: %w", *wp.Endpoint, err)
		}
		endpoint = udpAddr
	}

	return wg.Peer{
		Name:       wp.Name,
		PublicKey:  pubKey,
		AllowedIPs: allowed,
		Endpoint:   endpoint,
	}, nil
}

func handleStatus(w http.ResponseWriter, r *http.Request, iface string) {
	out, err := exec.Command("wg", "show", iface, "dump").CombinedOutput()
	if err != nil {
		http.Error(w, fmt.Sprintf("wg show %s dump: %v\n%s", iface, err, out), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func requireEnv(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("required env var %s not set", key)
	}
	return v, nil
}
