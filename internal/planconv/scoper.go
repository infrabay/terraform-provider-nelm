package planconv

import (
	"errors"
	"sort"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// RenderScoper is the KeyScoper for the planned side of the diff. It asks
// scoper first; for a kind the cluster does not serve yet (scoper returns a
// "no matches for kind" error) it answers from the CustomResourceDefinitions
// in the chart's own render — the CRD+CR-in-one-chart case, where nelm plans
// the CR before its CRD exists. A kind neither knows is left to
// keyWithScopeFallback's namespace guess and recorded (Unresolved): its key,
// and so the planned map's key set, may change once the kind is served
// (e.g. its CRD comes from another release in the same apply), so the
// caller must not plan that map as a known value.
//
// For a served kind it answers exactly like scoper, and the live side never
// sees an unserved kind (LiveObjects skips it), so both sides of the diff
// still key through the same scope answers (CONTRACTS.md seam 2).
type RenderScoper struct {
	scoper     KeyScoper
	crdScopes  map[schema.GroupVersionKind]bool
	unresolved map[schema.GroupVersionKind]struct{}
}

var _ KeyScoper = (*RenderScoper)(nil)

// NewRenderScoper builds a RenderScoper over scoper from the CRDs among
// objs (a chart render, crds/ included).
func NewRenderScoper(objs []*unstructured.Unstructured, scoper KeyScoper) *RenderScoper {
	s := &RenderScoper{
		scoper:     scoper,
		crdScopes:  map[schema.GroupVersionKind]bool{},
		unresolved: map[schema.GroupVersionKind]struct{}{},
	}

	for _, obj := range objs {
		if obj == nil || obj.GetKind() != "CustomResourceDefinition" ||
			obj.GroupVersionKind().Group != "apiextensions.k8s.io" {
			continue
		}

		group, _, _ := unstructured.NestedString(obj.Object, "spec", "group")
		kind, _, _ := unstructured.NestedString(obj.Object, "spec", "names", "kind")
		scope, _, _ := unstructured.NestedString(obj.Object, "spec", "scope")

		if group == "" || kind == "" || (scope != "Namespaced" && scope != "Cluster") {
			continue
		}

		for _, version := range crdVersions(obj) {
			s.crdScopes[schema.GroupVersionKind{Group: group, Version: version, Kind: kind}] = scope == "Namespaced"
		}
	}

	return s
}

// crdVersions lists the versions a CRD serves: spec.versions[].name
// (apiextensions.k8s.io/v1), or the single spec.version of a v1beta1 CRD.
func crdVersions(crd *unstructured.Unstructured) []string {
	var out []string

	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for _, v := range versions {
		if vm, ok := v.(map[string]interface{}); ok {
			if name, ok := vm["name"].(string); ok && name != "" {
				out = append(out, name)
			}
		}
	}

	if version, _, _ := unstructured.NestedString(crd.Object, "spec", "version"); version != "" {
		out = append(out, version)
	}

	return out
}

// IsNamespaced implements KeyScoper.
func (s *RenderScoper) IsNamespaced(gvk schema.GroupVersionKind) (bool, error) {
	namespaced, err := s.scoper.IsNamespaced(gvk)
	if err == nil {
		return namespaced, nil
	}

	var noMatch *meta.NoKindMatchError
	if !errors.As(err, &noMatch) {
		return false, err
	}

	if namespaced, ok := s.crdScopes[gvk]; ok {
		return namespaced, nil
	}

	s.unresolved[gvk] = struct{}{}

	return false, err
}

// Unresolved returns the kinds whose scope had to be guessed, sorted.
func (s *RenderScoper) Unresolved() []schema.GroupVersionKind {
	out := make([]schema.GroupVersionKind, 0, len(s.unresolved))
	for gvk := range s.unresolved {
		out = append(out, gvk)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })

	return out
}
