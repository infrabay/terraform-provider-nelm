package planconv

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// deployment builds a minimal apps/v1 Deployment object for the update-
// projection tests. replicas < 0 omits spec.replicas entirely.
func deployment(image string, replicas int64, extraAnno map[string]interface{}) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "web"}},
		"template": map[string]interface{}{
			"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "web"}},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{"name": "app", "image": image},
				},
			},
		},
	}
	if replicas >= 0 {
		spec["replicas"] = replicas
	}

	meta := map[string]interface{}{"name": "web"}
	if len(extraAnno) > 0 {
		meta["annotations"] = extraAnno
	}

	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   meta,
		"spec":       spec,
	}}
}

// TestNormalizeUpdateAfter_DropsLiveCarriedFields is the regression test for
// plan consistency of updates: an update change's After is the server-side
// dry-run MERGE, which carries live-mutable fields the chart never set (an
// HPA-owned spec.replicas, controller-written annotations). Those must not
// enter the KNOWN plan value — they would differ between the plan-phase and
// apply-phase ModifyPlan whenever the cluster moves in between, aborting the
// apply with "inconsistent final plan".
func TestNormalizeUpdateAfter_DropsLiveCarriedFields(t *testing.T) {
	// Prior desired: chart renders no replicas (HPA owns it), image v1.
	prior, err := NormalizeUnstructured(deployment("web:v1", -1, nil), nil)
	if err != nil {
		t.Fatalf("normalize prior: %v", err)
	}

	// Live (Before): HPA has set replicas=3; a controller wrote an annotation.
	before := deployment("web:v1", 3, map[string]interface{}{"kubectl.kubernetes.io/restartedAt": "2026-01-01"})
	// Dry-run merge (After): chart bumps image to v2; merge preserves live
	// replicas and the controller annotation.
	after := deployment("web:v2", 3, map[string]interface{}{"kubectl.kubernetes.io/restartedAt": "2026-01-01"})

	got, err := NormalizeUpdateAfter(after, before, prior, nil)
	if err != nil {
		t.Fatalf("NormalizeUpdateAfter: %v", err)
	}

	if strings.Contains(got, `"replicas"`) {
		t.Errorf("live-carried HPA replicas leaked into the planned value: %s", got)
	}
	if strings.Contains(got, "restartedAt") {
		t.Errorf("live-carried controller annotation leaked into the planned value: %s", got)
	}
	if !strings.Contains(got, "web:v2") {
		t.Errorf("chart-managed image change lost: %s", got)
	}

	// Determinism across cluster movement: at the apply-phase re-plan the HPA
	// has scaled to 5 — the projected value must be byte-identical anyway.
	before5 := deployment("web:v1", 5, map[string]interface{}{"kubectl.kubernetes.io/restartedAt": "2026-01-02"})
	after5 := deployment("web:v2", 5, map[string]interface{}{"kubectl.kubernetes.io/restartedAt": "2026-01-02"})

	got5, err := NormalizeUpdateAfter(after5, before5, prior, nil)
	if err != nil {
		t.Fatalf("NormalizeUpdateAfter (apply-phase): %v", err)
	}
	if got5 != got {
		t.Fatalf("planned value depends on live-mutable state:\nplan:  %s\napply: %s", got, got5)
	}
}

// TestNormalizeUpdateAfter_KeepsChartNewFields: a field the chart introduces
// in THIS update (absent from both prior desired and live Before) must
// survive projection — it is the diff, and it must enter the stored desired
// shape or it would be invisible forever.
func TestNormalizeUpdateAfter_KeepsChartNewFields(t *testing.T) {
	prior, err := NormalizeUnstructured(deployment("web:v1", -1, nil), nil)
	if err != nil {
		t.Fatalf("normalize prior: %v", err)
	}

	before := deployment("web:v1", 3, nil)

	after := deployment("web:v1", 3, nil)
	// Chart newly renders a strategy this update.
	_ = unstructured.SetNestedField(after.Object, "Recreate", "spec", "strategy", "type")

	got, err := NormalizeUpdateAfter(after, before, prior, nil)
	if err != nil {
		t.Fatalf("NormalizeUpdateAfter: %v", err)
	}

	if !strings.Contains(got, "Recreate") {
		t.Errorf("chart-new field dropped by projection: %s", got)
	}
	if strings.Contains(got, `"replicas"`) {
		t.Errorf("live-carried replicas still leaked: %s", got)
	}
}

