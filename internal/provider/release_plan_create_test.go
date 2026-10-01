package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/werf/nelm/pkg/plan"
	"github.com/werf/nelm/pkg/resource/spec"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
	"github.com/infrabay/terraform-provider-nelm/internal/planconv"
)

// renderedObjects is what the chart renders for my-release: a namespaced
// ConfigMap and Deployment and a cluster-scoped ClusterRole, WITHOUT the
// release-ownership metadata nelm adds at install (ChartRender output).
func renderedObjects() []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "my-release"},
			"data":       map[string]any{"message": "hello"},
		}},
		{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": "my-release"},
			"spec": map[string]any{
				"replicas": int64(2),
				"template": map[string]any{"spec": map[string]any{"containers": []any{
					map[string]any{"name": "app", "image": "app:1.0.0"},
				}}},
			},
		}},
		{Object: map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "ClusterRole",
			"metadata":   map[string]any{"name": "my-release-reader"},
		}},
	}
}

// installCreateChanges is nelm's plan against an EMPTY namespace: every
// rendered object is a create whose After also carries the release-ownership
// metadata nelm stamps on install.
func installCreateChanges() []*plan.ResourceChange {
	var changes []*plan.ResourceChange

	for _, obj := range renderedObjects() {
		after := obj.DeepCopy()
		after.SetNamespace("default")
		after.SetAnnotations(map[string]string{
			"meta.helm.sh/release-name":      "my-release",
			"meta.helm.sh/release-namespace": "default",
		})
		after.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm"})

		changes = append(changes, &plan.ResourceChange{
			Type:         "create",
			ResourceMeta: &spec.ResourceMeta{Name: obj.GetName(), GroupVersionKind: obj.GroupVersionKind()},
			After:        after,
		})
	}

	return changes
}

// runModifyPlan runs ModifyPlan for model against prior (nil: a create).
func runModifyPlan(t *testing.T, client *fakeReleaseClient, model releaseModel, prior *releaseModel) *resource.ModifyPlanResponse {
	t.Helper()

	ctx := context.Background()
	p := buildPlan(t, ctx, model)

	state := nullState(ctx)
	if prior != nil {
		state = buildState(t, ctx, *prior)
	}

	resp := &resource.ModifyPlanResponse{Plan: p}
	(&releaseResource{client: client}).ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: p, State: state}, resp)

	return resp
}

// plannedResources reads the planned "resources" map out of a ModifyPlan
// response.
func plannedResources(t *testing.T, p tfsdk.Plan) map[string]string {
	t.Helper()

	ctx := context.Background()

	var m types.Map
	if diags := p.GetAttribute(ctx, path.Root("resources"), &m); diags.HasError() {
		t.Fatalf("read planned resources: %v", diags)
	}

	if m.IsUnknown() || m.IsNull() {
		t.Fatalf("planned resources = %v, want a known map", m)
	}

	out := map[string]string{}
	if diags := m.ElementsAs(ctx, &out, false); diags.HasError() {
		t.Fatalf("decode planned resources: %v", diags)
	}

	return out
}

