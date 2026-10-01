package provider

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/werf/nelm/pkg/plan"
	"github.com/werf/nelm/pkg/resource/spec"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
	"github.com/infrabay/terraform-provider-nelm/internal/planconv"
)

const (
	volatileDeployKey = "apps/v1/Deployment/default/my-release-volatile"
	volatileSecretKey = "v1/Secret/default/my-release-volatile"
	volatileCMKey     = "v1/ConfigMap/default/my-release-volatile"
)

// volatileChart renders like testdata/charts/volatile (nelmclient's
// TestVolatileFixtureRendersDifferentlyEveryTime pins that nelm's engine
// really does): every render gets a new rollme annotation and deploy-date
// timestamp, and a new generated password until the Secret exists (its
// lookup guard then reuses the stored one); the ConfigMap never changes.
type volatileChart struct {
	renders int
	// storedPassword is the password lookup finds; "" before the first
	// install created the Secret.
	storedPassword string
	replicas       int64
}

func (c *volatileChart) render() []*unstructured.Unstructured {
	c.renders++

	password := c.storedPassword
	if password == "" {
		password = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("generated-%06d", c.renders)))
	}

	replicas := c.replicas
	if replicas == 0 {
		replicas = 1
	}

	return []*unstructured.Unstructured{
		{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "my-release-volatile"},
			"data":       map[string]any{"message": "hello"},
		}},
		{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]any{"name": "my-release-volatile"},
			"type":       "Opaque",
			"data":       map[string]any{"password": password},
		}},
		{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": "my-release-volatile"},
			"spec": map[string]any{
				"replicas": replicas,
				"template": map[string]any{
					"metadata": map[string]any{"annotations": map[string]any{
						"rollme":      fmt.Sprintf("r%04d", c.renders),
						"deploy-date": fmt.Sprintf("2026-10-01 13:16:39.%06d +0200 CEST", c.renders),
					}},
					"spec": map[string]any{"containers": []any{
						map[string]any{"name": "volatile", "image": "nginx:1.27-alpine"},
					}},
				},
			},
		}},
	}
}

// renderedMap normalizes objs the way ModifyPlan does.
func renderedMap(t *testing.T, objs []*unstructured.Unstructured) map[string]string {
	t.Helper()

	out, err := planconv.BuildRenderedResources(objs, "default", &fakeReleaseClient{}, nil)
	if err != nil {
		t.Fatalf("BuildRenderedResources: %v", err)
	}

	return out
}

// installedVolatileModel is the state right after the volatile chart's first
// install (render installed): every value known, the Secret now exists.
func installedVolatileModel(t *testing.T, chart *volatileChart) releaseModel {
	t.Helper()

	installed := renderedMap(t, chart.render())

	m := appliedModel(1, "deployed")
	m.Resources = types.MapValueMust(types.StringType, map[string]attr.Value{
		volatileCMKey:     types.StringValue(installed[volatileCMKey]),
		volatileSecretKey: types.StringValue(installed[volatileSecretKey]),
		volatileDeployKey: types.StringValue(installed[volatileDeployKey]),
	})

	return m
}

// deploymentUpdate is nelm's plan for a live volatile Deployment: its rollme
// and deploy-date always differ from the new render, so it is an update.
func deploymentUpdate() *plan.ResourceChange {
	after := (&volatileChart{}).render()[2]
	after.SetNamespace("default")

	return &plan.ResourceChange{
		Type:         "update",
		ResourceMeta: &spec.ResourceMeta{Name: after.GetName(), GroupVersionKind: after.GroupVersionKind()},
		Before:       after.DeepCopy(),
		After:        after,
	}
}

// plannedResourcesValue reads the raw planned "resources" value.
func plannedResourcesValue(t *testing.T, p tfsdk.Plan) types.Map {
	t.Helper()

	var m types.Map
	if diags := p.GetAttribute(context.Background(), path.Root("resources"), &m); diags.HasError() {
		t.Fatalf("read planned resources: %v", diags)
	}

	return m
}

