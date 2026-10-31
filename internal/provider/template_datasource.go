package provider

import (
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
	"github.com/infrabay/terraform-provider-nelm/internal/planconv"
)

// templateClient is the subset of *nelmclient.Client that templateDataSource
// calls. Configure always stores a *nelmclient.Client; the interface exists so
// Read can be unit-tested offline against a fake.
//
// Its planconv.KeyScoper is the cluster's RESTMapper, which scopes the
// release namespace Read restores after a validate = true render. An offline
// render never asks it (nor ResetKindCache): on a client that has a cluster
// it would contact it.
type templateClient interface {
	planconv.KeyScoper

	Template(ctx context.Context, spec nelmclient.ReleaseSpec, opts nelmclient.TemplateOptions, timeout time.Duration) ([]nelmclient.TemplateObject, error)

	// NoConnection reports the nelmclient.NewNoConnectionClient client (the
	// provider configuration names no cluster): the error a validate = true
	// render then fails with.
	NoConnection() error

	// ResetKindCache makes the next IsNamespaced discover the cluster's
	// kinds again: the RESTMapper is cached for the life of the provider
	// process, shared with nelm_release, and can predate a CRD an earlier
	// apply in the same run installed.
	ResetKindCache()
}

var _ templateClient = (*nelmclient.Client)(nil)

// templateDataSource implements the nelm_template data source: an offline
// (or, with validate, cluster-backed) chart render, helm_template parity.
type templateDataSource struct {
	client templateClient
}

var (
	_ datasource.DataSource                   = &templateDataSource{}
	_ datasource.DataSourceWithConfigure      = &templateDataSource{}
	_ datasource.DataSourceWithValidateConfig = &templateDataSource{}
)

// NewTemplateDataSource is the data source constructor registered in
// provider.go's DataSources().
func NewTemplateDataSource() datasource.DataSource {
	return &templateDataSource{}
}

func (d *templateDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_template"
}

func (d *templateDataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = templateDataSourceSchema(ctx)
}

func (d *templateDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	// ProviderData is nil until the provider's own Configure has run (e.g.
	// during ValidateDataSourceConfig), so this must be a no-op rather than an
	// error in that case.
	if req.ProviderData == nil {
		return
	}

	client, ok := req.ProviderData.(*nelmclient.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Data Source Configure Type",
			fmt.Sprintf("Expected *nelmclient.Client, got: %T. This is a provider bug; please report it.", req.ProviderData),
		)
		return
	}

	d.client = client
}

// ValidateConfig rejects kube_version together with validate = true: a
// cluster-backed render sees the cluster's real version, so kube_version
// would be silently ignored. An unknown value of either is skipped here, and
// Read does not check again: Terraform core re-runs ValidateDataResourceConfig
// with the known configuration right before every ReadDataSource, so the
// conflict is still reported once both values are known.
func (d *templateDataSource) ValidateConfig(ctx context.Context, req datasource.ValidateConfigRequest, resp *datasource.ValidateConfigResponse) {
	var kubeVersion types.String
	var validate types.Bool

	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("kube_version"), &kubeVersion)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("validate"), &validate)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if kubeVersion.IsUnknown() || validate.IsUnknown() {
		return
	}

	if kubeVersion.ValueString() != "" && validate.ValueBool() {
		resp.Diagnostics.AddAttributeError(
			path.Root("kube_version"),
			"Invalid nelm_template kube_version",
			"kube_version sets the Kubernetes version of an offline render, but validate = true renders "+
				"against the cluster, which reports its own. Remove kube_version or set validate = false.",
		)
	}
}

