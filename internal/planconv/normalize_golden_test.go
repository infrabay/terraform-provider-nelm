package planconv

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// releaseNSFixture is the release namespace scripts/smoke/lifecycle used to
// capture every testdata/normalize/* fixture (see STRIP_LIST.md and the
// per-kind .cleaned-diff.json files, which all show it as the added
// /metadata/namespace value).
const releaseNSFixture = "tfnelm-fix-lifecycle-74a6c3"

// unpatchedKinds are the 5 of the 6 captured resource kinds whose live GET
// and plan-After object are expected to normalize byte-identically: nothing
// in scripts/smoke/lifecycle drifted them out-of-band the way step (c)'s
// `kubectl patch deployment ... replicas=3` drifted the Deployment (see
// STRIP_LIST.md's caveat). Deployment is asserted separately.
var unpatchedKinds = []string{"ClusterRole", "ClusterRoleBinding", "ConfigMap", "Secret", "Service"}

// refOf builds a Ref the way BuildLiveResources/BuildPlannedResources do:
// from the object's own GVK/namespace/name.
func refOf(obj *unstructured.Unstructured) Ref {
	return Ref{
		GroupVersionKind: obj.GroupVersionKind(),
		Namespace:        obj.GetNamespace(),
		Name:             obj.GetName(),
	}
}

// TestNormalizeGoldenPhantomDiff is the phantom-diff golden suite: it loads
// the committed captured-live testdata/normalize fixtures (produced by
// scripts/smoke/normalize, read-only here) — never hand-written objects — and
// drives them through the exact pair CONTRACTS.md's bold invariant names: the
// PLANNED side is NormalizeUnstructured(planAfter) and the LIVE side is
// NormalizeLiveAgainst(live, planNorm) (project the live object onto the
// planned shape), with the same KeyScoper keying both.
//
// For the 5 kinds nothing drifted out-of-band, it asserts the planned and live
// sides key identically AND normalize byte-identically — i.e. no phantom diff,
// proving projection strips Kubernetes' server-side defaulting for every kind
// generically (not just the 2 the old per-kind strip list special-cased). For
// Deployment (deliberately drifted post-capture, see STRIP_LIST.md), it
// asserts the two sides still key identically but their normalized values
// differ in exactly one place: /spec/replicas — the real, intentional drift,
// proving projection does not also erase genuine changes.
func TestNormalizeGoldenPhantomDiff(t *testing.T) {
	scoper := basicScoper()

	assertKeysMatch := func(t *testing.T, kind string, live, planAfter *unstructured.Unstructured) {
		t.Helper()

		liveKey, err := Key(refOf(live), releaseNSFixture, scoper)
		if err != nil {
			t.Fatalf("Key(live): %v", err)
		}
		planKey, err := Key(refOf(planAfter), releaseNSFixture, scoper)
		if err != nil {
			t.Fatalf("Key(planAfter): %v", err)
		}
		if liveKey != planKey {
			t.Fatalf("live and planAfter keyed differently for %s:\n  live: %s\n  plan: %s", kind, liveKey, planKey)
		}
	}

	// normalizedPair returns the planned-side normalization and the live-side
	// projection onto it — the two strings Terraform compares for this key.
	normalizedPair := func(t *testing.T, live, planAfter *unstructured.Unstructured) (planNorm, projected string) {
		t.Helper()

		planNorm, err := NormalizeUnstructured(planAfter, nil)
		if err != nil {
			t.Fatalf("NormalizeUnstructured(planAfter): %v", err)
		}
		projected, err = NormalizeLiveAgainst(live, planNorm, nil)
		if err != nil {
			t.Fatalf("NormalizeLiveAgainst(live): %v", err)
		}

		return planNorm, projected
	}

	for _, kind := range unpatchedKinds {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			live := loadUnstructured(t, normalizeFixture(kind+".live.raw.json"))
			planAfter := loadUnstructured(t, normalizeFixture(kind+".planafter.raw.json"))

			assertKeysMatch(t, kind, live, planAfter)

			planNorm, projected := normalizedPair(t, live, planAfter)
			if projected != planNorm {
				paths := diffJSONPaths(t, projected, planNorm)
				t.Fatalf("%s: expected the projected live side to equal the planned side (no phantom diff), got residual paths %v\nlive:  %s\nplan:  %s", kind, paths, projected, planNorm)
			}
		})
	}

	t.Run("Deployment", func(t *testing.T) {
		live := loadUnstructured(t, normalizeFixture("Deployment.live.raw.json"))
		planAfter := loadUnstructured(t, normalizeFixture("Deployment.planafter.raw.json"))

		assertKeysMatch(t, "Deployment", live, planAfter)

		planNorm, projected := normalizedPair(t, live, planAfter)
		if projected == planNorm {
			t.Fatalf("Deployment: expected the intentional replicas drift to survive projection, got byte-identical output")
		}

		paths := diffJSONPaths(t, projected, planNorm)
		if len(paths) != 1 || paths[0] != "/spec/replicas" {
			t.Fatalf("Deployment: expected exactly one residual diff path [/spec/replicas], got %v\nlive:  %s\nplan:  %s", paths, projected, planNorm)
		}
	})
}