// assertCompatible applies Terraform core's plan-vs-final-plan rule for the
// "resources" map (plans/objchange.AssertObjectCompatible): the apply-phase
// ModifyPlan must produce the same keys, and every element the plan phase
// planned KNOWN must come out identical; an Unknown planned element may
// become anything. A violation is "Provider produced inconsistent final
// plan".
func assertCompatible(t *testing.T, planned, actual types.Map) {
	t.Helper()

	if planned.IsUnknown() {
		return
	}

	if actual.IsUnknown() {
		t.Fatalf("resources: was known, but now unknown")
	}

	for key, pv := range planned.Elements() {
		av, ok := actual.Elements()[key]
		if !ok {
			t.Errorf("resources: element %q has vanished", key)
			continue
		}

		if pv.IsUnknown() {
			continue
		}

		if !pv.Equal(av) {
			t.Errorf("resources[%q]: was %s, but now %s", key, pv, av)
		}
	}

	for key := range actual.Elements() {
		if _, ok := planned.Elements()[key]; !ok {
			t.Errorf("resources: new element %q has appeared", key)
		}
	}
}

// assertComputedUnknown asserts whether status/revision/metadata are Unknown
// (a reinstall) or not (left as proposed: the prior values on a no-change
// plan).
func assertComputedUnknown(t *testing.T, p tfsdk.Plan, want bool) {
	t.Helper()

	ctx := context.Background()

	var (
		status   types.String
		revision types.Int64
		metadata types.Object
	)

	p.GetAttribute(ctx, path.Root("status"), &status)
	p.GetAttribute(ctx, path.Root("revision"), &revision)
	p.GetAttribute(ctx, path.Root("metadata"), &metadata)

	if status.IsUnknown() != want || revision.IsUnknown() != want || metadata.IsUnknown() != want {
		t.Errorf("status/revision/metadata unknown = %v/%v/%v, want %v", status.IsUnknown(), revision.IsUnknown(), metadata.IsUnknown(), want)
	}
}

// TestModifyPlan_VolatileCreate is the F01 regression test (first install).
// The plan-phase and the apply-phase ModifyPlan each render the chart, so a
// generated password and a rollme/timestamp annotation were planned as two
// different KNOWN values and every apply — the first install included —
// aborted with "Provider produced inconsistent final plan". The objects two
// renders disagree on are now Unknown; the rest stays known.
func TestModifyPlan_VolatileCreate(t *testing.T) {
	chart := &volatileChart{}
	client := &fakeReleaseClient{renderFn: chart.render}

	planPhase := runModifyPlan(t, client, baseTestReleaseModel(), nil)
	applyPhase := runModifyPlan(t, client, baseTestReleaseModel(), nil)

	for name, resp := range map[string]*resource.ModifyPlanResponse{"plan phase": planPhase, "apply phase": applyPhase} {
		if resp.Diagnostics.HasError() {
			t.Fatalf("%s: unexpected errors: %v", name, resp.Diagnostics)
		}
	}

	planned := plannedResourcesValue(t, planPhase.Plan)
	if planned.IsUnknown() {
		t.Fatal("resources must stay a known map: only the volatile objects are unknown")
	}

	for _, key := range []string{volatileDeployKey, volatileSecretKey} {
		if v, ok := planned.Elements()[key]; !ok || !v.IsUnknown() {
			t.Errorf("resources[%q] = %v, want Unknown (it renders differently every time)", key, v)
		}
	}

	wantCM := renderedMap(t, (&volatileChart{}).render())[volatileCMKey]
	if v := planned.Elements()[volatileCMKey]; !v.Equal(types.StringValue(wantCM)) {
		t.Errorf("resources[%q] = %v, want the known render %s", volatileCMKey, v, wantCM)
	}

	assertComputedUnknown(t, planPhase.Plan, true)
	assertCompatible(t, planned, plannedResourcesValue(t, applyPhase.Plan))
}

