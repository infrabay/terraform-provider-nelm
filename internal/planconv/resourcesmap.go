package planconv

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/werf/nelm/pkg/plan"
)

// Warning is a non-fatal condition surfaced while building the "resources"
// map that the caller (release_plan.go's ModifyPlan) should attach to the
// Terraform plan response as a warning diagnostic, e.g. via
// resp.Diagnostics.AddWarning(w.Resource, w.Reason).
type Warning struct {
	// Resource is the resources-map key (see Key) the warning is about.
	Resource string
	// Reason is a human-readable explanation, e.g. nelm's ResourceChange.Reason
	// for a "blind apply" change.
	Reason string
}

// BuildPlannedResources builds the planned side of the "resources" map
// (design §2.1) from nelm's own plan.ResourceChange classification:
//
//   - starts from a copy of prior (the previous/live resources map — nelm's
//     Changes contains only changed resources, so unchanged ones must retain
//     their prior normalized value; this is correct even if Changes ever
//     included unchanged entries, since normalize(After) for an unchanged
//     resource is idempotent with what's already there)
//   - Type == "delete" deletes the key
//   - Type == "create" / "recreate" / "update" / "blind apply" sets the key
//     to the rendered value (see below)
//   - "blind apply" (Before nil, possibly carrying a DryApplyErr in Reason)
//     is treated as a create for the map, plus it emits a Warning naming the
//     resource and the Reason so the caller can surface it as a plan warning.
//
// rendered is the chart-desired shape from BuildRenderedResources (nil when
// the render was unavailable), and it is authoritative for every change
// type: nelm's update After is the server dry-run merge, and only the actual
// chart render can say which fields the chart manages; a create/recreate/
// blind-apply After is nelm's own render of the same object, so taking the
// rendered value there too keeps the planned value independent of how nelm
// classified the change (a webhook timing out between the plan and apply
// phases turns an update into a blind apply). Without a rendered entry an
// update falls back to the three-way NormalizeUpdateAfter heuristic and the
// other types to NormalizeUnstructured(After).
//
// changes == nil (a no-change plan; nelm's plan artifact stores this as a
// JSON null "changes" field) is a no-op: ranging over a nil slice yields
// zero iterations, so the result is exactly a copy of prior.
func BuildPlannedResources(prior map[string]string, changes []*plan.ResourceChange, releaseNS string, scoper KeyScoper, rendered map[string]string) (map[string]string, []Warning, error) {
	out := make(map[string]string, len(prior))
	for k, v := range prior {
		out[k] = v
	}

	var warnings []Warning

	for _, change := range changes {
		if change == nil || change.ResourceMeta == nil {
			continue
		}

		// Skip helm/nelm hooks. nelm stores hooks separately from regular
		// resources and Client.Get (Read's live side) only ever returns the
		// regular Resources, never Hooks — so if a hook change were kept here
		// the planned map would carry a key that Read can never reproduce,
		// yielding a perpetual resources-map diff/update cycle for any chart
		// with a hook (a pre-install Job, a migration hook, ...).
		if isHookChange(change) {
			continue
		}

		key, err := keyWithScopeFallback(plannedRef(change), releaseNS, scoper)
		if err != nil {
			return nil, nil, fmt.Errorf("planconv: BuildPlannedResources: key for %s %q: %w",
				change.ResourceMeta.GroupVersionKind, change.ResourceMeta.Name, err)
		}

		switch change.Type {
		case "delete":
			delete(out, key)

		case "update":
			if change.After == nil {
				return nil, nil, fmt.Errorf("planconv: BuildPlannedResources: change type %q for %s has nil After", change.Type, key)
			}

			// An update's After is the server-side dry-run merge, which
			// carries live fields the chart never set. The RENDERED manifest
			// is the authoritative chart-desired value: a function of
			// configuration, and the only source that can distinguish "chart
			// manages this field" from "this field lives on the cluster"
			// (heuristics over After/Before/prior provably cannot — see
			// NormalizeUpdateAfter's doc for the counterexamples). It is NOT
			// necessarily the same at the plan and the apply phase: a
			// template using randAlphaNum, genCA, now, ... renders a
			// different value every time, which the caller detects with
			// CompareRenders. Fall back to the three-way heuristic only when
			// no rendered entry exists for this key.
			if r, ok := rendered[key]; ok {
				out[key] = r

				break
			}

			normalized, err := NormalizeUpdateAfter(change.After, change.Before, out[key])
			if err != nil {
				return nil, nil, fmt.Errorf("planconv: BuildPlannedResources: normalize update %s: %w", key, err)
			}

			out[key] = normalized

		case "create", "recreate", "blind apply":
			if change.After == nil {
				return nil, nil, fmt.Errorf("planconv: BuildPlannedResources: change type %q for %s has nil After", change.Type, key)
			}

			if r, ok := rendered[key]; ok {
				out[key] = r
			} else {
				normalized, err := NormalizeUnstructured(change.After)
				if err != nil {
					return nil, nil, fmt.Errorf("planconv: BuildPlannedResources: normalize %s: %w", key, err)
				}

				out[key] = normalized
			}

			if change.Type == "blind apply" {
				warnings = append(warnings, Warning{
					Resource: key,
					Reason:   change.Reason,
				})
			}

		default:
			// Defensive: nelm's own literal set is {create, recreate, update,
			// "blind apply", delete} (planned_changes.go). Surface anything
			// else as a warning rather than silently misclassifying it.
			warnings = append(warnings, Warning{
				Resource: key,
				Reason:   fmt.Sprintf("unrecognized nelm change type %q", change.Type),
			})
		}
	}

	return out, warnings, nil
}

