package planconv

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestNormalizeLiveAgainst_GenericKind is the regression test for the
// per-kind-strip-list phantom-diff bug. The old normalize path only stripped
// server-side defaulting for apps/v1 Deployment and v1 Service, so any other
// workload kind — here a StatefulSet, the original repro — produced a
// permanent phantom diff. Projection (NormalizeLiveAgainst) fixes it
// generically for every kind.
//
// The objects are hand-built (not captured-live goldens) on purpose: this
// exercises the kind-agnostic projection mechanism, and `live` is a strict
// structural superset of `desired` — desired's chart-rendered shape plus the
// PodSpec/StatefulSet server-defaulting fields the old code never stripped for
// this kind.
func TestNormalizeLiveAgainst_GenericKind(t *testing.T) {
	desired := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "StatefulSet",
		"metadata": map[string]interface{}{
			"name":   "web",
			"labels": map[string]interface{}{"app": "web"},
		},
		"spec": map[string]interface{}{
			"serviceName": "web",
			"replicas":    int64(3),
			"selector":    map[string]interface{}{"matchLabels": map[string]interface{}{"app": "web"}},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "web"}},
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "nginx",
							"image": "nginx:1.25",
							"ports": []interface{}{
								map[string]interface{}{"containerPort": int64(80)},
							},
						},
					},
				},
			},
		},
	}}

	// live = desired + everything the API server defaults in for a StatefulSet
	// (none of which the old strip list covered for this kind), plus runtime
	// metadata/status that CleanUnstruct removes.
	live := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "StatefulSet",
		"metadata": map[string]interface{}{
			"name":              "web",
			"namespace":         "default",
			"labels":            map[string]interface{}{"app": "web"},
			"uid":               "d290f1ee-6c54-4b01-90e6-d701748f0851",
			"resourceVersion":   "12345",
			"generation":        int64(1),
			"creationTimestamp": "2024-01-01T00:00:00Z",
		},
		"spec": map[string]interface{}{
			"serviceName":          "web",
			"replicas":             int64(3),
			"selector":             map[string]interface{}{"matchLabels": map[string]interface{}{"app": "web"}},
			"podManagementPolicy":  "OrderedReady",
			"revisionHistoryLimit": int64(10),
			"updateStrategy": map[string]interface{}{
				"type":          "RollingUpdate",
				"rollingUpdate": map[string]interface{}{"partition": int64(0)},
			},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "web"}},
				"spec": map[string]interface{}{
					"dnsPolicy":                     "ClusterFirst",
					"restartPolicy":                 "Always",
					"schedulerName":                 "default-scheduler",
					"securityContext":               map[string]interface{}{},
					"terminationGracePeriodSeconds": int64(30),
					"containers": []interface{}{
						map[string]interface{}{
							"name":                     "nginx",
							"image":                    "nginx:1.25",
							"imagePullPolicy":          "IfNotPresent",
							"resources":                map[string]interface{}{},
							"terminationMessagePath":   "/dev/termination-log",
							"terminationMessagePolicy": "File",
							"ports": []interface{}{
								map[string]interface{}{"containerPort": int64(80), "protocol": "TCP"},
							},
						},
					},
				},
			},
		},
		"status": map[string]interface{}{"replicas": int64(3), "readyReplicas": int64(3)},
	}}

	desiredNorm, err := NormalizeUnstructured(desired, nil)
	if err != nil {
		t.Fatalf("NormalizeUnstructured(desired): %v", err)
	}

	projected, err := NormalizeLiveAgainst(live, desiredNorm, nil)
	if err != nil {
		t.Fatalf("NormalizeLiveAgainst(live): %v", err)
	}

	if projected != desiredNorm {
		paths := diffJSONPaths(t, projected, desiredNorm)
		t.Fatalf("StatefulSet: projection left a phantom diff at %v\nprojected: %s\ndesired:   %s", paths, projected, desiredNorm)
	}

	// Genuine drift on a chart-managed field (replicas) must still surface.
	drift := live.DeepCopy()
	if err := unstructured.SetNestedField(drift.Object, int64(5), "spec", "replicas"); err != nil {
		t.Fatalf("set drifted replicas: %v", err)
	}

	driftProjected, err := NormalizeLiveAgainst(drift, desiredNorm, nil)
	if err != nil {
		t.Fatalf("NormalizeLiveAgainst(drift): %v", err)
	}

	if driftProjected == desiredNorm {
		t.Fatal("StatefulSet: expected replicas drift to survive projection, got byte-identical output")
	}
	if paths := diffJSONPaths(t, driftProjected, desiredNorm); len(paths) != 1 || paths[0] != "/spec/replicas" {
		t.Fatalf("StatefulSet: expected exactly one drift path [/spec/replicas], got %v", paths)
	}
}