// TestModifyPlan_DeterministicChartStaysKnown: the volatility probe changes
// nothing for a chart that renders identically every time.
func TestModifyPlan_DeterministicChartStaysKnown(t *testing.T) {
	client := &fakeReleaseClient{renderObjs: renderedObjects()}

	resp := runModifyPlan(t, client, baseTestReleaseModel(), nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	for key, v := range plannedResourcesValue(t, resp.Plan).Elements() {
		if v.IsUnknown() {
			t.Errorf("resources[%q] is Unknown, want known", key)
		}
	}
}

// TestModifyPlan_VolatileSteadyStateIsEmpty: with nothing changed, an object
// that differs from its live value only where every render differs keeps
// its prior value — the plan is empty, like helm_release's, instead of a
// perpetual update that rolls the pods on every apply (and, under a
// plan-hash-checking CI such as dflook, a plan that never matches).
func TestModifyPlan_VolatileSteadyStateIsEmpty(t *testing.T) {
	chart := &volatileChart{}
	prior := installedVolatileModel(t, chart)
	chart.storedPassword = base64.StdEncoding.EncodeToString([]byte("generated-000001"))

	client := &fakeReleaseClient{
		renderFn:   chart.render,
		planResult: &nelmclient.PlanResult{DeployType: nelmclient.DeployTypeUpgrade, Changes: []*plan.ResourceChange{deploymentUpdate()}},
	}

	// Nothing changed: Terraform proposes the prior state itself.
	model := prior

	for _, phase := range []string{"plan", "re-plan"} {
		resp := runModifyPlan(t, client, model, &prior)
		if resp.Diagnostics.HasError() {
			t.Fatalf("%s: unexpected errors: %v", phase, resp.Diagnostics)
		}

		if got := plannedResourcesValue(t, resp.Plan); !got.Equal(prior.Resources) {
			t.Errorf("%s: resources = %v, want the prior value (no change)", phase, got)
		}

		assertComputedUnknown(t, resp.Plan, false)
	}
}

// TestModifyPlan_VolatileUpdate: anything that reinstalls the release
// re-renders its volatile objects, so they are Unknown; the deterministic
// ones (the Secret, whose lookup guard now finds it) stay known.
func TestModifyPlan_VolatileUpdate(t *testing.T) {
	tests := map[string]struct {
		model    func() releaseModel
		replicas int64
	}{
		"values change": {model: func() releaseModel {
			m := baseTestReleaseModel()
			m.Values = types.ListValueMust(types.StringType, []attr.Value{types.StringValue("image: {tag: \"1.28-alpine\"}")})

			return m
		}},
		// Drift outside the volatile annotations (replicas scaled by hand)
		// is still a change.
		"drift in a volatile object": {model: baseTestReleaseModel, replicas: 3},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			chart := &volatileChart{}
			prior := installedVolatileModel(t, chart)
			chart.storedPassword = base64.StdEncoding.EncodeToString([]byte("generated-000001"))
			chart.replicas = tt.replicas

			client := &fakeReleaseClient{
				renderFn:   chart.render,
				planResult: &nelmclient.PlanResult{DeployType: nelmclient.DeployTypeUpgrade, Changes: []*plan.ResourceChange{deploymentUpdate()}},
			}

			planPhase := runModifyPlan(t, client, tt.model(), &prior)
			applyPhase := runModifyPlan(t, client, tt.model(), &prior)

			if planPhase.Diagnostics.HasError() || applyPhase.Diagnostics.HasError() {
				t.Fatalf("unexpected errors: %v %v", planPhase.Diagnostics, applyPhase.Diagnostics)
			}

			planned := plannedResourcesValue(t, planPhase.Plan)

			if v := planned.Elements()[volatileDeployKey]; !v.IsUnknown() {
				t.Errorf("resources[%q] = %v, want Unknown", volatileDeployKey, v)
			}

			for _, key := range []string{volatileSecretKey, volatileCMKey} {
				if v := planned.Elements()[key]; !v.Equal(prior.Resources.Elements()[key]) {
					t.Errorf("resources[%q] = %v, want the unchanged known value", key, v)
				}
			}

			assertComputedUnknown(t, planPhase.Plan, true)
			assertCompatible(t, planned, plannedResourcesValue(t, applyPhase.Plan))
		})
	}
}

