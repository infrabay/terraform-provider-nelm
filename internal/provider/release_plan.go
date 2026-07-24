package provider

import (
	"context"
	"fmt"
	"maps"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
	"github.com/infrabay/terraform-provider-nelm/internal/planconv"
)

// metadataAttrTypes mirrors the "metadata" schema attribute's nested attribute
// types (release_schema.go). It is the single source of truth for the metadata
// object's shape. It is used to build
// types.ObjectUnknown(...) values when degrading the diff surface (design
// §2.2 steps 2 and 5a).
var metadataAttrTypes = map[string]attr.Type{
	"app_version":   types.StringType,
	"chart_name":    types.StringType,
	"chart_version": types.StringType,
	"values_json":   types.StringType,
}

// unknownCheckStringPaths/unknownCheckBoolPaths are the top-level scalar
// config attributes design §2.2 step 2 says must never be guessed at: if any
// is Unknown, the whole diff surface degrades to Unknown rather than being
// computed against a guessed value. release_history_limit and auto_rollback
// are deliberately excluded: toReleaseSpec already treats an unknown/null
// history limit as "omit" (release_model.go), and auto_rollback has no
// effect on Client.Plan's inputs at all (it is an Install-only option).
var (
	unknownCheckStringPaths = []string{"chart", "repository", "version", "name", "namespace", "release_storage_driver"}
	unknownCheckBoolPaths   = []string{"force_adoption", "no_remove_manual_changes", "no_install_crds"}
)

// containsUnknown reports whether v, or (recursively) any element/attribute
// nested inside it, is Unknown. It is generic over the attr.Value shapes this
// provider's schema actually uses for "values"/"set"/"set_sensitive" (List of
// scalars, List of Object of scalars); a bare scalar has no children and is
// handled by the direct IsUnknown() check alone.
func containsUnknown(v attr.Value) bool {
	if v.IsUnknown() {
		return true
	}

	switch val := v.(type) {
	case types.List:
		for _, e := range val.Elements() {
			if containsUnknown(e) {
				return true
			}
		}
	case types.Object:
		for _, a := range val.Attributes() {
			if containsUnknown(a) {
				return true
			}
		}
	}

	return false
}

// planHasUnknownInputs implements design §2.2 step 2: it reports whether any
// input ModifyPlan would need to feed to Client.Plan is Unknown, WITHOUT ever
// calling plan.Get(ctx, &releaseModel{}) first. That distinction matters:
// releaseModel.Set/SetSensitive are native Go slices ([]setModel), and
// reflecting a wholly-Unknown "set"/"set_sensitive" list into a native slice
// is a hard reflection error (the framework has no way to tell a native slice
// how many elements an Unknown list "has"), not a value ModifyPlan could
// branch on. Fetching each attribute individually via GetAttribute into a
// `types` package value (String/Bool/List) sidesteps that entirely: those
// types represent Unknown directly, whole-collection or per-element, with no
// error — see release_plan_test.go's reflection-error regression test.
func planHasUnknownInputs(ctx context.Context, plan tfsdk.Plan) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics

	for _, p := range unknownCheckStringPaths {
		var v types.String

		diags.Append(plan.GetAttribute(ctx, path.Root(p), &v)...)
		if diags.HasError() {
			return false, diags
		}

		if v.IsUnknown() {
			return true, diags
		}
	}

	for _, p := range unknownCheckBoolPaths {
		var v types.Bool

		diags.Append(plan.GetAttribute(ctx, path.Root(p), &v)...)
		if diags.HasError() {
			return false, diags
		}

		if v.IsUnknown() {
			return true, diags
		}
	}

	var values types.List

	diags.Append(plan.GetAttribute(ctx, path.Root("values"), &values)...)
	if diags.HasError() {
		return false, diags
	}

	if containsUnknown(values) {
		return true, diags
	}

	for _, p := range []string{"set", "set_sensitive"} {
		var lst types.List

		diags.Append(plan.GetAttribute(ctx, path.Root(p), &lst)...)
		if diags.HasError() {
			return false, diags
		}

		if containsUnknown(lst) {
			return true, diags
		}
	}

	return false, diags
}

