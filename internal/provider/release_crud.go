package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
	"github.com/infrabay/terraform-provider-nelm/internal/planconv"
)

// releaseResource implements the nelm_release managed resource type.
//
// Phase A wires Metadata/Schema/Configure for real (this is the frozen
// contract Phase B codes against). Create/Read/Update/Delete/ImportState are
// implemented here (design §2.3-2.4); ModifyPlan's implementation
// (release_plan.go) is owned by T-resplan (design §2.2).
type releaseResource struct {
	client *nelmclient.Client
}

var (
	_ resource.Resource                = &releaseResource{}
	_ resource.ResourceWithConfigure   = &releaseResource{}
	_ resource.ResourceWithImportState = &releaseResource{}
	_ resource.ResourceWithModifyPlan  = &releaseResource{}
)

// NewReleaseResource is the resource constructor registered in
// provider.go's Resources().
func NewReleaseResource() resource.Resource {
	return &releaseResource{}
}

func (r *releaseResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_release"
}

func (r *releaseResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = releaseResourceSchema(ctx)
}

func (r *releaseResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	// Configure is called once per resource instance; ProviderData is nil
	// until the provider's own Configure has run (e.g. during early
	// GetProviderSchema / ValidateResourceConfig RPCs), so this must be a
	// no-op rather than an error in that case.
	if req.ProviderData == nil {
		return
	}

	client, ok := req.ProviderData.(*nelmclient.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *nelmclient.Client, got: %T. This is a provider bug; please report it.", req.ProviderData),
		)
		return
	}

	r.client = client
}

// canonicalValuesJSON marshals values (the coalesced values nelm used to
// render the release) to canonical JSON for metadata.values_json.
// encoding/json sorts map[string]interface{} keys alphabetically by
// construction, giving a deterministic byte representation (design §2.4).
func canonicalValuesJSON(values map[string]any) (string, error) {
	b, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("marshal values to canonical json: %w", err)
	}

	return string(b), nil
}

// applyReleaseInfo copies the cluster-derived fields of info onto model:
// id, status, revision, and metadata. It never touches config-only
// attributes (chart/repository/version/values/set/set_sensitive) or
// resources — callers set resources separately (design §2.3/§2.4: the
// resources-population strategy differs between Create/Update's
// plan-known-verbatim-copy path and Read's always-live path).
func applyReleaseInfo(model *releaseModel, info *nelmclient.ReleaseInfo) diag.Diagnostics {
	var diags diag.Diagnostics

	model.ID = types.StringValue(info.Namespace + "/" + info.Name)
	model.Status = types.StringValue(info.Status)
	model.Revision = types.Int64Value(int64(info.Revision))

	valuesJSON, err := canonicalValuesJSON(info.Values)
	if err != nil {
		diags.AddError("Failed to canonicalize release values", err.Error())
		return diags
	}

	metaObj, d := types.ObjectValue(metadataAttrTypes, map[string]attr.Value{
		"app_version":   types.StringValue(info.AppVersion),
		"chart_name":    types.StringValue(info.ChartName),
		"chart_version": types.StringValue(info.ChartVersion),
		"values_json":   types.StringValue(valuesJSON),
	})
	diags.Append(d...)
	model.Metadata = metaObj

	return diags
}

// liveResourcesMap builds the "resources" map attribute value from a live
// cluster read of refs (design §2.4: Read's live path, and Create/Update's
// degraded-Unknown-plan path). It fetches the live objects via
// r.client.LiveObjects, normalizes them through the SAME planconv pipeline +
// Key as ModifyPlan (CONTRACTS.md seam 2's bold invariant), using r.client
// itself as the KeyScoper (it implements planconv.KeyScoper via IsNamespaced).
//
// desired is the previously stored resources map (prior state's, or a KNOWN
// plan's) keyed identically; each live object is projected onto its desired
// counterpart so Kubernetes' server-side defaulting is stripped generically
// (planconv.NormalizeLiveAgainst). Pass nil when there is no stored desired
// (e.g. a degraded/Unknown plan) — the live objects are then normalized in
// full and converge on the next plan.
func (r *releaseResource) liveResourcesMap(ctx context.Context, refs []nelmclient.ResourceRef, releaseNS string, desired map[string]string, timeout time.Duration) (types.Map, diag.Diagnostics) {
	var diags diag.Diagnostics

	// Bound the live GET phase: LiveObjects' client-go calls honour ctx, so
	// this is what makes timeouts.read actually cap a Read against a half-open
	// API server (a bare RPC ctx has no deadline of its own).
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	objsByRef, err := r.client.LiveObjects(ctx, refs)
	if err != nil {
		diags.AddError("Failed to read live resources", err.Error())
		return types.MapNull(types.StringType), diags
	}

	objs := make([]*unstructured.Unstructured, 0, len(objsByRef))
	for _, obj := range objsByRef {
		objs = append(objs, obj)
	}

	resourcesMap, err := planconv.BuildLiveResources(objs, releaseNS, r.client, desired)
	if err != nil {
		diags.AddError("Failed to build live resources map", err.Error())
		return types.MapNull(types.StringType), diags
	}

	mapVal, d := types.MapValueFrom(ctx, types.StringType, resourcesMap)
	diags.Append(d...)

	return mapVal, diags
}

