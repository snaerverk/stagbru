package peers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/snaerverk/stagbru/pkg/config"
)

// fakeSource delivers a fixed payload once started, then blocks until ctx
// is canceled.
type fakeSource struct {
	payload []byte
	started chan struct{}
}

func (f *fakeSource) Run(ctx context.Context, onUpdate func(data []byte)) error {
	onUpdate(f.payload)
	if f.started != nil {
		close(f.started)
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestManager_Run_StartsSourcesAndReconciles(t *testing.T) {
	src := config.PeerSource{Type: config.PeerSourceKubernetesSecret, SecretName: "wg0-conf"}
	node := &fakeReconciler{}
	started := make(chan struct{})

	m := NewManager([]NetworkSpec{{
		Name:    "default",
		Sources: []config.PeerSource{src},
		Node:    node,
	}}, func(config.PeerSource) (Source, error) {
		return &fakeSource{payload: []byte(`[` + peerJSON("peer-a", keyA, "10.0.0.1/32") + `]`), started: started}, nil
	}, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- m.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the fake source to start")
	}

	// Run's errgroup goroutine calls onUpdate before signaling started, but
	// scheduling the Reconcile call itself is still async relative to this
	// goroutine; poll briefly rather than asserting on a single read.
	deadline := time.Now().Add(2 * time.Second)
	for node.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if node.callCount() == 0 {
		t.Fatal("Reconcile was never called")
	}

	cancel()
	select {
	case err := <-runErrCh:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Errorf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}
}

func TestManager_Run_PropagatesSourceBuildError(t *testing.T) {
	src := config.PeerSource{Type: config.PeerSourceKubernetesSecret, SecretName: "wg0-conf"}
	m := NewManager([]NetworkSpec{{
		Name:    "default",
		Sources: []config.PeerSource{src},
		Node:    &fakeReconciler{},
	}}, func(config.PeerSource) (Source, error) {
		return nil, errors.New("boom")
	}, testLogger())

	if err := m.Run(context.Background()); err == nil {
		t.Fatal("Run() error = nil, want the source-build error surfaced")
	}
}

func TestManager_ResyncEvery_ReReconciles(t *testing.T) {
	src := config.PeerSource{Type: config.PeerSourceKubernetesSecret, SecretName: "wg0-conf"}
	node := &fakeReconciler{}

	m := NewManager([]NetworkSpec{{
		Name:    "default",
		Sources: []config.PeerSource{src},
		Node:    node,
	}}, nil, testLogger())

	m.onUpdate(context.Background(), src.ID(), []byte(`[`+peerJSON("peer-a", keyA, "10.0.0.1/32")+`]`))
	if node.callCount() != 1 {
		t.Fatalf("callCount after initial sync = %d, want 1", node.callCount())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	m.ResyncEvery(ctx, 20*time.Millisecond)

	if node.callCount() <= 1 {
		t.Errorf("callCount after ResyncEvery = %d, want > 1 (periodic re-reconcile)", node.callCount())
	}
}