// setModelsEqual reports whether a and b are the same ordered sequence of
// set/set_sensitive entries. Used by ModifyPlan step 8's metadata-unknown
// condition, which must compare "set"/"set_sensitive" against prior state
// alongside chart/repository/version/values.
func setModelsEqual(a, b []setModel) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i].Name.ValueString() != b[i].Name.ValueString() {
			return false
		}

		if a[i].Value.ValueString() != b[i].Value.ValueString() {
			return false
		}

		if a[i].Type.ValueString() != b[i].Type.ValueString() {
			return false
		}
	}

	return true
}

// releaseWillReinstall reports whether the planned apply will re-run nelm's
// Install in a way that can bump the release revision (and change its
// status/metadata) — i.e. whether ANYTHING is actually changing: a fresh
// create, out-of-band drift (the planned resources map differs from prior), or
// any chart-rendering config input differing from prior state.
//
// It is deliberately BROADER than "nelm reported a resource-level change". A
// values/set/set_sensitive/version edit that changes the coalesced release
// config but renders no manifest change (e.g. a value used only by NOTES.txt
// or a disabled subchart) still makes nelm create a new revision: its
// IsReleaseUpToDate compares Config/Notes, not just resources, so Install is
// NOT skipped. If ModifyPlan left status/revision KNOWN at their prior values
// in that case, Terraform's "Provider produced inconsistent result after
// apply" check would trip on the bumped revision. Conversely, when this
// returns false the config and rendered resources both match prior state, so
// nelm skips the install, the revision does not move, and leaving
// status/revision/metadata untouched yields the clean, empty no-change plan.
func releaseWillReinstall(plan, priorState releaseModel, planned, prior map[string]string, stateIsNull bool) bool {
	return stateIsNull ||
		// A prior release that is not cleanly deployed (failed, pending-*) is
		// ALWAYS re-installed by nelm: IsReleaseUpToDate returns false purely
		// on status != deployed, so Install never takes its skip branch and
		// cuts a new revision. Without this term a failed release yields an
		// eternally-empty plan (never retried without -replace), and any
		// no-op-rendering config edit (e.g. only timeouts) aborts with
		// "inconsistent result after apply" when the retry bumps the revision.
		(!stateIsNull && priorState.Status.ValueString() != "deployed") ||
		// Chart-rendering / values inputs (change the coalesced config).
		plan.Chart.ValueString() != priorState.Chart.ValueString() ||
		plan.Repository.ValueString() != priorState.Repository.ValueString() ||
		plan.Version.ValueString() != priorState.Version.ValueString() ||
		!plan.Values.Equal(priorState.Values) ||
		!setModelsEqual(plan.Set, priorState.Set) ||
		!setModelsEqual(plan.SetSensitive, priorState.SetSensitive) ||
		// Other install-affecting options: a change to any of these triggers
		// an Update whose Install may bump the revision for a reason the
		// rendered-resources map does not capture (a different storage driver,
		// adoption/CRD/manual-change handling, rollback behavior, history
		// pruning). Including them keeps this a strict superset of "Install
		// bumps the revision", so status/revision are never left stale KNOWN.
		!plan.ReleaseStorageDriver.Equal(priorState.ReleaseStorageDriver) ||
		!plan.ForceAdoption.Equal(priorState.ForceAdoption) ||
		!plan.NoRemoveManualChanges.Equal(priorState.NoRemoveManualChanges) ||
		!plan.NoInstallCRDs.Equal(priorState.NoInstallCRDs) ||
		!plan.AutoRollback.Equal(priorState.AutoRollback) ||
		!plan.ReleaseHistoryLimit.Equal(priorState.ReleaseHistoryLimit) ||
		// Out-of-band drift: the planned resources differ from prior state.
		!maps.Equal(planned, prior)
}

