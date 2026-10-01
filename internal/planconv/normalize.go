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
// this provider: it turns an arbitrary Kubernetes object into a canonical,
// redacted JSON string. It is used verbatim for the PLANNED side of the diff
// (a nelm plan's After object, which nelm renders client-side and so carries
// no server-side defaulting); the LIVE side goes through NormalizeLiveAgainst,
// which calls this and then projects onto the planned shape (see below).
//
// CONTRACTS.md's bold invariant: both sides of the diff MUST key identically
// (Key, same KeyScoper) and normalize through this same pipeline, or phantom
// diffs result.
//
// Pipeline:
//  1. sensitivePathsFor(gvk, annotations) — the redaction paths, including the
//     unconditional core/v1 Secret data/stringData override.
//  2. resource.RedactSensitiveData(obj, paths) — deterministic, pure;
//     redaction happens before anything else touches the object, so cleartext
//     Secret data never reaches a later step, let alone Terraform state.
//  3. spec.CleanUnstruct with {CleanRuntimeData, CleanHelmShAnnos,
//     CleanWerfIoAnnos, CleanManagedFields, CleanReleaseAnnosLabels} — the
//     same cleaning nelm's own UDiff uses; removes status, managedFields,
//     creationTimestamp, helm/werf bookkeeping annotations, and the release
//     ownership metadata (meta.helm.sh/release-name, meta.helm.sh/release-
//     namespace, app.kubernetes.io/managed-by). nelm stamps that ownership
//     metadata on every object it installs but not on a chart render, so
//     keeping it would make a create's or a live object's value differ from
//     a rendered one for no change at all.
//  4. ScrubSecrets(obj, secrets) — every verbatim occurrence of a sensitive
//     input value (set_sensitive) in a string or map key is replaced with a
//     placeholder of step 2's format, so a value a chart renders into a
//     non-Secret object (an env value, ConfigMap data) does not reach plan
//     output or state in cleartext either.
//  5. Marshal canonical JSON. encoding/json sorts map[string]interface{} keys
//     alphabetically by construction, which combined with never reordering
//     JSON arrays (semantically ordered, e.g. container lists) gives a
//     deterministic, canonical byte representation.
//
// secrets are the sensitive input values of the configuration the result
// describes; both sides of the diff MUST pass the same ones (CONTRACTS.md),
// or an unchanged object compares unequal.
//
// It intentionally does NOT strip Kubernetes server-side defaulting fields
// (empty resources{}, dnsPolicy, Service clusterIP, StatefulSet
// updateStrategy, ...). Those are handled generically, for every kind, by
// NormalizeLiveAgainst's projection — not by a hand-maintained per-kind strip
// list, which only ever covered the handful of kinds in testdata/charts/basic
// and left every other workload kind (StatefulSet, DaemonSet, Job, ...) with a
// permanent phantom diff.
func NormalizeUnstructured(obj *unstructured.Unstructured, secrets []string) (out string, err error) {
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
		CleanRuntimeData:        true,
		CleanHelmShAnnos:        true,
		CleanWerfIoAnnos:        true,
		CleanManagedFields:      true,
		CleanReleaseAnnosLabels: true,
	})

	stripClientBookkeeping(cleaned)

	canon, err := json.Marshal(ScrubSecrets(cleaned.Object, secrets))
	if err != nil {
		return "", fmt.Errorf("planconv: NormalizeUnstructured: marshal canonical json: %w", err)
	}

	return string(canon), nil
}

