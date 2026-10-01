package provider

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/defaults"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
)

// --- parseImportID -----------------------------------------------------

func TestParseImportID(t *testing.T) {
	tests := []struct {
		name      string
		id        string
		wantNS    string
		wantName  string
		wantError bool
	}{
		{
			name:     "valid namespace/name",
			id:       "default/my-release",
			wantNS:   "default",
			wantName: "my-release",
		},
		{
			name:     "valid with dashes and dots",
			id:       "kube-system/my.release-1",
			wantNS:   "kube-system",
			wantName: "my.release-1",
		},
		{
			name:      "missing slash",
			id:        "my-release",
			wantError: true,
		},
		{
			name:      "empty string",
			id:        "",
			wantError: true,
		},
		{
			name:      "empty namespace",
			id:        "/my-release",
			wantError: true,
		},
		{
			name:      "empty name",
			id:        "default/",
			wantError: true,
		},
		{
			name:      "only a slash",
			id:        "/",
			wantError: true,
		},
		{
			// A Kubernetes/Helm release name cannot contain "/", so an ID with
			// extra slashes is malformed and must be rejected immediately (not
			// silently parsed as name="my/release").
			name:      "extra slash is rejected",
			id:        "default/my/release",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, name, err := parseImportID(tt.id)

			if tt.wantError {
				if err == nil {
					t.Fatalf("parseImportID(%q): expected error, got ns=%q name=%q", tt.id, ns, name)
				}
				return
			}

			if err != nil {
				t.Fatalf("parseImportID(%q): unexpected error: %v", tt.id, err)
			}
			if ns != tt.wantNS || name != tt.wantName {
				t.Fatalf("parseImportID(%q) = (%q, %q), want (%q, %q)", tt.id, ns, name, tt.wantNS, tt.wantName)
			}
		})
	}
}

// --- canonicalValuesJSON -------------------------------------------------