func (d *templateDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var model templateModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if d.client == nil {
		resp.Diagnostics.AddError(
			"Unconfigured nelm provider",
			"nelm_template was read before the nelm provider was configured. This is a provider bug; please report it.",
		)
		return
	}

	readTimeout, diags := model.Timeouts.Read(ctx, defaultReadTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	if model.Namespace.IsNull() {
		model.Namespace = types.StringValue("default")
	}

	// The chart and values inputs go through nelm_release's own model, so
	// they translate exactly as a release's do; its scrubSensitive guards
	// every diagnostic derived from nelm (nelm echoes --set arguments).
	release := model.releaseModel()

	spec, diags := release.toReleaseSpec(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	opts := nelmclient.TemplateOptions{
		Remote:      model.Validate.ValueBool(),
		KubeVersion: model.KubeVersion.ValueString(),
	}

	if !model.APIVersions.IsNull() {
		resp.Diagnostics.Append(model.APIVersions.ElementsAs(ctx, &opts.APIVersions, false)...)
	}

	// show_only keeps a null element as "" so each pattern keeps its list
	// index for diagnostics; selectObjects ignores it, as helm_template does.
	var showOnly []string
	if !model.ShowOnly.IsNull() {
		var elems []types.String
		resp.Diagnostics.Append(model.ShowOnly.ElementsAs(ctx, &elems, false)...)

		for _, elem := range elems {
			showOnly = append(showOnly, elem.ValueString())
		}
	}

	if resp.Diagnostics.HasError() {
		return
	}

	objs, err := d.client.Template(ctx, spec, opts, readTimeout)
	if err != nil {
		resp.Diagnostics.Append(d.templateErrDiags(err, opts.Remote, release)...)
		return
	}

	filter := templateFilter{
		includeCRDs: model.IncludeCRDs.ValueBool(),
		skipTests:   model.SkipTests.ValueBool(),
		showOnly:    showOnly,
	}

	selected, diags := filter.selectObjects(objs)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// nelm strips the release namespace from the objects it renders; put it
	// back on the selected ones whose kind is namespace-scoped. The filters
	// do not look at namespaces, so restoring after them changes no
	// selection, and kinds show_only leaves out are neither looked up nor
	// warned about.
	scoper := d.namespaceScoper(objs, opts.Remote)

	selected, diags = restoreReleaseNamespace(selected, spec.Namespace, scoper, release)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	if unresolved := scoper.Unresolved(); len(unresolved) > 0 {
		resp.Diagnostics.Append(unresolvedNamespaceWarning(unresolved, spec.Namespace, opts.Remote, release))
	}

	manifest, manifests := templateManifests(selected)

	model.ID = types.StringValue(model.Namespace.ValueString() + "/" + model.Name.ValueString())
	model.Manifest = types.StringValue(manifest)

	model.Manifests, diags = types.MapValueFrom(ctx, types.StringType, manifests)
	resp.Diagnostics.Append(diags...)

	model.CRDs, diags = types.ListValueFrom(ctx, types.StringType, templateCRDs(objs))
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

// templateErrDiags turns a failed Template call into diagnostics. The
// placeholder clients' refusals (no cluster configured, configuration not
// known yet) get data-source wording; anything else came from nelm and is
// scrubbed of set_sensitive values (release.scrubSensitive).
func (d *templateDataSource) templateErrDiags(err error, remote bool, release releaseModel) diag.Diagnostics {
	var diags diag.Diagnostics

	noConn := d.client.NoConnection()

	switch {
	case remote && noConn != nil && errors.Is(err, noConn):
		diags.AddAttributeError(
			path.Root("validate"),
			"No Kubernetes connection configured",
			"nelm_template with validate = true renders against the provider's cluster. "+noConn.Error()+
				"\n\nWith validate = false (the default) the chart is rendered offline and needs no cluster.",
		)
	case remote && errors.Is(err, nelmclient.ErrConfigUnknown):
		diags.AddAttributeError(
			path.Root("validate"),
			"Provider configuration not known at plan time",
			"nelm_template with validate = true renders against the provider's cluster, but the nelm "+
				"provider configuration depends on values that are only known after apply (for example a "+
				"cluster created in this run), so there is no cluster to render against yet. Apply the "+
				"cluster change first (terraform apply -target=...), keep the cluster and what reads it in "+
				"separate root modules, or render offline with validate = false.",
		)
	case errors.Is(err, nelmclient.ErrConfigUnknown):
		diags.AddError(
			"Provider configuration not known at plan time",
			"nelm_template renders offline, but pulls an oci:// chart with the nelm provider's registries "+
				"credentials, and those depend on values that are only known after apply. Make registries "+
				"independent of resources created in this run, or apply those first (terraform apply "+
				"-target=...). A local chart or a chart from a classic https repository needs no registries "+
				"credentials.",
		)
	case errors.Is(err, nelmclient.ErrChartDependenciesMissing):
		diags.AddAttributeError(
			path.Root("chart"),
			"Missing chart dependencies",
			release.scrubSensitive(err.Error())+"\n\nnelm_template renders a local chart only with the "+
				"dependencies vendored in its charts/ directory, as an archive or an unpacked chart, like "+
				"helm_template without dependency_update: it never builds or downloads a dependency from "+
				"Chart.lock or requirements.lock (nelm_release does build locked dependencies). Vendor them "+
				"first, for example with `helm dependency build`, and commit charts/ or build it before "+
				"terraform runs.",
		)
	default:
		diags.AddError("nelm_template: chart render failed", release.scrubSensitive(err.Error()))
	}

	return diags
}

// namespaceScoper is the KeyScoper restoreReleaseNamespace scopes a render's
// objects with: the cluster's RESTMapper (d.client), refreshed first, for a
// validate = true render, or the built-in Kubernetes kinds
// (planconv.BuiltinScoper) for an offline one, never the client then. A kind
// that one does not know takes its scope from the CustomResourceDefinitions
// in the render (objs, crds/ included), and is otherwise recorded as
// Unresolved.
func (d *templateDataSource) namespaceScoper(objs []nelmclient.TemplateObject, remote bool) *planconv.RenderScoper {
	var scoper planconv.KeyScoper = planconv.BuiltinScoper{}
	if remote {
		// nelm built a fresh kube client for the render, while the client's
		// RESTMapper may predate a CRD installed earlier in this run: a kind
		// the render's lookup and .Capabilities saw must be served here too.
		d.client.ResetKindCache()
		scoper = d.client
	}

	render := make([]*unstructured.Unstructured, 0, len(objs))
	for _, obj := range objs {
		render = append(render, obj.Object)
	}

	return planconv.NewRenderScoper(render, scoper)
}

// restoreReleaseNamespace returns objs with metadata.namespace set to
// namespace on every object that has none and whose kind scoper reports as
// namespace-scoped, hooks included. nelm removes a namespace equal to the
// release namespace from every object it renders, so an object the chart put
// in the release namespace would otherwise come out without one, and
// kubernetes_manifest or kubectl apply would create it in their own default
// namespace. An object the chart wrote without a namespace gets it too, as
// nelm installs it there: this is where the output deliberately differs from
// helm_template, which keeps only a namespace the template wrote.
//
// Objects from crds/ directories (the crds output too) stay as nelm renders
// them, as do objects with a namespace and those of cluster-scoped kinds. A
// changed object is a copy, re-encoded with nelmclient.ManifestYAML; objs
// itself is not modified. An object of a kind scoper cannot resolve keeps no
// namespace (scoper.Unresolved lists the kind); any other scoper failure (the
// cluster's RESTMapper) is an error, scrubbed of set_sensitive values.
func restoreReleaseNamespace(objs []nelmclient.TemplateObject, namespace string, scoper *planconv.RenderScoper, release releaseModel) ([]nelmclient.TemplateObject, diag.Diagnostics) {
	var diags diag.Diagnostics

	out := slices.Clone(objs)

	for i, obj := range out {
		if obj.StandaloneCRD || obj.Object == nil || obj.Object.GetNamespace() != "" {
			continue
		}

		gvk := obj.Object.GroupVersionKind()

		namespaced, err := scoper.IsNamespaced(gvk)

		var noMatch *meta.NoKindMatchError

		switch {
		case errors.As(err, &noMatch):
			// Unresolved: left without a namespace, and warned about.
			continue
		case err != nil:
			diags.AddError(
				"nelm_template: failed to look up a resource kind",
				release.scrubSensitive(fmt.Sprintf("Could not tell whether %s is namespace-scoped, to set the release "+
					"namespace on %s %q (%s): %s", gvk, gvk.Kind, obj.Object.GetName(), obj.Path, err)),
			)

			return nil, diags
		case !namespaced:
			continue
		}

		restored := obj.Object.DeepCopy()
		restored.SetNamespace(namespace)

		y, err := nelmclient.ManifestYAML(restored)
		if err != nil {
			diags.AddError(
				"nelm_template: failed to encode a rendered object",
				release.scrubSensitive(fmt.Sprintf("Could not encode %s %q from %s with the release namespace set: %s",
					gvk.Kind, restored.GetName(), obj.Path, err)),
			)

			return nil, diags
		}

		out[i].Object = restored
		out[i].YAML = y
	}

	return out, diags
}

// unresolvedNamespaceWarning is the one warning for the kinds whose release
// namespace restoreReleaseNamespace could not restore: their objects come
// out without metadata.namespace. Like every diagnostic of the data source
// it is scrubbed of set_sensitive values (release.scrubSensitive).
func unresolvedNamespaceWarning(unresolved []schema.GroupVersionKind, namespace string, remote bool, release releaseModel) diag.Diagnostic {
	why := "These kinds are neither built into Kubernetes nor defined by a CustomResourceDefinition in the chart, " +
		"so an offline render cannot tell whether they are namespace-scoped"
	fix := fmt.Sprintf("Set validate = true to look them up in the cluster, or set metadata.namespace = %q where the "+
		"manifests are applied.", namespace)

	if remote {
		why = "The cluster does not serve these kinds and the chart defines no CustomResourceDefinition for them, " +
			"so it is not known whether they are namespace-scoped"
		fix = fmt.Sprintf("If they are, set metadata.namespace = %q where the manifests are applied.", namespace)
	}

	return diag.NewWarningDiagnostic(
		"nelm_template: release namespace not set on objects of unknown kinds",
		release.scrubSensitive(fmt.Sprintf("nelm removes metadata.namespace from every object it renders into the "+
			"release namespace, and nelm_template sets it back on the objects of namespace-scoped kinds. %s: %s. "+
			"Their objects in manifest and manifests have no metadata.namespace unless the chart sets one other than "+
			"the release namespace. %s", why, gvkList(unresolved), fix)),
	)
}

// templateFilter selects the objects of a Template render that go into
// manifest and manifests: include_crds, skip_tests and show_only.
type templateFilter struct {
	includeCRDs bool
	skipTests   bool

	// showOnly is the show_only list, in order (its index is the list index
	// diagnostics point at); an empty entry (an empty or null element) is
	// ignored, as helm_template ignores it.
	showOnly []string
}

// selectObjects returns the objects of objs that f selects, in objs' order.
// Standalone CRDs need includeCRDs; skipTests drops test hooks; showOnly
// patterns then keep the objects whose chart-root-relative template path
// (chartRelativePath) matches one of them. Matching is path.Match on the
// slash form, so "*" never crosses a "/" on any OS (helm_template's
// filepath.Match lets it cross one on Windows). Like helm's, a pattern that
// matches none of the objects include_crds and skip_tests leave is an error;
// a showOnly with no non-empty pattern filters nothing.
func (f templateFilter) selectObjects(objs []nelmclient.TemplateObject) ([]nelmclient.TemplateObject, diag.Diagnostics) {
	var diags diag.Diagnostics

	candidates := make([]nelmclient.TemplateObject, 0, len(objs))

	for _, obj := range objs {
		if obj.StandaloneCRD && !f.includeCRDs {
			continue
		}

		if f.skipTests && isTestHook(obj) {
			continue
		}

		candidates = append(candidates, obj)
	}

	if !slices.ContainsFunc(f.showOnly, func(pattern string) bool { return pattern != "" }) {
		return candidates, diags
	}

	keep := make([]bool, len(candidates))

	for i, pattern := range f.showOnly {
		if pattern == "" {
			continue
		}

		pattern = filepath.ToSlash(pattern)

		// Match checks the whole pattern even when the name does not match,
		// so this catches a malformed one before any object is tested.
		if _, err := pathpkg.Match(pattern, ""); err != nil {
			diags.AddAttributeError(
				path.Root("show_only").AtListIndex(i),
				"Invalid show_only pattern",
				fmt.Sprintf("%q is not a valid glob pattern: %s", pattern, err),
			)
			continue
		}

		found := false

		for j, obj := range candidates {
			if matched, _ := pathpkg.Match(pattern, chartRelativePath(obj.Path)); matched {
				keep[j] = true
				found = true
			}
		}

		if !found {
			diags.AddAttributeError(
				path.Root("show_only").AtListIndex(i),
				"Template Not Found",
				fmt.Sprintf("Could not find template %q in chart. show_only patterns match the paths, relative "+
					"to the chart root (e.g. \"templates/deployment.yaml\"), of the templates that rendered an "+
					"object, after include_crds and skip_tests.", pattern),
			)
		}
	}

	if diags.HasError() {
		return nil, diags
	}

	selected := make([]nelmclient.TemplateObject, 0, len(candidates))

	for j, obj := range candidates {
		if keep[j] {
			selected = append(selected, obj)
		}
	}

	return selected, diags
}

// isTestHook reports a Helm test hook: a hook whose helm.sh/hook events
// include "test" (or its legacy alias "test-success"). The events are read
// as Helm's manifest sorter reads them (releaseutil, which helm template
// --skip-tests and helm_template's skip_tests rely on): the annotation split
// on ",", each event trimmed of white space and lower-cased.
func isTestHook(obj nelmclient.TemplateObject) bool {
	if !obj.Hook || obj.Object == nil {
		return false
	}

	for _, event := range strings.Split(obj.Object.GetAnnotations()["helm.sh/hook"], ",") {
		switch strings.ToLower(strings.TrimSpace(event)) {
		case "test", "test-success":
			return true
		}
	}

	return false
}

// chartRelativePath strips the chart name nelm prefixes a template path with
// ("app/templates/x.yaml" -> "templates/x.yaml"), helm_template's
// `# Source: [^/]+/(.+)` rule, on the slash form of the path: every `\` is a
// "/" first, on every OS. nelm reports a crds/ object's path with the OS
// separator (Helm's CRDObjects builds it with filepath.Join), so a Windows
// render has `app\crds\x.yaml`; templates/ paths are slash-separated
// everywhere (and no chart file name has a `\` in practice). manifests keys
// and show_only matches so come out the same on every OS, while manifest's
// "# Source:" lines keep the path exactly as nelm prints it.
func chartRelativePath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")

	if _, rel, ok := strings.Cut(p, "/"); ok {
		return rel
	}

	return p
}

// templateManifests builds manifest and manifests from the selected objects:
// each object as `nelm chart render` prints it ("---", "# Source: <Path>",
// its YAML), concatenated in objs' order, and the same per chart-root-relative
// template path. Both are empty, never null, when nothing is selected.
func templateManifests(objs []nelmclient.TemplateObject) (string, map[string]string) {
	var all strings.Builder

	manifests := make(map[string]string, len(objs))

	for _, obj := range objs {
		doc := "---\n# Source: " + obj.Path + "\n" + obj.YAML

		all.WriteString(doc)
		manifests[chartRelativePath(obj.Path)] += doc
	}

	return all.String(), manifests
}

// templateCRDs is the YAML of every standalone CRD of objs, whatever the
// filters select (helm_template parity); empty, never nil, without any.
func templateCRDs(objs []nelmclient.TemplateObject) []string {
	crds := make([]string, 0)

	for _, obj := range objs {
		if obj.StandaloneCRD {
			crds = append(crds, obj.YAML)
		}
	}

	return crds
}
