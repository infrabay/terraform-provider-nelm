package planconv

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/werf/nelm/pkg/plan"
	"github.com/werf/nelm/pkg/resource/spec"
)

// TestBuildPlannedResources_SkipsHooks: helm/nelm hooks (marked by the
// helm.sh/hook annotation) must NOT enter the planned resources map, because
// Read's live side (Client.Get) only ever returns non-hook Resources. A hook
// left in the planned map would be a key Read can never reproduce → a
// perpetual diff/update cycle.
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
		"ns", scoper, nil, nil,
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

// TestBuildPlannedResources_UpdatePrefersRender is the regression suite for
// the update planned-value source: the chart's client render is authoritative,
// immune to BOTH failure modes of the three-way heuristic —
// (a) a prior poisoned with live fields (import's full-live first Read) must
// not make the KNOWN value track live-mutable state, and (b) a field the
// chart NEWLY manages that already exists live must not be dropped.
func TestBuildPlannedResources_UpdatePrefersRender(t *testing.T) {
	scoper := basicScoper()

	dep := func(image string, replicas int64) *unstructured.Unstructured {
		return deployment(image, replicas, nil)
	}

	// Rendered desired: chart manages image v2 AND (newly) replicas=5.
	renderedNorm, err := NormalizeUnstructured(dep("web:v2", 5), nil)
	if err != nil {
		t.Fatalf("normalize rendered: %v", err)
	}

	key := "apps/v1/Deployment/ns/web"
	rendered := map[string]string{key: renderedNorm}

	// Poisoned prior: import's first Read stored the FULL live object,
	// including an HPA-owned replicas=3 the old chart never set.
	poisonedPrior, err := NormalizeUnstructured(dep("web:v1", 3), nil)
	if err != nil {
		t.Fatalf("normalize prior: %v", err)
	}

	mkUpdate := func(liveReplicas int64) []*plan.ResourceChange {
		return []*plan.ResourceChange{{
			Type: "update",
			ResourceMeta: &spec.ResourceMeta{
				Name:             "web",
				Namespace:        "ns",
				GroupVersionKind: gvkDeployment,
			},
			Before: dep("web:v1", liveReplicas),
			After:  dep("web:v2", liveReplicas), // SSA dry-run merge carries live replicas
		}}
	}

	prior := map[string]string{key: poisonedPrior}

	// Plan phase: HPA at 3.
	planPhase, _, err := BuildPlannedResources(prior, mkUpdate(3), "ns", scoper, rendered, nil)
	if err != nil {
		t.Fatalf("BuildPlannedResources (plan phase): %v", err)
	}

	// Apply phase re-plan: HPA moved to 7 in between.
	applyPhase, _, err := BuildPlannedResources(prior, mkUpdate(7), "ns", scoper, rendered, nil)
	if err != nil {
		t.Fatalf("BuildPlannedResources (apply phase): %v", err)
	}

	if planPhase[key] != applyPhase[key] {
		t.Fatalf("KNOWN planned value depends on live-mutable state:\nplan:  %s\napply: %s", planPhase[key], applyPhase[key])
	}
	if planPhase[key] != renderedNorm {
		t.Fatalf("planned value must equal the chart render:\ngot:  %s\nwant: %s", planPhase[key], renderedNorm)
	}
	if !strings.Contains(planPhase[key], `"replicas":5`) {
		t.Errorf("chart-newly-managed replicas (already present live) was lost: %s", planPhase[key])
	}
}

// TestKeyWithScopeFallback_NoKindMatch: a kind the RESTMapper cannot resolve
// (CRD shipping in this very release, or removed out-of-band) must still key
// on the planned side using the manifest/release namespace, not hard-error.
func TestKeyWithScopeFallback_NoKindMatch(t *testing.T) {
	gvkWidget := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}

	noMatch := noKindScoper{err: &meta.NoKindMatchError{GroupKind: gvkWidget.GroupKind()}}

	got, err := keyWithScopeFallback(Ref{GroupVersionKind: gvkWidget, Name: "w"}, "rel-ns", noMatch)
	if err != nil {
		t.Fatalf("keyWithScopeFallback: %v", err)
	}
	if want := "example.com/v1/Widget/rel-ns/w"; got != want {
		t.Fatalf("key = %q, want %q", got, want)
	}

	// A non-NoKindMatch scoper failure must still be a hard error.
	broken := noKindScoper{err: errFakeScoper}
	if _, err := keyWithScopeFallback(Ref{GroupVersionKind: gvkWidget, Name: "w"}, "rel-ns", broken); err == nil {
		t.Fatal("expected a hard error for a non-NoKindMatch scoper failure")
	}
}

var errFakeScoper = fmt.Errorf("scoper exploded")

type noKindScoper struct{ err error }

func (s noKindScoper) IsNamespaced(schema.GroupVersionKind) (bool, error) {
	return false, s.err
}