func TestCanonicalValuesJSON(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
		want   string
	}{
		{
			name:   "nil map marshals to null",
			values: nil,
			want:   "null",
		},
		{
			name:   "empty map",
			values: map[string]any{},
			want:   "{}",
		},
		{
			name: "keys are sorted regardless of insertion order",
			values: map[string]any{
				"zeta":  "z",
				"alpha": "a",
				"mid":   "m",
			},
			want: `{"alpha":"a","mid":"m","zeta":"z"}`,
		},
		{
			name: "nested maps also canonicalize key order",
			values: map[string]any{
				"outer": map[string]any{
					"b": 2,
					"a": 1,
				},
			},
			want: `{"outer":{"a":1,"b":2}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := canonicalValuesJSON(tt.values)
			if err != nil {
				t.Fatalf("canonicalValuesJSON: unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("canonicalValuesJSON = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCanonicalValuesJSON_Deterministic(t *testing.T) {
	// The same logical map, built via different insertion orders, MUST
	// canonicalize to byte-identical JSON — this is the property
	// metadata.values_json's plan/apply consistency relies on.
	a := map[string]any{}
	a["one"] = 1
	a["two"] = 2
	a["three"] = 3

	b := map[string]any{}
	b["three"] = 3
	b["one"] = 1
	b["two"] = 2

	gotA, err := canonicalValuesJSON(a)
	if err != nil {
		t.Fatalf("canonicalValuesJSON(a): %v", err)
	}
	gotB, err := canonicalValuesJSON(b)
	if err != nil {
		t.Fatalf("canonicalValuesJSON(b): %v", err)
	}

	if gotA != gotB {
		t.Fatalf("canonicalValuesJSON not deterministic across insertion order: %q != %q", gotA, gotB)
	}
}

// --- applyReleaseInfo (model<->state round-trip helper) -----------------

func TestApplyReleaseInfo(t *testing.T) {
	model := releaseModel{
		// Config-only attrs that applyReleaseInfo must NEVER touch.
		Name:       types.StringValue("my-release"),
		Namespace:  types.StringValue("default"),
		Chart:      types.StringValue("./chart"),
		Repository: types.StringValue("https://example.invalid/charts"),
		Version:    types.StringValue("1.2.3"),
	}

	info := &nelmclient.ReleaseInfo{
		Name:         "my-release",
		Namespace:    "default",
		Revision:     3,
		Status:       "deployed",
		ChartName:    "mychart",
		ChartVersion: "1.2.3",
		AppVersion:   "9.9.9",
		Values: map[string]any{
			"replicaCount": 2,
		},
	}

	diags := applyReleaseInfo(&model, info)
	if diags.HasError() {
		t.Fatalf("applyReleaseInfo: unexpected error diagnostics: %v", diags)
	}

	if got, want := model.ID.ValueString(), "default/my-release"; got != want {
		t.Errorf("ID = %q, want %q", got, want)
	}
	if got, want := model.Status.ValueString(), "deployed"; got != want {
		t.Errorf("Status = %q, want %q", got, want)
	}
	if got, want := model.Revision.ValueInt64(), int64(3); got != want {
		t.Errorf("Revision = %d, want %d", got, want)
	}

	if model.Metadata.IsNull() || model.Metadata.IsUnknown() {
		t.Fatalf("Metadata = null/unknown, want populated")
	}
	metaAttrs := model.Metadata.Attributes()
	if got, want := metaAttrs["app_version"].(types.String).ValueString(), "9.9.9"; got != want {
		t.Errorf("Metadata.app_version = %q, want %q", got, want)
	}
	if got, want := metaAttrs["chart_name"].(types.String).ValueString(), "mychart"; got != want {
		t.Errorf("Metadata.chart_name = %q, want %q", got, want)
	}
	if got, want := metaAttrs["chart_version"].(types.String).ValueString(), "1.2.3"; got != want {
		t.Errorf("Metadata.chart_version = %q, want %q", got, want)
	}
	if got, want := metaAttrs["values_json"].(types.String).ValueString(), `{"replicaCount":2}`; got != want {
		t.Errorf("Metadata.values_json = %q, want %q", got, want)
	}

	// Config-only attributes must be untouched.
	if got, want := model.Chart.ValueString(), "./chart"; got != want {
		t.Errorf("Chart was mutated: got %q, want %q", got, want)
	}
	if got, want := model.Repository.ValueString(), "https://example.invalid/charts"; got != want {
		t.Errorf("Repository was mutated: got %q, want %q", got, want)
	}
	if got, want := model.Version.ValueString(), "1.2.3"; got != want {
		t.Errorf("Version was mutated: got %q, want %q", got, want)
	}
}

func TestApplyReleaseInfo_IDDerivedFromReleaseInfoNamespace(t *testing.T) {
	// info.Namespace (not model.Namespace) drives the id, matching Read's
	// contract of deriving id from the authoritative cluster-reported
	// identity.
	model := releaseModel{
		Name:      types.StringValue("my-release"),
		Namespace: types.StringValue("default"),
	}

	info := &nelmclient.ReleaseInfo{
		Name:      "my-release",
		Namespace: "kube-system",
		Values:    map[string]any{},
	}

	diags := applyReleaseInfo(&model, info)
	if diags.HasError() {
		t.Fatalf("applyReleaseInfo: unexpected error diagnostics: %v", diags)
	}

	if got, want := model.ID.ValueString(), "kube-system/my-release"; got != want {
		t.Errorf("ID = %q, want %q", got, want)
	}
}

// --- ImportState ---------------------------------------------------------

// TestImportState_SeedsEverySchemaDefault guards ImportState's "force every
// defaulted flag to its schema default" rule: the framework does not apply
// schema defaults on import, so an attribute ImportState forgets (as `wait`
// would have been) stays null in state, and the first post-import plan shows
// a spurious `null -> default` update plus an ImportStateVerify mismatch. It
// walks the schema rather than listing attributes so a future defaulted
// attribute cannot be missed. The import ID uses the "default" namespace so
// namespace's own default is checked by the same loop.
func TestImportState_SeedsEverySchemaDefault(t *testing.T) {
	ctx := context.Background()

	sch := releaseResourceSchema(ctx)
	resp := &resource.ImportStateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}

	(&releaseResource{}).ImportState(ctx, resource.ImportStateRequest{ID: "default/my-release"}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ImportState: unexpected diagnostics: %v", resp.Diagnostics)
	}

	for name, a := range sch.Attributes {
		var want, got attr.Value

		switch a := a.(type) {
		case schema.BoolAttribute:
			if a.Default == nil {
				continue
			}

			var dr defaults.BoolResponse
			a.Default.DefaultBool(ctx, defaults.BoolRequest{}, &dr)
			want = dr.PlanValue

			var v types.Bool
			resp.Diagnostics.Append(resp.State.GetAttribute(ctx, path.Root(name), &v)...)
			got = v
		case schema.StringAttribute:
			if a.Default == nil {
				continue
			}

			var dr defaults.StringResponse
			a.Default.DefaultString(ctx, defaults.StringRequest{}, &dr)
			want = dr.PlanValue

			var v types.String
			resp.Diagnostics.Append(resp.State.GetAttribute(ctx, path.Root(name), &v)...)
			got = v
		default:
			continue
		}

		if resp.Diagnostics.HasError() {
			t.Fatalf("read imported %q: %v", name, resp.Diagnostics)
		}

		if !got.Equal(want) {
			t.Errorf("imported %q = %s, want its schema default %s", name, got, want)
		}
	}
}

// --- remainingTimeout ----------------------------------------------------

// TestRemainingTimeout pins the shared timeout budget of an apply: the
// helm_release field-manager hand-over runs first and the install gets what
// is left — never 0, which nelm would read as "no timeout" and run unbounded.
func TestRemainingTimeout(t *testing.T) {
	tests := []struct {
		name          string
		budget, spent time.Duration
		want          time.Duration
	}{
		{name: "what is left of the budget", budget: 10 * time.Minute, spent: 2 * time.Second, want: 10*time.Minute - 2*time.Second},
		{name: "exhausted budget stays bounded", budget: time.Minute, spent: time.Minute, want: time.Nanosecond},
		{name: "overspent budget stays bounded", budget: time.Minute, spent: 2 * time.Minute, want: time.Nanosecond},
		{name: "no timeout stays no timeout", budget: 0, spent: time.Minute, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := remainingTimeout(tt.budget, tt.spent); got != tt.want {
				t.Errorf("remainingTimeout(%s, %s) = %s, want %s", tt.budget, tt.spent, got, tt.want)
			}
		})
	}
}
