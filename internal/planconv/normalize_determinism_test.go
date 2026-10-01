package planconv

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// diffJSONPaths decodes two canonical JSON strings (as produced by
// NormalizeUnstructured) and returns the sorted, JSON-pointer-style set of
// leaf paths at which they differ. It is a debugging/assertion aid for the
// golden tests only — not a general-purpose JSON-diff implementation — and
// deliberately does not attempt list-element-wise diffing beyond index
// position, which matches NormalizeUnstructured's own "arrays are
// semantically ordered, never reordered" contract (normalize.go).
func diffJSONPaths(t *testing.T, a, b string) []string {
	t.Helper()

	var av, bv interface{}
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		t.Fatalf("diffJSONPaths: unmarshal a: %v", err)
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		t.Fatalf("diffJSONPaths: unmarshal b: %v", err)
	}

	var paths []string
	collectDiffPaths("", av, bv, &paths)
	sort.Strings(paths)

	return paths
}

func collectDiffPaths(prefix string, a, b interface{}, out *[]string) {
	switch av := a.(type) {
	case map[string]interface{}:
		bv, ok := b.(map[string]interface{})
		if !ok {
			*out = append(*out, prefix)
			return
		}

		keys := make(map[string]struct{}, len(av)+len(bv))
		for k := range av {
			keys[k] = struct{}{}
		}
		for k := range bv {
			keys[k] = struct{}{}
		}

		for k := range keys {
			aChild, aOK := av[k]
			bChild, bOK := bv[k]
			childPath := prefix + "/" + k

			switch {
			case aOK && bOK:
				collectDiffPaths(childPath, aChild, bChild, out)
			default:
				*out = append(*out, childPath)
			}
		}

	case []interface{}:
		bv, ok := b.([]interface{})
		if !ok {
			*out = append(*out, prefix)
			return
		}

		n := len(av)
		if len(bv) > n {
			n = len(bv)
		}
		for i := 0; i < n; i++ {
			childPath := fmt.Sprintf("%s/%d", prefix, i)
			if i >= len(av) || i >= len(bv) {
				*out = append(*out, childPath)
				continue
			}
			collectDiffPaths(childPath, av[i], bv[i], out)
		}

	default:
		if a != b {
			*out = append(*out, prefix)
		}
	}
}

// TestNormalizeUnstructuredDeterministic verifies the pipeline's determinism
// claim (normalize.go lines 41-44): encoding/json.Marshal sorts
// map[string]interface{} keys alphabetically by construction, so canonical
// JSON output must not depend on either (a) how many times
// NormalizeUnstructured runs against equivalent input, or (b) the original
// map's key insertion order.
func TestNormalizeUnstructuredDeterministic(t *testing.T) {
	t.Run("repeated calls agree", func(t *testing.T) {
		for _, kind := range append(append([]string{}, unpatchedKinds...), "Deployment") {
			kind := kind
			t.Run(kind, func(t *testing.T) {
				var first string

				for i := 0; i < 25; i++ {
					// Fresh unstructured.Unstructured decode each iteration:
					// Go's map iteration order is randomized per-process,
					// so re-decoding (rather than reusing one *Unstructured)
					// exercises independently-ordered map internals feeding
					// the same Marshal call.
					obj := loadUnstructured(t, normalizeFixture(kind+".live.raw.json"))

					got, err := NormalizeUnstructured(obj, nil)
					if err != nil {
						t.Fatalf("iteration %d: NormalizeUnstructured: %v", i, err)
					}

					if i == 0 {
						first = got
						continue
					}

					if got != first {
						t.Fatalf("iteration %d produced different canonical JSON than iteration 0:\n0: %s\n%d: %s", i, first, i, got)
					}
				}
			})
		}
	})

	t.Run("insertion order independent", func(t *testing.T) {
		// Two hand-built objects with identical content but deliberately
		// different map/slice key insertion order. If NormalizeUnstructured's
		// canonical-JSON claim holds, both must normalize byte-identically.
		objA := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name": "order-test",
				"labels": map[string]interface{}{
					"z-label": "1",
					"a-label": "2",
				},
			},
			"data": map[string]interface{}{
				"zzz": "last-key-first",
				"aaa": "first-key-last",
			},
		}}

		objB := &unstructured.Unstructured{Object: map[string]interface{}{
			"data": map[string]interface{}{
				"aaa": "first-key-last",
				"zzz": "last-key-first",
			},
			"kind": "ConfigMap",
			"metadata": map[string]interface{}{
				"labels": map[string]interface{}{
					"a-label": "2",
					"z-label": "1",
				},
				"name": "order-test",
			},
			"apiVersion": "v1",
		}}

		gotA, err := NormalizeUnstructured(objA, nil)
		if err != nil {
			t.Fatalf("NormalizeUnstructured(objA): %v", err)
		}
		gotB, err := NormalizeUnstructured(objB, nil)
		if err != nil {
			t.Fatalf("NormalizeUnstructured(objB): %v", err)
		}

		if gotA != gotB {
			t.Fatalf("canonical JSON depended on map insertion order:\nA: %s\nB: %s", gotA, gotB)
		}
	})
}
