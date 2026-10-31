package provider

import (
	"github.com/hashicorp/terraform-plugin-framework-timeouts/datasource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// templateModel mirrors the nelm_template schema in template_schema.go. tfsdk
// tags MUST stay in lockstep with the attribute names there.
type templateModel struct {
	// Config attributes.
	Name         types.String   `tfsdk:"name"`
	Namespace    types.String   `tfsdk:"namespace"`
	Chart        types.String   `tfsdk:"chart"`
	Repository   types.String   `tfsdk:"repository"`
	Version      types.String   `tfsdk:"version"`
	Values       types.List     `tfsdk:"values"`
	Set          []setModel     `tfsdk:"set"`
	SetSensitive []setModel     `tfsdk:"set_sensitive"`
	KubeVersion  types.String   `tfsdk:"kube_version"`
	APIVersions  types.List     `tfsdk:"api_versions"`
	IncludeCRDs  types.Bool     `tfsdk:"include_crds"`
	SkipTests    types.Bool     `tfsdk:"skip_tests"`
	ShowOnly     types.List     `tfsdk:"show_only"`
	Validate     types.Bool     `tfsdk:"validate"`
	Timeouts     timeouts.Value `tfsdk:"timeouts"`

	// Computed attributes.
	ID        types.String `tfsdk:"id"`
	Manifest  types.String `tfsdk:"manifest"`
	Manifests types.Map    `tfsdk:"manifests"`
	CRDs      types.List   `tfsdk:"crds"`
}

// releaseModel is the nelm_release model of m's chart and values inputs
// (name, namespace, chart, repository, version, values, set, set_sensitive),
// every other attribute null. The data source builds its ReleaseSpec and
// scrubs its diagnostics through it (toReleaseSpec, scrubSensitive), so a
// chart and values render, and set/set_sensitive translate, exactly as they
// do for a nelm_release.
func (m templateModel) releaseModel() releaseModel {
	return releaseModel{
		Name:         m.Name,
		Namespace:    m.Namespace,
		Chart:        m.Chart,
		Repository:   m.Repository,
		Version:      m.Version,
		Values:       m.Values,
		Set:          m.Set,
		SetSensitive: m.SetSensitive,
	}
}
