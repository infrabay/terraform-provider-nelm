package planconv

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/werf/nelm/pkg/plan"
	"github.com/werf/nelm/pkg/resource/spec"
)

// renderedConfigMap is a chart render (ChartRender output: no release
// ownership metadata).
func renderedConfigMap(labels map[string]interface{}) *unstructured.Unstructured {
	meta := map[string]interface{}{"name": "app"}
	if labels != nil {
		meta["labels"] = labels
	}

	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   meta,
		"data":       map[string]interface{}{"k": "v"},
	}}
}

// installed is obj as nelm installs it: ReleaseMetadataPatcher adds the
// release ownership annotations and the managed-by label.
func installed(obj *unstructured.Unstructured) *unstructured.Unstructured {
	out := obj.DeepCopy()
	out.SetNamespace("ns")

	annos := out.GetAnnotations()
	if annos == nil {
		annos = map[string]string{}
	}

	annos["meta.helm.sh/release-name"] = "app"
	annos["meta.helm.sh/release-namespace"] = "ns"
	out.SetAnnotations(annos)

	labels := out.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}

	labels["app.kubernetes.io/managed-by"] = "Helm"
	out.SetLabels(labels)

	return out
}

// TestNormalizeUnstructured_DropsReleaseOwnershipMetadata is the regression
// test for release ownership metadata: nelm stamps meta.helm.sh/release-name,
// meta.helm.sh/release-namespace and app.kubernetes.io/managed-by on what it
// installs (a create's After, every live object) but not on a chart render (an
// update's planned value), so the first update of every object showed them as
// removed although nothing changes.
func TestNormalizeUnstructured_DropsReleaseOwnershipMetadata(t *testing.T) {
	for name, labels := range map[string]map[string]interface{}{
		"chart labels":             {"app.kubernetes.io/name": "app"},
		"chart renders managed-by": {"app.kubernetes.io/name": "app", "app.kubernetes.io/managed-by": "Helm"},
		"no chart labels":          nil,
		"only a managed-by label":  {"app.kubernetes.io/managed-by": "Helm"},
	} {
		t.Run(name, func(t *testing.T) {
			render := renderedConfigMap(labels)

			want, err := NormalizeUnstructured(render, nil)
			if err != nil {
				t.Fatalf("normalize render: %v", err)
			}

			got, err := NormalizeUnstructured(installed(render), nil)
			if err != nil {
				t.Fatalf("normalize installed: %v", err)
			}

			if got != want {
				t.Errorf("installed object normalizes to\n%s\nwant the render's\n%s", got, want)
			}

			for _, leftover := range []string{"meta.helm.sh", "managed-by", `"labels":{}`, `"annotations":{}`} {
				if strings.Contains(got, leftover) {
					t.Errorf("normalized value still contains %q: %s", leftover, got)
				}
			}
		})
	}
}

// TestBuildPlannedResources_ValueIndependentOfChangeType: every change type
// plans the rendered value, so the value does not depend on how nelm
// classified the change (a webhook timing out between the plan and the apply
// phase turns an update into a blind apply).
func TestBuildPlannedResources_ValueIndependentOfChangeType(t *testing.T) {
	scoper := basicScoper()
	render := renderedConfigMap(map[string]interface{}{"app.kubernetes.io/name": "app"})

	rendered, err := BuildRenderedResources([]*unstructured.Unstructured{render}, "ns", scoper, nil)
	if err != nil {
		t.Fatalf("BuildRenderedResources: %v", err)
	}

	const key = "v1/ConfigMap/ns/app"

	for _, typ := range []string{"create", "recreate", "blind apply", "update"} {
		after := installed(render)
		// A dry-run merge or live object carries server-added fields.
		after.Object["immutable"] = false

		change := &plan.ResourceChange{
			Type:         typ,
			ResourceMeta: &spec.ResourceMeta{Name: "app", Namespace: "ns", GroupVersionKind: gvkConfigMap},
			After:        after,
			Before:       installed(render),
		}

		out, _, err := BuildPlannedResources(map[string]string{}, []*plan.ResourceChange{change}, "ns", scoper, rendered, nil)
		if err != nil {
			t.Fatalf("%s: BuildPlannedResources: %v", typ, err)
		}

		if out[key] != rendered[key] {
			t.Errorf("%s: planned %s, want the rendered %s", typ, out[key], rendered[key])
		}
	}
}
