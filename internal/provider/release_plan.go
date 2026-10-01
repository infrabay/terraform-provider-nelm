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
	"k8s.io/apimachinery/pkg/runtime/schema"

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
// computed against a guessed value. release_history_limit, auto_rollback,
// wait and adopt_existing are deliberately excluded: toReleaseSpec already
// treats an unknown/null history limit as "omit" (release_model.go),
// auto_rollback and wait have no effect on Client.Plan's inputs at all (they
// are Install-only options), and adopt_existing is read by Create alone (an
// unknown value only suppresses ModifyPlan's existing-release warning).
var (
	unknownCheckStringPaths = []string{"chart", "repository", "version", "name", "namespace", "release_storage_driver", "diff_mode"}
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
		priorState.Status.ValueString() != "deployed" ||
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
		// adoption/CRD/manual-change handling, rollback and readiness-wait
		// behavior, history pruning). Including them keeps this a strict
		// superset of "Install bumps the revision", so status/revision are
		// never left stale KNOWN.
		!plan.ReleaseStorageDriver.Equal(priorState.ReleaseStorageDriver) ||
		!plan.ForceAdoption.Equal(priorState.ForceAdoption) ||
		!plan.NoRemoveManualChanges.Equal(priorState.NoRemoveManualChanges) ||
		!plan.NoInstallCRDs.Equal(priorState.NoInstallCRDs) ||
		!plan.AutoRollback.Equal(priorState.AutoRollback) ||
		!plan.Wait.Equal(priorState.Wait) ||
		!plan.ReleaseHistoryLimit.Equal(priorState.ReleaseHistoryLimit) ||
		// adopt_existing only matters to Create, and diff_mode only to
		// ModifyPlan, but an edit to either still makes Terraform call Update,
		// and that Update's Install can bump the revision just like a
		// timeouts-only edit (below).
		!plan.AdoptExisting.Equal(priorState.AdoptExisting) ||
		!plan.DiffMode.Equal(priorState.DiffMode) ||
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
// both the plan and the apply phase, and Terraform aborts the apply ("Provider
// produced inconsistent final plan") unless every value the plan phase set
// KNOWN comes out identical at the apply phase. The ONLY values it ever sets
// KNOWN are "id" (a pure function of known inputs) and the elements of
// "resources" that two independent renders agree on (step 6e: an object that
// renders differently every time is Unknown, or keeps its prior value) —
// every other computed attribute either stays Unknown or is left untouched
// at whatever prior state already holds (CONTRACTS.md/design §2.2
// consistency policy). diff_mode = "none" skips all of this (planWithoutDiff).
func (r *releaseResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// 1. DESTROY: req.Plan.Raw is null on a destroy plan. The response
	// plan MUST stay entirely null. resp.Plan already equals req.Plan
	// going into this method (the framework's own contract — it seeds
	// ModifyPlanResponse.Plan from the request before calling us), so
	// simply not touching resp.Plan at all satisfies this.
	if req.Plan.Raw.IsNull() {
		return
	}

	// 1b. PROVIDER CONFIGURATION UNKNOWN (e.g. the cluster itself is created
	// in this run; see nelmProvider.Configure): there is no cluster to plan a
	// NEW release against yet. Degrade like step 5a; apply configures the
	// provider with the real values and computes the actual diff. A release
	// already in state falls through and fails on Client.Plan's
	// nelmclient.ErrConfigUnknown. A nil client (resource not yet
	// configured) is not the placeholder.
	if r.client != nil && r.client.ConfigUnknown() && req.State.Raw.IsNull() {
		degradeDiffToUnknown(ctx, resp)
		resp.Diagnostics.AddWarning(
			"Provider configuration not known at plan time",
			"The nelm provider configuration depends on values that are only known after apply (for "+
				"example a cluster created in this run), so this new nelm_release's diff will be computed "+
				"at apply instead.",
		)

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

	// The lock warning is about the release this plan updates; a replacement
	// destroys the prior release instead (and destroy takes no lock).
	if !createPlan {
		resp.Diagnostics.Append(pendingReleaseWarning(ctx, plan, priorState)...)
	}

	if plan.DiffMode.ValueString() == diffModeNone {
		planWithoutDiff(ctx, plan, priorState, createPlan, resp)
		return
	}

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

	// 6b. The chart's client render is the authoritative desired shape for
	// every changed object (an update's After is the server dry-run merge,
	// which no heuristic can reliably un-blend from live state) and for
	// creates (6c).
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

	// The chart is rendered twice, independently: what the two renders
	// disagree on is volatile (step 6e). Comparing two renders, rather than
	// a render with nelm's plan, works the same for a create (whose
	// live-cluster plan may have failed, 5b) and an update.
	renderObjs, err := r.client.Render(ctx, renderSpec, readTimeout)
	if err != nil {
		resp.Diagnostics.AddError("nelm_release: chart render for the plan diff failed", plan.scrubSensitive(err.Error()))
		return
	}

	probeObjs, err := r.client.Render(ctx, renderSpec, readTimeout)
	if err != nil {
		resp.Diagnostics.AddError("nelm_release: chart render for the plan diff failed", plan.scrubSensitive(err.Error()))
		return
	}

	// Kinds the cluster does not serve yet are scoped from the CRDs the chart
	// itself renders; a kind that still has to be guessed is handled in 6d.
	scoper := planconv.NewRenderScoper(renderObjs, r.client)

	// set_sensitive values the chart renders into a non-Secret object are
	// scrubbed from the planned values, exactly as Read scrubs the live ones
	// (with the state's set_sensitive, which an apply makes equal to these).
	secrets := plan.sensitiveValues()

	rendered, err := planconv.BuildRenderedResources(renderObjs, ns, scoper, secrets)
	if err != nil {
		resp.Diagnostics.AddError("nelm_release: failed to build the rendered resources map", err.Error())
		return
	}

	probe, err := planconv.BuildRenderedResources(probeObjs, ns, scoper, secrets)
	if err != nil {
		resp.Diagnostics.AddError("nelm_release: failed to build the rendered resources map", err.Error())
		return
	}

	volatility, err := planconv.CompareRenders(rendered, probe)
	if err != nil {
		resp.Diagnostics.AddError("nelm_release: failed to compare the chart renders", err.Error())
		return
	}

	// 6c. The planned "resources" value. On a create plan it is the
	// first-install render itself, never nelm's Changes. Terraform plans a
	// replacement's create TWICE — at plan time while the old release's
	// objects are still live, and again at apply after its destroy removed
	// them — and nelm's live-relative Changes differ between the two
	// (unchanged objects are omitted, then every object is a create that also
	// carries release-ownership metadata). Building the map from them aborted
	// every replacement with "Provider produced inconsistent final plan" after
	// the uninstall had already run. A render that reads the live cluster can
	// differ between the two phases as well, which is why a create whose
	// release is live plans no known map at all (6c'). On an update, prior
	// plus nelm's Changes stays authoritative. nelm's Changes still drive the
	// blind-apply warnings either way.
	planned := rendered

	// existingWarned: a warning above already tells the user that this
	// create's release is live (6c' adds none of its own then).
	existingWarned := false

	if planErr != nil {
		resp.Diagnostics.AddWarning(
			"nelm_release: the live-cluster plan for this create failed (it is re-checked at apply)",
			"nelm could not plan this create against the objects currently live in the cluster. When this "+
				"plan replaces the resource, those objects belong to the release being replaced, and its destroy "+
				"removes them before the create runs, so this create's \"resources\" are computed at apply. The "+
				"apply re-runs the same checks and fails if the conflict is still there.\n\n"+
				plan.scrubSensitive(planErr.Error()),
		)

		existingWarned = true
	} else {
		built, warns, err := planconv.BuildPlannedResources(prior, planRes.Changes, ns, scoper, rendered, secrets)
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

			existingWarned = true
		}
	}

	// 6c'. A create whose release is live right now: the release being
	// replaced (a tainted resource, -replace, or a destroy-first
	// release_storage_driver change, whose release is in the other backend),
	// a release adopt_existing takes over, or objects another release still
	// owns (planErr: a name or namespace move onto fixed object names). A
	// template that reads the live cluster — lookup, behind which Bitnami
	// charts and grafana keep their generated passwords — sees those objects
	// now and nothing once a replacement's destroy has removed them, so the
	// apply-time re-plan renders other values than a known map planned here,
	// and Terraform would abort the apply after the uninstall ("was known,
	// but now unknown"). The map is computed at apply instead: the re-plan
	// after the destroy finds no release and plans the known render, and
	// Unknown to known is allowed. Under create_before_destroy both phases
	// see the release and degrade alike, and Create refuses it
	// (installGuardDiags, otherBackendDiags). A create whose other backend
	// could not be read for a reason other than RBAC (releaseInOtherBackend)
	// may or may not be live, so it degrades too. The render above still
	// ran: it is what fails a bad chart or bad values at plan time.
	if createPlan {
		live := planErr != nil || planRes.DeployType != nelmclient.DeployTypeInitial

		var otherErr error
		if !live {
			live, otherErr = r.releaseInOtherBackend(ctx, plan, readTimeout)
		}

		if live || otherErr != nil {
			degradeDiffToUnknown(ctx, resp)

			switch {
			case otherErr != nil:
				resp.Diagnostics.AddWarning(
					"nelm_release: could not check the other storage backend, this create's resources are computed at apply",
					fmt.Sprintf("The records of release %q in namespace %q could not be read from the storage "+
						"backend that release_storage_driver does not select, so this plan cannot tell whether a "+
						"release this plan replaces (a release_storage_driver change) is still live there. Templates "+
						"that read live objects (lookup) render differently once a replacement's destroy has removed "+
						"them, so \"resources\" is known after apply.\n\n%s",
						name, ns, otherErr),
				)
			case !existingWarned:
				resp.Diagnostics.AddWarning(
					"nelm_release: this create's release is live, its resources are computed at apply",
					fmt.Sprintf("Release %q has records in namespace %q while this plan creates nelm_release for it: "+
						"the release this plan replaces (a tainted resource, -replace, a release_storage_driver change), "+
						"one adopt_existing takes over, or one left behind by a failed or interrupted install. Templates "+
						"that read live objects (lookup) render differently once a replacement's destroy has removed "+
						"them, so \"resources\" is known after apply.",
						name, ns),
				)
			}

			return
		}
	}

	// 6d. A key set that may change before the apply phase cannot be
	// planned as a known map (and per-element Unknown cannot express an
	// uncertain key): a kind whose scope had to be guessed — not served yet,
	// with no CRD for it in the chart, e.g. a cluster-scoped CR whose CRD
	// another release installs earlier in the same apply, which keys
	// "<ns>" now and "" once served — or a chart that renders a different
	// set of objects each time.
	if unresolved := scoper.Unresolved(); len(unresolved) > 0 {
		degradeDiffToUnknown(ctx, resp)
		resp.Diagnostics.AddWarning(
			"nelm_release: resource kinds not served by the cluster yet",
			fmt.Sprintf("The chart renders objects of kinds the cluster does not serve and whose "+
				"CustomResourceDefinition the chart does not contain: %s. Their keys in \"resources\" depend on "+
				"whether the kind is namespaced, which is only known once its CRD is installed (e.g. by another "+
				"release earlier in this apply), so the resource diff will be computed at apply instead.",
				gvkList(unresolved)),
		)

		return
	}

	if unstable := volatility.Unstable(); len(unstable) > 0 {
		degradeDiffToUnknown(ctx, resp)
		resp.Diagnostics.AddWarning(
			"nelm_release: the chart renders a different set of objects every time",
			"Two renders of the chart with the same configuration disagree on which objects it contains ("+
				strings.Join(unstable, ", ")+"), so the resource diff will be computed at apply instead.",
		)

		return
	}

	// 6e. Volatile objects: a template using randAlphaNum, uuidv4, genCA,
	// now, ... — directly, or behind a lookup guard whose object does not
	// exist yet (a first install) — renders differently every time, so its
	// planned value at the apply phase can never match a known value from
	// the plan phase. An object that differs from its prior value only where
	// every render differs (the rollme/timestamp annotation, the generated
	// password) did not change, so it keeps the prior value: helm_release
	// does not reinstall such a chart on every apply, and nor does this
	// provider. Any other difference is a change like any other, and
	// whenever the release is reinstalled (step 8) its volatile objects are
	// re-rendered, so they are planned Unknown and Create/Update read their
	// applied value back from the cluster. A template whose output changes
	// only once a second or slower (now | date ..., now | unixEpoch) can
	// render identically twice and escape this check; diff_mode = "none" is
	// the way out for those.
	for _, key := range volatility.Keys() {
		value, ok := planned[key]
		if createPlan || !ok || value == prior[key] {
			continue
		}

		same, err := volatility.EqualOutside(key, prior[key], rendered[key])
		if err != nil {
			resp.Diagnostics.AddError("nelm_release: failed to compare the chart renders", err.Error())
			return
		}

		if same {
			planned[key] = prior[key]
		}
	}

	reinstall := releaseWillReinstall(plan, priorState, planned, prior, createPlan)

	unknownKeys := map[string]bool{}

	if reinstall {
		for _, key := range volatility.Keys() {
			if _, ok := planned[key]; ok {
				unknownKeys[key] = true
			}
		}
	}

	plannedMap, mdiags := resourcesMapValue(planned, unknownKeys)
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
	// any config input that feeds the release spec differing from prior
	// state — exactly when 6e plans the volatile objects Unknown. They MUST
	// move together —
	// status/revision were previously gated on nelm's resource-level Changes
	// alone, which misses a values/version edit that renders no manifest
	// change yet still bumps the revision on apply (the "inconsistent result
	// after apply" bug). On a true no-op this is false and prior
	// status/revision/metadata are left entirely untouched — the clean
	// no-change plan.
	if reinstall {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("status"), types.StringUnknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("revision"), types.Int64Unknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("metadata"), types.ObjectUnknown(metadataAttrTypes))...)
	}
}