// stripClientBookkeeping removes fields that are pure client/CLI bookkeeping
// and must never participate in the diff:
//
//   - metadata.namespace: helm-rendered manifests rarely set it, live GETs
//     always do, and nelm's HideAll skeleton for fully-sensitive kinds
//     materializes it explicitly — Key already resolves the namespace
//     separately, so the compared VALUE must not carry it (or fully-sensitive
//     kinds would phantom-diff on it forever).
//   - kubectl.kubernetes.io/last-applied-configuration: written by client-side
//     `kubectl apply`, it embeds a full serialized copy of the object —
//     INCLUDING a Secret's cleartext data, which our path-based data.*/
//     stringData.* redaction does not reach. It is never chart-rendered, so
//     stripping it both closes that leak and avoids diff noise.
//   - an annotations or labels map left empty by the cleaning: the API
//     server never returns an empty map, so a rendered object whose only
//     label was app.kubernetes.io/managed-by must not keep "labels": {}.
func stripClientBookkeeping(obj *unstructured.Unstructured) {
	m := obj.Object

	unstructured.RemoveNestedField(m, "metadata", "namespace")

	// Object-embedding applier annotations: each holds a serialized copy of
	// the object (kubectl's client-side apply, Carvel kapp, Rancher wrangler),
	// which both bypasses path-based Secret redaction and is never
	// chart-rendered.
	for _, anno := range []string{
		"kubectl.kubernetes.io/last-applied-configuration",
		"kapp.k14s.io/original",
		"objectset.rio.cattle.io/applied",
	} {
		unstructured.RemoveNestedField(m, "metadata", "annotations", anno)
	}

	for _, field := range []string{"annotations", "labels"} {
		if v, found, _ := unstructured.NestedMap(m, "metadata", field); found && len(v) == 0 {
			unstructured.RemoveNestedField(m, "metadata", field)
		}
	}
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
// normalized live object is returned; subsequent plans converge it. desired
// must have been scrubbed of the same secrets, or a map key carrying one would
// not project.
func NormalizeLiveAgainst(obj *unstructured.Unstructured, desired string, secrets []string) (string, error) {
	liveJSON, err := NormalizeUnstructured(obj, secrets)
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

// NormalizeUpdateAfter normalizes the After object of an "update" plan change.
// Unlike create/recreate/blind-apply (whose After is nelm's CLIENT-rendered
// manifest), an update's After is the API server's server-side-apply DRY-RUN
// merge result: it carries live fields the chart never set — both static
// server defaulting AND live-mutable, externally-owned values (an HPA-managed
// spec.replicas, controller-written annotations). Committing those verbatim as
// a KNOWN plan value breaks Terraform's consistency contract: the apply-phase
// ModifyPlan re-runs the dry-run against a cluster that may have moved (HPA
// scaled between plan and apply), and the recomputed value differs — the whole
// apply aborts with "Provider produced inconsistent final plan".
//
// The fix is a THREE-WAY projection using the update change's own Before (the
// live object) and the prior stored desired value for this resource key:
//
//   - a field present in priorDesired is chart-managed: keep After's value
//     (the chart may have just changed it — that IS the diff);
//   - a field absent from priorDesired but also absent from Before is newly
//     introduced by the chart in this update: keep it (it must show in the
//     diff and enter the stored desired shape);
//   - a field absent from priorDesired but present in Before is live-carried
//     (server default or externally-owned): drop it. It is dropped identically
//     at the plan-phase and apply-phase invocations regardless of how the live
//     value moved in between, which is exactly what restores determinism.
//
// priorDesired == "" (no stored desired for this key) or a nil before degrades
// to plain NormalizeUnstructured(after). after and before are scrubbed of
// secrets like NormalizeUnstructured does.
func NormalizeUpdateAfter(after, before *unstructured.Unstructured, priorDesired string, secrets []string) (string, error) {
	afterJSON, err := NormalizeUnstructured(after, secrets)
	if err != nil {
		return "", err
	}

	if priorDesired == "" || before == nil {
		return afterJSON, nil
	}

	beforeJSON, err := NormalizeUnstructured(before, secrets)
	if err != nil {
		return "", err
	}

	afterMap, err := unmarshalCanonical(afterJSON)
	if err != nil {
		return "", fmt.Errorf("planconv: NormalizeUpdateAfter: unmarshal after: %w", err)
	}
	beforeMap, err := unmarshalCanonical(beforeJSON)
	if err != nil {
		return "", fmt.Errorf("planconv: NormalizeUpdateAfter: unmarshal before: %w", err)
	}
	priorMap, err := unmarshalCanonical(priorDesired)
	if err != nil {
		return "", fmt.Errorf("planconv: NormalizeUpdateAfter: unmarshal prior desired: %w", err)
	}

	projected := projectUpdate(afterMap, beforeMap, priorMap)

	canon, err := json.Marshal(projected)
	if err != nil {
		return "", fmt.Errorf("planconv: NormalizeUpdateAfter: marshal projected json: %w", err)
	}

	return string(canon), nil
}

// projectUpdate implements NormalizeUpdateAfter's three-way rule. Arrays are
// paired positionally (same declared limitation as projectOnto): elements
// within prior's length are recursed; elements beyond prior's length are kept
// when Before has no element at that index (chart-new) and dropped when it
// does (live-carried, e.g. a webhook-appended container).
func projectUpdate(after, before, prior interface{}) interface{} {
	switch pv := prior.(type) {
	case map[string]interface{}:
		am, ok := after.(map[string]interface{})
		if !ok {
			return after
		}

		bm, _ := before.(map[string]interface{})

		out := make(map[string]interface{}, len(am))
		for k, av := range am {
			if p, inPrior := pv[k]; inPrior {
				var b interface{}
				if bm != nil {
					b = bm[k]
				}
				out[k] = projectUpdate(av, b, p)

				continue
			}

			if bm != nil {
				if _, inBefore := bm[k]; inBefore {
					continue // live-carried
				}
			}

			out[k] = av // chart-new
		}

		return out

	case []interface{}:
		aa, ok := after.([]interface{})
		if !ok {
			return after
		}

		ba, _ := before.([]interface{})

		out := make([]interface{}, 0, len(aa))
		for i, av := range aa {
			switch {
			case i < len(pv):
				var b interface{}
				if i < len(ba) {
					b = ba[i]
				}
				out = append(out, projectUpdate(av, b, pv[i]))
			case i < len(ba):
				// beyond prior, present live at this index: live-carried.
			default:
				out = append(out, av) // chart-new element
			}
		}

		return out

	default:
		return after
	}
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
