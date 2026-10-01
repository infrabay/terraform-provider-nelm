package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// timeoutsAttrTypes mirrors the "timeouts" block's nested attribute types
// (release_schema.go's timeouts.Block(ctx, ...)) so tests can build a null
// timeouts.Value without going through the timeouts library's own
// construction helpers.
var timeoutsAttrTypes = map[string]attr.Type{
	"create": types.StringType,
	"read":   types.StringType,
	"update": types.StringType,
	"delete": types.StringType,
}

// baseTestReleaseModel returns a releaseModel with every config attribute set
// to a concrete, KNOWN value (a legal, fully-specified nelm_release
// configuration) and every computed attribute set to a plausible
// pre-ModifyPlan placeholder (Unknown, matching what the framework's own
// MarkComputedNilsAsUnknown would have produced before ModifyPlan runs).
// Individual tests mutate a copy to introduce exactly one Unknown input.
func baseTestReleaseModel() releaseModel {
	return releaseModel{
		Name:                  types.StringValue("my-release"),
		Namespace:             types.StringValue("default"),
		Chart:                 types.StringValue("./chart"),
		Repository:            types.StringNull(),
		Version:               types.StringNull(),
		Values:                types.ListNull(types.StringType),
		Set:                   nil,
		SetSensitive:          nil,
		AutoRollback:          types.BoolValue(false),
		ForceAdoption:         types.BoolValue(false),
		NoRemoveManualChanges: types.BoolValue(false),
		NoInstallCRDs:         types.BoolValue(false),
		AdoptExisting:         types.BoolValue(false),
		DiffMode:              types.StringValue("full"),
		ReleaseHistoryLimit:   types.Int64Null(),
		ReleaseStorageDriver:  types.StringValue("secret"),
		Timeouts:              timeouts.Value{Object: types.ObjectNull(timeoutsAttrTypes)},

		ID:        types.StringUnknown(),
		Status:    types.StringUnknown(),
		Revision:  types.Int64Unknown(),
		Metadata:  types.ObjectUnknown(metadataAttrTypes),
		Resources: types.MapUnknown(types.StringType),
	}
}

// buildPlan constructs a tfsdk.Plan for the frozen release_schema.go schema
// from a releaseModel, failing the test on any conversion diagnostic.
func buildPlan(t *testing.T, ctx context.Context, model releaseModel) tfsdk.Plan {
	t.Helper()

	sch := releaseResourceSchema(ctx)
	plan := tfsdk.Plan{Schema: sch}

	diags := plan.Set(ctx, &model)
	if diags.HasError() {
		t.Fatalf("buildPlan: unexpected error diagnostics: %v", diags)
	}

	return plan
}

// --- 1. Destroy: response plan MUST stay entirely null -------------------

func TestModifyPlan_Destroy_PlanStaysNull(t *testing.T) {
	ctx := context.Background()
	sch := releaseResourceSchema(ctx)

	nullRaw := tftypes.NewValue(sch.Type().TerraformType(ctx), nil)
	nullPlan := tfsdk.Plan{Raw: nullRaw, Schema: sch}

	req := resource.ModifyPlanRequest{Plan: nullPlan}
	// Mirror the framework's own contract: ModifyPlanResponse.Plan is
	// seeded from the request before ModifyPlan is ever called
	// (server_planresourcechange.go).
	resp := &resource.ModifyPlanResponse{Plan: nullPlan}

	r := &releaseResource{}
	r.ModifyPlan(ctx, req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error diagnostics on destroy plan: %v", resp.Diagnostics)
	}

	if !resp.Plan.Raw.IsNull() {
		t.Fatalf("destroy plan: resp.Plan.Raw = %v, want null", resp.Plan.Raw)
	}
}

// --- 2. Unknown inputs degrade the whole diff surface to Unknown ---------

func assertUnknownAttr(t *testing.T, ctx context.Context, plan tfsdk.Plan, attrName string, target attr.Value) {
	t.Helper()

	diags := plan.GetAttribute(ctx, path.Root(attrName), target)
	if diags.HasError() {
		t.Fatalf("GetAttribute(%q): unexpected error diagnostics: %v", attrName, diags)
	}

	if !target.IsUnknown() {
		t.Errorf("%s = %v, want Unknown", attrName, target)
	}
}

