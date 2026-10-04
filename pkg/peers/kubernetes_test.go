package peers

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newPeersSecret(namespace, name string, data []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string][]byte{peersSecretKey: data},
	}
}

// waitForUpdate reads one delivered payload off ch, failing the test if
// none arrives within the deadline.
func waitForUpdate(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case data := <-ch:
		return data
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for onUpdate")
		return nil
	}
}

func TestKubernetesSource_DeliversInitialAndUpdatedContent(t *testing.T) {
	const ns, name = "stagbru", "wg0-conf"
	clientset := fake.NewSimpleClientset(newPeersSecret(ns, name, []byte(`[{"name":"a"}]`)))

	src := &KubernetesSource{Clientset: clientset, Namespace: ns, Name: name, Resync: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := make(chan []byte, 10)
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- src.Run(ctx, func(data []byte) { updates <- data }) }()

	if got := waitForUpdate(t, updates); string(got) != `[{"name":"a"}]` {
		t.Fatalf("initial delivery = %q, want the Secret's initial peers.json", got)
	}

	// The fake clientset's watch only replays events emitted after a
	// watcher registers, and nothing here observes exactly when the
	// informer's Reflector gets that far — so keep updating (idempotently;
	// each call still emits a fresh Modified event) until the new content
	// is observed, rather than racing a single Update() against watch
	// registration.
	updateCtx, stopUpdating := context.WithCancel(ctx)
	defer stopUpdating()
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-updateCtx.Done():
				return
			case <-ticker.C:
				_, _ = clientset.CoreV1().Secrets(ns).Update(updateCtx, newPeersSecret(ns, name, []byte(`[{"name":"b"}]`)), metav1.UpdateOptions{})
			}
		}
	}()

	for {
		got := waitForUpdate(t, updates)
		if string(got) == `[{"name":"b"}]` {
			break
		}
	}
	stopUpdating()

	cancel()
	if err := <-runErrCh; err == nil {
		t.Error("Run() error = nil after context cancellation, want context.Canceled")
	}
}

func TestKubernetesSource_DeliversEmptyOnDelete(t *testing.T) {
	const ns, name = "stagbru", "wg0-conf"
	clientset := fake.NewSimpleClientset(newPeersSecret(ns, name, []byte(`[{"name":"a"}]`)))

	src := &KubernetesSource{Clientset: clientset, Namespace: ns, Name: name, Resync: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := make(chan []byte, 10)
	go func() { _ = src.Run(ctx, func(data []byte) { updates <- data }) }()

	waitForUpdate(t, updates) // initial sync

	// Same fake-watch registration race as the Update test above: keep
	// (re-)deleting until the delete is actually observed, rather than
	// racing a single call against watch registration. Once the Secret is
	// gone, a repeat Delete is a harmless not-found error.
	deleteCtx, stopDeleting := context.WithCancel(ctx)
	defer stopDeleting()
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-deleteCtx.Done():
				return
			case <-ticker.C:
				_ = clientset.CoreV1().Secrets(ns).Delete(deleteCtx, name, metav1.DeleteOptions{})
			}
		}
	}()

	for {
		got := waitForUpdate(t, updates)
		if got == nil {
			break
		}
	}
	stopDeleting()
}
