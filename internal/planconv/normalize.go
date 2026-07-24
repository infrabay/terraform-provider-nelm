package planconv

import (
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/werf/nelm/pkg/resource"
	"github.com/werf/nelm/pkg/resource/spec"
)

// NormalizeUnstructured is the single most correctness-critical function in
// this provider (design §2.1): it turns an arbitrary Kubernetes object into a
// canonical, redacted JSON string. It is used verbatim for the PLANNED side of
// the diff (a nelm plan's After object, which nelm renders client-side and so
// carries no server-side defaulting); the LIVE side goes through
// NormalizeLiveAgainst, which calls this and then projects onto the planned
// shape (see below).
//
// CONTRACTS.md's bold invariant: both sides of the diff MUST key identically
// (Key, same KeyScoper) and normalize through this same pipeline, or phantom
// diffs result.
//
// Pipeline (design §2.1):
//  1. sensitivePathsFor(gvk, annotations) — the redaction paths, including the
//     unconditional core/v1 Secret data/stringData override.
//  2. resource.RedactSensitiveData(obj, paths) — deterministic, pure;
//     redaction happens before anything else touches the object, so cleartext
//     Secret data never reaches a later step, let alone Terraform state.
//  3. spec.CleanUnstruct with {CleanRuntimeData, CleanHelmShAnnos,
//     CleanWerfIoAnnos, CleanManagedFields} — the same cleaning nelm's own
//     UDiff uses; removes status, managedFields, creationTimestamp, and helm/
//     werf bookkeeping annotations.
//  4. Marshal canonical JSON. encoding/json sorts map[string]interface{} keys
//     alphabetically by construction, which combined with never reordering
//     JSON arrays (semantically ordered, e.g. container lists) gives a
//     deterministic, canonical byte representation.
//
// It intentionally does NOT strip Kubernetes server-side defaulting fields
// (empty resources{}, dnsPolicy, Service clusterIP, StatefulSet
// updateStrategy, ...). Those are handled generically, for every kind, by
// NormalizeLiveAgainst's projection — not by a hand-maintained per-kind strip
// list, which only ever covered the handful of kinds in testdata/charts/basic
// and left every other workload kind (StatefulSet, DaemonSet, Job, ...) with a
// permanent phantom diff.
func NormalizeUnstructured(obj *unstructured.Unstructured) (out string, err error) {
	if obj == nil {
		return "", fmt.Errorf("planconv: NormalizeUnstructured: nil object")
	}

	// nelm's GetSensitiveInfo/RedactSensitiveData use lo.Must around
	// strconv.ParseBool (for werf.io/sensitive) and jp.ParseString (for
	// werf.io/sensitive-paths), so a live cluster object carrying a malformed
	// value for one of those annotations — e.g. werf.io/sensitive: "definitely"
	// — makes them PANIC. nelm validates chart manifests before planning, but
	// the LIVE objects this reads during Read bypass that validation, so an
	// untrusted/hand-edited annotation must not be allowed to crash the whole
	// provider plugin (which would abort every concurrent resource). Convert
	// any such panic into an ordinary error diagnostic.
	defer func() {
		if r := recover(); r != nil {
			out = ""
			err = fmt.Errorf("planconv: NormalizeUnstructured: recovered from a panic while redacting/cleaning %s %q "+
				"(likely a malformed werf.io/sensitive or werf.io/sensitive-paths annotation on the live object): %v",
				obj.GroupVersionKind().String(), obj.GetName(), r)
		}
	}()

	gvk := obj.GroupVersionKind()
	paths := sensitivePathsFor(gvk, obj.GetAnnotations())

	redacted := resource.RedactSensitiveData(obj, paths)

	cleaned := spec.CleanUnstruct(redacted, spec.CleanUnstructOptions{
		CleanRuntimeData:   true,
		CleanHelmShAnnos:   true,
		CleanWerfIoAnnos:   true,
		CleanManagedFields: true,
	})

	canon, err := json.Marshal(cleaned.Object)
	if err != nil {
		return "", fmt.Errorf("planconv: NormalizeUnstructured: marshal canonical json: %w", err)
	}

	return string(canon), nil
}

