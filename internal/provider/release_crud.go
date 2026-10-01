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
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
	"github.com/infrabay/terraform-provider-nelm/internal/planconv"
)

// releaseResource implements the nelm_release managed resource type.
//
// Metadata/Schema/Configure and Create/Read/Update/Delete/ImportState are
// implemented here; ModifyPlan is implemented in release_plan.go.
type releaseResource struct {
	client releaseClient
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
// cluster read of refs (design §2.4: Read's live path, a failed install's
// partial state, and — via liveResources — what Create/Update read back for
// an Unknown plan value). It fetches the live objects via
// r.client.LiveObjects, normalizes them through the SAME planconv pipeline +
// Key as ModifyPlan (CONTRACTS.md seam 2's bold invariant), using r.client
// itself as the KeyScoper (it implements planconv.KeyScoper via IsNamespaced).
//
// desired is the projection template keyed identically (projectionTemplate:
// the stored values, or the release manifests); each live object is
// projected onto its desired counterpart so Kubernetes' server-side
// defaulting is stripped generically (planconv.NormalizeLiveAgainst). A live
// object without one is normalized in full. secrets are the set_sensitive
// values (releaseModel.sensitiveValues) of the configuration the map is
// stored with, scrubbed like ModifyPlan scrubs the planned side, and desired
// must have been built with them.
func (r *releaseResource) liveResourcesMap(ctx context.Context, refs []nelmclient.ResourceRef, releaseNS string, desired map[string]string, secrets []string, timeout time.Duration) (types.Map, diag.Diagnostics) {
	resourcesMap, diags := r.liveResources(ctx, refs, releaseNS, desired, secrets, timeout)
	if diags.HasError() {
		return types.MapNull(types.StringType), diags
	}

	mapVal, d := types.MapValueFrom(ctx, types.StringType, resourcesMap)
	diags.Append(d...)

	return mapVal, diags
}

// liveResources is liveResourcesMap's body, returning the plain map.
func (r *releaseResource) liveResources(ctx context.Context, refs []nelmclient.ResourceRef, releaseNS string, desired map[string]string, secrets []string, timeout time.Duration) (map[string]string, diag.Diagnostics) {
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
		return nil, diags
	}

	objs := make([]*unstructured.Unstructured, 0, len(objsByRef))
	for _, obj := range objsByRef {
		objs = append(objs, obj)
	}

	resourcesMap, err := planconv.BuildLiveResources(objs, releaseNS, r.client, desired, secrets)
	if err != nil {
		diags.AddError("Failed to build live resources map", err.Error())
		return nil, diags
	}

	return resourcesMap, diags
}

// desiredResources extracts the stored resources map (prior state's or a
// plan's) as a plain map for projection in liveResourcesMap. A null/unknown
// map yields nil (no projection template — the live objects are normalized in
// full); a plan's Unknown elements (ModifyPlan's volatile objects) are left
// out.
func desiredResources(ctx context.Context, m types.Map) (map[string]string, diag.Diagnostics) {
	if m.IsNull() || m.IsUnknown() {
		return nil, nil
	}

	out := map[string]string{}

	for key, v := range m.Elements() {
		if v.IsUnknown() {
			continue
		}

		var str types.String

		diags := tfsdk.ValueAs(ctx, v, &str)
		if diags.HasError() {
			return nil, diags
		}

		out[key] = str.ValueString()
	}

	return out, nil
}

// unknownResourceKeys returns the keys of m's Unknown elements (none for a
// null or wholly Unknown map).
func unknownResourceKeys(m types.Map) map[string]bool {
	out := map[string]bool{}

	if m.IsNull() || m.IsUnknown() {
		return out
	}

	for key, v := range m.Elements() {
		if v.IsUnknown() {
			out[key] = true
		}
	}

	return out
}

// projectionTemplate is what a live read projects onto: the stored values
// (prior state's, or the plan's known ones), and for every object without a
// value — a state seeded by import or a moved block, a plan's Unknown
// element or wholly Unknown map — the release's stored manifest, what its
// last install rendered and applied, normalized like a planned value.
// Projecting a live object onto either strips the server's defaulting the
// same way, so the result has the chart's shape instead of carrying every
// live-only field. The manifests are best-effort: if they cannot be keyed,
// those objects are normalized in full. They are scrubbed of secrets, which
// must be the ones stored was built with.
func (r *releaseResource) projectionTemplate(ctx context.Context, stored types.Map, info *nelmclient.ReleaseInfo, releaseNS string, secrets []string) (map[string]string, diag.Diagnostics) {
	out := map[string]string{}

	if info != nil && len(info.Manifests) > 0 {
		if manifests, err := planconv.BuildRenderedResources(info.Manifests, releaseNS, r.client, secrets); err == nil {
			out = manifests
		}
	}

	known, diags := desiredResources(ctx, stored)
	for key, value := range known {
		if value != "" {
			out[key] = value
		}
	}

	return out, diags
}

