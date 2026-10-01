package provider

import (
	"context"
	"fmt"
	"maps"
	"strings"

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
// computed against a guessed value. release_history_limit, auto_rollback and
// adopt_existing are deliberately excluded: toReleaseSpec already treats an
// unknown/null history limit as "omit" (release_model.go), auto_rollback has
// no effect on Client.Plan's inputs at all (it is an Install-only option),
// and adopt_existing is read by Create alone (an unknown value only
// suppresses ModifyPlan's existing-release warning).
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
// status/metadata) — i.e. whether ANYTHING is actually changing: a create
// (isCreate, ModifyPlan's createPlan), out-of-band drift (the planned
// resources map differs from prior), or any chart-rendering config input
// differing from prior state.
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
func releaseWillReinstall(plan, priorState releaseModel, planned, prior map[string]string, isCreate bool) bool {
	return isCreate ||
		// A prior release that is not cleanly deployed (failed, pending-*) is
		// ALWAYS re-installed by nelm: IsReleaseUpToDate returns false purely
		// on status != deployed, so Install never takes its skip branch and
		// cuts a new revision. Without this term a failed release yields an
		// eternally-empty plan (never retried without -replace), and any
		// no-op-rendering config edit (e.g. only timeouts) aborts with
		// "inconsistent result after apply" when the retry bumps the revision.
		(!isCreate && priorState.Status.ValueString() != "deployed") ||
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
		// adopt_existing only matters to Create, but an edit to it still makes
		// Terraform call Update, and that Update's Install can bump the
		// revision just like a timeouts-only edit (below).
		!plan.AdoptExisting.Equal(priorState.AdoptExisting) ||
		// timeouts is the one remaining in-place-updatable attribute: its edit
		// makes Terraform call Update, and nelm's Install can bump the
		// revision even for an identical chart (e.g. a chart with an
		// upgrade-active hook makes the install plan non-useless, so the skip
		// branch is not taken). Compare the embedded Objects — timeouts.Value
		// itself is not a types.Object, so Value.Equal(Value) would be
		// unconditionally false and freeze every plan as a reinstall.
		!plan.Timeouts.Object.Equal(priorState.Timeouts.Object) ||
		// Out-of-band drift: the planned resources differ from prior state.
		!maps.Equal(planned, prior)
}

// releaseIdentityChanged reports whether plan addresses a different release
// than priorState: a change of name, namespace or release_storage_driver,
// the RequiresReplace attributes. The plan then installs a release that does
// not exist yet, like a create, while the prior one is destroyed.
func releaseIdentityChanged(plan, priorState releaseModel) bool {
	return !plan.Name.Equal(priorState.Name) ||
		!plan.Namespace.Equal(priorState.Namespace) ||
		!plan.ReleaseStorageDriver.Equal(priorState.ReleaseStorageDriver)
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
// (via Client.Plan) — or, on a create, from the chart's first-install render
// (step 6c) — and set UNCONDITIONALLY, even on an entirely unchanged
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

	// A null prior state means this plan CREATES the resource: a fresh
	// create, or the create half of a replacement (a tainted resource,
	// -replace, a name/namespace/release_storage_driver change).
	stateIsNull := req.State.Raw.IsNull()

	// State values are always fully known (Terraform state never stores
	// Unknown), so reflecting the whole prior state into releaseModel here is
	// safe.
	var priorState releaseModel

	if !stateIsNull {
		resp.Diagnostics.Append(req.State.Get(ctx, &priorState)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	// createPlan: this plan installs a release that does not exist yet. That
	// is every null-prior plan, and also a change of the release's identity
	// (releaseIdentityChanged): Terraform core plans such a replacement FIRST
	// with the real prior state, and re-plans it with a null prior only after
	// seeing the RequiresReplace. The first call is the one that can fail the
	// plan and whose warnings are shown (core keeps only the errors of the
	// re-plan), so it must already treat the change as the create it is.
	// ModifyPlan cannot see the RequiresReplace itself: the framework hands
	// it an empty resp.RequiresReplace.
	createPlan := stateIsNull || releaseIdentityChanged(plan, priorState)

	planRes, planErr := r.client.Plan(ctx, spec, readTimeout)
	if planErr != nil {
		// 5a. Cluster unreachable at plan time: degrade (helm-parity),
		// warn (not error), and let apply compute the real diff.
		if nelmclient.IsClusterUnreachable(planErr) {
			degradeDiffToUnknown(ctx, resp)
			resp.Diagnostics.AddWarning(
				"Cluster unreachable at plan time",
				"nelm_release could not reach the Kubernetes cluster while computing this plan; "+
					"the resource diff will be computed at apply instead.\n\n"+planErr.Error(),
			)

			return
		}

		// 5b. Anything else (bad chart, bad values, render error, ...)
		// is a hard error — except, on a create plan, a conflict with
		// objects that are live right now (nelmclient.IsLiveConflict). During
		// a replacement those objects belong to the release being replaced,
		// whose destroy removes them before the create runs (e.g. a namespace
		// move of a chart whose cluster-scoped objects have fixed names), and
		// Install re-runs both checks at apply; so that case only warns (step
		// 6c), once the render below has shown the chart and values
		// themselves are valid. Scrub set_sensitive values first: nelm echoes
		// the raw --set-json/--set argument in parse errors.
		if !createPlan || !nelmclient.IsLiveConflict(planErr) {
			resp.Diagnostics.AddError("nelm_release plan failed", plan.scrubSensitive(planErr.Error()))

			return
		}
	}

	// 6. prior is the resources map already in state — empty (not
	// missing) on create, since nelm's Changes only ever describes
	// *changed* resources; BuildPlannedResources needs a base map to
	// merge them into either way.
	prior := map[string]string{}

	if !stateIsNull && !priorState.Resources.IsNull() && !priorState.Resources.IsUnknown() {
		resp.Diagnostics.Append(priorState.Resources.ElementsAs(ctx, &prior, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	// The lock warning is about the release this plan updates; a replacement
	// destroys the prior release instead (and destroy takes no lock).
	if !createPlan {
		resp.Diagnostics.Append(pendingReleaseWarning(ctx, plan, priorState)...)
	}

	// 6b. The chart's client render is the authoritative desired shape for
	// update changes (an update's After is the server dry-run merge, which no
	// heuristic can reliably un-blend from live state) and for creates (6c).
	// A render failure here is a HARD error, deliberately: it is how a bad
	// chart or bad values fail a create plan whose live-cluster Plan was
	// downgraded to a warning (5b); otherwise Plan just succeeded with
	// identical inputs, so a failing render is exceptional — and silently
	// degrading to the heuristic at ONE of the two ModifyPlan phases while the
	// other used the render would itself manufacture an inconsistent-final-plan
	// abort.
	//
	// A create plan renders as a FIRST install (no release history),
	// which is exactly what Install renders whenever the create actually
	// runs: on a fresh create, and after a replacement's destroy has
	// uninstalled the old release. Rendering against the old release's
	// history instead would feed revision- or upgrade-dependent templates
	// (.Release.Revision, .Release.IsUpgrade) different inputs at plan time
	// (old release still live) and at the apply-time re-plan (already gone).
	renderSpec := spec
	renderSpec.RenderAsFirstInstall = createPlan

	renderObjs, err := r.client.Render(ctx, renderSpec, readTimeout)
	if err != nil {
		resp.Diagnostics.AddError("nelm_release: chart render for the plan diff failed", plan.scrubSensitive(err.Error()))
		return
	}

	rendered, err := planconv.BuildRenderedResources(renderObjs, ns, r.client)
	if err != nil {
		resp.Diagnostics.AddError("nelm_release: failed to build the rendered resources map", err.Error())
		return
	}

	// 6c. The planned "resources" value. On a create plan it is the
	// first-install render itself. Terraform plans a replacement's create
	// TWICE — at plan time while the old release's objects are still live,
	// and again at apply after its destroy removed them — and nelm's
	// live-relative Changes differ between the two (unchanged objects are
	// omitted, then every object is a create that also carries
	// release-ownership metadata). Building the map from them aborted every
	// replacement with "Provider produced inconsistent final plan" after the
	// uninstall had already run; the render is identical in both phases. On
	// an update, prior plus nelm's Changes stays authoritative. nelm's Changes
	// still drive the blind-apply warnings either way.
	planned := rendered

	if planErr != nil {
		resp.Diagnostics.AddWarning(
			"nelm_release: the live-cluster plan for this create failed (it is re-checked at apply)",
			"nelm could not plan this create against the objects currently live in the cluster. When this "+
				"plan replaces the resource, those objects belong to the release being replaced, and its destroy "+
				"removes them before the create runs. The apply re-runs the same checks and fails if the "+
				"conflict is still there.\n\n"+plan.scrubSensitive(planErr.Error()),
		)
	} else {
		built, warns, err := planconv.BuildPlannedResources(prior, planRes.Changes, ns, r.client, rendered)
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

		if !createPlan {
			planned = built
		}

		if createPlan && planRes.DeployType == nelmclient.DeployTypeUpgrade && !plan.AdoptExisting.ValueBool() {
			resp.Diagnostics.Append(existingReleaseWarning(ns, name))
		}
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
	// hasChanges/createPlan below.
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
	if releaseWillReinstall(plan, priorState, planned, prior, createPlan) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("status"), types.StringUnknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("revision"), types.Int64Unknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("metadata"), types.ObjectUnknown(metadataAttrTypes))...)
	}
}

// existingReleaseWarning is ModifyPlan's plan-time heads-up for a create that
// targets a release nelm would UPGRADE (it already has a deployed revision):
// Create refuses that at apply unless adopt_existing is set
// (installGuardDiags). It is only a warning, never an error, on purpose:
// ModifyPlan cannot tell a destroy-first replacement (where the old release is
// legitimately still there at plan time) from the cases Create refuses, and
// an error from the apply-time re-plan of a create_before_destroy replacement
// would leave the old object deposed instead of letting Terraform restore it
// after Create's refusal.
func existingReleaseWarning(namespace, name string) diag.Diagnostic {
	return diag.NewWarningDiagnostic(
		"nelm_release: a release with this name already exists",
		fmt.Sprintf("Release %q already exists in namespace %q and this plan creates nelm_release for it. "+
			"That is expected when the plan replaces the resource destroy-first (\"-/+\", e.g. a tainted resource "+
			"or -replace): the destroy uninstalls the old release before the create runs.\n\n"+
			"In every other case the apply refuses to take the existing release over: a new nelm_release for a "+
			"release installed elsewhere (e.g. migrating from helm_release without an import), a duplicate "+
			"resource, or a create_before_destroy replacement (\"+/-\", also when create_before_destroy is "+
			"inherited from a dependent resource). Import the release instead (terraform import <address> %s/%s, "+
			"or an import block), or set adopt_existing = true.",
			name, namespace, namespace, name),
	)
}

// pendingReleaseWarning warns when the refreshed release is pending-*: an
// install, upgrade or rollback holds it (Helm's lock), or one was
// interrupted. The apply refuses to install over a pending revision younger
// than pendingTakeoverAge and takes over an older one (installGuardDiags).
func pendingReleaseWarning(ctx context.Context, plan, priorState releaseModel) diag.Diagnostics {
	var diags diag.Diagnostics

	status := priorState.Status.ValueString()
	if !strings.HasPrefix(status, "pending-") {
		return diags
	}

	updateTimeout, tdiags := plan.Timeouts.Update(ctx, defaultUpdateTimeout)
	diags.Append(tdiags...)

	diags.AddWarning(
		"nelm_release: the release is locked by a pending operation",
		fmt.Sprintf("Release %s is %q (revision %d): an install, upgrade or rollback (helm, nelm, or another "+
			"Terraform run) is in progress, or one was interrupted. Like Helm, the apply refuses to run while "+
			"that revision is younger than %s, and takes it over once it is older.",
			priorState.ID.ValueString(), status, priorState.Revision.ValueInt64(), pendingTakeoverAge(updateTimeout)),
	)

	return diags
}
