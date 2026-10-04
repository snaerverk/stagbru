package config

import (
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// baseEnv is the minimal set of env vars Load needs to produce a valid
// Config via the flat-env-var fallback (no wg.networks): the required,
// no-default fields from docs/plan.md's config table.
func baseEnv() map[string]string {
	return map[string]string{
		"WG_SELF_SECRET":  "gateway-self",
		"WG_SELF_NAME":    "wg-node-name",
		"WG_PEERS_SECRET": "wg0-conf",
	}
}

// withEnv sets env vars for the duration of the test (cleared via
// t.Cleanup) and clears every var this package reads first, so tests don't
// pick up stray values from the ambient test-runner environment.
func withEnv(t *testing.T, overrides map[string]string) {
	t.Helper()

	managed := []string{
		"WG_INTERFACE", "WG_LISTEN_PORT", "WG_SELF_SECRET", "WG_PEERS_SECRET",
		"WG_PEERS_SECRETS",
		"WG_SELF_NAME", "WG_MASQUERADE", "WG_ADVERTISE_TO_TAILSCALE",
		"POD_NAMESPACE", "TS_LOGIN_SERVER", "TS_AUTHKEY", "TS_STATE_DIR",
		"TS_HOSTNAME", "TS_ROUTES", "HTTP_ADDR", "STAGBRU_CONFIG",
	}
	for _, k := range managed {
		orig, had := os.LookupEnv(k)
		if had {
			k, orig := k, orig
			t.Cleanup(func() { os.Setenv(k, orig) })
		} else {
			k := k
			t.Cleanup(func() { os.Unsetenv(k) })
		}
		os.Unsetenv(k)
	}

	env := baseEnv()
	maps.Copy(env, overrides)
	for k, v := range env {
		os.Setenv(k, v)
	}
}

func TestLoad_FlatEnvFallback_Defaults(t *testing.T) {
	withEnv(t, nil)

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(cfg.Networks) != 1 {
		t.Fatalf("Networks = %d entries, want 1", len(cfg.Networks))
	}
	n := cfg.Networks[0]

	want := Network{
		Name:                 "default",
		Interface:            "wg0",
		ListenPort:           51820,
		SelfSecret:           "gateway-self",
		SelfName:             "wg-node-name",
		PeersSources:         []PeerSource{{Type: PeerSourceKubernetesSecret, SecretName: "wg0-conf"}},
		Masquerade:           true,
		AdvertiseToTailscale: true,
	}
	if !reflect.DeepEqual(n, want) {
		t.Errorf("Networks[0] = %+v, want %+v", n, want)
	}

	if cfg.Tailscale.StateDir != "/var/lib/tailscale" {
		t.Errorf("Tailscale.StateDir = %q, want /var/lib/tailscale", cfg.Tailscale.StateDir)
	}
	wantRoutes := []netip.Prefix{
		netip.MustParsePrefix("172.16.6.0/24"),
		netip.MustParsePrefix("100.192.8.0/24"),
	}
	if len(cfg.Tailscale.Routes) != len(wantRoutes) {
		t.Fatalf("Tailscale.Routes = %v, want %v", cfg.Tailscale.Routes, wantRoutes)
	}
	for i, p := range wantRoutes {
		if cfg.Tailscale.Routes[i] != p {
			t.Errorf("Tailscale.Routes[%d] = %v, want %v", i, cfg.Tailscale.Routes[i], p)
		}
	}

	if cfg.HTTPAddr != ":9090" {
		t.Errorf("HTTPAddr = %q, want :9090", cfg.HTTPAddr)
	}
}

func TestLoad_FlatEnvFallback_Overrides(t *testing.T) {
	withEnv(t, map[string]string{
		"WG_INTERFACE":              "wg1",
		"WG_LISTEN_PORT":            "51821",
		"WG_MASQUERADE":             "false",
		"WG_ADVERTISE_TO_TAILSCALE": "false",
		"POD_NAMESPACE":             "wg-eyev",
		"TS_LOGIN_SERVER":           "https://headscale.example.net",
		"TS_AUTHKEY":                "tskey-abc",
		"TS_HOSTNAME":               "stagbru-gw",
		"TS_ROUTES":                 "10.0.0.0/8",
		"HTTP_ADDR":                 ":8080",
	})

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	n := cfg.Networks[0]
	if n.Interface != "wg1" || n.ListenPort != 51821 || n.Masquerade || n.AdvertiseToTailscale {
		t.Errorf("Networks[0] = %+v, overrides not applied", n)
	}
	if cfg.PodNamespace != "wg-eyev" {
		t.Errorf("PodNamespace = %q", cfg.PodNamespace)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.Tailscale.LoginServer != "https://headscale.example.net" {
		t.Errorf("Tailscale.LoginServer = %q", cfg.Tailscale.LoginServer)
	}
	if cfg.Tailscale.AuthKey != "tskey-abc" {
		t.Errorf("Tailscale.AuthKey = %q", cfg.Tailscale.AuthKey)
	}
	if cfg.Tailscale.Hostname != "stagbru-gw" {
		t.Errorf("Tailscale.Hostname = %q", cfg.Tailscale.Hostname)
	}
	wantRoutes := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	if len(cfg.Tailscale.Routes) != 1 || cfg.Tailscale.Routes[0] != wantRoutes[0] {
		t.Errorf("Tailscale.Routes = %v, want %v", cfg.Tailscale.Routes, wantRoutes)
	}
}

func TestLoad_MissingRequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		missing string // key to blank out from baseEnv
	}{
		{"missing self_secret", "WG_SELF_SECRET"},
		{"missing self_name", "WG_SELF_NAME"},
		{"missing peers_secret", "WG_PEERS_SECRET"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withEnv(t, map[string]string{tt.missing: ""})

			if _, err := Load(nil); err == nil {
				t.Fatalf("Load() error = nil, want error for blank %s", tt.missing)
			}
		})
	}
}