// appliedResources builds the "resources" value a successful Create/Update
// persists. A KNOWN planned value MUST be reproduced exactly (Terraform's
// "inconsistent result after apply" check), so the plan's known elements are
// copied verbatim — recomputing them would also pay a needless LiveObjects
// round trip on the common fast path. What the plan left Unknown is read
// from the cluster after the install: the whole map when ModifyPlan degraded
// the diff (cluster unreachable or kinds not served at plan time,
// diff_mode = "none"), or the single objects whose render changes on every
// render (ModifyPlan step 6e) — their applied value is only knowable now.
// The live objects are projected onto the stored release's manifests, so the
// persisted values have the chart's shape. A failed live read must not fail
// the successful install (see installedButUnreadWarnings): an object it
// could not read keeps its manifest value, or "" when it has none, and the
// next Read rebuilds it. secrets are the plan's set_sensitive values, which
// the planned values were scrubbed of.
func (r *releaseResource) appliedResources(ctx context.Context, planned types.Map, info *nelmclient.ReleaseInfo, releaseNS string, secrets []string, timeout time.Duration) (types.Map, diag.Diagnostics) {
	var diags diag.Diagnostics

	wholeUnknown := planned.IsUnknown() || planned.IsNull()
	unknownKeys := unknownResourceKeys(planned)

	if !wholeUnknown && len(unknownKeys) == 0 {
		return planned, diags
	}

	template, tdiags := r.projectionTemplate(ctx, planned, info, releaseNS, secrets)
	if tdiags.HasError() {
		diags.Append(installedButUnreadWarnings(tdiags)...)
	} else {
		diags.Append(tdiags...)
	}

	refs := info.Resources
	if !wholeUnknown {
		refs = refsWithKeys(info.Resources, releaseNS, r.client, unknownKeys)
	}

	live, ldiags := r.liveResources(ctx, refs, releaseNS, template, secrets, timeout)
	if ldiags.HasError() {
		diags.Append(installedButUnreadWarnings(ldiags)...)
		live = nil
	} else {
		diags.Append(ldiags...)
	}

	if wholeUnknown {
		if live == nil {
			return emptyResourcesMap(), diags
		}

		mapVal, d := types.MapValueFrom(ctx, types.StringType, live)
		diags.Append(d...)

		return mapVal, diags
	}

	elems := make(map[string]attr.Value, len(planned.Elements()))
	for key, v := range planned.Elements() {
		if !unknownKeys[key] {
			elems[key] = v
			continue
		}

		value, ok := live[key]
		if !ok {
			value = template[key]
		}

		elems[key] = types.StringValue(value)
	}

	mapVal, d := types.MapValue(types.StringType, elems)
	diags.Append(d...)

	return mapVal, diags
}