// isHookChange reports whether a plan change concerns a helm/nelm hook,
// identified (as nelm itself does, via spec.IsHook) by the "helm.sh/hook"
// annotation on the changed object.
func isHookChange(change *plan.ResourceChange) bool {
	for _, obj := range []*unstructured.Unstructured{change.After, change.Before} {
		if obj == nil {
			continue
		}
		if _, ok := obj.GetAnnotations()["helm.sh/hook"]; ok {
			return true
		}
	}

	return false
}

// plannedRef builds the Ref to key a plan.ResourceChange by, preferring the
// change's own object namespace (design §2.1: obj.GetNamespace() first) over
// nelm's ResourceMeta.Namespace (which NewResourceMeta zeroes out whenever it
// equals the release namespace — see nelm's pkg/resource/spec/resource_meta.go),
// falling back to Key's own releaseNS/scoper resolution when both are empty.
func plannedRef(change *plan.ResourceChange) Ref {
	ns := ""

	switch {
	case change.After != nil && change.After.GetNamespace() != "":
		ns = change.After.GetNamespace()
	case change.Before != nil && change.Before.GetNamespace() != "":
		ns = change.Before.GetNamespace()
	default:
		ns = change.ResourceMeta.Namespace
	}

	return Ref{
		GroupVersionKind: change.ResourceMeta.GroupVersionKind,
		Namespace:        ns,
		Name:             change.ResourceMeta.Name,
	}
}

// BuildRenderedResources normalizes the chart's client-rendered manifests
// (nelmclient.Render) into the same key->canonical-JSON shape as the other
// map builders. It is the authoritative chart-desired side consumed by
// BuildPlannedResources' update arm. Hook manifests (helm.sh/hook) are
// skipped, mirroring both the planned side's hook exclusion and Read's live
// side (which never sees hooks). Keys use the same scope-fallback as the
// planned side so a CR whose CRD ships in this very release still keys.
func BuildRenderedResources(objs []*unstructured.Unstructured, releaseNS string, scoper KeyScoper) (map[string]string, error) {
	out := make(map[string]string, len(objs))

	for _, obj := range objs {
		if obj == nil {
			continue
		}

		if _, hook := obj.GetAnnotations()["helm.sh/hook"]; hook {
			continue
		}

		ref := Ref{
			GroupVersionKind: obj.GroupVersionKind(),
			Namespace:        obj.GetNamespace(),
			Name:             obj.GetName(),
		}

		key, err := keyWithScopeFallback(ref, releaseNS, scoper)
		if err != nil {
			return nil, fmt.Errorf("planconv: BuildRenderedResources: key for %s %q: %w", ref.GroupVersionKind, ref.Name, err)
		}

		normalized, err := NormalizeUnstructured(obj)
		if err != nil {
			return nil, fmt.Errorf("planconv: BuildRenderedResources: normalize %s: %w", key, err)
		}

		out[key] = normalized
	}

	return out, nil
}

// BuildLiveResources builds the live side of the "resources" map (design
// §2.1) from a set of live cluster GETs: it is the Read-path counterpart to
// BuildPlannedResources and MUST key through the same Key + KeyScoper
// (CONTRACTS.md seam 2's bold invariant), or phantom diffs result.
//
// desired is the previously stored resources map (the prior state's, or the
// KNOWN plan's) keyed identically. Each live object is normalized and then
// projected onto its desired[key] counterpart (NormalizeLiveAgainst), which
// strips Kubernetes' server-side defaulting generically so the live side keys
// AND normalizes byte-identically to the planned side when nothing actually
// drifted. A live key with no desired counterpart (desired == nil, or a newly
// appeared resource) is normalized in full and converges on the next plan.
func BuildLiveResources(objs []*unstructured.Unstructured, releaseNS string, scoper KeyScoper, desired map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(objs))

	for _, obj := range objs {
		if obj == nil {
			continue
		}

		ref := Ref{
			GroupVersionKind: obj.GroupVersionKind(),
			Namespace:        obj.GetNamespace(),
			Name:             obj.GetName(),
		}

		key, err := Key(ref, releaseNS, scoper)
		if err != nil {
			return nil, fmt.Errorf("planconv: BuildLiveResources: key for %s %q: %w", ref.GroupVersionKind, ref.Name, err)
		}

		normalized, err := NormalizeLiveAgainst(obj, desired[key])
		if err != nil {
			return nil, fmt.Errorf("planconv: BuildLiveResources: normalize %s: %w", key, err)
		}

		out[key] = normalized
	}

	return out, nil
}