func TestLoad_InterfaceNameTooLong(t *testing.T) {
	// Linux IFNAMSIZ is 16 bytes including the NUL terminator, so the
	// usable name is at most 15 bytes.
	withEnv(t, map[string]string{"WG_INTERFACE": "this-name-is-way-too-long"})

	if _, err := Load(nil); err == nil {
		t.Fatal("Load() error = nil, want error for interface name > 15 bytes")
	}
}

func TestLoad_TSRoutes_InvalidCIDR(t *testing.T) {
	withEnv(t, map[string]string{"TS_ROUTES": "not-a-cidr"})

	if _, err := Load(nil); err == nil {
		t.Fatal("Load() error = nil, want error for invalid TS_ROUTES entry")
	}
}

func TestLoad_TSRoutes_Empty(t *testing.T) {
	withEnv(t, map[string]string{"TS_ROUTES": ""})

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Tailscale.Routes) != 0 {
		t.Errorf("Tailscale.Routes = %v, want empty", cfg.Tailscale.Routes)
	}
}

// TestValidate_NetworkList exercises the list-wide validation rules
// (docs/plan.md: unique name/interface/listen_port) directly against
// hand-built Config values. See TestLoad_ConfigFile_* below for the same
// rules exercised end-to-end through a real YAML/TOML file.
func TestValidate_NetworkList(t *testing.T) {
	validNetwork := func(name, iface string, port int) Network {
		return Network{
			Name:         name,
			Interface:    iface,
			ListenPort:   port,
			SelfSecret:   "self",
			SelfName:     "self-name",
			PeersSources: []PeerSource{{Type: PeerSourceKubernetesSecret, SecretName: "peers"}},
		}
	}

	tests := []struct {
		name     string
		networks []Network
		wantErr  bool
	}{
		{
			name: "two distinct valid networks",
			networks: []Network{
				validNetwork("default", "wg0", 51820),
				validNetwork("second", "wg1", 51821),
			},
			wantErr: false,
		},
		{
			name: "duplicate name",
			networks: []Network{
				validNetwork("default", "wg0", 51820),
				validNetwork("default", "wg1", 51821),
			},
			wantErr: true,
		},
		{
			name: "duplicate interface",
			networks: []Network{
				validNetwork("default", "wg0", 51820),
				validNetwork("second", "wg0", 51821),
			},
			wantErr: true,
		},
		{
			name: "duplicate listen_port",
			networks: []Network{
				validNetwork("default", "wg0", 51820),
				validNetwork("second", "wg1", 51820),
			},
			wantErr: true,
		},
		{
			name: "unknown peer source type",
			networks: func() []Network {
				n := validNetwork("default", "wg0", 51820)
				n.PeersSources = []PeerSource{{Type: "s3"}}
				return []Network{n}
			}(),
			wantErr: true,
		},
		{
			name: "kubernetes_secret missing secret_name",
			networks: func() []Network {
				n := validNetwork("default", "wg0", 51820)
				n.PeersSources = []PeerSource{{Type: PeerSourceKubernetesSecret}}
				return []Network{n}
			}(),
			wantErr: true,
		},
		{
			name: "openbao missing fields",
			networks: func() []Network {
				n := validNetwork("default", "wg0", 51820)
				n.PeersSources = []PeerSource{{Type: PeerSourceOpenBao, OpenBao: &OpenBaoPeerSource{Address: "https://openbao.internal:8200"}}}
				return []Network{n}
			}(),
			wantErr: true,
		},
		{
			name: "openbao fully configured is valid",
			networks: func() []Network {
				n := validNetwork("default", "wg0", 51820)
				n.PeersSources = []PeerSource{{Type: PeerSourceOpenBao, OpenBao: &OpenBaoPeerSource{
					Address: "https://openbao.internal:8200", Mount: "infra", Path: "network/default/peers", AuthRole: "stagbru",
				}}}
				return []Network{n}
			}(),
			wantErr: false,
		},
		{
			name: "duplicate peer source within one network",
			networks: func() []Network {
				n := validNetwork("default", "wg0", 51820)
				n.PeersSources = []PeerSource{
					{Type: PeerSourceKubernetesSecret, SecretName: "peers"},
					{Type: PeerSourceKubernetesSecret, SecretName: "peers"},
				}
				return []Network{n}
			}(),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Networks: tt.networks}
			err := validate(cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

const twoNetworksYAML = `
wg:
  networks:
    - name: default
      interface: wg0
      listen_port: 51820
      self_secret: gateway-self
      self_name: wg-node-name
      peers_sources:
        - type: kubernetes_secret
          secret_name: wg0-conf
        - type: kubernetes_secret
          secret_name: wg0-conf-extra
      masquerade: true
      advertise_to_tailscale: true
    - name: second
      interface: wg1
      listen_port: 51821
      self_secret: second-gateway-self
      self_name: second-wg-node-name
      peers_sources:
        - type: openbao
          openbao:
            address: https://openbao.internal:8200
            mount: infra
            path: network/second/peers
            auth_role: stagbru
      masquerade: false
      advertise_to_tailscale: false
`

func TestLoad_ConfigFile_YAML_Networks(t *testing.T) {
	withEnv(t, nil)
	path := writeFile(t, "config.yaml", twoNetworksYAML)
	os.Setenv("STAGBRU_CONFIG", path)

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Networks) != 2 {
		t.Fatalf("Networks = %d entries, want 2: %+v", len(cfg.Networks), cfg.Networks)
	}
	if cfg.Networks[0].Name != "default" || cfg.Networks[1].Name != "second" {
		t.Errorf("Networks = %+v, want default+second in order", cfg.Networks)
	}
	if cfg.Networks[1].Interface != "wg1" || cfg.Networks[1].ListenPort != 51821 {
		t.Errorf("Networks[1] = %+v", cfg.Networks[1])
	}
	wantSources := []PeerSource{
		{Type: PeerSourceKubernetesSecret, SecretName: "wg0-conf"},
		{Type: PeerSourceKubernetesSecret, SecretName: "wg0-conf-extra"},
	}
	if !reflect.DeepEqual(cfg.Networks[0].PeersSources, wantSources) {
		t.Errorf("Networks[0].PeersSources = %+v, want %+v", cfg.Networks[0].PeersSources, wantSources)
	}

	wantOpenBao := []PeerSource{{
		Type: PeerSourceOpenBao,
		OpenBao: &OpenBaoPeerSource{
			Address:       "https://openbao.internal:8200",
			Mount:         "infra",
			Path:          "network/second/peers",
			AuthRole:      "stagbru",
			AuthMountPath: "kubernetes", // defaulted by validate()
		},
	}}
	if !reflect.DeepEqual(cfg.Networks[1].PeersSources, wantOpenBao) {
		t.Errorf("Networks[1].PeersSources = %+v, want %+v", cfg.Networks[1].PeersSources, wantOpenBao)
	}
}

const twoNetworksTOML = `
[[wg.networks]]
name = "default"
interface = "wg0"
listen_port = 51820
self_secret = "gateway-self"
self_name = "wg-node-name"
masquerade = true
advertise_to_tailscale = true

  [[wg.networks.peers_sources]]
  type = "kubernetes_secret"
  secret_name = "wg0-conf"

[[wg.networks]]
name = "second"
interface = "wg1"
listen_port = 51821
self_secret = "second-gateway-self"
self_name = "second-wg-node-name"
masquerade = false
advertise_to_tailscale = false

  [[wg.networks.peers_sources]]
  type = "kubernetes_secret"
  secret_name = "wg1-conf"
`

func TestLoad_ConfigFile_TOML_Networks(t *testing.T) {
	withEnv(t, nil)
	path := writeFile(t, "config.toml", twoNetworksTOML)
	os.Setenv("STAGBRU_CONFIG", path)

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Networks) != 2 {
		t.Fatalf("Networks = %d entries, want 2: %+v", len(cfg.Networks), cfg.Networks)
	}
	if cfg.Networks[0].Name != "default" || cfg.Networks[1].Name != "second" {
		t.Errorf("Networks = %+v, want default+second in order", cfg.Networks)
	}
}

func TestLoad_ConfigFile_InvalidNetworkList(t *testing.T) {
	withEnv(t, nil)
	// Same interface on both networks -> validate() must reject it, same
	// as TestValidate_NetworkList's "duplicate interface" case, but now
	// exercised end-to-end through a real file.
	path := writeFile(t, "config.yaml", `
wg:
  networks:
    - name: default
      interface: wg0
      listen_port: 51820
      self_secret: gateway-self
      self_name: wg-node-name
      peers_sources:
        - type: kubernetes_secret
          secret_name: wg0-conf
    - name: second
      interface: wg0
      listen_port: 51821
      self_secret: second-gateway-self
      self_name: second-wg-node-name
      peers_sources:
        - type: kubernetes_secret
          secret_name: wg1-conf
`)
	os.Setenv("STAGBRU_CONFIG", path)

	if _, err := Load(nil); err == nil {
		t.Fatal("Load() error = nil, want error for duplicate interface across networks")
	}
}

func TestLoad_ConfigFile_MultipleFiles_LaterOverrides(t *testing.T) {
	withEnv(t, nil)
	base := writeFile(t, "base.yaml", "HTTP_ADDR: \":9090\"\n")
	override := writeFile(t, "override.yaml", "HTTP_ADDR: \":8080\"\n")
	os.Setenv("STAGBRU_CONFIG", base+","+override)

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080 (the later file should win)", cfg.HTTPAddr)
	}
}

func TestLoad_ConfigFile_EnvOverridesFile(t *testing.T) {
	path := writeFile(t, "config.yaml", "HTTP_ADDR: \":9090\"\n")
	withEnv(t, map[string]string{"HTTP_ADDR": ":7070"})
	os.Setenv("STAGBRU_CONFIG", path)

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddr != ":7070" {
		t.Errorf("HTTPAddr = %q, want :7070 (env must override the file)", cfg.HTTPAddr)
	}
}

func TestLoad_ConfigFile_MissingFile(t *testing.T) {
	withEnv(t, nil)
	os.Setenv("STAGBRU_CONFIG", filepath.Join(t.TempDir(), "does-not-exist.yaml"))

	if _, err := Load(nil); err == nil {
		t.Fatal("Load() error = nil, want error for a missing, explicitly-listed config file")
	}
}

func TestLoad_ConfigFile_UnsupportedExtension(t *testing.T) {
	withEnv(t, nil)
	os.Setenv("STAGBRU_CONFIG", filepath.Join(t.TempDir(), "config.json"))

	if _, err := Load(nil); err == nil {
		t.Fatal("Load() error = nil, want error for an unsupported config file extension")
	}
}
