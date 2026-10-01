package provider

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
)

// fakeReleaseClient is an in-memory releaseClient: canned results per method
// plus a record of what was called, so the plan and CRUD branches can be
// driven offline (no cluster, no nelm action ever runs).
type fakeReleaseClient struct {
	// configUnknown makes the fake the unknown-provider-configuration
	// placeholder (nelmclient.NewUnknownConfigClient).
	configUnknown bool

	planResult *nelmclient.PlanResult
	planErr    error
	plans      int

	renderObjs []*unstructured.Unstructured
	// renderFn, when set, answers Render instead of renderObjs: a chart
	// whose render differs from call to call (randAlphaNum, now, ...).
	renderFn    func() []*unstructured.Unstructured
	renderErr   error
	renderSpecs []nelmclient.ReleaseSpec

	// unservedKinds are kinds IsNamespaced reports as not served by the
	// cluster (their CRD is not installed).
	unservedKinds []string

	history    *nelmclient.ReleaseHistory
	historyErr error

	// historyByDriver, when set, answers History per storage driver instead
	// of history/historyErr (a driver it lacks has no release); historyErrs
	// fails History for the drivers it names. historyDrivers records the
	// driver of every History call.
	historyByDriver map[string]*nelmclient.ReleaseHistory
	historyErrs     map[string]error
	historyDrivers  []string

	installErr error
	installs   int

	getInfo *nelmclient.ReleaseInfo
	getErr  error

	live     map[nelmclient.ResourceRef]*unstructured.Unstructured
	liveErr  error
	liveRefs [][]nelmclient.ResourceRef
}

var _ releaseClient = (*fakeReleaseClient)(nil)

// IsNamespaced answers like a real RESTMapper for the kinds these tests use:
// RBAC cluster kinds, Namespace and CRDs are cluster-scoped, unservedKinds
// are not served, everything else is namespaced.
func (f *fakeReleaseClient) IsNamespaced(gvk schema.GroupVersionKind) (bool, error) {
	if slices.Contains(f.unservedKinds, gvk.Kind) {
		return false, &meta.NoKindMatchError{GroupKind: gvk.GroupKind(), SearchedVersions: []string{gvk.Version}}
	}

	switch gvk.Kind {
	case "ClusterRole", "ClusterRoleBinding", "Namespace", "CustomResourceDefinition":
		return false, nil
	default:
		return true, nil
	}
}

func (f *fakeReleaseClient) ConfigUnknown() bool {
	return f.configUnknown
}

func (f *fakeReleaseClient) Plan(context.Context, nelmclient.ReleaseSpec, time.Duration) (*nelmclient.PlanResult, error) {
	f.plans++

	if f.planErr != nil {
		return nil, f.planErr
	}

	if f.planResult == nil {
		return &nelmclient.PlanResult{DeployType: "Initial"}, nil
	}

	return f.planResult, nil
}

func (f *fakeReleaseClient) Render(_ context.Context, spec nelmclient.ReleaseSpec, _ time.Duration) ([]*unstructured.Unstructured, error) {
	f.renderSpecs = append(f.renderSpecs, spec)

	if f.renderErr != nil {
		return nil, f.renderErr
	}

	if f.renderFn != nil {
		return f.renderFn(), nil
	}

	return f.renderObjs, nil
}

func (f *fakeReleaseClient) Install(context.Context, nelmclient.ReleaseSpec, time.Duration) error {
	f.installs++

	return f.installErr
}

func (f *fakeReleaseClient) Uninstall(context.Context, string, string, string, time.Duration) error {
	return nil
}

func (f *fakeReleaseClient) Get(context.Context, string, string, string, time.Duration) (*nelmclient.ReleaseInfo, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}

	return f.getInfo, nil
}

func (f *fakeReleaseClient) History(_ context.Context, _, _, driver string, _ time.Duration) (*nelmclient.ReleaseHistory, error) {
	f.historyDrivers = append(f.historyDrivers, driver)

	if err := f.historyErrs[driver]; err != nil {
		return nil, err
	}

	h := f.history
	if f.historyByDriver != nil {
		h = f.historyByDriver[driver]
	} else if f.historyErr != nil {
		return nil, f.historyErr
	}

	if h == nil {
		return &nelmclient.ReleaseHistory{}, nil
	}

	return h, nil
}

func (f *fakeReleaseClient) LiveObjects(_ context.Context, refs []nelmclient.ResourceRef) (map[nelmclient.ResourceRef]*unstructured.Unstructured, error) {
	f.liveRefs = append(f.liveRefs, refs)

	if f.liveErr != nil {
		return nil, f.liveErr
	}

	return f.live, nil
}

// deployedInfo is a ReleaseGet result for my-release in default.
func deployedInfo(revision int, status string) *nelmclient.ReleaseInfo {
	return &nelmclient.ReleaseInfo{
		Name:         "my-release",
		Namespace:    "default",
		Revision:     revision,
		Status:       status,
		ChartName:    "chart",
		ChartVersion: "1.0.0",
		AppVersion:   "1.0.0",
		Values:       map[string]any{},
	}
}

// testResourcesMap is a KNOWN "resources" value with one entry.
func testResourcesMap() types.Map {
	return types.MapValueMust(types.StringType, map[string]attr.Value{
		"v1/ConfigMap/default/my-release": types.StringValue(`{"apiVersion":"v1","data":{"k":"v"},"kind":"ConfigMap","metadata":{"name":"my-release"}}`),
	})
}

// plannedCreateModel is a create plan as ModifyPlan leaves it: config known,
// id and resources known, status/revision/metadata Unknown.
func plannedCreateModel() releaseModel {
	m := baseTestReleaseModel()
	m.ID = types.StringValue("default/my-release")
	m.Resources = testResourcesMap()

	return m
}

// appliedModel is a fully known state of my-release (a prior state, or an
// update plan that expects no reinstall).
func appliedModel(revision int64, status string) releaseModel {
	m := plannedCreateModel()
	m.Status = types.StringValue(status)
	m.Revision = types.Int64Value(revision)
	m.Metadata = types.ObjectValueMust(metadataAttrTypes, map[string]attr.Value{
		"app_version":   types.StringValue("1.0.0"),
		"chart_name":    types.StringValue("chart"),
		"chart_version": types.StringValue("1.0.0"),
		"values_json":   types.StringValue("{}"),
	})

	return m
}

// buildState constructs a tfsdk.State for the release schema from model.
func buildState(t *testing.T, ctx context.Context, model releaseModel) tfsdk.State {
	t.Helper()

	state := tfsdk.State{Schema: releaseResourceSchema(ctx)}
	if diags := state.Set(ctx, &model); diags.HasError() {
		t.Fatalf("buildState: unexpected error diagnostics: %v", diags)
	}

	return state
}

// nullState is the empty state a create starts from.
func nullState(ctx context.Context) tfsdk.State {
	sch := releaseResourceSchema(ctx)

	return tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}
}

// stateModel reads a state back into a releaseModel.
func stateModel(t *testing.T, ctx context.Context, state tfsdk.State) releaseModel {
	t.Helper()

	var m releaseModel
	if diags := state.Get(ctx, &m); diags.HasError() {
		t.Fatalf("read state back: %v", diags)
	}

	return m
}

// diagSummaries lists the summaries of diags with the given severity.
func diagSummaries(diags diag.Diagnostics, sev diag.Severity) []string {
	var out []string

	for _, d := range diags {
		if d.Severity() == sev {
			out = append(out, d.Summary())
		}
	}

	return out
}