// TestModifyPlan_CreateResourcesIndependentOfLiveState is the F03 regression
// test. Terraform plans a replacement's create twice with a null prior: at
// plan time, while the release being replaced is still live (nelm plans an
// upgrade with no changes), and again at apply after the destroy uninstalled
// it (every object is a create carrying ownership metadata). Building
// "resources" from nelm's Changes made the two disagree ({} vs everything),
// so every taint/-replace/namespace/storage-driver replacement aborted with
// "Provider produced inconsistent final plan" — after the uninstall had run.
// While the old release is live the create's map is now Unknown (a render
// that reads live objects changes once they are gone, see
// TestModifyPlan_ReplaceOfLiveRelease); the apply-phase re-plan plans the
// first-install render, and Unknown to known is allowed.
func TestModifyPlan_CreateResourcesIndependentOfLiveState(t *testing.T) {
	oldReleaseLive := &fakeReleaseClient{
		planResult: &nelmclient.PlanResult{DeployType: nelmclient.DeployTypeUpgrade},
		renderObjs: renderedObjects(),
	}
	afterDestroy := &fakeReleaseClient{
		planResult: &nelmclient.PlanResult{DeployType: nelmclient.DeployTypeInitial, Changes: installCreateChanges()},
		renderObjs: renderedObjects(),
	}

	planPhase := runModifyPlan(t, oldReleaseLive, baseTestReleaseModel(), nil)
	applyPhase := runModifyPlan(t, afterDestroy, baseTestReleaseModel(), nil)

	for name, resp := range map[string]*resource.ModifyPlanResponse{"plan phase": planPhase, "apply phase": applyPhase} {
		if resp.Diagnostics.HasError() {
			t.Fatalf("%s: unexpected errors: %v", name, resp.Diagnostics)
		}
	}

	planned := plannedResourcesValue(t, planPhase.Plan)
	if !planned.IsUnknown() {
		t.Fatalf("plan phase: resources = %v, want Unknown while the replaced release is live", planned)
	}

	assertComputedUnknown(t, planPhase.Plan, true)

	want, err := planconv.BuildRenderedResources(renderedObjects(), "default", oldReleaseLive, nil)
	if err != nil {
		t.Fatalf("BuildRenderedResources: %v", err)
	}

	if got := plannedResources(t, applyPhase.Plan); !maps.Equal(got, want) {
		t.Fatalf("apply phase: planned resources = %v, want the first-install render %v", got, want)
	}

	assertCompatible(t, planned, plannedResourcesValue(t, applyPhase.Plan))
}

// TestModifyPlan_ReplaceOfLiveRelease is the regression test for F03's
// lookup case. A chart that reads live objects (testdata/charts/volatile's
// lookup-guarded password, Bitnami's and grafana's generated secrets) renders
// deterministically at plan time, while the replaced release's Secret still
// exists, and generates a new password at the apply-time re-plan, after the
// destroy removed it. Planning the plan-time render as a known map aborted
// the replacement with "Provider produced inconsistent final plan" after the
// uninstall, however the release being replaced was live.
func TestModifyPlan_ReplaceOfLiveRelease(t *testing.T) {
	stored := base64.StdEncoding.EncodeToString([]byte("stored-password"))

	tests := map[string]struct {
		model releaseModel
		// live makes the plan-phase client see the release being replaced.
		live        func(c *fakeReleaseClient)
		wantWarning string
	}{
		"tainted release, or -replace": {
			model: baseTestReleaseModel(),
			live: func(c *fakeReleaseClient) {
				c.planResult = &nelmclient.PlanResult{DeployType: nelmclient.DeployTypeUpgrade}
			},
			wantWarning: "already exists",
		},
		"tainted after a failed first install": {
			model: baseTestReleaseModel(),
			live: func(c *fakeReleaseClient) {
				c.planResult = &nelmclient.PlanResult{DeployType: "Install"}
			},
			wantWarning: "computed at apply",
		},
		"release_storage_driver change": {
			model: func() releaseModel {
				m := baseTestReleaseModel()
				m.ReleaseStorageDriver = types.StringValue("configmap")

				return m
			}(),
			live: func(c *fakeReleaseClient) {
				c.historyByDriver = map[string]*nelmclient.ReleaseHistory{
					"secret": {Revision: 4, Status: "deployed", Deployed: true},
				}
			},
			wantWarning: "computed at apply",
		},
		"objects another release owns": {
			model: baseTestReleaseModel(),
			live: func(c *fakeReleaseClient) {
				c.planErr = adoptionConflict()
			},
			wantWarning: "re-checked at apply",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Plan phase: the old release's Secret exists, so lookup reuses its
			// password in both renders and the Secret looks deterministic.
			chart := &volatileChart{storedPassword: stored}
			planClient := &fakeReleaseClient{renderFn: chart.render}
			tt.live(planClient)

			planPhase := runModifyPlan(t, planClient, tt.model, nil)
			if planPhase.Diagnostics.HasError() {
				t.Fatalf("plan phase: unexpected errors: %v", planPhase.Diagnostics)
			}

			// Apply phase, after the destroy: no release, no Secret to look up.
			chart.storedPassword = ""
			applyPhase := runModifyPlan(t, &fakeReleaseClient{renderFn: chart.render}, tt.model, nil)
			if applyPhase.Diagnostics.HasError() {
				t.Fatalf("apply phase: unexpected errors: %v", applyPhase.Diagnostics)
			}

			applied := plannedResourcesValue(t, applyPhase.Plan)
			if v := applied.Elements()[volatileSecretKey]; v == nil || !v.IsUnknown() {
				t.Fatalf("apply phase: resources[%q] = %v, want Unknown (a first install generates the password)", volatileSecretKey, v)
			}

			assertCompatible(t, plannedResourcesValue(t, planPhase.Plan), applied)
			assertOneDiag(t, planPhase.Diagnostics, diag.SeverityWarning, tt.wantWarning)
		})
	}
}

