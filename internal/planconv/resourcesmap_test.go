package planconv

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/werf/nelm/pkg/plan"
	"github.com/werf/nelm/pkg/resource/spec"
)

// TestBuildPlannedResources_SkipsHooks guards finding #5: helm/nelm hooks
// (marked by the helm.sh/hook annotation) must NOT enter the planned resources
// map, because Read's live side (Client.Get) only ever returns non-hook
// Resources. A hook left in the planned map would be a key Read can never
// reproduce → a perpetual diff/update cycle.
func TestBuildPlannedResources_SkipsHooks(t *testing.T) {
	scoper := basicScoper()

	mk := func(name string, hook bool) *plan.ResourceChange {
		annos := map[string]interface{}{}
		if hook {
			annos["helm.sh/hook"] = "pre-install"
		}
		return &plan.ResourceChange{
			Type: "create",
			ResourceMeta: &spec.ResourceMeta{
				Name:             name,
				Namespace:        "ns",
				GroupVersionKind: gvkConfigMap,
			},
			After: &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": name, "namespace": "ns", "annotations": annos},
				"data":       map[string]interface{}{"k": "v"},
			}},
		}
	}

	out, warns, err := BuildPlannedResources(
		nil,
		[]*plan.ResourceChange{mk("regular", false), mk("the-hook", true)},
		"ns", scoper,
	)
	if err != nil {
		t.Fatalf("BuildPlannedResources: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}

	if len(out) != 1 {
		t.Fatalf("expected exactly 1 entry (hook skipped), got %d: %v", len(out), out)
	}
	for k := range out {
		if strings.Contains(k, "the-hook") {
			t.Errorf("hook change was not skipped; key present: %s", k)
		}
		if !strings.Contains(k, "regular") {
			t.Errorf("regular resource key unexpected: %s", k)
		}
	}
}