// TestModifyPlan_DiffModeNone: diff_mode = "none" renders nothing and runs
// no nelm plan (helm_release parity for charts that can never converge, e.g.
// a .Release.Revision annotation): "resources" is Unknown whenever the
// release is (re)installed and keeps its prior value otherwise.
func TestModifyPlan_DiffModeNone(t *testing.T) {
	none := func() releaseModel {
		m := baseTestReleaseModel()
		m.DiffMode = types.StringValue(diffModeNone)

		return m
	}

	t.Run("create", func(t *testing.T) {
		client := &fakeReleaseClient{renderFn: (&volatileChart{}).render}

		resp := runModifyPlan(t, client, none(), nil)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected errors: %v", resp.Diagnostics)
		}

		if got := plannedResourcesValue(t, resp.Plan); !got.IsUnknown() {
			t.Errorf("resources = %v, want Unknown", got)
		}

		assertComputedUnknown(t, resp.Plan, true)

		if client.plans != 0 || len(client.renderSpecs) != 0 {
			t.Errorf("Plan called %d times, Render %d times; want neither", client.plans, len(client.renderSpecs))
		}
	})

	t.Run("no change", func(t *testing.T) {
		client := &fakeReleaseClient{renderFn: (&volatileChart{}).render}
		prior := installedVolatileModel(t, &volatileChart{})
		prior.DiffMode = types.StringValue(diffModeNone)

		resp := runModifyPlan(t, client, prior, &prior)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected errors: %v", resp.Diagnostics)
		}

		if got := plannedResourcesValue(t, resp.Plan); !got.Equal(prior.Resources) {
			t.Errorf("resources = %v, want the prior value", got)
		}

		assertComputedUnknown(t, resp.Plan, false)

		if client.plans != 0 || len(client.renderSpecs) != 0 {
			t.Errorf("Plan called %d times, Render %d times; want neither", client.plans, len(client.renderSpecs))
		}
	})

	t.Run("values change", func(t *testing.T) {
		client := &fakeReleaseClient{renderFn: (&volatileChart{}).render}
		prior := installedVolatileModel(t, &volatileChart{})
		prior.DiffMode = types.StringValue(diffModeNone)

		model := none()
		model.Values = types.ListValueMust(types.StringType, []attr.Value{types.StringValue("replicaCount: 2")})

		resp := runModifyPlan(t, client, model, &prior)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected errors: %v", resp.Diagnostics)
		}

		if got := plannedResourcesValue(t, resp.Plan); !got.IsUnknown() {
			t.Errorf("resources = %v, want Unknown", got)
		}

		assertComputedUnknown(t, resp.Plan, true)
	})
}

// liveObject is an installed object as the API server returns it: the
// values its install rendered, plus release ownership metadata, server-side
// defaulting and runtime metadata.
func liveObject(manifest *unstructured.Unstructured) *unstructured.Unstructured {
	live := manifest.DeepCopy()
	live.SetNamespace("default")
	live.SetUID("0b6c1d2e")
	live.SetAnnotations(map[string]string{"deployment.kubernetes.io/revision": "1", "meta.helm.sh/release-name": "my-release"})
	live.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm"})
	live.Object["status"] = map[string]any{"replicas": int64(1)}
	_ = unstructured.SetNestedField(live.Object, int64(600), "spec", "progressDeadlineSeconds")
	_ = unstructured.SetNestedField(live.Object, "ClusterFirst", "spec", "template", "spec", "dnsPolicy")

	return live
}

