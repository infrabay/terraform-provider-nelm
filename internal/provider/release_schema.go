package provider

import (
	"context"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Default op timeouts (design §1.2). Applied by Phase B's release_crud.go /
// release_plan.go via releaseModel.Timeouts.{Create,Read,Update,Delete}(ctx,
// defaultXTimeout); the schema itself only declares the attributes (the
// terraform-plugin-framework-timeouts library does not support schema-level
// defaults for duration strings).
const (
	defaultCreateTimeout = 10 * time.Minute
	defaultUpdateTimeout = 10 * time.Minute
	defaultDeleteTimeout = 5 * time.Minute
	// defaultReadTimeout also bounds the ReleasePlanInstall call inside
	// ModifyPlan (design §2.2 step 4) in addition to Read's ReleaseGet.
	defaultReadTimeout = 5 * time.Minute
)

// setValueTypes are the allowed values for set/set_sensitive[*].type. The
// empty string is an alias for "auto" (nelm ValuesSet semantics).
var setValueTypes = []string{"", "auto", "string", "literal", "json"}

// releaseResourceSchema is the FROZEN Phase A schema contract for
// nelm_release (design §1.2). Every Phase B task (T-planconv, T-resplan,
// T-rescrud) codes against this file; changing it after Phase A requires
// orchestrator sign-off per CONTRACTS.md.
func releaseResourceSchema(ctx context.Context) schema.Schema {
	setNestedObject := schema.NestedAttributeObject{
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Dotted value path (e.g. \"image.tag\").",
			},
			"value": schema.StringAttribute{
				Required:    true,
				Description: "Value to set.",
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

	return schema.Schema{
		Description: "Manages a Helm/Nelm release via the Nelm Go library " +
			"(action.ReleaseInstall for apply, action.ReleasePlanInstall for diff).",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Release name.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 53),
				},
			},
			"namespace": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: `Kubernetes namespace for the release. Defaults to "default". ` +
					`Nelm's ReleaseInstall always creates the namespace if missing; there is no ` +
					`create_namespace toggle (see docs).`,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Default: stringdefault.StaticString("default"),
			},
			"chart": schema.StringAttribute{
				Required: true,
				Description: "Chart reference: local directory (absolute or relative), .tgz archive, " +
					"oci:// URL, or repo/name. Local relative paths are normalized to absolute before " +
					"every Nelm call.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"repository": schema.StringAttribute{
				Optional: true,
				Description: "Chart repository URL used to resolve a bare chart name. Private-repo " +
					"authentication is out of scope for v1.",
			},
			"version": schema.StringAttribute{
				Optional: true,
				Description: "Chart version constraint. If omitted, the latest version is used; the " +
					"resolved version surfaces in metadata.chart_version.",
			},
			"values": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "List of raw YAML documents, merged in order with later entries " +
					"overriding earlier ones (mirrors Nelm's ValuesFiles precedence). There is no " +
					"values_files attribute by design: a file-path indirection would hide value-content " +
					"changes from the Terraform diff (see CONTRACTS.md).",
			},
			"set": schema.ListNestedAttribute{
				Optional:     true,
				Description:  `Individual value overrides, applied after "values" and before set_sensitive.`,
				NestedObject: setNestedObject,
			},
			"set_sensitive": schema.ListNestedAttribute{
				Optional: true,
				Description: `Same shape as "set", but "value" is marked sensitive. Entries are ` +
					`applied AFTER "set" so they win on key conflicts.`,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Required:    true,
							Description: "Dotted value path (e.g. \"image.tag\").",
						},
						"value": schema.StringAttribute{
							Required:    true,
							Sensitive:   true,
							Description: "Sensitive value to set.",
						},
						"type": schema.StringAttribute{
							Optional: true,
							Description: `How to parse "value": "" or "auto" infers the type ` +
								`(ValuesSet), "string" forces a string (ValuesSetString), "literal" ` +
								`forces a literal string (ValuesSetLiteral), "json" parses value as ` +
								`JSON (ValuesSetJSON).`,
							Validators: []validator.String{
								stringvalidator.OneOf(setValueTypes...),
							},
						},
					},
				},
			},
			"auto_rollback": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Automatically roll back to the previous deployed release on install " +
					"failure (ReleaseInstallOptions.AutoRollback). Only works if a previous release " +
					"successfully deployed.",
			},
			"force_adoption": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Allow adopting live resources that belong to a different Helm release " +
					"or were created out-of-band (ReleaseInstallRuntimeOptions.ForceAdoption). Not " +
					"required to import plain-helm-installed releases.",
			},
			"no_remove_manual_changes": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Preserve fields manually added to live resources that are not present " +
					"in the chart manifests, instead of removing them on update.",
			},
			"no_install_crds": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: `Skip installing CustomResourceDefinitions from the chart's "crds/" ` +
					`directory.`,
			},
			"adopt_existing": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Allow Create to take over a release of the same name that already exists in " +
					"the namespace (one with a deployed revision). Defaults to false: Create then fails " +
					"instead, like helm_release without upgrade_install, so a forgotten import, a duplicate " +
					"resource, or a create_before_destroy replacement can never silently adopt (and then " +
					"uninstall) a live release. Prefer `terraform import`. A release that only has failed or " +
					"uninstalled revisions (e.g. a failed first install), or a stale pending-install left by a " +
					"killed first install, is always installed over. It does not override the pending-* lock, " +
					"and it cannot take over a release stored in the other storage backend. Only Create reads it.",
			},
			"release_history_limit": schema.Int64Attribute{
				Optional: true,
				Description: "Maximum number of release revisions kept in storage. Null or 0 uses " +
					"Nelm's own default (10). Only release metadata is pruned; cluster resources are " +
					"unaffected.",
				Validators: []validator.Int64{
					int64validator.AtLeast(0),
				},
			},
			"release_storage_driver": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("secret"),
				Description: `Where release metadata is stored: "secret", "secrets", "configmap", or ` +
					`"configmaps". An unrecognized driver string panics inside Nelm, so this attribute ` +
					`is enum-validated ("memory" and "sql" are rejected in v1). Changing it forces ` +
					`replacement: Nelm does not migrate release history between backends, so an in-place ` +
					`switch would install into an empty new backend and orphan the old release records. ` +
					`RequiresReplace makes destroy use the OLD backend and create use the new one; apply ` +
					`it destroy-first, since under create_before_destroy Create refuses to install while ` +
					`the release is still deployed in the old backend.`,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringvalidator.OneOf("secret", "secrets", "configmap", "configmaps"),
				},
			},
			"id": schema.StringAttribute{
				Computed: true,
				Description: `"<namespace>/<name>". Set to a KNOWN value during ModifyPlan (pure ` +
					`function of known Required/defaulted attributes).`,
			},
			"status": schema.StringAttribute{
				Computed: true,
				Description: "Release status as reported by the cluster (ReleaseGet). Unknown " +
					"whenever the release will be re-installed: a create, out-of-band drift, or any " +
					"change to chart/version/values/set/set_sensitive/repository (even one that " +
					"renders no manifest change, since Nelm still bumps the revision then).",
			},
			"revision": schema.Int64Attribute{
				Computed:    true,
				Description: "Release revision number. Unknown under the same condition as status.",
			},
			"metadata": schema.SingleNestedAttribute{
				Computed: true,
				Description: "Release metadata resolved from the cluster (ReleaseGet). Unknown " +
					"under the same condition as status/revision.",
				Attributes: map[string]schema.Attribute{
					"app_version": schema.StringAttribute{
						Computed:    true,
						Description: "Chart.yaml appVersion as reported by Nelm.",
					},
					"chart_name": schema.StringAttribute{
						Computed:    true,
						Description: "Resolved chart name.",
					},
					"chart_version": schema.StringAttribute{
						Computed:    true,
						Description: "Resolved chart version.",
					},
					"values_json": schema.StringAttribute{
						Computed:    true,
						Sensitive:   true,
						Description: "Canonical JSON of the coalesced values used to render the release.",
					},
				},
			},
			"resources": schema.MapAttribute{
				ElementType: types.StringType,
				Computed:    true,
				Description: `The diff surface: map of "<apiVersion>/<Kind>/<namespace>/<name>" to ` +
					`canonical, redacted JSON of the resource. Set explicitly on every ModifyPlan ` +
					`invocation, including no-change plans, so cluster drift is always visible ` +
					`(MarkComputedNilsAsUnknown is skipped on no-change plans). Secret data is ` +
					`redacted, but a sensitive value rendered into a non-Secret resource appears here ` +
					`in cleartext — see "Sensitive values in non-Secret resources" in the docs.`,
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{
				Create:            true,
				Read:              true,
				Update:            true,
				Delete:            true,
				CreateDescription: "Timeout for the install action backing Create. Defaults to 10m.",
				ReadDescription: "Timeout bounding ReleaseGet during Read AND the ReleasePlanInstall " +
					"call inside ModifyPlan. Defaults to 5m.",
				UpdateDescription: "Timeout for the install action backing Update. Defaults to 10m.",
				DeleteDescription: "Timeout for the uninstall action backing Delete. Defaults to 5m.",
			}),
		},
	}
}
