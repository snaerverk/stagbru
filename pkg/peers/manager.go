package peers

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/snaerverk/stagbru/pkg/config"
	"github.com/snaerverk/stagbru/pkg/wg"
)

// Reconciler is the subset of *wg.Node Manager needs. Letting tests
// substitute a fake keeps Manager's tests from needing a real kernel
// WireGuard device.
type Reconciler interface {
	Reconcile(peers []wg.Peer) error
}

// NetworkSpec is one network's peer-merge configuration, derived from a
// config.Network.
type NetworkSpec struct {
	Name     string
	SelfName string
	Sources  []config.PeerSource
	Node     Reconciler
}

// Manager watches every distinct PeerSource referenced by any configured
// network (an identical source referenced by more than one network is
// started once, see config.PeerSource.ID) and keeps each network's
// interface reconciled against the union of its own sources' peer lists,
// per docs/plan.md's "Peer reconcile" section. A failure isolated to one
// network (an invalid source, a duplicate key across its own sources)
// logs and skips that network's reconcile; it never stops other networks'.
type Manager struct {
	networks  []NetworkSpec
	newSource func(config.PeerSource) (Source, error)
	logger    *slog.Logger

	mu     sync.Mutex
	raw    map[string][]wg.Peer // source ID -> last-known valid peers
	synced map[string]bool      // source ID -> has received at least one update
}

// NewManager builds a Manager. newSource builds the Source for one
// config.PeerSource (ordinarily peers.NewSource bound to a real
// kubernetes.Interface; tests pass a fake).
func NewManager(networks []NetworkSpec, newSource func(config.PeerSource) (Source, error), logger *slog.Logger) *Manager {
	return &Manager{
		networks:  networks,
		newSource: newSource,
		logger:    logger,
		raw:       make(map[string][]wg.Peer),
		synced:    make(map[string]bool),
	}
}

// Run starts one Source per distinct PeerSource and reconciles every
// affected network on each update, until ctx is canceled or a Source
// returns an unrecoverable error (in which case every other Source is
// stopped too — see docs/plan.md's isolation note on *sources*, not
// networks: a source failing to even start is a config problem, not a
// transient one, and nothing in Run can route around a dead source).
func (m *Manager) Run(ctx context.Context) error {
	bySource := make(map[string]config.PeerSource)
	for _, n := range m.networks {
		for _, src := range n.Sources {
			bySource[src.ID()] = src
		}
	}

	g, ctx := errgroup.WithContext(ctx)
	for id, src := range bySource {
		id, src := id, src
		source, err := m.newSource(src)
		if err != nil {
			return fmt.Errorf("peers: build source %q: %w", id, err)
		}
		g.Go(func() error {
			return source.Run(ctx, func(data []byte) { m.onUpdate(ctx, id, data) })
		})
	}
	return g.Wait()
}

// onUpdate records one source's latest payload and reconciles every
// network that references it.
func (m *Manager) onUpdate(ctx context.Context, sourceID string, data []byte) {
	valid, invalid, err := Parse(sourceID, data)
	if err != nil {
		m.logger.Error("peers: dropping source update, malformed payload", "source", sourceID, "error", err)
		return
	}
	for _, inv := range invalid {
		m.logger.Warn("peers: dropping invalid peer entry", "source", inv.Source, "peer", inv.Name, "error", inv.Err)
	}

	m.mu.Lock()
	m.raw[sourceID] = valid
	m.synced[sourceID] = true
	m.mu.Unlock()

	for _, n := range m.networks {
		if !referencesSource(n, sourceID) {
			continue
		}
		m.reconcileNetwork(n)
	}
}

func referencesSource(n NetworkSpec, sourceID string) bool {
	for _, src := range n.Sources {
		if src.ID() == sourceID {
			return true
		}
	}
	return false
}

// reconcileNetwork merges one network's sources' current peer lists and
// calls its Node's Reconcile, unless some referenced source hasn't synced
// yet (in which case it's a no-op: the first sync of the last outstanding
// source will trigger it).
func (m *Manager) reconcileNetwork(n NetworkSpec) {
	merged, ready, err := m.mergedPeers(n)
	if err != nil {
		m.logger.Error("peers: reconcile skipped", "network", n.Name, "error", err)
		return
	}
	if !ready {
		return
	}

	if err := n.Node.Reconcile(merged); err != nil {
		m.logger.Error("peers: reconcile failed", "network", n.Name, "error", err)
		return
	}
	m.logger.Info("peers: reconciled", "network", n.Name, "peers", len(merged))
}

// mergedPeers unions n's sources' last-known peer lists, dropping any
// entry whose Name matches n.SelfName (defensive -- peers.json is expected
// to already exclude self upstream, see pkg/wg's doc comment) and erroring
// if the same public key appears in more than one of n's own sources
// (ambiguous, rejected rather than resolved by source order — see
// config.Network.PeersSources's doc comment). ready is false only while at
// least one referenced source hasn't delivered its first update yet.
func (m *Manager) mergedPeers(n NetworkSpec) (peers []wg.Peer, ready bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	seenBy := make(map[wgtypes.Key]string)
	var merged []wg.Peer
	for _, src := range n.Sources {
		id := src.ID()
		if !m.synced[id] {
			return nil, false, nil
		}
		for _, p := range m.raw[id] {
			if p.Name == n.SelfName {
				continue
			}
			if prevID, dup := seenBy[p.PublicKey]; dup {
				return nil, true, fmt.Errorf("network %q: public key %s appears in both source %q and %q", n.Name, p.PublicKey, prevID, id)
			}
			seenBy[p.PublicKey] = id
			merged = append(merged, p)
		}
	}
	return merged, true, nil
}

// ResyncEvery is a convenience for callers (cmd/stagbru) that also want a
// periodic re-Reconcile independent of any source's own resync, matching
// docs/plan.md's "own periodic 5-minute resync" language literally. It
// blocks until ctx is canceled.
func (m *Manager) ResyncEvery(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, n := range m.networks {
				m.reconcileNetwork(n)
			}
		}
	}
}
