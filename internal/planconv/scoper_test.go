package planconv

import (
	"errors"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	gvkClusterIssuer = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "ClusterIssuer"}
	gvkCertificate   = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}
	gvkWidget        = schema.GroupVersionKind{Group: "example.com", Version: "v1beta1", Kind: "Widget"}
)

// unservedScoper answers like a cluster that does not serve the kinds in
// unserved (their CRDs are not installed) and serves the basic chart's kinds.
type unservedScoper struct {
	unserved []schema.GroupVersionKind
}

func (s *unservedScoper) IsNamespaced(gvk schema.GroupVersionKind) (bool, error) {
	if gvk.Kind == "CustomResourceDefinition" {
		return false, nil
	}

	for _, u := range s.unserved {
		if u == gvk {
			return false, &meta.NoKindMatchError{GroupKind: gvk.GroupKind(), SearchedVersions: []string{gvk.Version}}
		}
	}

	return basicScoper().IsNamespaced(gvk)
}

func crd(group, kind, scope string, versions ...string) *unstructured.Unstructured {
	vs := make([]interface{}, len(versions))
	for i, v := range versions {
		vs[i] = map[string]interface{}{"name": v, "served": true}
	}

	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]interface{}{"name": kind + "s." + group},
		"spec": map[string]interface{}{
			"group":    group,
			"names":    map[string]interface{}{"kind": kind},
			"scope":    scope,
			"versions": vs,
		},
	}}
}

func cr(gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(name)

	return obj
}

// TestRenderScoper_ScopesUnservedKindsFromTheChartsCRDs is the F13
// regression test: a CR whose kind the cluster does not serve yet used to be
// keyed by a namespace GUESS, which is wrong for a cluster-scoped kind and
// changes once the kind is served (a CRD installed by another release
// earlier in the same apply) — an "inconsistent final plan" at apply. A CRD
// in the chart's own render now gives the real scope; a kind nothing knows
// is reported as unresolved so the caller degrades instead of committing the
// guess.
func TestRenderScoper_ScopesUnservedKindsFromTheChartsCRDs(t *testing.T) {
	objs := []*unstructured.Unstructured{
		crd("cert-manager.io", "ClusterIssuer", "Cluster", "v1"),
		crd("cert-manager.io", "Certificate", "Namespaced", "v1"),
		cr(gvkClusterIssuer, "letsencrypt"),
		cr(gvkCertificate, "web"),
	}

	scoper := NewRenderScoper(objs, &unservedScoper{unserved: []schema.GroupVersionKind{gvkClusterIssuer, gvkCertificate, gvkWidget}})

	rendered, err := BuildRenderedResources(objs, "issuers", scoper, nil)
	if err != nil {
		t.Fatalf("BuildRenderedResources: %v", err)
	}

	for _, key := range []string{"cert-manager.io/v1/ClusterIssuer//letsencrypt", "cert-manager.io/v1/Certificate/issuers/web"} {
		if _, ok := rendered[key]; !ok {
			t.Errorf("missing key %s in %v", key, rendered)
		}
	}

	if got := scoper.Unresolved(); len(got) != 0 {
		t.Errorf("unresolved = %v, want none (both kinds have a CRD in the chart)", got)
	}

	// A kind whose CRD the chart does not contain is still guessed, and
	// reported.
	guessed, err := BuildRenderedResources([]*unstructured.Unstructured{cr(gvkWidget, "w")}, "issuers", scoper, nil)
	if err != nil {
		t.Fatalf("BuildRenderedResources: %v", err)
	}

	if _, ok := guessed["example.com/v1beta1/Widget/issuers/w"]; !ok {
		t.Errorf("guessed keys = %v, want the release-namespace guess", guessed)
	}

	if got := scoper.Unresolved(); !reflect.DeepEqual(got, []schema.GroupVersionKind{gvkWidget}) {
		t.Errorf("unresolved = %v, want [%v]", got, gvkWidget)
	}
}

func TestRenderScoper_ServedKindsAndErrorsPassThrough(t *testing.T) {
	// A CRD in the chart never overrides what the cluster serves.
	scoper := NewRenderScoper([]*unstructured.Unstructured{crd("apps", "Deployment", "Cluster", "v1")}, &unservedScoper{})

	if namespaced, err := scoper.IsNamespaced(gvkDeployment); err != nil || !namespaced {
		t.Errorf("IsNamespaced(Deployment) = %v, %v; want the cluster's answer (namespaced)", namespaced, err)
	}

	// Errors other than "no matches for kind" are not guessed over.
	boom := errors.New("connection refused")
	failing := NewRenderScoper(nil, fakeScoper{err: boom})

	if _, err := failing.IsNamespaced(gvkClusterIssuer); !errors.Is(err, boom) {
		t.Errorf("IsNamespaced error = %v, want %v", err, boom)
	}

	if len(failing.Unresolved()) != 0 {
		t.Errorf("unresolved = %v, want none for a non-NoKindMatch error", failing.Unresolved())
	}

	// A v1beta1 CRD (spec.version) and a version the CRD does not serve.
	legacy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apiextensions.k8s.io/v1beta1",
		"kind":       "CustomResourceDefinition",
		"spec": map[string]interface{}{
			"group":   "example.com",
			"names":   map[string]interface{}{"kind": "Widget"},
			"scope":   "Cluster",
			"version": "v1beta1",
		},
	}}
	v1Widget := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}
	scoper = NewRenderScoper([]*unstructured.Unstructured{legacy}, &unservedScoper{unserved: []schema.GroupVersionKind{gvkWidget, v1Widget}})

	if namespaced, err := scoper.IsNamespaced(gvkWidget); err != nil || namespaced {
		t.Errorf("IsNamespaced(Widget v1beta1) = %v, %v; want cluster-scoped from the v1beta1 CRD", namespaced, err)
	}

	if _, err := scoper.IsNamespaced(v1Widget); err == nil {
		t.Error("a version the CRD does not define must stay unresolved")
	}
}
