package peers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/snaerverk/stagbru/pkg/wg"
)

// wireEntry is one peers.json array entry (docs/plan.md's "Manifest
// changes" section: `{name, public_key, allowed_ips[], endpoint|null}`).
type wireEntry struct {
	Name       string   `json:"name"`
	PublicKey  string   `json:"public_key"`
	AllowedIPs []string `json:"allowed_ips"`
	Endpoint   *string  `json:"endpoint"`
}

// InvalidEntry describes one peers.json entry a Parse call dropped rather
// than failing the whole source on.
type InvalidEntry struct {
	Source string
	Name   string
	Err    error
}

func (e InvalidEntry) String() string {
	return fmt.Sprintf("source %q peer %q: %v", e.Source, e.Name, e.Err)
}

// Parse decodes one source's peers.json payload into valid wg.Peer values.
// Per docs/plan.md's "Peer reconcile" section, an individually invalid
// entry (bad key, unparseable CIDR, unresolvable endpoint) is dropped, not
// a failure of the whole batch — only a malformed top-level document (not
// a JSON array of objects) is a hard error. sourceID is used only to label
// InvalidEntry for logging.
func Parse(sourceID string, data []byte) (valid []wg.Peer, invalid []InvalidEntry, err error) {
	var entries []wireEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, nil, fmt.Errorf("peers: source %q: decode peers.json: %w", sourceID, err)
	}

	for _, e := range entries {
		peer, err := parseEntry(e)
		if err != nil {
			invalid = append(invalid, InvalidEntry{Source: sourceID, Name: e.Name, Err: err})
			continue
		}
		valid = append(valid, peer)
	}
	return valid, invalid, nil
}

func parseEntry(e wireEntry) (wg.Peer, error) {
	if e.PublicKey == "" {
		return wg.Peer{}, errors.New("public_key: empty")
	}
	key, err := wgtypes.ParseKey(e.PublicKey)
	if err != nil {
		return wg.Peer{}, fmt.Errorf("public_key: %w", err)
	}

	if len(e.AllowedIPs) == 0 {
		return wg.Peer{}, errors.New("allowed_ips: empty")
	}
	allowed := make([]netip.Prefix, 0, len(e.AllowedIPs))
	for _, raw := range e.AllowedIPs {
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			return wg.Peer{}, fmt.Errorf("allowed_ips: %q: %w", raw, err)
		}
		allowed = append(allowed, p)
	}

	var endpoint *net.UDPAddr
	if e.Endpoint != nil && *e.Endpoint != "" {
		addr, err := net.ResolveUDPAddr("udp", *e.Endpoint)
		if err != nil {
			return wg.Peer{}, fmt.Errorf("endpoint %q: %w", *e.Endpoint, err)
		}
		endpoint = addr
	}

	return wg.Peer{
		Name:       e.Name,
		PublicKey:  key,
		AllowedIPs: allowed,
		Endpoint:   endpoint,
	}, nil
}