// TestCreateOrUpdate_FillsVolatileResources: Create/Update copied a KNOWN
// plan "resources" value verbatim; with volatile objects planned Unknown it
// must read their applied value back from the cluster (state cannot hold
// Unknown), projected onto the stored release manifest so it has the chart's
// shape, and keep every known element exactly as planned.
func TestCreateOrUpdate_FillsVolatileResources(t *testing.T) {
	installedObjs := (&volatileChart{renders: 41}).render()
	manifest := installedObjs[2]
	cmManifest := installedObjs[0]

	refOf := func(obj *unstructured.Unstructured) nelmclient.ResourceRef {
		gvk := obj.GroupVersionKind()
		return nelmclient.ResourceRef{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind, Namespace: "default", Name: obj.GetName()}
	}

	info := deployedInfo(1, "deployed")
	info.Resources = []nelmclient.ResourceRef{refOf(cmManifest), refOf(manifest)}
	info.Manifests = installedObjs

	knownCM := types.StringValue(renderedMap(t, installedObjs)[volatileCMKey])

	plannedModel := func() releaseModel {
		m := plannedCreateModel()
		m.Resources = types.MapValueMust(types.StringType, map[string]attr.Value{
			volatileCMKey:     knownCM,
			volatileDeployKey: types.StringUnknown(),
		})

		return m
	}

	wantDeploy, err := planconv.NormalizeUnstructured(manifest, nil)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}

	check := func(t *testing.T, diags diag.Diagnostics, state tfsdk.State, client *fakeReleaseClient) {
		t.Helper()

		if diags.HasError() {
			t.Fatalf("unexpected errors: %v", diags)
		}

		got := stateModel(t, context.Background(), state).Resources

		if v := got.Elements()[volatileCMKey]; !v.Equal(knownCM) {
			t.Errorf("resources[%q] = %v, want the planned known value", volatileCMKey, v)
		}

		if v := got.Elements()[volatileDeployKey]; !v.Equal(types.StringValue(wantDeploy)) {
			t.Errorf("resources[%q] = %v,\nwant the live object projected onto its manifest %s", volatileDeployKey, v, wantDeploy)
		}

		if len(client.liveRefs) != 1 || len(client.liveRefs[0]) != 1 || client.liveRefs[0][0].Kind != "Deployment" {
			t.Errorf("live reads = %v, want one read of the Deployment only", client.liveRefs)
		}
	}

	newClient := func() *fakeReleaseClient {
		return &fakeReleaseClient{
			getInfo: info,
			live:    map[nelmclient.ResourceRef]*unstructured.Unstructured{refOf(manifest): liveObject(manifest)},
		}
	}

	t.Run("create", func(t *testing.T) {
		client := newClient()
		resp := runCreate(t, client, plannedModel())
		check(t, resp.Diagnostics, resp.State, client)
	})

	t.Run("update", func(t *testing.T) {
		client := newClient()
		client.history = &nelmclient.ReleaseHistory{Revision: 1, Status: "deployed", Deployed: true}
		resp := runUpdate(t, client, plannedModel(), appliedModel(1, "deployed"))
		check(t, resp.Diagnostics, resp.State, client)
	})

	t.Run("live read fails", func(t *testing.T) {
		client := newClient()
		client.liveErr = fmt.Errorf("connection reset by peer")

		resp := runCreate(t, client, plannedModel())
		if resp.Diagnostics.HasError() {
			t.Fatalf("a failed read-back must not fail the successful install: %v", resp.Diagnostics)
		}

		got := stateModel(t, context.Background(), resp.State).Resources
		if v := got.Elements()[volatileDeployKey]; !v.Equal(types.StringValue(wantDeploy)) {
			t.Errorf("resources[%q] = %v, want the stored manifest value", volatileDeployKey, v)
		}
	})
}

// TestModifyPlan_UnservedKindDegrades is the F13 regression test: a
// cluster-scoped CR whose CRD is not served yet (another release installs it
// earlier in the same apply) used to be planned under a GUESSED namespaced
// key, which the apply-phase re-plan — once the CRD exists — keys
// differently: "Provider produced inconsistent final plan". The diff is now
// computed at apply instead, with a warning; a CRD the chart itself renders
// gives the real scope, so that case stays known.
func TestModifyPlan_UnservedKindDegrades(t *testing.T) {
	issuer := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "ClusterIssuer",
		"metadata":   map[string]any{"name": "letsencrypt"},
		"spec":       map[string]any{"acme": map[string]any{}},
	}}
	issuerCRD := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "clusterissuers.cert-manager.io"},
		"spec": map[string]any{
			"group":    "cert-manager.io",
			"names":    map[string]any{"kind": "ClusterIssuer"},
			"scope":    "Cluster",
			"versions": []any{map[string]any{"name": "v1"}},
		},
	}}

	t.Run("CRD from another release", func(t *testing.T) {
		client := &fakeReleaseClient{renderObjs: []*unstructured.Unstructured{issuer}, unservedKinds: []string{"ClusterIssuer"}}

		resp := runModifyPlan(t, client, baseTestReleaseModel(), nil)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected errors: %v", resp.Diagnostics)
		}

		assertOneDiag(t, resp.Diagnostics, diag.SeverityWarning, "not served by the cluster yet")

		if !strings.Contains(resp.Diagnostics[0].Detail(), "ClusterIssuer") {
			t.Errorf("the warning does not name the kind: %s", resp.Diagnostics[0].Detail())
		}

		if got := plannedResourcesValue(t, resp.Plan); !got.IsUnknown() {
			t.Errorf("resources = %v, want Unknown (its keys are not known yet)", got)
		}

		assertComputedUnknown(t, resp.Plan, true)
	})

	t.Run("CRD in the same chart", func(t *testing.T) {
		client := &fakeReleaseClient{renderObjs: []*unstructured.Unstructured{issuerCRD, issuer}, unservedKinds: []string{"ClusterIssuer"}}

		resp := runModifyPlan(t, client, baseTestReleaseModel(), nil)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected errors: %v", resp.Diagnostics)
		}

		assertOneDiag(t, resp.Diagnostics, diag.SeverityWarning, "")

		if _, ok := plannedResources(t, resp.Plan)["cert-manager.io/v1/ClusterIssuer//letsencrypt"]; !ok {
			t.Errorf("resources = %v, want the cluster-scoped key from the chart's CRD", plannedResources(t, resp.Plan))
		}
	})
}