// releaseID computes the "id" attribute value: a pure, deterministic
// function of namespace and name, so it is always safe to set KNOWN (design
// §2.2 step 3's consistency requirement — it must be byte-identical whether
// this is the plan-phase or the apply-phase ModifyPlan invocation).
func releaseID(namespace, name string) string {
	return namespace + "/" + name
}

// degradeDiffToUnknown sets every volatile computed attribute (resources,
// status, revision, metadata) to Unknown on resp.Plan. It is the shared body
// of design §2.2 steps 2 (unknown inputs) and 5a (cluster unreachable at plan
// time): in both cases ModifyPlan has no safe value to compute the diff
// surface from and must never guess. "id" is deliberately NOT touched here:
// callers of this helper return before step 3 ever runs, so id stays exactly
// whatever the framework's own MarkComputedNilsAsUnknown left it as.
func degradeDiffToUnknown(ctx context.Context, resp *resource.ModifyPlanResponse) {
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("resources"), types.MapUnknown(types.StringType))...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("status"), types.StringUnknown())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("revision"), types.Int64Unknown())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("metadata"), types.ObjectUnknown(metadataAttrTypes))...)
}

// ModifyPlan implements the diff architecture in design §2.2 — the heart of
// this provider: destroy plans stay null, unknown inputs (and an unreachable
// cluster) degrade the diff surface to Unknown rather than guess, and
// otherwise "resources" is rebuilt from a fresh action.ReleasePlanInstall
// (via Client.Plan) and set UNCONDITIONALLY, even on an entirely unchanged
// plan, so out-of-band cluster drift is always visible. ModifyPlan runs at
// both the plan and the apply phase; the ONLY values it ever sets KNOWN are
// "id" (a pure function of known inputs) and "resources" (a deterministic
// function of nelm's plan output, prior state, and canonical JSON) — every
// other computed attribute either stays Unknown or is left untouched at
// whatever prior state already holds (CONTRACTS.md/design §2.2 consistency
// policy).
func (r *releaseResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// 1. DESTROY: req.Plan.Raw is null on a destroy plan. The response
	// plan MUST stay entirely null. resp.Plan already equals req.Plan
	// going into this method (the framework's own contract — it seeds
	// ModifyPlanResponse.Plan from the request before calling us), so
	// simply not touching resp.Plan at all satisfies this.
	if req.Plan.Raw.IsNull() {
		return
	}

	// 2. UNKNOWN INPUTS: never guess. This MUST run before any
	// req.Plan.Get(ctx, &releaseModel{}) — see planHasUnknownInputs' doc.
	unknown, diags := planHasUnknownInputs(ctx, req.Plan)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	if unknown {
		degradeDiffToUnknown(ctx, resp)
		return
	}

	// Only safe to reflect the full plan into releaseModel once we know
	// none of its Set/SetSensitive entries are Unknown (see above).
	var plan releaseModel

	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ns := plan.Namespace.ValueString()
	name := plan.Name.ValueString()

	// 3. id is a pure function of the now-known ns/name: deterministic
	// and byte-identical across the plan-phase and apply-phase
	// ModifyPlan calls (the consistency rule), so it is always safe to
	// set KNOWN here.
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), releaseID(ns, name))...)
	if resp.Diagnostics.HasError() {
		return
	}

	// 4. Run nelm's plan machinery (action.ReleasePlanInstall under the
	// hood) against the plan-phase spec.
	spec, sdiags := plan.toReleaseSpec(ctx)
	resp.Diagnostics.Append(sdiags...)

	if resp.Diagnostics.HasError() {
		return
	}

	readTimeout, tdiags := plan.Timeouts.Read(ctx, defaultReadTimeout)
	resp.Diagnostics.Append(tdiags...)

	if resp.Diagnostics.HasError() {
		return
	}

	planRes, err := r.client.Plan(ctx, spec, readTimeout)
	if err != nil {
		// 5a. Cluster unreachable at plan time: degrade (helm-parity),
		// warn (not error), and let apply compute the real diff.
		if nelmclient.IsClusterUnreachable(err) {
			degradeDiffToUnknown(ctx, resp)
			resp.Diagnostics.AddWarning(
				"Cluster unreachable at plan time",
				"nelm_release could not reach the Kubernetes cluster while computing this plan; "+
					"the resource diff will be computed at apply instead.\n\n"+err.Error(),
			)

			return
		}

		// 5b. Anything else (bad chart, bad values, render error, ...)
		// is a hard error. Scrub set_sensitive values first: nelm echoes the
		// raw --set-json/--set argument in parse errors.
		resp.Diagnostics.AddError("nelm_release plan failed", plan.scrubSensitive(err.Error()))

		return
	}

	// 6. prior is the resources map already in state — empty (not
	// missing) on create, since nelm's Changes only ever describes
	// *changed* resources; BuildPlannedResources needs a base map to
	// merge them into either way. State values are always fully known
	// (Terraform state never stores Unknown), so reflecting the whole
	// prior state into releaseModel here is safe.
	prior := map[string]string{}
	stateIsNull := req.State.Raw.IsNull()

	var priorState releaseModel

	if !stateIsNull {
		resp.Diagnostics.Append(req.State.Get(ctx, &priorState)...)
		if resp.Diagnostics.HasError() {
			return
		}

		if !priorState.Resources.IsNull() && !priorState.Resources.IsUnknown() {
			resp.Diagnostics.Append(priorState.Resources.ElementsAs(ctx, &prior, false)...)
			if resp.Diagnostics.HasError() {
				return
			}
		}
	}

	planned, warns, err := planconv.BuildPlannedResources(prior, planRes.Changes, ns, r.client)
	if err != nil {
		resp.Diagnostics.AddError("nelm_release: failed to build the planned resources map", err.Error())
		return
	}

	for _, w := range warns {
		// w.Reason can embed a raw Kubernetes dry-run apply error, which may
		// quote rendered manifest content back — including set_sensitive
		// values. Scrub before it reaches plan output.
		resp.Diagnostics.AddWarning(fmt.Sprintf("nelm_release: blind apply for %s", w.Resource), plan.scrubSensitive(w.Reason))
	}

	plannedMap, mdiags := types.MapValueFrom(ctx, types.StringType, planned)
	resp.Diagnostics.Append(mdiags...)

	if resp.Diagnostics.HasError() {
		return
	}

	// 7. ALWAYS set resources explicitly — including on a completely
	// unchanged plan. Terraform core skips MarkComputedNilsAsUnknown
	// whenever the proposed new state equals the prior state, so an
	// unconditional overwrite here is the ONLY thing that makes
	// out-of-band cluster drift visible on an otherwise no-change plan
	// (design §2.2 step 7). This call must NOT be made conditional on
	// hasChanges/stateIsNull below.
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("resources"), plannedMap)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// 8. status, revision, AND metadata are all Unknown whenever anything is
	// actually changing (see releaseWillReinstall): a fresh create, drift, or
	// any config input that feeds the release spec differing from prior state.
	// They MUST move together — status/revision were previously gated on
	// nelm's resource-level Changes alone, which misses a values/version edit
	// that renders no manifest change yet still bumps the revision on apply
	// (the "inconsistent result after apply" bug). On a true no-op this is
	// false and prior status/revision/metadata are left entirely untouched —
	// the clean no-change plan.
	if releaseWillReinstall(plan, priorState, planned, prior, stateIsNull) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("status"), types.StringUnknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("revision"), types.Int64Unknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("metadata"), types.ObjectUnknown(metadataAttrTypes))...)
	}
}
