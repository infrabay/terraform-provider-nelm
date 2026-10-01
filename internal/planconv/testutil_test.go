package planconv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// loadUnstructured reads a captured-live JSON fixture (testdata/**, produced
// by scripts/smoke — read-only from here) into an *unstructured.Unstructured.
func loadUnstructured(t *testing.T, path string) *unstructured.Unstructured {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}

	return &unstructured.Unstructured{Object: m}
}

func normalizeFixture(elems ...string) string {
	return filepath.Join(append([]string{"testdata", "normalize"}, elems...)...)
}

// fakeScoper is a deterministic, pure test double for KeyScoper: no cluster
// access, no cached RESTMapper — just a fixed GVK->namespaced table, matching
// planconv's leaf-package/pure-function design (CONTRACTS.md, Seam 2).
type fakeScoper struct {
	namespaced map[schema.GroupVersionKind]bool
	err        error
}

func (f fakeScoper) IsNamespaced(gvk schema.GroupVersionKind) (bool, error) {
	if f.err != nil {
		return false, f.err
	}

	v, ok := f.namespaced[gvk]
	if !ok {
		return false, fmt.Errorf("fakeScoper: unknown gvk %s", gvk)
	}

	return v, nil
}

var (
	gvkClusterRole = schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}
	gvkConfigMap   = schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	gvkSecret      = schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"}
	gvkDeployment  = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	gvkService     = schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"}
)

// basicScoper mirrors the real RESTMapper's answers for the fixture chart's
// kinds (testdata/charts/basic): ClusterRole/ClusterRoleBinding are
// cluster-scoped, everything else here is namespaced.
func basicScoper() fakeScoper {
	return fakeScoper{namespaced: map[schema.GroupVersionKind]bool{
		gvkClusterRole: false,
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"}: false,
		gvkConfigMap:  true,
		gvkSecret:     true,
		gvkDeployment: true,
		gvkService:    true,
	}}
}