func TestModifyPlan_UnknownInputs_DegradeToUnknown(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(m *releaseModel)
	}{
		{"chart unknown", func(m *releaseModel) { m.Chart = types.StringUnknown() }},
		{"repository unknown", func(m *releaseModel) { m.Repository = types.StringUnknown() }},
		{"version unknown", func(m *releaseModel) { m.Version = types.StringUnknown() }},
		{"name unknown", func(m *releaseModel) { m.Name = types.StringUnknown() }},
		{"namespace unknown", func(m *releaseModel) { m.Namespace = types.StringUnknown() }},
		{"release_storage_driver unknown", func(m *releaseModel) { m.ReleaseStorageDriver = types.StringUnknown() }},
		{"diff_mode unknown", func(m *releaseModel) { m.DiffMode = types.StringUnknown() }},
		{"force_adoption unknown", func(m *releaseModel) { m.ForceAdoption = types.BoolUnknown() }},
		{"no_remove_manual_changes unknown", func(m *releaseModel) { m.NoRemoveManualChanges = types.BoolUnknown() }},
		{"no_install_crds unknown", func(m *releaseModel) { m.NoInstallCRDs = types.BoolUnknown() }},
		{"values wholly unknown", func(m *releaseModel) { m.Values = types.ListUnknown(types.StringType) }},
		{
			"values element unknown",
			func(m *releaseModel) {
				m.Values = types.ListValueMust(types.StringType, []attr.Value{
					types.StringValue("replicaCount: 1"),
					types.StringUnknown(),
				})
			},
		},
		{
			"set entry value unknown",
			func(m *releaseModel) {
				m.Set = []setModel{
					{Name: types.StringValue("image.tag"), Value: types.StringUnknown(), Type: types.StringValue("")},
				}
			},
		},
		{
			"set_sensitive entry value unknown",
			func(m *releaseModel) {
				m.SetSensitive = []setModel{
					{Name: types.StringValue("db.password"), Value: types.StringUnknown(), Type: types.StringValue("")},
				}
			},
		},
		{
			"set entry name unknown (non-value field)",
			func(m *releaseModel) {
				m.Set = []setModel{
					{Name: types.StringUnknown(), Value: types.StringValue("v1"), Type: types.StringValue("")},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			model := baseTestReleaseModel()
			tt.mutate(&model)

			plan := buildPlan(t, ctx, model)

			req := resource.ModifyPlanRequest{
				Plan:  plan,
				State: tfsdk.State{Raw: tftypes.NewValue(plan.Raw.Type(), nil), Schema: plan.Schema},
			}
			resp := &resource.ModifyPlanResponse{Plan: plan}

			r := &releaseResource{}
			r.ModifyPlan(ctx, req, resp)

			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected error diagnostics: %v", resp.Diagnostics)
			}

			var resources types.Map
			assertUnknownAttr(t, ctx, resp.Plan, "resources", &resources)

			var status types.String
			assertUnknownAttr(t, ctx, resp.Plan, "status", &status)

			var revision types.Int64
			assertUnknownAttr(t, ctx, resp.Plan, "revision", &revision)

			var metadata types.Object
			assertUnknownAttr(t, ctx, resp.Plan, "metadata", &metadata)

			// id must NEVER be guessed: since we degrade before step 3
			// ever runs, it must be left exactly as it went in
			// (Unknown, per baseTestReleaseModel's placeholder).
			var id types.String
			assertUnknownAttr(t, ctx, resp.Plan, "id", &id)
		})
	}
}

// TestPlanHasUnknownInputs_WhollyUnknownNestedList is a regression test for
// the reflection hazard documented on planHasUnknownInputs: a wholly-Unknown
// "set"/"set_sensitive" list cannot be built via releaseModel + tfsdk.Plan.Set
// (a native Go slice has no way to represent "unknown length"), but Terraform
// itself CAN produce exactly that plan shape (e.g. a "set" block built from
// an unknown-at-plan-time collection expression). This constructs that raw
// shape directly and asserts planHasUnknownInputs detects it without error —
// unlike reflecting straight into releaseModel, which errors (see the
// package doc comment on planHasUnknownInputs).
func TestPlanHasUnknownInputs_WhollyUnknownNestedList(t *testing.T) {
	ctx := context.Background()

	model := baseTestReleaseModel()
	plan := buildPlan(t, ctx, model)

	objType, ok := plan.Raw.Type().(tftypes.Object)
	if !ok {
		t.Fatalf("plan raw type = %T, want tftypes.Object", plan.Raw.Type())
	}

	setType, ok := objType.AttributeTypes["set"]
	if !ok {
		t.Fatalf("schema object type has no %q attribute", "set")
	}

	var attrs map[string]tftypes.Value
	if err := plan.Raw.As(&attrs); err != nil {
		t.Fatalf("decompose plan raw value: %v", err)
	}

	attrs["set"] = tftypes.NewValue(setType, tftypes.UnknownValue)

	wholeListUnknownRaw := tftypes.NewValue(objType, attrs)
	wholeListUnknownPlan := tfsdk.Plan{Raw: wholeListUnknownRaw, Schema: plan.Schema}

	// Sanity check: reflecting this directly into releaseModel is exactly
	// the hazard planHasUnknownInputs exists to avoid.
	var m releaseModel
	if diags := wholeListUnknownPlan.Get(ctx, &m); !diags.HasError() {
		t.Fatalf("expected plan.Get into releaseModel to fail for a wholly-Unknown nested list, got no error")
	}

	unknown, diags := planHasUnknownInputs(ctx, wholeListUnknownPlan)
	if diags.HasError() {
		t.Fatalf("planHasUnknownInputs: unexpected error diagnostics: %v", diags)
	}

	if !unknown {
		t.Fatalf("planHasUnknownInputs = false, want true for a wholly-Unknown \"set\" list")
	}
}

