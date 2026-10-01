package provider

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"

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
// Both phases must now plan the first-install render, byte-identically.
func TestModifyPlan_CreateResourcesIndependentOfLiveState(t *testing.T) {
	oldReleaseLive := &fakeReleaseClient{
		planResult: &nelmclient.PlanResult{DeployType: "Upgrade"},
		renderObjs: renderedObjects(),
	}
	afterDestroy := &fakeReleaseClient{
		planResult: &nelmclient.PlanResult{DeployType: "Initial", Changes: installCreateChanges()},
		renderObjs: renderedObjects(),
	}

	// adopt_existing only silences the (expected) existing-release warning.
	model := baseTestReleaseModel()
	model.AdoptExisting = types.BoolValue(true)

	planPhase := runModifyPlan(t, oldReleaseLive, model, nil)
	applyPhase := runModifyPlan(t, afterDestroy, model, nil)

	for name, resp := range map[string]*resource.ModifyPlanResponse{"plan phase": planPhase, "apply phase": applyPhase} {
		if resp.Diagnostics.HasError() {
			t.Fatalf("%s: unexpected errors: %v", name, resp.Diagnostics)
		}
	}

	want, err := planconv.BuildRenderedResources(renderedObjects(), "default", oldReleaseLive)
	if err != nil {
		t.Fatalf("BuildRenderedResources: %v", err)
	}

	gotPlan := plannedResources(t, planPhase.Plan)
	gotApply := plannedResources(t, applyPhase.Plan)

	if !maps.Equal(gotPlan, gotApply) {
		t.Fatalf("plan-phase and apply-phase resources differ (inconsistent final plan):\nplan:  %v\napply: %v", gotPlan, gotApply)
	}

	if !maps.Equal(gotPlan, want) {
		t.Fatalf("planned resources = %v, want the first-install render %v", gotPlan, want)
	}
}

// TestModifyPlan_CreateRendersAsFirstInstall: only a create's render ignores
// the release history (it must match what Install renders after a
// replacement's destroy); an update renders against the real history.
func TestModifyPlan_CreateRendersAsFirstInstall(t *testing.T) {
	client := &fakeReleaseClient{renderObjs: renderedObjects()}

	runModifyPlan(t, client, baseTestReleaseModel(), nil)

	prior := appliedModel(1, "deployed")
	runModifyPlan(t, client, baseTestReleaseModel(), &prior)

	if len(client.renderSpecs) != 2 {
		t.Fatalf("Render called %d times, want 2", len(client.renderSpecs))
	}

	if !client.renderSpecs[0].RenderAsFirstInstall {
		t.Error("create plan: Render must render as a first install")
	}

	if client.renderSpecs[1].RenderAsFirstInstall {
		t.Error("update plan: Render must use the release's real history")
	}
}

func adoptionConflict() error {
	return fmt.Errorf("release plan install: %w", fmt.Errorf("remotely validate resources: %w",
		errors.New(`validate adoptable resources: adopt "ClusterRole/my-release-reader": annotation "meta.helm.sh/release-namespace=old" must have value "default"`)))
}

// TestModifyPlan_CreateLiveConflictIsAdvisory: on a create (e.g. a namespace
// move whose fixed-name ClusterRole is still owned by the release being
// replaced) nelm's live adoption check only warns; the render is planned.
func TestModifyPlan_CreateLiveConflictIsAdvisory(t *testing.T) {
	client := &fakeReleaseClient{planErr: adoptionConflict(), renderObjs: renderedObjects()}

	resp := runModifyPlan(t, client, baseTestReleaseModel(), nil)

	if resp.Diagnostics.HasError() {
		t.Fatalf("a live conflict must not fail a create plan, got: %v", resp.Diagnostics)
	}

	assertOneDiag(t, resp.Diagnostics, diag.SeverityWarning, "re-checked at apply")

	if got := plannedResources(t, resp.Plan); len(got) != len(renderedObjects()) {
		t.Errorf("planned resources = %v, want the %d rendered objects", got, len(renderedObjects()))
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

	adopting := baseTestReleaseModel()
	adopting.AdoptExisting = types.BoolValue(true)
	assertOneDiag(t, runModifyPlan(t, client, adopting, nil).Diagnostics, diag.SeverityWarning, "")

	fresh := &fakeReleaseClient{planResult: &nelmclient.PlanResult{DeployType: "Initial"}, renderObjs: renderedObjects()}
	assertOneDiag(t, runModifyPlan(t, fresh, baseTestReleaseModel(), nil).Diagnostics, diag.SeverityWarning, "")
}

func TestModifyPlan_PendingReleaseWarns(t *testing.T) {
	client := &fakeReleaseClient{renderObjs: renderedObjects()}
	prior := appliedModel(7, "pending-upgrade")

	resp := runModifyPlan(t, client, baseTestReleaseModel(), &prior)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	assertOneDiag(t, resp.Diagnostics, diag.SeverityWarning, "locked by a pending operation")
}
