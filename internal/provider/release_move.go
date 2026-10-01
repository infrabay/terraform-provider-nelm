package provider

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.ResourceWithMoveState = &releaseResource{}

// helmProviderAddress is hashicorp/helm's provider address as Terraform
// sends it in a cross-provider move.
const helmProviderAddress = "registry.terraform.io/hashicorp/helm"

// helmReleaseState is the part of a hashicorp/helm helm_release state that
// maps onto nelm_release. The attribute names and JSON shapes are the same in
// the provider's v3 (schema version 2, framework) and v2 (SDK, where
// set/set_sensitive were blocks) releases; everything else in the source
// state is ignored. JSON nulls decode to the zero values.
type helmReleaseState struct {
	Name          string         `json:"name"`
	Namespace     string         `json:"namespace"`
	Chart         string         `json:"chart"`
	Repository    string         `json:"repository"`
	Version       string         `json:"version"`
	Values        []string       `json:"values"`
	Set           []helmSetState `json:"set"`
	SetSensitive  []helmSetState `json:"set_sensitive"`
	MaxHistory    int64          `json:"max_history"`
	Atomic        bool           `json:"atomic"`
	SkipCRDs      bool           `json:"skip_crds"`
	TakeOwnership bool           `json:"take_ownership"`
}

// helmSetState is one helm_release set/set_sensitive entry.
type helmSetState struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Type  string `json:"type"`
}

// MoveState makes `moved { from = helm_release.x  to = nelm_release.x }`
// (Terraform 1.8+) hand a release managed by hashicorp/helm over to
// nelm_release in place: the same Helm release, nothing uninstalled or
// reinstalled. The mapped state carries the release identity and the
// helm_release inputs that have a nelm_release counterpart; the computed
// attributes (status, revision, metadata, resources) are left null for the
// refresh that precedes the next plan to fill in from the cluster.
func (r *releaseResource) MoveState(context.Context) []resource.StateMover {
	return []resource.StateMover{{StateMover: moveFromHelmRelease}}
}

// moveFromHelmRelease is the helm_release StateMover. A move from any other
// source is left alone (the framework then reports that no mover handles it).
func moveFromHelmRelease(ctx context.Context, req resource.MoveStateRequest, resp *resource.MoveStateResponse) {
	if req.SourceProviderAddress != helmProviderAddress || req.SourceTypeName != "helm_release" {
		return
	}

	if req.SourceRawState == nil || req.SourceRawState.JSON == nil {
		resp.Diagnostics.AddError(
			"Unable to move helm_release state",
			"Terraform sent no JSON state for the helm_release being moved.",
		)

		return
	}

	var src helmReleaseState
	if err := json.Unmarshal(req.SourceRawState.JSON, &src); err != nil {
		resp.Diagnostics.AddError("Unable to move helm_release state", "decode helm_release state: "+err.Error())
		return
	}

	if src.Name == "" {
		resp.Diagnostics.AddError("Unable to move helm_release state", "The helm_release state has no release name.")
		return
	}

	ns := src.Namespace
	if ns == "" {
		ns = "default"
	}

	// helm_release's split OCI form (repository = "oci://host/path", chart =
	// "name") becomes nelm_release's single chart reference.
	chart, repository := src.Chart, src.Repository
	if strings.HasPrefix(repository, "oci://") && !strings.Contains(chart, "://") {
		chart = strings.TrimSuffix(repository, "/") + "/" + chart
		repository = ""
	}

	// helm_release's version is Computed: for a local chart it holds the
	// chart's own Chart.yaml version even though no configuration sets it,
	// so carrying it over would plan a spurious change. A remote chart's
	// version is the pinned (or last resolved) one and is kept.
	version := src.Version
	if isLocalChartPath(chart, repository) {
		version = ""
	}

	values := types.ListNull(types.StringType)
	if len(src.Values) > 0 {
		elems := make([]attr.Value, len(src.Values))
		for i, v := range src.Values {
			elems[i] = types.StringValue(v)
		}

		values = types.ListValueMust(types.StringType, elems)
	}

	historyLimit := types.Int64Null()
	if src.MaxHistory > 0 {
		historyLimit = types.Int64Value(src.MaxHistory)
	}

	// Every other attribute stays null: the TargetState starts as a null
	// object of the nelm_release schema.
	attrs := map[string]any{
		"id":                       releaseID(ns, src.Name),
		"name":                     src.Name,
		"namespace":                ns,
		"chart":                    chart,
		"repository":               optionalString(repository),
		"version":                  optionalString(version),
		"values":                   values,
		"set":                      movedSetEntries(src.Set),
		"set_sensitive":            movedSetEntries(src.SetSensitive),
		"auto_rollback":            src.Atomic,
		"force_adoption":           src.TakeOwnership,
		"no_remove_manual_changes": false,
		"no_install_crds":          src.SkipCRDs,
		"adopt_existing":           false,
		"diff_mode":                diffModeFull,
		"release_history_limit":    historyLimit,
		// helm_release's storage backend is a provider-level setting
		// (helm_driver) that its state does not record; "secret" is Helm's
		// default, as on import.
		"release_storage_driver": "secret",
	}

	for name, value := range attrs {
		resp.Diagnostics.Append(resp.TargetState.SetAttribute(ctx, path.Root(name), value)...)
	}
}

// isLocalChartPath reports whether chart is a local chart directory or
// archive path (absolute, "./"/"../"-relative, or a .tgz) rather than a
// remote reference — the same signals nelm's own local-chart classification
// and nelmclient.NormalizeChartRef use.
func isLocalChartPath(chart, repository string) bool {
	if repository != "" || strings.Contains(chart, "://") {
		return false
	}

	return filepath.IsAbs(chart) || chart == "." || chart == ".." ||
		strings.HasPrefix(chart, "./") || strings.HasPrefix(chart, "../") ||
		strings.HasSuffix(chart, ".tgz")
}

// optionalString maps helm_release's empty optional strings to null, which
// is what a nelm_release configuration that omits the attribute plans.
func optionalString(s string) types.String {
	if s == "" {
		return types.StringNull()
	}

	return types.StringValue(s)
}

// movedSetEntries converts helm_release set/set_sensitive entries; an empty
// list becomes null and helm's default type "" becomes an omitted (null)
// type, matching a nelm_release configuration that leaves them out.
func movedSetEntries(entries []helmSetState) []setModel {
	if len(entries) == 0 {
		return nil
	}

	out := make([]setModel, len(entries))
	for i, e := range entries {
		out[i] = setModel{
			Name:  types.StringValue(e.Name),
			Value: types.StringValue(e.Value),
			Type:  optionalString(e.Type),
		}
	}

	return out
}
