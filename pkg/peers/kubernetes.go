package peers

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
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

	selector := fields.OneTermEqualSelector("metadata.name", s.Name)
	lw := cache.NewListWatchFromClient(s.Clientset.CoreV1().RESTClient(), "secrets", s.Namespace, selector)

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