// desiredResources extracts the stored resources map (prior state's or a KNOWN
// plan's) as a plain map for projection in liveResourcesMap. A null/unknown
// map yields nil (no projection template — the live objects are normalized in
// full).
func desiredResources(ctx context.Context, m types.Map) (map[string]string, diag.Diagnostics) {
	if m.IsNull() || m.IsUnknown() {
		return nil, nil
	}

	out := map[string]string{}
	diags := m.ElementsAs(ctx, &out, false)

	return out, diags
}

// refreshedRelease fetches the current cluster state for the release
// identified by model's name/namespace/release_storage_driver and, if
// found, returns a releaseModel with id/status/revision/metadata populated
// (config attrs copied verbatim from model) plus the raw ReleaseInfo so
// callers can decide how to populate "resources" (design §2.3: Create/Update
// copy it verbatim from a KNOWN plan value instead of re-deriving it; only
// the degraded/partial-failure paths need a live resources computation, so
// that cost — and its own failure mode — is left to the caller rather than
// paid unconditionally here). found is false (with nil diags) when the
// release does not exist, distinguishing "not found" from a real error
// (design §2.3's partial-failure capture: Install err + Get finds a release
// => persist full refreshed state + AddError; Install err + no release =>
// AddError only).
func (r *releaseResource) refreshedRelease(ctx context.Context, model releaseModel, timeout time.Duration) (out releaseModel, info *nelmclient.ReleaseInfo, found bool, diags diag.Diagnostics) {
	name := model.Name.ValueString()
	ns := model.Namespace.ValueString()
	driver := model.ReleaseStorageDriver.ValueString()

	info, err := r.client.Get(ctx, name, ns, driver, timeout)
	if err != nil {
		if nelmclient.IsReleaseNotFound(err) {
			// Genuine "not found": found=false with NIL diags. Callers rely on
			// this to distinguish an absent release from a failed Get (a Get
			// error yields found=false WITH an error diagnostic).
			return releaseModel{}, nil, false, nil
		}

		diags.AddError("Failed to read nelm release", err.Error())
		return releaseModel{}, nil, false, diags
	}

	out = model
	diags.Append(applyReleaseInfo(&out, info)...)

	return out, info, true, diags
}

// planFallbackState derives a persistable state from the plan for the case
// where Install succeeded (or may have partially succeeded) but the immediate
// refresh failed, so the release is not lost from Terraform state. The computed
// cluster attrs (status/revision/metadata) are set to safe concretes — the next
// Read replaces them with live values. The KNOWN plan resources value is
// reproduced verbatim (the ModifyPlan consistency rule); an Unknown plan value
// becomes an empty map.
func planFallbackState(plan releaseModel) (releaseModel, diag.Diagnostics) {
	var diags diag.Diagnostics

	out := plan
	out.ID = types.StringValue(plan.Namespace.ValueString() + "/" + plan.Name.ValueString())
	out.Status = types.StringValue("")
	out.Revision = types.Int64Value(0)

	metaObj, d := types.ObjectValue(metadataAttrTypes, map[string]attr.Value{
		"app_version":   types.StringValue(""),
		"chart_name":    types.StringValue(""),
		"chart_version": types.StringValue(""),
		"values_json":   types.StringValue("{}"),
	})
	diags.Append(d...)
	out.Metadata = metaObj

	if plan.Resources.IsUnknown() || plan.Resources.IsNull() {
		emptyMap, md := types.MapValue(types.StringType, map[string]attr.Value{})
		diags.Append(md...)
		out.Resources = emptyMap
	} else {
		out.Resources = plan.Resources
	}

	return out, diags
}