// refsWithKeys returns the refs whose resources-map key is in keys.
func refsWithKeys(refs []nelmclient.ResourceRef, releaseNS string, scoper planconv.KeyScoper, keys map[string]bool) []nelmclient.ResourceRef {
	var out []nelmclient.ResourceRef

	for _, ref := range refs {
		key, err := planconv.Key(planconv.Ref{
			GroupVersionKind: schema.GroupVersionKind{Group: ref.Group, Version: ref.Version, Kind: ref.Kind},
			Namespace:        ref.Namespace,
			Name:             ref.Name,
		}, releaseNS, scoper)
		if err == nil && keys[key] {
			out = append(out, ref)
		}
	}

	return out
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
// where Install succeeded but the immediate refresh failed, so the release is
// not lost from Terraform state. Since the apply then succeeds, the result
// must match the plan wherever the plan is KNOWN (Terraform's "inconsistent
// result after apply" check): known plan values — resources, and an Update's
// status/revision/metadata when ModifyPlan expected no reinstall — are
// reproduced verbatim. Unknown computed cluster attrs are set to safe
// concretes that the next Read replaces with live values; an Unknown
// resources value becomes an empty map, and an Unknown element of it "".
func planFallbackState(plan releaseModel) (releaseModel, diag.Diagnostics) {
	var diags diag.Diagnostics

	out := plan
	out.ID = types.StringValue(plan.Namespace.ValueString() + "/" + plan.Name.ValueString())

	if plan.Status.IsUnknown() || plan.Status.IsNull() {
		out.Status = types.StringValue("")
	}

	if plan.Revision.IsUnknown() || plan.Revision.IsNull() {
		out.Revision = types.Int64Value(0)
	}

	if plan.Metadata.IsUnknown() || plan.Metadata.IsNull() {
		metaObj, d := types.ObjectValue(metadataAttrTypes, map[string]attr.Value{
			"app_version":   types.StringValue(""),
			"chart_name":    types.StringValue(""),
			"chart_version": types.StringValue(""),
			"values_json":   types.StringValue("{}"),
		})
		diags.Append(d...)
		out.Metadata = metaObj
	}

	switch {
	case plan.Resources.IsUnknown() || plan.Resources.IsNull():
		out.Resources = emptyResourcesMap()

	case len(unknownResourceKeys(plan.Resources)) > 0:
		// ModifyPlan's volatile objects: "" until the next Read rebuilds them
		// from the cluster (an empty desired value projects nothing).
		elems := make(map[string]attr.Value, len(plan.Resources.Elements()))
		for key, v := range plan.Resources.Elements() {
			if v.IsUnknown() {
				v = types.StringValue("")
			}

			elems[key] = v
		}

		resMap, d := types.MapValue(types.StringType, elems)
		diags.Append(d...)
		out.Resources = resMap
	}

	return out, diags
}

// emptyResourcesMap is the known, empty "resources" value persisted when no
// resources map could be computed; the next Read rebuilds it from live refs.
func emptyResourcesMap() types.Map {
	return types.MapValueMust(types.StringType, map[string]attr.Value{})
}

// failedUpdateState builds the state a FAILED Update persists: the
// PRIOR state's configuration — so the attempted change is still a diff on
// the next plan and is retried, instead of being recorded as applied —
// overlaid with what the cluster reports now (id/status/revision/metadata/
// resources from refreshed). Persisting the plan's configuration instead
// silently dropped any failed change that the retry triggers (status !=
// deployed, a resources-map difference) do not see: e.g. a hook-only change
// after a successful auto_rollback, or a failure before nelm wrote a revision.
// Terraform skips its "inconsistent result after apply" check when Update
// returns an error, so a state that differs from the plan is legal here.
func failedUpdateState(prior, refreshed releaseModel) releaseModel {
	out := prior
	out.ID = refreshed.ID
	out.Status = refreshed.Status
	out.Revision = refreshed.Revision
	out.Metadata = refreshed.Metadata
	out.Resources = refreshed.Resources

	return out
}

// installedButUnreadWarnings downgrades the error diagnostics of a failed
// post-install read to warnings. The install itself succeeded, so the
// apply must not fail: a failed Create makes Terraform taint the resource and
// replace — uninstall and reinstall — a healthy release on the next apply.
func installedButUnreadWarnings(diags diag.Diagnostics) diag.Diagnostics {
	var out diag.Diagnostics

	for _, d := range diags {
		if d.Severity() != diag.SeverityError {
			out.Append(d)
			continue
		}

		out.AddWarning(
			"nelm_release installed, but reading it back failed",
			"The install succeeded, so the apply is not failed; status, revision, metadata and resources are "+
				"filled in from the cluster by the next refresh.\n\n"+d.Summary()+": "+d.Detail(),
		)
	}

	return out
}

// createOrUpdate is the shared body of Create and Update (design §2.3): both
// call client.Install (a fresh install, never artifact replay — nelm
// install is an idempotent upgrade) and, on success, build the full state
// from the plan (config attrs verbatim) plus the cluster (computed attrs).
// resources keeps the plan's KNOWN elements verbatim (a known plan value MUST
// be reproduced exactly at apply); whatever the plan left Unknown is read
// from the cluster (appliedResources). On a partial failure (Install errors
// but the release exists per client.Get), the refreshed state is persisted
// so Terraform does not lose track of a partially-applied release, and an
// error diagnostic is still added so the apply fails; on an Update it keeps
// the prior configuration (failedUpdateState) so the change is retried.
//
// prior is the Update's prior state, nil on Create. Before installing, the
// release's stored history is checked by installGuardDiags: Create never
// silently adopts an existing release (nor, via otherBackendDiags, one still
// deployed in the other storage backend), and neither path installs over a
// pending-* revision another operation still holds.
func (r *releaseResource) createOrUpdate(ctx context.Context, plan releaseModel, prior *releaseModel, timeout timeoutKind) (state releaseModel, hasState bool, diags diag.Diagnostics) {
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

	ns := plan.Namespace.ValueString()

	// Pre-install guards against the release's CURRENT history (read at
	// apply, not plan, time): no silent adoption on Create, no install over a
	// pending-* revision another operation holds. A history read failure
	// fails the apply before anything is changed.
	history, err := r.client.History(ctx, plan.Name.ValueString(), ns, plan.ReleaseStorageDriver.ValueString(), readTimeout)
	if err != nil {
		diags.AddError("Failed to read nelm release history", err.Error())
		return releaseModel{}, false, diags
	}

	diags.Append(installGuardDiags(plan, history, prior == nil, pendingTakeoverAge(opTimeout), time.Now())...)
	if diags.HasError() {
		return releaseModel{}, false, diags
	}

	// A Create whose backend holds no deployed revision may still be the
	// create half of a create_before_destroy release_storage_driver change,
	// with the release deployed in the other backend.
	if prior == nil && !history.Deployed {
		diags.Append(r.otherBackendDiags(ctx, plan, readTimeout)...)
		if diags.HasError() {
			return releaseModel{}, false, diags
		}
	}

	// Hand any field ownership hashicorp/helm's helm_release left on the
	// release's objects over to nelm first, or this install cannot prune what
	// the chart no longer renders (nelmclient.HandOverHelmProviderFieldManagers).
	// Apply-only by design: ModifyPlan never runs it, so a plan adds no write
	// of its own. It shares the operation's timeout budget with the install.
	start := time.Now()

	skipped, err := r.client.HandOverHelmProviderFieldManagers(ctx, spec.Name, spec.Namespace, spec.StorageDriver, opTimeout)
	if err != nil {
		diags.AddError("nelm_release: helm_release field-manager hand-over failed", plan.scrubSensitive(err.Error()))
		return releaseModel{}, false, diags
	}

	if len(skipped) > 0 {
		diags.AddWarning(
			"nelm_release: helm_release field-manager hand-over skipped",
			"An admission webhook was unavailable, so these objects keep a terraform-provider-helm field manager: "+
				"fields it set that the chart no longer renders stay live until a later apply of this release "+
				"completes the hand-over.\n\n"+plan.scrubSensitive(strings.Join(skipped, "\n")),
		)
	}

	installErr := r.client.Install(ctx, spec, remainingTimeout(opTimeout, time.Since(start)))

	// Always re-check the cluster after Install, success or failure: on
	// failure this is the partial-failure-capture read (design §2.3); on
	// success it fetches the computed attrs (status/revision/metadata) that
	// were Unknown going into apply. refreshedRelease returns found=false with
	// NIL diags for a genuine not-found, and found=false WITH an error
	// diagnostic when the Get itself failed (getErrored) — that distinction is
	// what lets a successful install survive a transient refresh failure.
	refreshed, info, found, rdiags := r.refreshedRelease(ctx, plan, readTimeout)
	getErrored := rdiags.HasError()

	switch {
	case installErr == nil && getErrored:
		// Install SUCCEEDED but the immediate refresh failed (e.g. a transient
		// API error). The release exists and is healthy: do NOT drop it from
		// state, and do NOT fail the apply — a failed Create is tainted, and
		// the next apply would replace (uninstall and reinstall) the release.
		// Persist a plan-derived state and only warn; the next Read fills in
		// status/revision/metadata.
		fallback, fdiags := planFallbackState(plan)
		diags.Append(fdiags...)
		diags.Append(installedButUnreadWarnings(rdiags)...)
		return fallback, true, diags

	case installErr == nil && found:
		// Happy path — handled after the switch.

	case installErr == nil:
		diags.AddError(
			"nelm_release install reported success but the release was not found",
			"This is unexpected; please report it as a provider bug.",
		)
		return releaseModel{}, false, diags

	case found:
		// Partial failure: Install errored but the release exists (e.g. it
		// installed some resources before failing, or a prior apply already
		// created it). Persist the refreshed state — including a freshly
		// live-computed resources map, since the plan's (possibly KNOWN)
		// resources value described the intended post-apply state that this
		// partial apply did NOT fully reach — so Terraform does not lose track
		// of the release, but still fail the apply. The plan's known resources
		// are the projection template, the stored manifests fill in the rest.
		// A failed Update keeps the PRIOR configuration so the change is
		// retried (failedUpdateState). Its objects can carry either the
		// prior's or the plan's set_sensitive values, so both are scrubbed.
		diags.Append(rdiags...)

		secrets := plan.sensitiveValues()
		if prior != nil {
			secrets = append(secrets, prior.sensitiveValues()...)
		}

		desired, ddiags := r.projectionTemplate(ctx, plan.Resources, info, ns, secrets)
		diags.Append(ddiags...)
		resMap, mdiags := r.liveResourcesMap(ctx, info.Resources, ns, desired, secrets, readTimeout)
		diags.Append(mdiags...)
		refreshed.Resources = resMap

		if prior != nil {
			refreshed = failedUpdateState(*prior, refreshed)
		}

		diags.AddError(
			"nelm_release install failed (partial state persisted)",
			fmt.Sprintf("install error: %s", plan.scrubSensitive(installErr.Error())),
		)
		return refreshed, true, diags

	default:
		// installErr != nil && (getErrored || genuine not-found): the release
		// is absent or unconfirmable, so don't persist a possibly-nonexistent
		// release (an Update's prior state is kept by the framework).
		diags.Append(rdiags...)
		diags.AddError("nelm_release install failed", plan.scrubSensitive(installErr.Error()))
		return releaseModel{}, false, diags
	}

	// installErr == nil && found.
	diags.Append(rdiags...)
	state = refreshed

	resMap, mdiags := r.appliedResources(ctx, plan.Resources, info, ns, plan.sensitiveValues(), readTimeout)
	diags.Append(mdiags...)
	state.Resources = resMap

	return state, true, diags
}

// remainingTimeout is what is left of an operation's timeout budget after
// spent. A zero budget (no timeout) is returned as-is; an exhausted one is
// floored at 1ns, because nelm reads a zero Timeout as "no timeout".
func remainingTimeout(budget, spent time.Duration) time.Duration {
	if budget <= 0 {
		return budget
	}

	return max(budget-spent, time.Nanosecond)
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

	state, hasState, diags := r.createOrUpdate(ctx, plan, nil, timeoutCreate)
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
	var plan, prior releaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}

	state, hasState, diags := r.createOrUpdate(ctx, plan, &prior, timeoutUpdate)
	resp.Diagnostics.Append(diags...)
	if !hasState {
		// UpdateResponse.State starts as the prior state: not setting it
		// keeps the prior state.
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
	// keeping genuine drift visible. An object state has no value for (the
	// first Read after an import or a moved block) is projected onto its
	// stored release manifest instead of being kept in full: a volatile
	// object (ModifyPlan step 6e) only keeps its value when it has the
	// chart's shape. The state's set_sensitive values are scrubbed, as
	// ModifyPlan scrubs the plan's from the planned side (CONTRACTS.md seam
	// 2); state has none right after an import, until the first apply. So
	// are the values the release's last revision holds at those names: a
	// failed update keeps the previous configuration in state, while the
	// objects it partly applied carry the new values.
	secrets := append(state.sensitiveValues(), state.storedSensitiveValues(info.Values)...)

	desired, ddiags := r.projectionTemplate(ctx, state.Resources, info, state.Namespace.ValueString(), secrets)
	resp.Diagnostics.Append(ddiags...)
	if resp.Diagnostics.HasError() {
		return
	}

	resMap, rdiags := r.liveResourcesMap(ctx, info.Resources, state.Namespace.ValueString(), desired, secrets, readTimeout)
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
		// Uninstall-time hooks can echo rendered manifest content in nelm's
		// error output — scrub set_sensitive values like every other
		// nelm-output-carrying diagnostic.
		resp.Diagnostics.AddError("nelm_release uninstall failed", state.scrubSensitive(err.Error()))
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
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("wait"), true)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("force_adoption"), false)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("no_remove_manual_changes"), false)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("no_install_crds"), false)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("release_storage_driver"), "secret")...)
	// adopt_existing is seeded false, not true: it is only read by Create,
	// which an imported resource never runs (its first apply is an Update),
	// and false matches a configuration that leaves it at its default, so
	// importing never plans a change to it.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("adopt_existing"), false)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("diff_mode"), diffModeFull)...)

	// The framework runs Read after ImportState to fill in the remaining
	// computed attributes (status/revision/metadata/resources).
}
