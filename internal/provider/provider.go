// Package provider implements the Terraform provider for Nelm
// (registry.terraform.io/infrabay/nelm). See CONTRACTS.md for the seams between
// this package, internal/nelmclient, and internal/planconv.
package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
)

// Ensure nelmProvider satisfies the expected interface.
var _ provider.Provider = &nelmProvider{}

// nelmProvider is the top-level Terraform provider implementation.
type nelmProvider struct {
	// version is set by main.go (build-time -ldflags or "dev").
	version string
}

// providerModel mirrors the schema in Schema below (design §1.1). Every
// field maps onto github.com/werf/nelm/pkg/common.KubeConnectionOptions via
// nelmclient.Config / nelmclient.Client.toKubeConnectionOptions.
type providerModel struct {
	KubeConfigPaths    types.List   `tfsdk:"kube_config_paths"`
	KubeConfigBase64   types.String `tfsdk:"kube_config_base64"`
	KubeContext        types.String `tfsdk:"kube_context"`
	KubeQPS            types.Int64  `tfsdk:"kube_qps"`
	KubeBurst          types.Int64  `tfsdk:"kube_burst"`
	KubeRequestTimeout types.String `tfsdk:"kube_request_timeout"`

	// Inline API-server connection (mirrors the kubernetes/helm providers).
	Host                 types.String `tfsdk:"host"`
	Token                types.String `tfsdk:"token"`
	ClusterCACertificate types.String `tfsdk:"cluster_ca_certificate"`
	Insecure             types.Bool   `tfsdk:"insecure"`
	TLSServerName        types.String `tfsdk:"tls_server_name"`

	// Static OCI registry credentials (mirrors the helm provider).
	Registries types.List `tfsdk:"registries"`
}

// registryAuthModel mirrors one element of the "registries" list-nested
// attribute.
type registryAuthModel struct {
	URL      types.String `tfsdk:"url"`
	Username types.String `tfsdk:"username"`
	Password types.String `tfsdk:"password"`
}

// New returns the provider constructor consumed by main.go's
// providerserver.Serve.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &nelmProvider{version: version}
	}
}

func (p *nelmProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "nelm"
	resp.Version = p.version
}

func (p *nelmProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Interacts with Kubernetes clusters through the Nelm (https://github.com/werf/nelm) Go library.",
		Attributes: map[string]schema.Attribute{
			"kube_config_paths": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: `Paths to kubeconfig files; contents are merged if more than one is given. ` +
					`Defaults to "~/.kube/config" when this and kube_config_base64 are both empty.`,
			},
			"kube_config_base64": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Base64-encoded kubeconfig content. Takes precedence over kube_config_paths.",
			},
			"kube_context": schema.StringAttribute{
				Optional:    true,
				Description: "Kubeconfig context to use.",
			},
			"kube_qps": schema.Int64Attribute{
				Optional:    true,
				Description: "Queries-per-second limit for the Kubernetes client. Nelm defaults to 30 if unset.",
				Validators: []validator.Int64{
					int64validator.AtLeast(1),
				},
			},
			"kube_burst": schema.Int64Attribute{
				Optional:    true,
				Description: "Burst limit for the Kubernetes client. Nelm defaults to 100 if unset.",
				Validators: []validator.Int64{
					int64validator.AtLeast(1),
				},
			},
			"kube_request_timeout": schema.StringAttribute{
				Optional: true,
				Description: `Timeout for individual Kubernetes API requests, as a Go duration string ` +
					`(e.g. "30s"). Unset means no timeout.`,
				Validators: []validator.String{
					durationValidator{},
				},
			},
			"host": schema.StringAttribute{
				Optional: true,
				Description: `Kubernetes API server URL, e.g. "https://10.0.0.1". Mirrors the ` +
					`kubernetes/helm providers' host. When set, host + token + cluster_ca_certificate ` +
					`form a STANDALONE connection (a complete kubeconfig is synthesized internally) that ` +
					`fully replaces kube_config_paths/kube_config_base64/kube_context — the ambient ` +
					`~/.kube/config is never read. A missing scheme defaults to https://.`,
			},
			"token": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Bearer token for the Kubernetes API (e.g. data.google_client_config.default.access_token).",
			},
			"cluster_ca_certificate": schema.StringAttribute{
				Optional: true,
				Description: "PEM-encoded root certificate bundle for the Kubernetes API server " +
					"(the decoded value, as with the kubernetes/helm providers' base64decode(...)).",
			},
			"insecure": schema.BoolAttribute{
				Optional:    true,
				Description: "Skip TLS verification of the Kubernetes API server certificate. Testing only.",
			},
			"tls_server_name": schema.StringAttribute{
				Optional:    true,
				Description: "Server name used for Kubernetes API TLS validation when it differs from the host.",
			},
			"registries": schema.ListNestedAttribute{
				Optional: true,
				Description: "Static OCI registry credentials for pulling oci:// charts (mirrors the helm " +
					"provider's `registries` block). Written to a per-operation Docker config.json and passed " +
					"to Nelm; a static entry is required because Nelm's (helm v4) OCI client does not reliably " +
					"use an external credential helper for some registries such as Google Artifact Registry.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"url": schema.StringAttribute{
							Required:    true,
							Description: `Registry URL, e.g. "oci://us-central1-docker.pkg.dev" (only the host is used).`,
						},
						"username": schema.StringAttribute{
							Required:    true,
							Description: `Registry username (for Google Artifact Registry use "oauth2accesstoken").`,
						},
						"password": schema.StringAttribute{
							Required:    true,
							Sensitive:   true,
							Description: "Registry password or access token (e.g. data.google_client_config.default.access_token).",
						},
					},
				},
			},
		},
	}
}