// createOrUpdate is the shared body of Create and Update (design §2.3): both
// call client.Install (a fresh install, never artifact replay — nelm
// install is an idempotent upgrade) and, on success, build the full state
// from the plan (config attrs verbatim) plus the cluster (computed attrs).
// resources is copied verbatim from the plan when the plan value is KNOWN
// (a known plan value MUST be reproduced exactly at apply); otherwise it is
// computed from a live read. On a partial failure (Install errors but the
// release exists per client.Get), the full refreshed state is persisted so
// Terraform does not lose track of a partially-applied release, and an
// error diagnostic is still added so the apply fails.
func (r *releaseResource) createOrUpdate(ctx context.Context, plan releaseModel, timeout timeoutKind) (state releaseModel, hasState bool, diags diag.Diagnostics) {
	spec, d := plan.toReleaseSpec(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return releaseModel{}, false, diags
	}

	var (
		opTimeout time.Duration
		tdiags    diag.Diagnostics
	)

	switch timeout {
	case timeoutCreate:
		opTimeout, tdiags = plan.Timeouts.Create(ctx, defaultCreateTimeout)
	case timeoutUpdate:
		opTimeout, tdiags = plan.Timeouts.Update(ctx, defaultUpdateTimeout)
	}

	readTimeout, rtdiags := plan.Timeouts.Read(ctx, defaultReadTimeout)
	diags.Append(rtdiags...)

	diags.Append(tdiags...)
	if diags.HasError() {
		return releaseModel{}, false, diags
	}

	installErr := r.client.Install(ctx, spec, opTimeout)

	// Always re-check the cluster after Install, success or failure: on
	// failure this is the partial-failure-capture read (design §2.3); on
	// success it fetches the computed attrs (status/revision/metadata) that
	// were Unknown going into apply. refreshedRelease returns found=false with
	// NIL diags for a genuine not-found, and found=false WITH an error
	// diagnostic when the Get itself failed (getErrored) — that distinction is
	// what lets a successful install survive a transient refresh failure.
	refreshed, info, found, rdiags := r.refreshedRelease(ctx, plan, readTimeout)
	getErrored := rdiags.HasError()
	diags.Append(rdiags...)

	ns := plan.Namespace.ValueString()

	switch {
	case installErr == nil && found:
		// Happy path — handled after the switch.

	case installErr == nil && getErrored:
		// Install SUCCEEDED but the immediate refresh failed transiently (e.g.
		// the API connection reset). The release exists; do NOT drop it from
		// state. Persist a plan-derived state so Terraform keeps tracking it
		// (the next Read fills in status/revision/metadata) — the getErrored
		// diagnostic already in diags still fails this apply.
		fallback, fdiags := planFallbackState(plan)
		diags.Append(fdiags...)
		return fallback, true, diags

	case installErr == nil && !found:
		diags.AddError(
			"nelm_release install reported success but the release was not found",
			"This is unexpected; please report it as a provider bug.",
		)
		return releaseModel{}, false, diags

	case installErr != nil && found:
		// Partial failure: Install errored but the release exists (e.g. it
		// installed some resources before failing, or a prior apply already
		// created it). Persist the full refreshed state — including a
		// freshly live-computed resources map, since the plan's (possibly
		// KNOWN) resources value described the intended post-apply state
		// that this partial apply did NOT fully reach — so Terraform does
		// not lose track of the release, but still fail the apply. The plan's
		// resources (when KNOWN) are the projection template.
		desired, ddiags := desiredResources(ctx, plan.Resources)
		diags.Append(ddiags...)
		resMap, mdiags := r.liveResourcesMap(ctx, info.Resources, ns, desired, readTimeout)
		diags.Append(mdiags...)
		refreshed.Resources = resMap

		diags.AddError(
			"nelm_release install failed (partial state persisted)",
			fmt.Sprintf("install error: %s", plan.scrubSensitive(installErr.Error())),
		)
		return refreshed, true, diags

	default:
		// installErr != nil && (getErrored || genuine not-found): the release
		// is absent or unconfirmable, so don't persist a possibly-nonexistent
		// release. Any getErrored diagnostic is already in diags.
		diags.AddError("nelm_release install failed", plan.scrubSensitive(installErr.Error()))
		return releaseModel{}, false, diags
	}

	// installErr == nil && found.
	state = refreshed

	// A KNOWN plan value MUST be reproduced exactly at apply (ModifyPlan
	// consistency rule) — do NOT recompute it from the live cluster in that
	// case; recomputing it would also pay a needless LiveObjects round trip
	// on the common fast path. Only the degraded Unknown-plan path (cluster
	// was unreachable at plan time) needs a live resources computation here.
	if !plan.Resources.IsUnknown() && !plan.Resources.IsNull() {
		state.Resources = plan.Resources
	} else {
		// Degraded (Unknown) plan: the cluster was unreachable at plan time,
		// so there is no stored desired to project against — pass nil and let
		// the map converge on the next plan.
		resMap, mdiags := r.liveResourcesMap(ctx, info.Resources, ns, nil, readTimeout)
		diags.Append(mdiags...)
		state.Resources = resMap
	}

	return state, true, diags
}

