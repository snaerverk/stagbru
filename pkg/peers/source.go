package peers

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"

	"github.com/snaerverk/stagbru/pkg/config"
)

// Source watches one config.PeerSource and reports its raw peers.json
// payload whenever it changes, including once for its initial state. Run
// blocks until ctx is canceled or an unrecoverable error occurs; a
// recoverable error (a transient API error, a malformed payload) should be
// logged and retried internally, not returned. See pkg/peers/doc.go for
// why this interface exists.
type Source interface {
	Run(ctx context.Context, onUpdate func(data []byte)) error
}

// NewSource builds the Source for one config.PeerSource entry, by its
// Type. clientset and namespace are only used by
// config.PeerSourceKubernetesSecret.
func NewSource(src config.PeerSource, clientset kubernetes.Interface, namespace string) (Source, error) {
	switch src.Type {
	case config.PeerSourceKubernetesSecret:
		if src.SecretName == "" {
			return nil, fmt.Errorf("peers: %s source: empty secret_name", config.PeerSourceKubernetesSecret)
		}
		return &KubernetesSource{
			Clientset: clientset,
			Namespace: namespace,
			Name:      src.SecretName,
		}, nil
	case config.PeerSourceOpenBao:
		// Accepted by pkg/config (see config.PeerSourceOpenBao's doc
		// comment) so manifests/configs can adopt the shape ahead of
		// time, but not yet implemented -- see pkg/peers/doc.go for the
		// planned design. Failing loudly here (rather than silently
		// skipping the source) matches this repo's existing rule for
		// not-yet-supported config (see cmd/stagbru's multi-network
		// self-identity TODO).
		return nil, fmt.Errorf("peers: source type %q is accepted by config but not yet implemented (see pkg/peers/doc.go)", src.Type)
	default:
		return nil, fmt.Errorf("peers: unknown source type %q", src.Type)
	}
}
