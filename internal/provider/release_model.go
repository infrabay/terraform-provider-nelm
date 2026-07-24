package provider

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
)

// scrubSensitive replaces any set_sensitive value that appears verbatim in s
// with a placeholder. Nelm echoes the raw "--set-json"/"--set" argument
// (name=value) in its parse errors (e.g. `failed parsing --set-json data
// db.password=not-json`), and Terraform's sensitivity metadata does NOT redact
// arbitrary provider error strings — so any error derived from a nelm
// plan/install call MUST be run through this before reaching a diagnostic, or a
// sensitive value leaks into `terraform plan` output and CI logs.
func (m releaseModel) scrubSensitive(s string) string {
	for _, e := range m.SetSensitive {
		if v := e.Value.ValueString(); v != "" {
			s = strings.ReplaceAll(s, v, "(sensitive value redacted)")
		}
	}

	return s
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

	chart, err := nelmclient.NormalizeChartRef(m.Chart.ValueString())
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