func TestPlanFallbackState_VolatileElements(t *testing.T) {
	plan := plannedCreateModel()
	plan.Resources = types.MapValueMust(types.StringType, map[string]attr.Value{
		volatileCMKey:     types.StringValue("{}"),
		volatileDeployKey: types.StringUnknown(),
	})

	got, diags := planFallbackState(plan)
	if diags.HasError() {
		t.Fatalf("unexpected errors: %v", diags)
	}

	want := types.MapValueMust(types.StringType, map[string]attr.Value{
		volatileCMKey:     types.StringValue("{}"),
		volatileDeployKey: types.StringValue(""),
	})

	if !got.Resources.Equal(want) {
		t.Errorf("resources = %v, want %v (state cannot hold Unknown; the next Read fills it in)", got.Resources, want)
	}
}

// TestRead_MovedVolatileReleaseConverges: right after a moved block (or an
// import) state has no resources, so the first Read used to keep each live
// object in full, server defaulting included. A volatile object then never
// matched a render outside its volatile fields, and the first plan after
// migrating such a chart reinstalled it (a rollout). Read now projects such
// objects onto the stored release manifest, and the plan is empty.
func TestRead_MovedVolatileReleaseConverges(t *testing.T) {
	ctx := context.Background()

	chart := &volatileChart{}
	installedObjs := chart.render()
	chart.storedPassword = base64.StdEncoding.EncodeToString([]byte("generated-000001"))

	info := deployedInfo(1, "deployed")
	info.Manifests = installedObjs
	live := map[nelmclient.ResourceRef]*unstructured.Unstructured{}

	for _, obj := range installedObjs {
		gvk := obj.GroupVersionKind()
		ref := nelmclient.ResourceRef{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind, Namespace: "default", Name: obj.GetName()}
		info.Resources = append(info.Resources, ref)

		live[ref] = liveObject(obj)
	}

	client := &fakeReleaseClient{
		getInfo:    info,
		live:       live,
		renderFn:   chart.render,
		planResult: &nelmclient.PlanResult{DeployType: nelmclient.DeployTypeUpgrade, Changes: []*plan.ResourceChange{deploymentUpdate()}},
	}

	moved := appliedModel(1, "deployed")
	moved.Resources = types.MapNull(types.StringType)

	readReq := resource.ReadRequest{State: buildState(t, ctx, moved)}
	readResp := &resource.ReadResponse{State: readReq.State}
	(&releaseResource{client: client}).Read(ctx, readReq, readResp)

	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read: unexpected errors: %v", readResp.Diagnostics)
	}

	refreshed := stateModel(t, ctx, readResp.State)

	want := renderedMap(t, installedObjs)
	for key, v := range refreshed.Resources.Elements() {
		if !v.Equal(types.StringValue(want[key])) {
			t.Errorf("resources[%q] = %v,\nwant the live object projected onto its manifest %s", key, v, want[key])
		}
	}

	resp := runModifyPlan(t, client, refreshed, &refreshed)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ModifyPlan: unexpected errors: %v", resp.Diagnostics)
	}

	if got := plannedResourcesValue(t, resp.Plan); !got.Equal(refreshed.Resources) {
		t.Errorf("resources = %v, want the refreshed value (an empty plan)", got)
	}

	assertComputedUnknown(t, resp.Plan, false)
}
