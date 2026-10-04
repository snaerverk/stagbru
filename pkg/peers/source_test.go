package peers

import (
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/snaerverk/stagbru/pkg/config"
)

func TestNewSource_KubernetesSecret(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	src, err := NewSource(config.PeerSource{Type: config.PeerSourceKubernetesSecret, SecretName: "wg0-conf"}, clientset, "stagbru")
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}
	k, ok := src.(*KubernetesSource)
	if !ok {
		t.Fatalf("NewSource() = %T, want *KubernetesSource", src)
	}
	if k.Name != "wg0-conf" || k.Namespace != "stagbru" {
		t.Errorf("KubernetesSource = %+v, want Name=wg0-conf Namespace=stagbru", k)
	}
}

func TestNewSource_KubernetesSecretMissingName(t *testing.T) {
	if _, err := NewSource(config.PeerSource{Type: config.PeerSourceKubernetesSecret}, fake.NewSimpleClientset(), "stagbru"); err == nil {
		t.Fatal("NewSource() error = nil, want error for empty secret_name")
	}
}

func TestNewSource_OpenBaoNotImplemented(t *testing.T) {
	src := config.PeerSource{Type: config.PeerSourceOpenBao, OpenBao: &config.OpenBaoPeerSource{
		Address: "https://openbao.internal:8200", Mount: "infra", Path: "network/default/peers", AuthRole: "stagbru",
	}}
	if _, err := NewSource(src, fake.NewSimpleClientset(), "stagbru"); err == nil {
		t.Fatal("NewSource() error = nil, want error: openbao has no Source implementation yet")
	}
}

func TestNewSource_UnknownType(t *testing.T) {
	if _, err := NewSource(config.PeerSource{Type: "s3"}, fake.NewSimpleClientset(), "stagbru"); err == nil {
		t.Fatal("NewSource() error = nil, want error for unknown type")
	}
}