// --- 3. id is a pure, deterministic function of namespace/name -----------

func TestReleaseID_Deterministic(t *testing.T) {
	tests := []struct {
		ns, name, want string
	}{
		{"default", "my-release", "default/my-release"},
		{"kube-system", "app", "kube-system/app"},
		{"", "name-only", "/name-only"},
	}

	for _, tt := range tests {
		got1 := releaseID(tt.ns, tt.name)
		got2 := releaseID(tt.ns, tt.name)

		if got1 != tt.want {
			t.Errorf("releaseID(%q, %q) = %q, want %q", tt.ns, tt.name, got1, tt.want)
		}

		if got1 != got2 {
			t.Errorf("releaseID(%q, %q) not deterministic: %q != %q", tt.ns, tt.name, got1, got2)
		}
	}
}

// --- containsUnknown -------------------------------------------------------

func TestContainsUnknown(t *testing.T) {
	tests := []struct {
		name string
		v    attr.Value
		want bool
	}{
		{"known string", types.StringValue("x"), false},
		{"unknown string", types.StringUnknown(), true},
		{"null string", types.StringNull(), false},
		{"known list, known elements", types.ListValueMust(types.StringType, []attr.Value{types.StringValue("a")}), false},
		{"known list, unknown element", types.ListValueMust(types.StringType, []attr.Value{types.StringUnknown()}), true},
		{"wholly unknown list", types.ListUnknown(types.StringType), true},
		{"null list", types.ListNull(types.StringType), false},
		{
			"object with unknown attribute",
			types.ObjectValueMust(
				map[string]attr.Type{"name": types.StringType, "value": types.StringType},
				map[string]attr.Value{"name": types.StringValue("n"), "value": types.StringUnknown()},
			),
			true,
		},
		{
			"object with all known attributes",
			types.ObjectValueMust(
				map[string]attr.Type{"name": types.StringType, "value": types.StringType},
				map[string]attr.Value{"name": types.StringValue("n"), "value": types.StringValue("v")},
			),
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containsUnknown(tt.v); got != tt.want {
				t.Errorf("containsUnknown(%v) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}

// --- setModelsEqual ---------------------------------------------------------

func TestSetModelsEqual(t *testing.T) {
	entry := func(name, value, typ string) setModel {
		return setModel{Name: types.StringValue(name), Value: types.StringValue(value), Type: types.StringValue(typ)}
	}

	tests := []struct {
		name string
		a, b []setModel
		want bool
	}{
		{"both nil", nil, nil, true},
		{"both empty", []setModel{}, []setModel{}, true},
		{"identical single entry", []setModel{entry("a", "1", "")}, []setModel{entry("a", "1", "")}, true},
		{"different length", []setModel{entry("a", "1", "")}, nil, false},
		{"different value", []setModel{entry("a", "1", "")}, []setModel{entry("a", "2", "")}, false},
		{"different name", []setModel{entry("a", "1", "")}, []setModel{entry("b", "1", "")}, false},
		{"different type", []setModel{entry("a", "1", "")}, []setModel{entry("a", "1", "string")}, false},
		{
			"same entries, different order (order matters)",
			[]setModel{entry("a", "1", ""), entry("b", "2", "")},
			[]setModel{entry("b", "2", ""), entry("a", "1", "")},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := setModelsEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("setModelsEqual(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// --- releaseWillReinstall (finding #1 regression) --------------------------

// TestReleaseWillReinstall is the Phase D regression test for the
// "inconsistent result after apply" bug: status/revision used to be marked
// Unknown only when nelm reported a resource-level change, so a values/version
// edit that changed the coalesced config but rendered no manifest change left
// revision KNOWN at its prior value even though the apply bumps it. This
// asserts the decision now fires for any config change, not just a
// resource-level one, while still staying false on a true no-op (so the
// no-change plan stays empty).
func TestReleaseWillReinstall(t *testing.T) {
	valuesList := func(docs ...string) types.List {
		elems := make([]attr.Value, len(docs))
		for i, d := range docs {
			elems[i] = types.StringValue(d)
		}
		return types.ListValueMust(types.StringType, elems)
	}

	base := func() releaseModel {
		m := baseTestReleaseModel()
		m.Values = valuesList("replicaCount: 1")
		return m
	}

	sameMap := map[string]string{"v1/ConfigMap/default/app": `{"data":{"k":"v"}}`}

	tests := []struct {
		name           string
		mutatePlan     func(*releaseModel)
		mutatePrior    func(*releaseModel)
		planned, prior map[string]string
		stateIsNull    bool
		want           bool
	}{
		{
			name:        "create (null prior state)",
			stateIsNull: true,
			want:        true,
		},
		{
			name:    "true no-op: identical config and resources, prior deployed",
			planned: sameMap, prior: sameMap,
			want: false,
		},
		{
			// A failed (or pending-*) prior release is ALWAYS re-installed by
			// nelm (IsReleaseUpToDate is false on status alone), so leaving
			// status/revision KNOWN would either freeze it un-retried behind
			// an empty plan or abort the next apply with "inconsistent result
			// after apply" when the retry bumps the revision.
			name:        "prior status failed forces reinstall with identical config",
			mutatePrior: func(m *releaseModel) { m.Status = types.StringValue("failed") },
			planned:     sameMap, prior: sameMap,
			want: true,
		},
		{
			name:        "prior status pending-upgrade forces reinstall",
			mutatePrior: func(m *releaseModel) { m.Status = types.StringValue("pending-upgrade") },
			planned:     sameMap, prior: sameMap,
			want: true,
		},
		{
			// The exact finding #1 repro: a values edit that alters no
			// rendered manifest (planned == prior) must still count as a
			// change, because nelm bumps the revision on the config change.
			name:       "values change with no resource change",
			mutatePlan: func(m *releaseModel) { m.Values = valuesList("replicaCount: 1", "notesOnlyValue: changed") },
			planned:    sameMap, prior: sameMap,
			want: true,
		},
		{
			name:       "version change with no resource change",
			mutatePlan: func(m *releaseModel) { m.Version = types.StringValue("2.0.0") },
			planned:    sameMap, prior: sameMap,
			want: true,
		},
		{
			// Non-surface install-affecting attributes must also count: they
			// trigger an Update whose Install may bump the revision.
			name:       "storage driver change with no resource change",
			mutatePlan: func(m *releaseModel) { m.ReleaseStorageDriver = types.StringValue("configmap") },
			planned:    sameMap, prior: sameMap,
			want: true,
		},
		{
			name:       "flag change with no resource change",
			mutatePlan: func(m *releaseModel) { m.NoInstallCRDs = types.BoolValue(true) },
			planned:    sameMap, prior: sameMap,
			want: true,
		},
		{
			// adopt_existing only matters to Create, but an edit to it makes
			// Terraform call Update, whose Install can bump the revision.
			name:       "adopt_existing change with no resource change",
			mutatePlan: func(m *releaseModel) { m.AdoptExisting = types.BoolValue(true) },
			planned:    sameMap, prior: sameMap,
			want: true,
		},
		{
			// diff_mode only matters to ModifyPlan, but an edit to it makes
			// Terraform call Update, whose Install can bump the revision.
			name:       "diff_mode change with no resource change",
			mutatePlan: func(m *releaseModel) { m.DiffMode = types.StringValue("none") },
			planned:    sameMap, prior: sameMap,
			want: true,
		},
		{
			name:    "out-of-band drift: resources differ, config identical",
			planned: map[string]string{"v1/ConfigMap/default/app": `{"data":{"k":"v"}}`},
			prior:   map[string]string{"v1/ConfigMap/default/app": `{"data":{"k":"DRIFTED"}}`},
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := base()
			if tt.mutatePlan != nil {
				tt.mutatePlan(&plan)
			}

			var prior releaseModel
			if !tt.stateIsNull {
				prior = base()
				// A realistic prior state carries a concrete deployed
				// status (state never stores Unknown).
				prior.Status = types.StringValue("deployed")
				if tt.mutatePrior != nil {
					tt.mutatePrior(&prior)
				}
			}

			got := releaseWillReinstall(plan, prior, tt.planned, tt.prior, tt.stateIsNull)
			if got != tt.want {
				t.Errorf("releaseWillReinstall = %v, want %v", got, tt.want)
			}
		})
	}
}
