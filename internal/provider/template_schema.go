package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/datasource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// templateDataSourceSchema is the schema contract for the nelm_template data
// source. templateModel (template_model.go) mirrors it field-for-field; the
// chart and values inputs keep nelm_release's names, shapes and validators so
// templateModel.releaseModel can feed them to the same toReleaseSpec.
func templateDataSourceSchema(ctx context.Context) schema.Schema {
	return schema.Schema{
		Description: "Renders a chart locally like `nelm chart render` (helm_template parity): the " +
			"manifests a nelm_release of the same chart and values would install. The render is " +
			"offline unless validate is true, and works with a provider configuration that names " +
			"no cluster. The outputs are NOT sensitive: rendered Secrets and every value, " +
			"set_sensitive ones included, appear in cleartext in the plan and the state.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Release name (.Release.Name).",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 53),
				},
			},
			"namespace": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: `Release namespace (.Release.Namespace). Defaults to "default"; it must not be ` +
					`empty. It is also written into every rendered namespace-scoped object that has no ` +
					`metadata.namespace, hooks included and the objects of "crds/" directories excepted.`,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"chart": schema.StringAttribute{
				Required: true,
				Description: "Chart reference: local directory (absolute or relative), .tgz archive, " +
					"oci:// URL, or repo/name, as in nelm_release. A local directory is rendered with the " +
					"dependencies vendored in its charts/ directory only, like helm_template without " +
					"dependency_update: nothing is built or downloaded from its Chart.lock or " +
					"requirements.lock, and a dependency charts/ lacks fails the read.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"repository": schema.StringAttribute{
				Optional: true,
				Description: "Chart repository URL used to resolve a bare chart name; an oci:// URL is " +
					"joined with chart into one oci:// reference, as in nelm_release.",
			},
			"version": schema.StringAttribute{
				Optional:    true,
				Description: "Chart version constraint. If omitted, the latest version is used.",
			},
			"values": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "List of raw YAML documents, merged in order with later entries " +
					"overriding earlier ones, as in nelm_release.",
			},
			"set": schema.ListNestedAttribute{
				Optional:     true,
				Description:  `Individual value overrides, applied after "values" and before set_sensitive.`,
				NestedObject: templateSetNestedObject(false),
			},
			"set_sensitive": schema.ListNestedAttribute{
				Optional: true,
				Description: `Same shape as "set", but "value" is marked sensitive. Entries are ` +
					`applied AFTER "set" so they win on key conflicts. A value the chart renders ` +
					`still appears in cleartext in manifest, manifests and crds.`,
				NestedObject: templateSetNestedObject(true),
			},
			"kube_version": schema.StringAttribute{
				Optional: true,
				Description: `Kubernetes version the offline render reports as .Capabilities.KubeVersion ` +
					`(e.g. "1.30.0"). Defaults to nelm's offline default, "1.20.0" (helm_template's ` +
					`too). Cannot be set together with validate = true, which uses the cluster's version.`,
			},
			"api_versions": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Extra API versions added to .Capabilities.APIVersions " +
					`(e.g. "monitoring.coreos.com/v1"), with or without validate.`,
			},
			"include_crds": schema.BoolAttribute{
				Optional: true,
				Description: `Include the objects of the charts' "crds/" directories (their ` +
					`CustomResourceDefinitions) in manifest and manifests. They are always in crds.`,
			},
			"skip_tests": schema.BoolAttribute{
				Optional: true,
				Description: `Leave out test hooks (a helm.sh/hook event "test" or "test-success", ` +
					`matched like Helm: case-insensitively, surrounding spaces ignored) from manifest and ` +
					`manifests.`,
			},
			"show_only": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Only put the objects rendered from these templates into manifest and " +
					"manifests: glob patterns (Go path.Match: * never crosses a /, on every OS) on the " +
					`template path relative to the chart root, e.g. "templates/deployment.yaml", ` +
					`"templates/*.yaml" or "charts/sub/templates/*". Empty entries are ignored, as with ` +
					`helm_template. A pattern that matches no rendered template is an error.`,
			},
			"validate": schema.BoolAttribute{
				Optional: true,
				Description: "Render against the provider's cluster instead of offline, for its " +
					"capabilities (server version and served API versions) and the lookup function only. " +
					"The release history is not read: as offline and with helm_template, " +
					".Release.IsUpgrade is false and .Release.Revision 1, so the cluster needs to allow " +
					"only discovery and whatever lookup reads, not the release Secrets. It does not " +
					"validate the manifests against the cluster's OpenAPI schemas. Defaults to false, " +
					"which never contacts a cluster.",
			},
			"id": schema.StringAttribute{
				Computed:    true,
				Description: `"<namespace>/<name>".`,
			},
			"manifest": schema.StringAttribute{
				Computed: true,
				Description: "The selected objects in nelm's order (\"crds/\" objects, hooks, then the " +
					"rest, each group sorted by kind and name, not in Helm's install order), each as " +
					"`nelm chart render` prints it: \"---\", a \"# Source: <chart>/<template path>\" " +
					"line, then the object's YAML (with the release namespace restored, see namespace).",
			},
			"manifests": schema.MapAttribute{
				ElementType: types.StringType,
				Computed:    true,
				Description: "The selected objects by template path relative to the chart root " +
					`("templates/deployment.yaml", "charts/sub/templates/cm.yaml"), each value in ` +
					"manifest's format.",
			},
			"crds": schema.ListAttribute{
				ElementType: types.StringType,
				Computed:    true,
				Description: `The YAML of every object of the charts' "crds/" directories (their ` +
					`CustomResourceDefinitions), one element per object, whatever include_crds and ` +
					`show_only say.`,
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.BlockWithOpts(ctx, timeouts.Opts{
				ReadDescription: "Timeout for the read: the whole render, the remote-chart download " +
					"included. A render still running when it expires fails the read; nelm cannot stop " +
					"it, so it finishes in the background. Defaults to 5m.",
			}),
		},
	}
}

// templateSetNestedObject is the element of nelm_template's "set" and
// "set_sensitive" (sensitive), identical to nelm_release's.
func templateSetNestedObject(sensitive bool) schema.NestedAttributeObject {
	valueDescription := "Value to set."
	if sensitive {
		valueDescription = "Sensitive value to set."
	}

	return schema.NestedAttributeObject{
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Dotted value path (e.g. \"image.tag\").",
			},
			"value": schema.StringAttribute{
				Required:    true,
				Sensitive:   sensitive,
				Description: valueDescription,
			},
			"type": schema.StringAttribute{
				Optional: true,
				Description: `How to parse "value": "" or "auto" infers the type (ValuesSet), ` +
					`"string" forces a string (ValuesSetString), "literal" forces a literal string even ` +
					`if it looks like a number/bool (ValuesSetLiteral), "json" parses value as JSON ` +
					`(ValuesSetJSON).`,
				Validators: []validator.String{
					stringvalidator.OneOf(setValueTypes...),
				},
			},
		},
	}
}
