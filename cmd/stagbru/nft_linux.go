//go:build linux

package main

import (
	"fmt"

	"github.com/google/nftables"

	"github.com/snaerverk/stagbru/pkg/config"
	"github.com/snaerverk/stagbru/pkg/nft"
)

// installNFTables brings up `table inet stagbru` from the configured
// networks (startup order step 3, docs/plan.md), and returns a cleanup
// func that removes it again (shutdown step 3).
//
// Kept in its own linux-only file (see pkg/nft's doc comment for why
// pkg/nft itself can't build on other GOOS) so main.go -- and therefore
// `go build ./...`/`go vet ./...` for the whole module -- stays buildable
// from a non-Linux workstation; nft_other.go supplies the same signature's
// stub for everywhere else.
func installNFTables(networks []config.Network) (cleanup func() error, err error) {
	conn, err := nftables.New()
	if err != nil {
		return nil, fmt.Errorf("nft: open netlink connection: %w", err)
	}

	nftNetworks := make([]nft.Network, 0, len(networks))
	for _, n := range networks {
		nftNetworks = append(nftNetworks, nft.Network{Interface: n.Interface, Masquerade: n.Masquerade})
	}

	if err := nft.Reconcile(conn, nftNetworks, nft.TailscaleInterface); err != nil {
		return nil, err
	}

	return func() error { return nft.Delete(conn) }, nil
}
