package peers

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// peersSecretKey is the Secret data key holding the peers.json payload
// (docs/plan.md's "Manifest changes" section).
const peersSecretKey = "peers.json"

// defaultResync is the informer's full relist interval. Every relist
// re-delivers an Update event for the watched Secret even when nothing
// changed, which is what gives each network its "own periodic 5-minute
// resync" safety net from docs/plan.md's "Peer reconcile" section, without
// Manager needing its own separate timer.
const defaultResync = 5 * time.Minute

// KubernetesSource is a Source backed by one Kubernetes Secret, watched via
// its own informer scoped to exactly that Secret's name (a field selector
// on metadata.name) — this is what lets the RBAC grant be
// get/list/watch with `resourceNames: [<Name>]` rather than namespace-wide
// (docs/plan.md's "Manifest changes" section), and matches "one informer
// per distinct peers_secret" from its "Generalize internal/wg" section.
//
// Built on the typed Secrets(namespace) List/Watch calls (rather than a raw
// RESTClient ListWatch) specifically so it's exercisable against
// k8s.io/client-go/kubernetes/fake in tests — a fake backend doesn't honor
// the field selector (a known client-go fake limitation: only label
// selectors are filtered, see kubernetes_test.go), but the selector is
// still sent on every real request, which is what the RBAC grant above
// actually depends on.
type KubernetesSource struct {
	Clientset kubernetes.Interface
	Namespace string
	Name      string

	// Resync overrides defaultResync; zero means use the default.
	Resync time.Duration
}

// Run implements Source.
func (s *KubernetesSource) Run(ctx context.Context, onUpdate func(data []byte)) error {
	resync := s.Resync
	if resync == 0 {
		resync = defaultResync
	}

	selector := fields.OneTermEqualSelector("metadata.name", s.Name).String()
	secrets := s.Clientset.CoreV1().Secrets(s.Namespace)

	lw := &cache.ListWatch{
		ListFunc: func(opts metav1.ListOptions) (runtime.Object, error) {
			opts.FieldSelector = selector
			return secrets.List(ctx, opts)
		},
		WatchFunc: func(opts metav1.ListOptions) (watch.Interface, error) {
			opts.FieldSelector = selector
			return secrets.Watch(ctx, opts)
		},
	}

	deliver := func(obj any) {
		if secret, ok := obj.(*corev1.Secret); ok {
			onUpdate(secret.Data[peersSecretKey])
		}
	}

	_, controller := cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: lw,
		ObjectType:    &corev1.Secret{},
		ResyncPeriod:  resync,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    deliver,
			UpdateFunc: func(_, newObj any) { deliver(newObj) },
			// A deleted Secret means no peers: report an empty payload
			// rather than leaving Manager's last-known list stale.
			DeleteFunc: func(any) { onUpdate(nil) },
		},
	})

	controller.Run(ctx.Done())
	return ctx.Err()
}
