package nelmclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// errTestNoConnection stands in for the "No Kubernetes connection configured"
// explanation the provider builds a NewNoConnectionClient with.
var errTestNoConnection = errors.New("test: no Kubernetes connection configured")

// trapAmbientKubeconfig points HOME and KUBECONFIG at a kubeconfig whose
// current-context is a counting fake API server, and returns the counter. A
// client that falls back to nelm's default kubeconfig loading (a zero Config
// resolves to ~/.kube/config's current-context) sends its requests there. It
// uses t.Setenv, so callers cannot be parallel.
func trapAmbientKubeconfig(t *testing.T) *atomic.Int64 {
	t.Helper()

	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	home := isolateHome(t)

	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: ambient
  cluster:
    server: %s
contexts:
- name: ambient
  context:
    cluster: ambient
    user: ambient
current-context: ambient
users:
- name: ambient
  user:
    token: ambient
`, srv.URL)

	path := filepath.Join(home, ".kube", "config")

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("KUBECONFIG", path)

	return &hits
}

// TestClusterlessClients_NeverReachCluster is the regression test for the
// clients the provider hands out without connection settings: the
// placeholder for a configuration that is still Unknown at plan time, and the
// client for a configuration that names no cluster at all. Both have a ZERO
// Kubernetes Config, which nelm would happily resolve to ~/.kube/config's
// current-context — so every cluster-facing method (a remote Template
// included) must fail with exactly the client's error before nelm is ever
// called. HOME and KUBECONFIG point at a kubeconfig whose current-context is a
// counting fake API server: any request reaching it means a client leaked
// through.
func TestClusterlessClients_NeverReachCluster(t *testing.T) {
	hits := trapAmbientKubeconfig(t)

	registries := []RegistryAuth{{URL: "registry.invalid", Username: "test", Password: "test"}}

	tests := []struct {
		name   string
		client *Client
		want   error
	}{
		{
			name:   "unknown config",
			client: NewUnknownConfigClient(registries, true),
			want:   ErrConfigUnknown,
		},
		{
			name:   "unknown config and registries",
			client: NewUnknownConfigClient(nil, false),
			want:   ErrConfigUnknown,
		},
		{
			name:   "no connection",
			client: NewNoConnectionClient(registries, errTestNoConnection),
			want:   errTestNoConnection,
		},
		{
			// A nil error must not make an open client.
			name:   "no connection, nil error",
			client: NewNoConnectionClient(registries, nil),
			want:   errNoConnection,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for name, call := range clusterCalls(context.Background(), tt.client) {
				// Exactly that error, not merely one wrapping it: the
				// provider shows its text as the diagnostic.
				if err := call(); err != tt.want {
					t.Errorf("%s: err = %v, want %v", name, err, tt.want)
				}
			}
		})
	}

	if n := hits.Load(); n != 0 {
		t.Fatalf("a clusterless client sent %d request(s) to the ambient ~/.kube/config cluster", n)
	}
}

// TestClusterlessClients_KeepRegistries checks the clusterless clients keep
// the provider's static OCI credentials for the registry config every render
// writes (chartRender's writeRegistryConfig of c.cfg.Registries): an offline
// Template of a private oci:// chart needs them while the configuration
// names no cluster, or while the cluster is created in the same run. Dropped,
// the pull would fall back to nelm's default ~/.docker/config.json and fail
// with a 401.
func TestClusterlessClients_KeepRegistries(t *testing.T) {
	registries := []RegistryAuth{{URL: "oci://registry.invalid/charts", Username: "u", Password: "p"}}
	wantAuth := base64.StdEncoding.EncodeToString([]byte("u:p"))

	tests := []struct {
		name   string
		client *Client
	}{
		{name: "unknown config, registries known", client: NewUnknownConfigClient(registries, true)},
		{name: "no connection", client: NewNoConnectionClient(registries, errTestNoConnection)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, err := writeRegistryConfig(t.TempDir(), tt.client.cfg.Registries)
			if err != nil {
				t.Fatalf("writeRegistryConfig: %v", err)
			}

			if path == "" {
				t.Fatal("no registry config written: the client kept no registries")
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read registry config: %v", err)
			}

			var parsed struct {
				Auths map[string]struct {
					Auth string `json:"auth"`
				} `json:"auths"`
			}

			if err := json.Unmarshal(data, &parsed); err != nil {
				t.Fatalf("unmarshal registry config: %v", err)
			}

			if got := parsed.Auths["registry.invalid"].Auth; got != wantAuth {
				t.Errorf("auths[registry.invalid].auth = %q, want %q (auths: %v)", got, wantAuth, parsed.Auths)
			}
		})
	}
}

// clusterCalls returns a call of every cluster-facing method of c, keyed by
// the method's name.
func clusterCalls(ctx context.Context, c *Client) map[string]func() error {
	spec := ReleaseSpec{Name: "app", Namespace: "default", Chart: "./chart", StorageDriver: "secret"}

	return map[string]func() error{
		"Plan": func() error {
			_, err := c.Plan(ctx, spec, 0)
			return err
		},
		"Install": func() error {
			return c.Install(ctx, spec, 0)
		},
		"Uninstall": func() error {
			return c.Uninstall(ctx, "app", "default", "secret", 0)
		},
		"Get": func() error {
			_, err := c.Get(ctx, "app", "default", "secret", 0)
			return err
		},
		"History": func() error {
			_, err := c.History(ctx, "app", "default", "secret", 0)
			return err
		},
		"Render": func() error {
			_, err := c.Render(ctx, spec, 0)
			return err
		},
		"Template(Remote)": func() error {
			_, err := c.Template(ctx, spec, TemplateOptions{Remote: true}, 0)
			return err
		},
		"HandOverHelmProviderFieldManagers": func() error {
			_, err := c.HandOverHelmProviderFieldManagers(ctx, "app", "default", "secret", 0)
			return err
		},
		"LiveObjects": func() error {
			_, err := c.LiveObjects(ctx, []ResourceRef{{Version: "v1", Kind: "ConfigMap", Namespace: "default", Name: "app"}})
			return err
		},
		"IsNamespaced": func() error {
			_, err := c.IsNamespaced(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			return err
		},
	}
}

func TestClient_ConfigUnknown(t *testing.T) {
	if !NewUnknownConfigClient(nil, true).ConfigUnknown() {
		t.Error("NewUnknownConfigClient(nil, true).ConfigUnknown() = false, want true")
	}

	if NewClient(Config{}).ConfigUnknown() {
		t.Error("NewClient(Config{}).ConfigUnknown() = true, want false")
	}

	if NewNoConnectionClient(nil, errTestNoConnection).ConfigUnknown() {
		t.Error("NewNoConnectionClient(nil, err).ConfigUnknown() = true, want false")
	}

	// A resource whose provider has not been configured yet holds a nil
	// client; ModifyPlan asks it anyway.
	var nilClient *Client
	if nilClient.ConfigUnknown() {
		t.Error("(*Client)(nil).ConfigUnknown() = true, want false")
	}
}

func TestClient_NoConnection(t *testing.T) {
	if err := NewNoConnectionClient(nil, errTestNoConnection).NoConnection(); err != errTestNoConnection {
		t.Errorf("NewNoConnectionClient(nil, err).NoConnection() = %v, want err", err)
	}

	// A nil error must not turn the client into an open one: nelm_release
	// refuses to run only on a non-nil NoConnection().
	if err := NewNoConnectionClient(nil, nil).NoConnection(); err != errNoConnection {
		t.Errorf("NewNoConnectionClient(nil, nil).NoConnection() = %v, want errNoConnection", err)
	}

	if err := NewClient(Config{}).NoConnection(); err != nil {
		t.Errorf("NewClient(Config{}).NoConnection() = %v, want nil", err)
	}

	// The placeholder has no connection either, but that is ConfigUnknown's
	// case: nelm_release plans it at apply instead of refusing.
	if err := NewUnknownConfigClient(nil, true).NoConnection(); err != nil {
		t.Errorf("NewUnknownConfigClient(nil, true).NoConnection() = %v, want nil", err)
	}

	// Resource Configure asks before the provider has configured, too.
	var nilClient *Client
	if err := nilClient.NoConnection(); err != nil {
		t.Errorf("(*Client)(nil).NoConnection() = %v, want nil", err)
	}
}