// planWithoutDiff is ModifyPlan for diff_mode = "none": no render and no
// nelm plan, like helm_release (without its manifest experiment). Whenever
// the release is (re)installed — a create, a non-deployed release, any input
// change — "resources", status, revision and metadata are Unknown, and
// Create/Update read them back from the cluster; otherwise "resources" keeps
// its refreshed value, so the plan is empty. That is the only way to manage
// a chart whose render never converges with its live objects (a
// .Release.Revision-dependent template renders revision N+1 against live N
// on every plan), at the cost of the object diff and drift detection.
func planWithoutDiff(ctx context.Context, plan, priorState releaseModel, createPlan bool, resp *resource.ModifyPlanResponse) {
	if releaseWillReinstall(plan, priorState, nil, nil, createPlan) {
		degradeDiffToUnknown(ctx, resp)
		return
	}

	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("resources"), priorState.Resources)...)
}

// resourcesMapValue builds the planned "resources" value: a known map of
// planned, with the keys in unknown as Unknown elements.
func resourcesMapValue(planned map[string]string, unknown map[string]bool) (types.Map, diag.Diagnostics) {
	elems := make(map[string]attr.Value, len(planned))

	for key, value := range planned {
		if unknown[key] {
			elems[key] = types.StringUnknown()
			continue
		}

		elems[key] = types.StringValue(value)
	}

	return types.MapValue(types.StringType, elems)
}

// gvkList renders kinds for a diagnostic, e.g. "cert-manager.io/v1,
// Kind=ClusterIssuer".
func gvkList(gvks []schema.GroupVersionKind) string {
	out := make([]string, len(gvks))
	for i, gvk := range gvks {
		out[i] = gvk.String()
	}

	return strings.Join(out, "; ")
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
			"or -replace): the destroy uninstalls the old release before the create runs. As the release is live "+
			"while this plan runs, the create's \"resources\" are computed at apply.\n\n"+
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