// TestModifyPlan_FreshCreateChecksOtherBackend: a create whose release exists
// in neither backend plans the known render after checking the other backend,
// and so does one whose credentials may not read it (the apply-time re-plan
// is forbidden the same read). Any other failed read plans the map Unknown:
// the re-plan's read may succeed and find records there, and a known map
// planned here would then abort the apply ("was known, but now unknown").
func TestModifyPlan_FreshCreateChecksOtherBackend(t *testing.T) {
	tests := map[string]struct {
		err         error
		wantUnknown bool
		wantWarning string
	}{
		"no release":     {},
		"read forbidden": {err: apierrors.NewForbidden(k8sschema.GroupResource{Resource: "configmaps"}, "", errors.New("rbac"))},
		"read failed": {
			err:         errors.New("connection reset"),
			wantUnknown: true,
			wantWarning: "could not check the other storage backend",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			client := &fakeReleaseClient{renderObjs: renderedObjects(), historyErrs: map[string]error{"configmap": tt.err}}

			resp := runModifyPlan(t, client, baseTestReleaseModel(), nil)
			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected errors: %v", resp.Diagnostics)
			}

			assertOneDiag(t, resp.Diagnostics, diag.SeverityWarning, tt.wantWarning)

			if tt.wantUnknown {
				if got := plannedResourcesValue(t, resp.Plan); !got.IsUnknown() {
					t.Errorf("planned resources = %v, want Unknown (computed at apply)", got)
				}
			} else if got := plannedResources(t, resp.Plan); len(got) != len(renderedObjects()) {
				t.Errorf("planned resources = %v, want the %d rendered objects", got, len(renderedObjects()))
			}

			if !slices.Equal(client.historyDrivers, []string{"configmap"}) {
				t.Errorf("History read the %q backends, want the other one (configmap) once", client.historyDrivers)
			}
		})
	}
}

// TestModifyPlan_FreshCreateOtherBackendReadFailureIsConsistent: the read of
// the other backend fails transiently at the plan phase and succeeds at the
// apply-time re-plan, which finds the records of a failed install there and
// degrades. The plan phase must not have planned a known map.
func TestModifyPlan_FreshCreateOtherBackendReadFailureIsConsistent(t *testing.T) {
	planClient := &fakeReleaseClient{
		renderObjs:  renderedObjects(),
		historyErrs: map[string]error{"configmap": errors.New("connection reset")},
	}

	planPhase := runModifyPlan(t, planClient, baseTestReleaseModel(), nil)
	if planPhase.Diagnostics.HasError() {
		t.Fatalf("plan phase: unexpected errors: %v", planPhase.Diagnostics)
	}

	applyClient := &fakeReleaseClient{
		renderObjs:      renderedObjects(),
		historyByDriver: map[string]*nelmclient.ReleaseHistory{"configmap": {Revision: 1, Status: "failed"}},
	}

	applyPhase := runModifyPlan(t, applyClient, baseTestReleaseModel(), nil)
	if applyPhase.Diagnostics.HasError() {
		t.Fatalf("apply phase: unexpected errors: %v", applyPhase.Diagnostics)
	}

	assertCompatible(t, plannedResourcesValue(t, planPhase.Plan), plannedResourcesValue(t, applyPhase.Plan))
}

