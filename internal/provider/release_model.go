package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
	"github.com/infrabay/terraform-provider-nelm/internal/planconv"
)

// scrubSensitive replaces any set_sensitive value (sensitiveValues) that
// appears in s with a placeholder. Nelm echoes the raw "--set-json"/"--set"
// argument (name=value) in its parse errors (e.g. `failed parsing --set-json
// data db.password=not-json`), and Terraform's sensitivity metadata does NOT
// redact arbitrary provider error strings — so any error derived from a nelm
// plan/install call MUST be run through this before reaching a diagnostic, or
// a sensitive value leaks into `terraform plan` output and CI logs. Values
// are replaced in one pass (planconv.ScrubString): a value that contains
// another one is replaced whole, whatever their order in set_sensitive.
func (m releaseModel) scrubSensitive(s string) string {
	return planconv.ScrubString(s, m.sensitiveValues(), func(string) string { return "(sensitive value redacted)" })
}

// sensitiveValues returns the strings a set_sensitive value can surface as in
// nelm's output — an error, a rendered manifest: each value as written, the
// strings nelm parses out of it (nelmclient.SetValueStrings: escapes
// resolved, lists split, JSON decoded), and each of those as a template embeds
// it with quote, toJson or b64enc. These are the secrets scrubbed from both
// sides of the resources diff (planconv.ScrubSecrets) and from diagnostics.
// A string shorter than planconv.MinSecretLength gets no derived forms (they
// would match all over a manifest); the value as written is still scrubbed
// from diagnostics.
func (m releaseModel) sensitiveValues() []string {
	var out []string

	for _, e := range m.SetSensitive {
		v := e.Value.ValueString()
		if v == "" {
			continue
		}

		out = append(out, v)

		parsed := nelmclient.SetValueStrings(e.Type.ValueString(), e.Name.ValueString()+"="+v)
		for _, s := range append([]string{v}, parsed...) {
			if len(s) < planconv.MinSecretLength {
				continue
			}

			quoted := strconv.Quote(s)
			jsonQuoted, _ := json.Marshal(s)

			out = append(out,
				s,
				quoted[1:len(quoted)-1],
				string(jsonQuoted[1:len(jsonQuoted)-1]),
				base64.StdEncoding.EncodeToString([]byte(s)),
			)
		}
	}

	slices.Sort(out)

	return slices.Compact(out)
}

// setModel mirrors the nested object shared by the "set" and "set_sensitive"
// list-nested attributes in release_schema.go.
type setModel struct {
	Name  types.String `tfsdk:"name"`
	Value types.String `tfsdk:"value"`
	Type  types.String `tfsdk:"type"`
}

// releaseModel mirrors the nelm_release schema in release_schema.go.
// tfsdk tags MUST stay in lockstep with the attribute names there; this is
// the frozen Phase A contract every Phase B task (T-resplan, T-rescrud)
// codes against (CONTRACTS.md).
type releaseModel struct {
	// Config attributes.
	Name                  types.String   `tfsdk:"name"`
	Namespace             types.String   `tfsdk:"namespace"`
	Chart                 types.String   `tfsdk:"chart"`
	Repository            types.String   `tfsdk:"repository"`
	Version               types.String   `tfsdk:"version"`
	Values                types.List     `tfsdk:"values"`
	Set                   []setModel     `tfsdk:"set"`
	SetSensitive          []setModel     `tfsdk:"set_sensitive"`
	AutoRollback          types.Bool     `tfsdk:"auto_rollback"`
	ForceAdoption         types.Bool     `tfsdk:"force_adoption"`
	NoRemoveManualChanges types.Bool     `tfsdk:"no_remove_manual_changes"`
	NoInstallCRDs         types.Bool     `tfsdk:"no_install_crds"`
	AdoptExisting         types.Bool     `tfsdk:"adopt_existing"`
	DiffMode              types.String   `tfsdk:"diff_mode"`
	ReleaseHistoryLimit   types.Int64    `tfsdk:"release_history_limit"`
	ReleaseStorageDriver  types.String   `tfsdk:"release_storage_driver"`
	Timeouts              timeouts.Value `tfsdk:"timeouts"`

	// Computed attributes (the diff surface + status).
	ID       types.String `tfsdk:"id"`
	Status   types.String `tfsdk:"status"`
	Revision types.Int64  `tfsdk:"revision"`
	// Metadata is a types.Object (not a nested Go struct pointer) because
	// ModifyPlan sets it to types.ObjectUnknown on create/change, and a Go
	// struct pointer cannot hold an unknown value — the framework errors "the
	// target type cannot handle unknown values" when reading such a plan back
	// into the model. metadataAttrTypes (release_plan.go) describes its shape.
	Metadata  types.Object `tfsdk:"metadata"`
	Resources types.Map    `tfsdk:"resources"`
}