// TestProjectOnto covers the structural rules directly.
func TestProjectOnto(t *testing.T) {
	t.Run("extra live object keys dropped", func(t *testing.T) {
		live := map[string]interface{}{"a": "1", "server": "default"}
		desired := map[string]interface{}{"a": "1"}
		got := projectOnto(live, desired).(map[string]interface{})
		if _, ok := got["server"]; ok {
			t.Errorf("server-added key not dropped: %v", got)
		}
		if got["a"] != "1" {
			t.Errorf("desired key not preserved: %v", got)
		}
	})

	t.Run("extra live array elements dropped", func(t *testing.T) {
		live := []interface{}{"a", "b", "injected"}
		desired := []interface{}{"a", "b"}
		got := projectOnto(live, desired).([]interface{})
		if len(got) != 2 {
			t.Errorf("expected length 2 (extra live element dropped), got %v", got)
		}
	})

	t.Run("scalar drift kept from live", func(t *testing.T) {
		live := map[string]interface{}{"replicas": int64(5)}
		desired := map[string]interface{}{"replicas": int64(3)}
		got := projectOnto(live, desired).(map[string]interface{})
		if got["replicas"] != int64(5) {
			t.Errorf("expected live's drifted value 5, got %v", got["replicas"])
		}
	})

	t.Run("desired-only key surfaces as absent", func(t *testing.T) {
		live := map[string]interface{}{}
		desired := map[string]interface{}{"gone": "x"}
		got := projectOnto(live, desired).(map[string]interface{})
		if _, ok := got["gone"]; ok {
			t.Errorf("a key absent on the cluster must stay absent (so it diffs): %v", got)
		}
	})
}

// TestNormalizeLiveAgainst_LargeInteger guards against a float64 round-trip in
// the projection: an integer field above 2^53 must survive byte-identically,
// or every plan would show a permanent phantom diff on it (the planned side
// marshals int64 exactly). Uses UseNumber under the hood.
func TestNormalizeLiveAgainst_LargeInteger(t *testing.T) {
	const bigInt = int64(9007199254740993) // 2^53 + 1: not representable as float64

	obj := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "example.com/v1",
			"kind":       "Widget",
			"metadata":   map[string]interface{}{"name": "w"},
			"spec":       map[string]interface{}{"big": bigInt},
		}}
	}

	desiredNorm, err := NormalizeUnstructured(obj(), nil)
	if err != nil {
		t.Fatalf("NormalizeUnstructured(desired): %v", err)
	}

	projected, err := NormalizeLiveAgainst(obj(), desiredNorm, nil)
	if err != nil {
		t.Fatalf("NormalizeLiveAgainst: %v", err)
	}

	if projected != desiredNorm {
		t.Fatalf("large integer diverged through projection:\nprojected: %s\ndesired:   %s", projected, desiredNorm)
	}
	if !strings.Contains(projected, "9007199254740993") {
		t.Fatalf("expected the exact integer digits, got: %s", projected)
	}
}