// NormalizeLiveAgainst normalizes a LIVE cluster object for comparison against
// the stored desired (planned) normalized JSON for the same resource key. It
// first normalizes obj exactly like the planned side (NormalizeUnstructured),
// then PROJECTS the result onto the shape of desired: only fields present in
// the desired object are kept.
//
// This is what makes the diff surface converge WITHOUT a per-kind strip list.
// nelm renders the planned/After object client-side, so it carries none of the
// API server's defaulting (empty resources{}, terminationMessagePath,
// dnsPolicy, Service clusterIP, StatefulSet updateStrategy, ServiceAccount
// tokens, ...); a live GET always carries them. Projecting the live object
// onto the desired shape drops exactly those server-added fields — for ANY
// kind — while preserving genuine drift: a field the chart DOES set whose live
// value changed stays in the projection and still diffs, and a desired field
// the cluster dropped is absent from the projection and so diffs too.
//
// desired is the prior/planned normalized JSON string for this resource key
// (from BuildPlannedResources or a previous Read). When it is empty — a
// resource seen live with no stored desired counterpart, e.g. the first Read
// after a plain-helm import — no projection is possible and the full
// normalized live object is returned; subsequent plans converge it (design
// §2.4).
func NormalizeLiveAgainst(obj *unstructured.Unstructured, desired string) (string, error) {
	liveJSON, err := NormalizeUnstructured(obj)
	if err != nil {
		return "", err
	}

	if desired == "" {
		return liveJSON, nil
	}

	liveMap, err := unmarshalCanonical(liveJSON)
	if err != nil {
		return "", fmt.Errorf("planconv: NormalizeLiveAgainst: unmarshal live json: %w", err)
	}
	desiredMap, err := unmarshalCanonical(desired)
	if err != nil {
		return "", fmt.Errorf("planconv: NormalizeLiveAgainst: unmarshal desired json: %w", err)
	}

	projected := projectOnto(liveMap, desiredMap)

	canon, err := json.Marshal(projected)
	if err != nil {
		return "", fmt.Errorf("planconv: NormalizeLiveAgainst: marshal projected json: %w", err)
	}

	return string(canon), nil
}

// unmarshalCanonical decodes a canonical JSON object with UseNumber, so every
// number is kept as a json.Number (its exact source digits) rather than being
// coerced to float64. That preservation is required for byte-equality with the
// planned side: the planned side marshals unstructured int64 values directly
// (json.Marshal(int64(n)) -> the exact integer), so a float64 round-trip here
// would silently lose precision above 2^53 and manufacture a permanent phantom
// diff on any large integer field. json.Number re-marshals to the same digits.
func unmarshalCanonical(s string) (map[string]interface{}, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()

	var m map[string]interface{}
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}

	return m, nil
}

// projectOnto returns the subtree of live that structurally matches desired:
//   - objects: keep only keys present in desired, recursing into each. Keys in
//     live but not desired (server-side defaulting) are dropped; keys in
//     desired but not live (a field the cluster dropped) are absent from the
//     result, so they still surface as drift against the desired side.
//   - arrays: project element-wise up to the shorter length (helm renders
//     ordered lists — containers, ports, env — deterministically and the API
//     server preserves order). Extra live elements (e.g. an admission-webhook-
//     injected sidecar) are dropped; a shorter live array still diffs.
//   - scalars, and any type mismatch between the two sides: take live's own
//     value, so a genuine value change on a chart-managed field still surfaces.
func projectOnto(live, desired interface{}) interface{} {
	switch d := desired.(type) {
	case map[string]interface{}:
		lm, ok := live.(map[string]interface{})
		if !ok {
			return live
		}

		out := make(map[string]interface{}, len(d))
		for k, dv := range d {
			if lv, ok := lm[k]; ok {
				out[k] = projectOnto(lv, dv)
			}
		}

		return out

	case []interface{}:
		ls, ok := live.([]interface{})
		if !ok {
			return live
		}

		n := len(ls)
		if len(d) < n {
			n = len(d)
		}

		out := make([]interface{}, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, projectOnto(ls[i], d[i]))
		}

		return out

	default:
		return live
	}
}
