package nelmclient

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestEnsureKubeFactory_TransientErrorNotMemoized is the regression test for
// the factory-error-poisoning bug: a single transient failure of the
// connectivity check used to be cached in c.kubeFactoryErr and returned by
// every later ensureKubeFactory call (from any resource's LiveObjects /
// IsNamespaced) for the whole process, even after the cluster recovered. The
// fix caches only a SUCCESSFUL factory, so a failed call never short-circuits
// the next one.
//
// The test is fully offline: call 1 fails deterministically while building the
// KubeConfig (garbage base64), then the config is corrected to a syntactically
// valid kubeconfig pointing at an unreachable local port. With the pre-fix
// memoization, call 2 would return the cached call-1 "construct kube config"
// error verbatim; the fix makes it re-attempt and reach — and fail at — the
// later factory connectivity stage instead ("construct kube client factory").
func TestEnsureKubeFactory_TransientErrorNotMemoized(t *testing.T) {
	ctx := context.Background()

	c := NewClient(Config{KubeConfigBase64: "!!! not valid base64 !!!"})

	if _, err := c.ensureKubeFactory(ctx); err == nil {
		t.Fatal("call 1: expected an error for garbage kube_config_base64")
	}

	// Correct the configuration. A pre-fix Client would ignore this and keep
	// returning the memoized call-1 error forever.
	c.cfg.KubeConfigBase64 = ""
	c.cfg.KubeConfigPaths = []string{writeUnreachableKubeconfig(t)}

	_, err := c.ensureKubeFactory(ctx)
	if err == nil {
		t.Fatal("call 2: expected a connectivity error against the unreachable server")
	}
	if strings.Contains(err.Error(), "construct kube config") {
		t.Fatalf("call 2 returned a memoized construct-kube-config error — the transient failure was cached instead of retried: %v", err)
	}
	if c.kubeFactory != nil {
		t.Fatal("kubeFactory must stay nil after a failed construction so the next call retries")
	}
}

// TestEnsureKubeFactory_ConstructionIsBounded is the regression test for a
// wedged factory: the connectivity check inside factory construction ignores
// the caller context entirely (client-go discovery has no ctx), so a
// BLACKHOLED endpoint (accepts nothing, SYN just hangs — RFC 5737 TEST-NET
// address) used to block ensureKubeFactory forever while holding kubeMu,
// freezing every resource in the process. The watchdog must cut it at
// Config.RequestTimeout.
func TestEnsureKubeFactory_ConstructionIsBounded(t *testing.T) {
	const cfgYAML = `apiVersion: v1
kind: Config
clusters:
- name: blackhole
  cluster:
    server: https://203.0.113.1:6443
contexts:
- name: blackhole
  context:
    cluster: blackhole
    user: blackhole
current-context: blackhole
users:
- name: blackhole
  user: {}
`

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	c := NewClient(Config{
		KubeConfigPaths: []string{path},
		RequestTimeout:  2 * time.Second,
	})

	start := time.Now()
	_, err := c.ensureKubeFactory(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error against a blackholed endpoint")
	}
	if elapsed > 20*time.Second {
		t.Fatalf("factory construction was not bounded: took %s (watchdog should fire at ~2s)", elapsed)
	}
	if c.kubeFactory != nil {
		t.Fatal("nothing must be cached from a timed-out construction attempt")
	}
}

// writeUnreachableKubeconfig writes a syntactically valid kubeconfig whose
// server is an unreachable local port (connection refused, no network needed):
// kube.NewKubeConfig parses it successfully but kube.NewClientFactory's
// ServerVersion connectivity check fails fast.
func writeUnreachableKubeconfig(t *testing.T) string {
	t.Helper()

	const cfg = `apiVersion: v1
kind: Config
clusters:
- name: unreachable
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: unreachable
  context:
    cluster: unreachable
    user: unreachable
current-context: unreachable
users:
- name: unreachable
  user: {}
`

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write temp kubeconfig: %v", err)
	}

	return path
}

// TestLiveObjectsAndIsNamespaced_RequiresCluster exercises LiveObjects and
// IsNamespaced against a real cluster. Both require nelm's
// kube.NewClientFactory, which dials the API server to check connectivity —
// there is no way to unit test them without a cluster, so this test only
// runs when NELM_TEST_KUBE_CONTEXT is set (the same guard GNUmakefile's
// testacc target uses); ./gates.sh pkg (no such env set) always skips it,
// keeping the package's default unit-test run cluster-free.
func TestLiveObjectsAndIsNamespaced_RequiresCluster(t *testing.T) {
	kubeCtx := os.Getenv("NELM_TEST_KUBE_CONTEXT")
	if kubeCtx == "" {
		t.Skip("NELM_TEST_KUBE_CONTEXT not set; skipping cluster-dependent liveread test")
	}

	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	c := NewClient(Config{
		KubeContext:    kubeCtx,
		RequestTimeout: 10 * time.Second,
	})

	namespaced, err := c.IsNamespaced(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	if err != nil {
		t.Fatalf("IsNamespaced(ConfigMap): %v", err)
	}
	if !namespaced {
		t.Fatal("expected core/v1 ConfigMap to be namespaced")
	}

	clusterScoped, err := c.IsNamespaced(schema.GroupVersionKind{
		Group:   "rbac.authorization.k8s.io",
		Version: "v1",
		Kind:    "ClusterRole",
	})
	if err != nil {
		t.Fatalf("IsNamespaced(ClusterRole): %v", err)
	}
	if clusterScoped {
		t.Fatal("expected rbac.authorization.k8s.io/v1 ClusterRole to be cluster-scoped")
	}

	objs, err := c.LiveObjects(ctx, []ResourceRef{
		{Version: "v1", Kind: "ConfigMap", Namespace: "default", Name: "this-configmap-should-not-exist-anywhere"},
	})
	if err != nil {
		t.Fatalf("LiveObjects: %v", err)
	}
	if len(objs) != 0 {
		t.Fatalf("LiveObjects returned %d entries for a nonexistent object, want 0 (NotFound omitted)", len(objs))
	}
}