// TestModifyPlan_CreateRendersAsFirstInstall: only a create's render ignores
// the release history (it must match what Install renders after a
// replacement's destroy); an update renders against the real history. Each
// plan renders twice (the volatility probe), both times with the same spec.
func TestModifyPlan_CreateRendersAsFirstInstall(t *testing.T) {
	client := &fakeReleaseClient{renderObjs: renderedObjects()}

	runModifyPlan(t, client, baseTestReleaseModel(), nil)

	prior := appliedModel(1, "deployed")
	runModifyPlan(t, client, baseTestReleaseModel(), &prior)

	if len(client.renderSpecs) != 4 {
		t.Fatalf("Render called %d times, want 4", len(client.renderSpecs))
	}

	for i, spec := range client.renderSpecs[:2] {
		if !spec.RenderAsFirstInstall {
			t.Errorf("create plan, render %d: Render must render as a first install", i)
		}
	}

	for i, spec := range client.renderSpecs[2:] {
		if spec.RenderAsFirstInstall {
			t.Errorf("update plan, render %d: Render must use the release's real history", i)
		}
	}
}

func adoptionConflict() error {
	return fmt.Errorf("release plan install: %w", fmt.Errorf("remotely validate resources: %w",
		errors.New(`validate adoptable resources: adopt "ClusterRole/my-release-reader": annotation "meta.helm.sh/release-namespace=old" must have value "default"`)))
}

// TestModifyPlan_CreateLiveConflictIsAdvisory: on a create (e.g. a namespace
// move whose fixed-name ClusterRole is still owned by the release being
// replaced) nelm's live adoption check only warns, once the render has
// validated the chart; the objects are live, so the map is computed at apply.
func TestModifyPlan_CreateLiveConflictIsAdvisory(t *testing.T) {
	client := &fakeReleaseClient{planErr: adoptionConflict(), renderObjs: renderedObjects()}

	resp := runModifyPlan(t, client, baseTestReleaseModel(), nil)

	if resp.Diagnostics.HasError() {
		t.Fatalf("a live conflict must not fail a create plan, got: %v", resp.Diagnostics)
	}

	assertOneDiag(t, resp.Diagnostics, diag.SeverityWarning, "re-checked at apply")

	if got := plannedResourcesValue(t, resp.Plan); !got.IsUnknown() {
		t.Errorf("planned resources = %v, want Unknown (computed at apply)", got)
	}

	if len(client.renderSpecs) != 2 {
		t.Errorf("Render called %d times, want 2: the render validates the chart", len(client.renderSpecs))
	}
}

// TestModifyPlan_IdentityChangeLiveConflictIsAdvisory: Terraform core plans
// a name/namespace/release_storage_driver change FIRST with the real prior
// state and only then re-plans the create with a null prior, so the
// non-null-prior call must already treat the change as a create. A live
// conflict with the release being replaced (here a fixed-name ClusterRole
// still owned by the old namespace's release) used to fail that first call,
// so such a move could not be planned at all.
func TestModifyPlan_IdentityChangeLiveConflictIsAdvisory(t *testing.T) {
	tests := map[string]func(prior *releaseModel){
		"namespace": func(prior *releaseModel) {
			prior.Namespace = types.StringValue("ingress-old")
			prior.ID = types.StringValue("ingress-old/my-release")
		},
		"name": func(prior *releaseModel) {
			prior.Name = types.StringValue("old-release")
			prior.ID = types.StringValue("default/old-release")
		},
		"release_storage_driver": func(prior *releaseModel) {
			prior.ReleaseStorageDriver = types.StringValue("configmap")
		},
	}

	for attr, change := range tests {
		t.Run(attr, func(t *testing.T) {
			client := &fakeReleaseClient{planErr: adoptionConflict(), renderObjs: renderedObjects()}
			prior := appliedModel(3, "deployed")
			change(&prior)

			resp := runModifyPlan(t, client, baseTestReleaseModel(), &prior)

			if resp.Diagnostics.HasError() {
				t.Fatalf("a live conflict must not fail the plan of a %s change, got: %v", attr, resp.Diagnostics)
			}

			assertOneDiag(t, resp.Diagnostics, diag.SeverityWarning, "re-checked at apply")

			if got := plannedResourcesValue(t, resp.Plan); !got.IsUnknown() {
				t.Errorf("planned resources = %v, want Unknown (computed at apply)", got)
			}

			if len(client.renderSpecs) != 2 || !client.renderSpecs[0].RenderAsFirstInstall || !client.renderSpecs[1].RenderAsFirstInstall {
				t.Error("the new release must render as a first install")
			}
		})
	}
}

