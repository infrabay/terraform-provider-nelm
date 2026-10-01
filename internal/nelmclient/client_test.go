package nelmclient

import (
	"context"
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

// TestUnknownConfigClient_NeverReachesCluster is the F14 regression test for
// the placeholder client the provider hands out while its configuration is
// still Unknown at plan time. That client has a ZERO Config, which nelm would
// happily resolve to ~/.kube/config's current-context — so every
// cluster-facing method must fail with ErrConfigUnknown before nelm is ever
// called. HOME points at a kubeconfig whose current-context is a counting fake
// API server: any request reaching it means the placeholder leaked through.
func TestUnknownConfigClient_NeverReachesCluster(t *testing.T) {
	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECACHEDIR", t.TempDir())

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

	if err := os.MkdirAll(filepath.Join(home, ".kube"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(home, ".kube", "config"), []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	c := NewUnknownConfigClient()
	spec := ReleaseSpec{Name: "app", Namespace: "default", Chart: "./chart", StorageDriver: "secret"}

	calls := map[string]func() error{
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
		"Render": func() error {
			_, err := c.Render(ctx, spec, 0)
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

	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrConfigUnknown) {
			t.Errorf("%s: err = %v, want ErrConfigUnknown", name, err)
		}
	}

	if n := hits.Load(); n != 0 {
		t.Fatalf("the placeholder client sent %d request(s) to the ambient ~/.kube/config cluster", n)
	}
}

func TestClient_ConfigUnknown(t *testing.T) {
	if !NewUnknownConfigClient().ConfigUnknown() {
		t.Error("NewUnknownConfigClient().ConfigUnknown() = false, want true")
	}

	if NewClient(Config{}).ConfigUnknown() {
		t.Error("NewClient(Config{}).ConfigUnknown() = true, want false")
	}

	// A resource whose provider has not been configured yet holds a nil
	// client; ModifyPlan asks it anyway.
	var nilClient *Client
	if nilClient.ConfigUnknown() {
		t.Error("(*Client)(nil).ConfigUnknown() = true, want false")
	}
}