// Configure builds an nelmclient.Config from the provider block, performs
// the one-time process-wide Nelm bootstrap (nelmclient.Init), and stores the
// resulting *nelmclient.Client as ResourceData for every nelm_release
// resource instance.
//
// Documented v1 limitation: any Unknown provider-config value (e.g. derived
// from an unapplied resource/data source) is a hard error. Framework v1.19.0
// deferred-provider-configuration support is explicitly experimental; this
// provider does not build on it.
func (p *nelmProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var model providerModel

	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if model.KubeConfigPaths.IsUnknown() ||
		model.KubeConfigBase64.IsUnknown() ||
		model.KubeContext.IsUnknown() ||
		model.KubeQPS.IsUnknown() ||
		model.KubeBurst.IsUnknown() ||
		model.KubeRequestTimeout.IsUnknown() ||
		model.Host.IsUnknown() ||
		model.Token.IsUnknown() ||
		model.ClusterCACertificate.IsUnknown() ||
		model.Insecure.IsUnknown() ||
		model.TLSServerName.IsUnknown() ||
		model.Registries.IsUnknown() {
		resp.Diagnostics.AddError(
			"Unknown Provider Configuration Value",
			"The nelm provider configuration depends on values that are not known until apply. "+
				"This is a documented limitation in v1: provider configuration must not depend on "+
				"unapplied resource or data source attributes. (Data sources such as "+
				"google_client_config are read during plan and are fine.)",
		)
		return
	}

	cfg := nelmclient.Config{}

	if !model.KubeConfigPaths.IsNull() {
		var paths []string
		resp.Diagnostics.Append(model.KubeConfigPaths.ElementsAs(ctx, &paths, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		cfg.KubeConfigPaths = paths
	}

	if !model.KubeConfigBase64.IsNull() {
		cfg.KubeConfigBase64 = model.KubeConfigBase64.ValueString()
	}

	if !model.KubeContext.IsNull() {
		cfg.KubeContext = model.KubeContext.ValueString()
	}

	if !model.KubeQPS.IsNull() {
		cfg.QPS = model.KubeQPS.ValueInt64()
	}

	if !model.KubeBurst.IsNull() {
		cfg.Burst = model.KubeBurst.ValueInt64()
	}

	if !model.KubeRequestTimeout.IsNull() && model.KubeRequestTimeout.ValueString() != "" {
		d, err := time.ParseDuration(model.KubeRequestTimeout.ValueString())
		if err != nil {
			resp.Diagnostics.AddAttributeError(
				path.Root("kube_request_timeout"),
				"Invalid kube_request_timeout",
				fmt.Sprintf("could not parse duration: %s", err),
			)
			return
		}
		cfg.RequestTimeout = d
	}

	// The inline connection attributes are ONLY consumed together with host.
	// If any of them is set while host is null or empty, failing hard is
	// mandatory: silently ignoring them would make the provider fall back to
	// the ambient ~/.kube/config current-context — i.e. plan/apply against
	// whatever cluster the operator's kubectl happens to point at, with zero
	// warning. An empty-string host (e.g. an unset variable with a ""
	// default, or a private GKE cluster whose public_endpoint is "") is the
	// dangerous real-world shape of this, so it is called out explicitly.
	inlineAuxSet := (!model.Token.IsNull() && model.Token.ValueString() != "") ||
		(!model.ClusterCACertificate.IsNull() && model.ClusterCACertificate.ValueString() != "") ||
		(!model.Insecure.IsNull() && model.Insecure.ValueBool()) ||
		(!model.TLSServerName.IsNull() && model.TLSServerName.ValueString() != "")
	hostSet := !model.Host.IsNull() && model.Host.ValueString() != ""

	if inlineAuxSet && !hostSet {
		resp.Diagnostics.AddAttributeError(
			path.Root("host"),
			"Inline connection attributes require host",
			"token/cluster_ca_certificate/insecure/tls_server_name are only used together with a "+
				"non-empty host; without it the provider would silently fall back to the ambient "+
				"~/.kube/config current-context and could target an unintended cluster. Set host "+
				"(check that it does not evaluate to an empty string) or remove the inline attributes.",
		)
		return
	}

	// Inline host/token/cluster_ca_certificate is a STANDALONE connection
	// (like the kubernetes/helm providers): when host is set it fully replaces
	// the kubeconfig source. We synthesize a complete kubeconfig and hand it to
	// nelm as base64, so nelm never merges the ambient ~/.kube/config (whose
	// current-context could otherwise collide — e.g. a client-cert context vs
	// this bearer token).
	if hostSet {
		kubeconfig, err := nelmclient.BuildInlineKubeconfig(
			normalizeHost(model.Host.ValueString()),
			model.Token.ValueString(),
			model.ClusterCACertificate.ValueString(),
			model.Insecure.ValueBool(),
			model.TLSServerName.ValueString(),
		)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("host"), "Invalid inline Kubernetes connection", err.Error())
			return
		}

		cfg.KubeConfigBase64 = kubeconfig
		cfg.KubeConfigPaths = nil
		cfg.KubeContext = ""
	}

	if !model.Registries.IsNull() {
		var regs []registryAuthModel
		resp.Diagnostics.Append(model.Registries.ElementsAs(ctx, &regs, false)...)
		if resp.Diagnostics.HasError() {
			return
		}

		for i, r := range regs {
			if r.URL.IsUnknown() || r.Username.IsUnknown() || r.Password.IsUnknown() {
				resp.Diagnostics.AddAttributeError(
					path.Root("registries").AtListIndex(i),
					"Unknown Registry Credential",
					"registries[*].url/username/password must be known at plan time.",
				)
				return
			}

			cfg.Registries = append(cfg.Registries, nelmclient.RegistryAuth{
				URL:      r.URL.ValueString(),
				Username: r.Username.ValueString(),
				Password: r.Password.ValueString(),
			})
		}
	}

	if err := nelmclient.Init(ctx); err != nil {
		resp.Diagnostics.AddError("Nelm Bootstrap Failed", err.Error())
		return
	}

	client := nelmclient.NewClient(cfg)

	resp.ResourceData = client
	resp.DataSourceData = client
}

// normalizeHost ensures the Kubernetes API host carries a URL scheme. GKE's
// data.google_container_cluster...public_endpoint is a bare host, but nelm's
// KubeAPIServerAddress expects a full URL, so a missing scheme defaults to
// https:// (matching how the kubernetes/helm providers are typically written).
func normalizeHost(host string) string {
	if strings.Contains(host, "://") {
		return host
	}

	return "https://" + host
}

func (p *nelmProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewReleaseResource,
	}
}

func (p *nelmProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}