func TestModifyPlan_UpdateLiveConflictStillFails(t *testing.T) {
	client := &fakeReleaseClient{planErr: adoptionConflict(), renderObjs: renderedObjects()}
	prior := appliedModel(1, "deployed")

	resp := runModifyPlan(t, client, baseTestReleaseModel(), &prior)

	assertOneDiag(t, resp.Diagnostics, diag.SeverityError, "plan failed")
}

// TestModifyPlan_CreateBadChartStillFails: only live conflicts are
// advisory; a broken chart or values still fail a create plan, via Plan or
// via the render.
func TestModifyPlan_CreateBadChartStillFails(t *testing.T) {
	badValues := &fakeReleaseClient{planErr: errors.New("release plan install: render chart: failed parsing --set data")}
	assertOneDiag(t, runModifyPlan(t, badValues, baseTestReleaseModel(), nil).Diagnostics, diag.SeverityError, "plan failed")

	badRender := &fakeReleaseClient{planErr: adoptionConflict(), renderErr: errors.New("chart render: template: boom")}
	assertOneDiag(t, runModifyPlan(t, badRender, baseTestReleaseModel(), nil).Diagnostics, diag.SeverityError, "chart render")
}

// TestModifyPlan_CreateOverExistingReleaseWarns: a create targeting a
// release nelm would upgrade gets a plan-time heads-up (never an error: the
// plan may be a legitimate destroy-first replacement), unless adopt_existing
// is set.
func TestModifyPlan_CreateOverExistingReleaseWarns(t *testing.T) {
	client := &fakeReleaseClient{planResult: &nelmclient.PlanResult{DeployType: nelmclient.DeployTypeUpgrade}, renderObjs: renderedObjects()}

	resp := runModifyPlan(t, client, baseTestReleaseModel(), nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	assertOneDiag(t, resp.Diagnostics, diag.SeverityWarning, "already exists")

	// adopt_existing: no refusal to warn about, but the plan still says why
	// "resources" is known after apply.
	adopting := baseTestReleaseModel()
	adopting.AdoptExisting = types.BoolValue(true)
	assertOneDiag(t, runModifyPlan(t, client, adopting, nil).Diagnostics, diag.SeverityWarning, "computed at apply")

	fresh := &fakeReleaseClient{planResult: &nelmclient.PlanResult{DeployType: nelmclient.DeployTypeInitial}, renderObjs: renderedObjects()}
	assertOneDiag(t, runModifyPlan(t, fresh, baseTestReleaseModel(), nil).Diagnostics, diag.SeverityWarning, "")

	// A namespace move onto a release that already exists in the target
	// namespace: the destroy removes the OLD release, not that one, so the
	// create is refused at apply. Only the first, non-null-prior call's
	// warnings reach the plan output.
	moved := appliedModel(3, "deployed")
	moved.Namespace = types.StringValue("old")
	moved.ID = types.StringValue("old/my-release")
	assertOneDiag(t, runModifyPlan(t, client, baseTestReleaseModel(), &moved).Diagnostics, diag.SeverityWarning, "already exists")
}

func TestModifyPlan_PendingReleaseWarns(t *testing.T) {
	client := &fakeReleaseClient{renderObjs: renderedObjects()}
	prior := appliedModel(7, "pending-upgrade")

	resp := runModifyPlan(t, client, baseTestReleaseModel(), &prior)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	assertOneDiag(t, resp.Diagnostics, diag.SeverityWarning, "locked by a pending operation")

	// A namespace move destroys the pending release instead of updating it,
	// and destroy takes no lock: no lock warning.
	prior.Namespace = types.StringValue("old")
	prior.ID = types.StringValue("old/my-release")

	resp = runModifyPlan(t, client, baseTestReleaseModel(), &prior)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	assertOneDiag(t, resp.Diagnostics, diag.SeverityWarning, "")
}