// TestNormalizeUpdateAfter_ChartManagedChangeSurvives: a field present in the
// prior desired keeps After's (possibly changed) value — that is the visible
// diff for chart-managed fields.
func TestNormalizeUpdateAfter_ChartManagedChangeSurvives(t *testing.T) {
	prior, err := NormalizeUnstructured(deployment("web:v1", 2, nil), nil)
	if err != nil {
		t.Fatalf("normalize prior: %v", err)
	}

	before := deployment("web:v1", 2, nil)
	after := deployment("web:v1", 4, nil) // chart changed its own replicas 2 -> 4

	got, err := NormalizeUpdateAfter(after, before, prior, nil)
	if err != nil {
		t.Fatalf("NormalizeUpdateAfter: %v", err)
	}

	if !strings.Contains(got, `"replicas":4`) {
		t.Errorf("chart-managed replicas change lost: %s", got)
	}
}

// TestNormalizeUpdateAfter_NoPriorFallsBack: with no stored desired for the
// key, projection degrades to plain normalization of After.
func TestNormalizeUpdateAfter_NoPriorFallsBack(t *testing.T) {
	after := deployment("web:v2", 3, nil)

	got, err := NormalizeUpdateAfter(after, deployment("web:v1", 3, nil), "", nil)
	if err != nil {
		t.Fatalf("NormalizeUpdateAfter: %v", err)
	}

	want, err := NormalizeUnstructured(after, nil)
	if err != nil {
		t.Fatalf("NormalizeUnstructured: %v", err)
	}

	if got != want {
		t.Fatalf("empty prior must degrade to NormalizeUnstructured(after):\ngot:  %s\nwant: %s", got, want)
	}
}

// TestNormalize_LastAppliedConfigurationStripped guards the security fix: the
// kubectl.kubernetes.io/last-applied-configuration annotation embeds a full
// serialized copy of the object — including a Secret's cleartext data, which
// path-based data.*/stringData.* redaction does not reach. It must be
// stripped unconditionally.
func TestNormalize_LastAppliedConfigurationStripped(t *testing.T) {
	const cleartext = "c3VwZXItc2VjcmV0" // base64 payload mirrored into the annotation

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]interface{}{
			"name": "db",
			"annotations": map[string]interface{}{
				"kubectl.kubernetes.io/last-applied-configuration": `{"apiVersion":"v1","kind":"Secret","data":{"password":"` + cleartext + `"}}`,
			},
		},
		"data": map[string]interface{}{"password": cleartext},
	}}

	got, err := NormalizeUnstructured(obj, nil)
	if err != nil {
		t.Fatalf("NormalizeUnstructured: %v", err)
	}

	if strings.Contains(got, cleartext) {
		t.Fatalf("cleartext leaked via last-applied-configuration: %s", got)
	}
	if strings.Contains(got, "last-applied-configuration") {
		t.Fatalf("bookkeeping annotation survived normalization: %s", got)
	}
}

// TestKey_ClusterScopedIgnoresPinnedNamespace: a manifest that (incorrectly)
// pins metadata.namespace on a cluster-scoped kind must still key with an
// empty namespace segment, or the planned and live sides would key-split
// forever.
func TestKey_ClusterScopedIgnoresPinnedNamespace(t *testing.T) {
	scoper := basicScoper()

	got, err := Key(Ref{GroupVersionKind: gvkClusterRole, Namespace: "pinned-ns", Name: "admin"}, "rel-ns", scoper)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}

	want := "rbac.authorization.k8s.io/v1/ClusterRole//admin"
	if got != want {
		t.Fatalf("Key = %q, want %q (pinned namespace must be ignored for cluster-scoped kinds)", got, want)
	}
}