type timeoutKind int

const (
	timeoutCreate timeoutKind = iota
	timeoutUpdate
)

func (r *releaseResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan releaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	state, hasState, diags := r.createOrUpdate(ctx, plan, timeoutCreate)
	resp.Diagnostics.Append(diags...)
	if !hasState {
		// No usable state to persist (hard failure, no release found).
		return
	}

	// CreateResponse.State starts NULL; a full Set is mandatory or the
	// framework errors "Missing Resource State After Create".
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *releaseResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan releaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	state, hasState, diags := r.createOrUpdate(ctx, plan, timeoutUpdate)
	resp.Diagnostics.Append(diags...)
	if !hasState {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *releaseResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state releaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	readTimeout, rtdiags := state.Timeouts.Read(ctx, defaultReadTimeout)
	resp.Diagnostics.Append(rtdiags...)
	if resp.Diagnostics.HasError() {
		return
	}

	refreshed, info, found, diags := r.refreshedRelease(ctx, state, readTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	// Resources is always recomputed from a LIVE cluster read (never from
	// the stored release manifests) so out-of-band drift is visible at the
	// next plan (design §2.4). The resources already in state are the
	// projection template: their shape is the chart's desired shape, so
	// projecting the live objects onto it strips server-side defaulting while
	// keeping genuine drift visible.
	desired, ddiags := desiredResources(ctx, state.Resources)
	resp.Diagnostics.Append(ddiags...)
	if resp.Diagnostics.HasError() {
		return
	}

	resMap, rdiags := r.liveResourcesMap(ctx, info.Resources, state.Namespace.ValueString(), desired, readTimeout)
	resp.Diagnostics.Append(rdiags...)
	if resp.Diagnostics.HasError() {
		return
	}
	refreshed.Resources = resMap

	resp.Diagnostics.Append(resp.State.Set(ctx, &refreshed)...)
}

func (r *releaseResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state releaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	deleteTimeout, diags := state.Timeouts.Delete(ctx, defaultDeleteTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := r.client.Uninstall(
		ctx,
		state.Name.ValueString(),
		state.Namespace.ValueString(),
		state.ReleaseStorageDriver.ValueString(),
		deleteTimeout,
	)
	if err != nil {
		resp.Diagnostics.AddError("nelm_release uninstall failed", err.Error())
		return
	}

	// Uninstall is idempotent-silent on a missing release/namespace, so
	// reaching here means either a clean delete or a no-op on an
	// already-gone release — both are success. The framework automatically
	// clears State on a Delete that returns without error diagnostics; do
	// not call resp.State.RemoveResource ourselves.
}

// parseImportID splits "namespace/name" import IDs (design §2.4). It is a
// free function so it can be unit-tested without a *releaseResource.
func parseImportID(id string) (namespace, name string, err error) {
	ns, n, ok := strings.Cut(id, "/")
	// Reject extra slashes too: Kubernetes/Helm release names cannot contain
	// "/", so "default/my/release" is a malformed ID, not namespace "default"
	// name "my/release" — surface that immediately rather than as a later
	// storage-label lookup error.
	if !ok || ns == "" || n == "" || strings.Contains(n, "/") {
		return "", "", fmt.Errorf(`expected import ID in the form "namespace/name" (exactly one "/"), got %q`, id)
	}

	return ns, n, nil
}

func (r *releaseResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	ns, name, err := parseImportID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid nelm_release import ID", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("namespace"), ns)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), ns+"/"+name)...)

	// Explicitly force every defaulted flag to its schema default so a
	// post-import ImportStateVerify matches post-create state (design
	// §2.4): the framework does not run schema defaults during import,
	// only during a "create" plan.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("auto_rollback"), false)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("force_adoption"), false)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("no_remove_manual_changes"), false)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("no_install_crds"), false)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("release_storage_driver"), "secret")...)

	// The framework runs Read after ImportState to fill in the remaining
	// computed attributes (status/revision/metadata/resources).
}
