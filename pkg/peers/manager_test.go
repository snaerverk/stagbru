package peers

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/snaerverk/stagbru/pkg/config"
	"github.com/snaerverk/stagbru/pkg/wg"
)

// fakeReconciler records every Reconcile call it receives.
type fakeReconciler struct {
	mu    sync.Mutex
	calls [][]wg.Peer
	err   error
}

func (f *fakeReconciler) Reconcile(peers []wg.Peer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, peers)
	return f.err
}

func (f *fakeReconciler) lastCall() ([]wg.Peer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil, false
	}
	return f.calls[len(f.calls)-1], true
}

func (f *fakeReconciler) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func peerJSON(name, pubkey, allowedIP string) string {
	return `{"name": "` + name + `", "public_key": "` + pubkey + `", "allowed_ips": ["` + allowedIP + `"]}`
}

const (
	keyA = "5nugtxtT5TAp2VF5Gns5zpHQRT0hQuF/ygU5pNGdqEw="
	keyB = "aecnfF7mIaRnImSJChoUWnidkQN5h2bPcKdjbjdLBVQ="
	keyC = "CPoM5y/cJPS9mc52uim6Q9l3xKw4Au22hWJ5+Sgwo2A="
)

func TestManager_UnionsMultipleSourcesForOneNetwork(t *testing.T) {
	srcA := config.PeerSource{Type: config.PeerSourceKubernetesSecret, SecretName: "secret-a"}
	srcB := config.PeerSource{Type: config.PeerSourceKubernetesSecret, SecretName: "secret-b"}
	node := &fakeReconciler{}

	m := NewManager([]NetworkSpec{{
		Name:    "default",
		Sources: []config.PeerSource{srcA, srcB},
		Node:    node,
	}}, nil, testLogger())

	ctx := context.Background()
	m.onUpdate(ctx, srcA.ID(), []byte("["+peerJSON("peer-a", keyA, "10.0.0.1/32")+"]"))
	if node.callCount() != 0 {
		t.Fatalf("Reconcile called after only one of two sources synced")
	}

	m.onUpdate(ctx, srcB.ID(), []byte("["+peerJSON("peer-b", keyB, "10.0.0.2/32")+"]"))
	call, ok := node.lastCall()
	if !ok {
		t.Fatalf("Reconcile not called once both sources synced")
	}
	if len(call) != 2 {
		t.Fatalf("Reconcile called with %d peers, want 2 (union): %+v", len(call), call)
	}
}

func TestManager_ExcludesSelfByName(t *testing.T) {
	src := config.PeerSource{Type: config.PeerSourceKubernetesSecret, SecretName: "secret-a"}
	node := &fakeReconciler{}

	m := NewManager([]NetworkSpec{{
		Name:     "default",
		SelfName: "me",
		Sources:  []config.PeerSource{src},
		Node:     node,
	}}, nil, testLogger())

	payload := "[" + peerJSON("me", keyA, "10.0.0.1/32") + "," + peerJSON("peer-b", keyB, "10.0.0.2/32") + "]"
	m.onUpdate(context.Background(), src.ID(), []byte(payload))

	call, ok := node.lastCall()
	if !ok {
		t.Fatal("Reconcile not called")
	}
	if len(call) != 1 || call[0].Name != "peer-b" {
		t.Errorf("Reconcile called with %+v, want only peer-b (self excluded)", call)
	}
}

func TestManager_DuplicateKeyAcrossOwnSourcesSkipsReconcile(t *testing.T) {
	srcA := config.PeerSource{Type: config.PeerSourceKubernetesSecret, SecretName: "secret-a"}
	srcB := config.PeerSource{Type: config.PeerSourceKubernetesSecret, SecretName: "secret-b"}
	node := &fakeReconciler{}

	m := NewManager([]NetworkSpec{{
		Name:    "default",
		Sources: []config.PeerSource{srcA, srcB},
		Node:    node,
	}}, nil, testLogger())

	ctx := context.Background()
	m.onUpdate(ctx, srcA.ID(), []byte("["+peerJSON("peer-a", keyA, "10.0.0.1/32")+"]"))
	// Same public key reappears under a different source -- ambiguous,
	// must not reconcile.
	m.onUpdate(ctx, srcB.ID(), []byte("["+peerJSON("peer-a-again", keyA, "10.0.0.9/32")+"]"))

	if node.callCount() != 0 {
		t.Errorf("Reconcile called %d times, want 0 (duplicate key across sources must be rejected)", node.callCount())
	}
}

func TestManager_SharedSourceReconcilesBothNetworks(t *testing.T) {
	shared := config.PeerSource{Type: config.PeerSourceKubernetesSecret, SecretName: "shared-secret"}
	nodeA := &fakeReconciler{}
	nodeB := &fakeReconciler{}

	m := NewManager([]NetworkSpec{
		{Name: "net-a", Sources: []config.PeerSource{shared}, Node: nodeA},
		{Name: "net-b", Sources: []config.PeerSource{shared}, Node: nodeB},
	}, nil, testLogger())

	m.onUpdate(context.Background(), shared.ID(), []byte("["+peerJSON("peer-a", keyA, "10.0.0.1/32")+"]"))

	if nodeA.callCount() != 1 || nodeB.callCount() != 1 {
		t.Errorf("callCounts = %d, %d, want 1, 1 (both networks reference the shared source)", nodeA.callCount(), nodeB.callCount())
	}
}

func TestManager_InvalidEntriesAreDroppedNotFatal(t *testing.T) {
	src := config.PeerSource{Type: config.PeerSourceKubernetesSecret, SecretName: "secret-a"}
	node := &fakeReconciler{}

	m := NewManager([]NetworkSpec{{
		Name:    "default",
		Sources: []config.PeerSource{src},
		Node:    node,
	}}, nil, testLogger())

	payload := `[` + peerJSON("good", keyC, "10.0.0.3/32") + `,{"name": "bad", "public_key": "not-valid", "allowed_ips": ["10.0.0.4/32"]}]`
	m.onUpdate(context.Background(), src.ID(), []byte(payload))

	call, ok := node.lastCall()
	if !ok {
		t.Fatal("Reconcile not called")
	}
	if len(call) != 1 || call[0].Name != "good" {
		t.Errorf("Reconcile called with %+v, want only the valid entry", call)
	}
}