// toReleaseSpec translates the config side of releaseModel into a
// nelmclient.ReleaseSpec. It is the SHARED seam between T-resplan (ModifyPlan)
// and T-rescrud (Create/Update): both build the spec through this one helper so
// plan-time and apply-time produce byte-identical inputs to nelm (required by
// the ModifyPlan consistency rule). The chart reference is normalized here
// (local refs -> absolute; remote refs pass through) so every call path applies
// the rule identically. Callers MUST first ensure the config attributes this
// reads are known (not Unknown) — ModifyPlan degrades to Unknown before calling
// this (design §2.2 step 2).
func (m releaseModel) toReleaseSpec(ctx context.Context) (nelmclient.ReleaseSpec, diag.Diagnostics) {
	var diags diag.Diagnostics

	spec := nelmclient.ReleaseSpec{
		Name:                  m.Name.ValueString(),
		Namespace:             m.Namespace.ValueString(),
		Repository:            m.Repository.ValueString(),
		Version:               m.Version.ValueString(),
		StorageDriver:         m.ReleaseStorageDriver.ValueString(),
		ForceAdoption:         m.ForceAdoption.ValueBool(),
		NoRemoveManualChanges: m.NoRemoveManualChanges.ValueBool(),
		NoInstallCRDs:         m.NoInstallCRDs.ValueBool(),
		AutoRollback:          m.AutoRollback.ValueBool(),
	}

	if !m.ReleaseHistoryLimit.IsNull() && !m.ReleaseHistoryLimit.IsUnknown() {
		spec.HistoryLimit = int(m.ReleaseHistoryLimit.ValueInt64())
	}

	chart, err := nelmclient.NormalizeChartRef(m.Chart.ValueString(), m.Repository.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("chart"), "Invalid chart reference", err.Error())
	}
	spec.Chart = chart

	if !m.Values.IsNull() && !m.Values.IsUnknown() {
		var vals []string
		diags.Append(m.Values.ElementsAs(ctx, &vals, false)...)
		spec.ValuesYAML = vals
	}

	// set_sensitive must WIN on key conflicts (the schema documents this
	// unconditionally). Appending set_sensitive after set is NOT enough:
	// nelm merges the four --set* categories in a FIXED order (json, set,
	// string, literal — last writer wins per key), independent of the order we
	// append within a category, so a non-sensitive `set` entry whose type
	// sorts into a later category (e.g. type="literal") would otherwise
	// override a set_sensitive entry of an earlier category and silently leak
	// the public value in place of the secret. To honor the contract for ANY
	// type pairing, drop every non-sensitive `set` entry whose name also
	// appears in set_sensitive: that makes the set_sensitive entry the sole
	// writer for the key, so it wins regardless of nelm's category order.
	sensitiveNames := make(map[string]struct{}, len(m.SetSensitive))
	for _, e := range m.SetSensitive {
		sensitiveNames[e.Name.ValueString()] = struct{}{}
	}

	appendSets := func(entries []setModel, dropSensitiveConflicts bool) {
		for _, e := range entries {
			name := e.Name.ValueString()
			if dropSensitiveConflicts {
				if _, clash := sensitiveNames[name]; clash {
					continue
				}
			}

			pair := name + "=" + e.Value.ValueString()
			switch e.Type.ValueString() {
			case "string":
				spec.SetString = append(spec.SetString, pair)
			case "literal":
				spec.SetLiteral = append(spec.SetLiteral, pair)
			case "json":
				spec.SetJSON = append(spec.SetJSON, pair)
			default: // "" or "auto"
				spec.Set = append(spec.Set, pair)
			}
		}
	}
	appendSets(m.Set, true)
	appendSets(m.SetSensitive, false)

	return spec, diags
}
