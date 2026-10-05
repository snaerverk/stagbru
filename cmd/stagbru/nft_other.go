//go:build !linux

package main

import (
	"errors"
	"runtime"

	"github.com/snaerverk/stagbru/pkg/config"
)

// installNFTables stub for non-Linux GOOS -- see nft_linux.go. stagbru
// already requires real Linux kernel facilities for wg.NewNode
// (netlink/wgctrl) and checkIPForward (/proc/sys/net/ipv4/ip_forward), so
// this isn't a new restriction; it only keeps this file's build tag
// symmetric and the error message specific to what's missing here.
func installNFTables(networks []config.Network) (cleanup func() error, err error) {
	return nil, errors.New("nftables management is only supported on linux, not " + runtime.GOOS)
}
