package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/datasource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dsschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
)

// templateTimeoutsAttrTypes mirrors the data source "timeouts" block's
// nested attribute types (read only).
var templateTimeoutsAttrTypes = map[string]attr.Type{"read": types.StringType}

// fakeTemplateClient is an in-memory templateClient: a canned render plus a
// record of every Template call and every scope lookup.
type fakeTemplateClient struct {
	objs []nelmclient.TemplateObject
	err  error

	// noConnection makes the fake the nelmclient.NewNoConnectionClient
	// client.
	noConnection error

	// served is the fake cluster's RESTMapper: whether each kind it serves
	// is namespaced. Any other kind is a *meta.NoKindMatchError, and
	// scopeErr, when set, fails every lookup.
	served   map[schema.GroupVersionKind]bool
	scopeErr error

	// servedAfterReset are kinds the cached RESTMapper only knows once
	// ResetKindCache has refreshed it: kinds whose CRD was installed after
	// it was built.
	servedAfterReset map[schema.GroupVersionKind]bool

	specs    []nelmclient.ReleaseSpec
	opts     []nelmclient.TemplateOptions
	timeouts []time.Duration
	scoped   []schema.GroupVersionKind
	resets   int
}

var _ templateClient = (*fakeTemplateClient)(nil)

func (f *fakeTemplateClient) Template(_ context.Context, spec nelmclient.ReleaseSpec, opts nelmclient.TemplateOptions, timeout time.Duration) ([]nelmclient.TemplateObject, error) {
	f.specs = append(f.specs, spec)
	f.opts = append(f.opts, opts)
	f.timeouts = append(f.timeouts, timeout)

	if f.err != nil {
		return nil, f.err
	}

	return f.objs, nil
}

func (f *fakeTemplateClient) NoConnection() error {
	return f.noConnection
}

func (f *fakeTemplateClient) IsNamespaced(gvk schema.GroupVersionKind) (bool, error) {
	f.scoped = append(f.scoped, gvk)

	if f.scopeErr != nil {
		return false, f.scopeErr
	}

	if namespaced, ok := f.servedAfterReset[gvk]; ok && f.resets > 0 {
		return namespaced, nil
	}

	namespaced, ok := f.served[gvk]
	if !ok {
		return false, &meta.NoKindMatchError{GroupKind: gvk.GroupKind(), SearchedVersions: []string{gvk.Version}}
	}

	return namespaced, nil
}

func (f *fakeTemplateClient) ResetKindCache() {
	f.resets++
}

// templateObject is a rendered object of chart "app": a ConfigMap (or a
// CustomResourceDefinition when crd) named name from template file (relative
// to the chart root), a hook when hook names its helm.sh/hook events.
func templateObject(file, name, hook string, crd bool) nelmclient.TemplateObject {
	kind, apiVersion := "ConfigMap", "v1"
	if crd {
		kind, apiVersion = "CustomResourceDefinition", "apiextensions.k8s.io/v1"
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
	}}

	if hook != "" {
		obj.SetAnnotations(map[string]string{"helm.sh/hook": hook})
	}

	return nelmclient.TemplateObject{
		Path:          "app/" + file,
		Hook:          hook != "",
		StandaloneCRD: crd,
		Object:        obj,
		YAML:          "apiVersion: " + apiVersion + "\nkind: " + kind + "\nmetadata:\n  name: " + name + "\n",
	}
}

// Objects of testTemplateRender, in nelm's order (standalone CRDs, hooks,
// the rest).
var (
	tmplCRD        = templateObject("crds/widget.yaml", "widgets.example.com", "", true)
	tmplPreInstall = templateObject("templates/hooks.yaml", "pre-install", "pre-install", false)
	tmplTest       = templateObject("templates/hooks.yaml", "test", "test", false)
	tmplTestLegacy = templateObject("templates/hooks.yaml", "test-legacy", "pre-install, test-success", false)
	tmplConfigMap  = templateObject("templates/configmap.yaml", "app", "", false)
	tmplSub        = templateObject("charts/sub/templates/configmap.yaml", "app-sub", "", false)
)

// testTemplateRender is a Template result shaped like
// testdata/charts/template's: a standalone CRD, hooks (two of them tests), a
// regular object and a subchart object.
func testTemplateRender() []nelmclient.TemplateObject {
	return []nelmclient.TemplateObject{tmplCRD, tmplPreInstall, tmplTest, tmplTestLegacy, tmplConfigMap, tmplSub}
}

// inNamespace is what Read makes of testTemplateRender's objects for release
// namespace ns: every ConfigMap (none of them sets a namespace) gets it, its
// YAML re-encoded; the standalone CRD stays as rendered.
func inNamespace(t *testing.T, ns string, objs ...nelmclient.TemplateObject) []nelmclient.TemplateObject {
	t.Helper()

	out := make([]nelmclient.TemplateObject, len(objs))

	for i, obj := range objs {
		out[i] = obj

		if obj.StandaloneCRD {
			continue
		}

		out[i].Object = obj.Object.DeepCopy()
		out[i].Object.SetNamespace(ns)

		y, err := nelmclient.ManifestYAML(out[i].Object)
		if err != nil {
			t.Fatalf("ManifestYAML: %v", err)
		}

		out[i].YAML = y
	}

	return out
}

// baseTestTemplateModel is a minimal nelm_template configuration: every
// optional attribute and every computed one null.
func baseTestTemplateModel() templateModel {
	return templateModel{
		Name:        types.StringValue("my-release"),
		Namespace:   types.StringNull(),
		Chart:       types.StringValue("/charts/app"),
		Repository:  types.StringNull(),
		Version:     types.StringNull(),
		Values:      types.ListNull(types.StringType),
		KubeVersion: types.StringNull(),
		APIVersions: types.ListNull(types.StringType),
		IncludeCRDs: types.BoolNull(),
		SkipTests:   types.BoolNull(),
		ShowOnly:    types.ListNull(types.StringType),
		Validate:    types.BoolNull(),
		Timeouts:    timeouts.Value{Object: types.ObjectNull(templateTimeoutsAttrTypes)},

		ID:        types.StringNull(),
		Manifest:  types.StringNull(),
		Manifests: types.MapNull(types.StringType),
		CRDs:      types.ListNull(types.StringType),
	}
}

// templateConfig builds a tfsdk.Config of the nelm_template schema from
// model (unknown values allowed).
func templateConfig(t *testing.T, ctx context.Context, model templateModel) tfsdk.Config {
	t.Helper()

	sch := templateDataSourceSchema(ctx)

	// tfsdk.Config has no Set; a Plan of the same schema converts the model.
	plan := tfsdk.Plan{Schema: sch}
	if diags := plan.Set(ctx, &model); diags.HasError() {
		t.Fatalf("templateConfig: unexpected error diagnostics: %v", diags)
	}

	return tfsdk.Config{Schema: sch, Raw: plan.Raw}
}

// readTemplate runs the data source's Read for model against client and
// returns the state it set (zero when Read failed) and its diagnostics.
func readTemplate(t *testing.T, client templateClient, model templateModel) (templateModel, diag.Diagnostics) {
	t.Helper()

	ctx := context.Background()
	sch := templateDataSourceSchema(ctx)

	// The framework seeds the response state with a null value of the schema.
	resp := &datasource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}}
	(&templateDataSource{client: client}).Read(ctx, datasource.ReadRequest{Config: templateConfig(t, ctx, model)}, resp)

	var got templateModel
	if resp.Diagnostics.HasError() {
		return got, resp.Diagnostics
	}

	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("read state back: %v", diags)
	}

	return got, resp.Diagnostics
}

// mustReadTemplate is readTemplate failing the test on any error.
func mustReadTemplate(t *testing.T, client templateClient, model templateModel) templateModel {
	t.Helper()

	got, diags := readTemplate(t, client, model)
	if diags.HasError() {
		t.Fatalf("Read: unexpected error diagnostics: %v", diags)
	}

	return got
}

// templateDocs is objs as manifest concatenates them.
func templateDocs(objs ...nelmclient.TemplateObject) string {
	var b strings.Builder

	for _, obj := range objs {
		b.WriteString("---\n# Source: " + obj.Path + "\n" + obj.YAML)
	}

	return b.String()
}

func stringList(values ...string) types.List {
	elems := make([]attr.Value, 0, len(values))
	for _, v := range values {
		elems = append(elems, types.StringValue(v))
	}

	return types.ListValueMust(types.StringType, elems)
}

// listStrings decodes a known list of strings.
func listStrings(t *testing.T, l types.List) []string {
	t.Helper()

	out := []string{}
	if diags := l.ElementsAs(context.Background(), &out, false); diags.HasError() {
		t.Fatalf("decode list: %v", diags)
	}

	return out
}

func TestTemplateRead_Filters(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(m *templateModel)
		want   []nelmclient.TemplateObject
	}{
		{
			name: "defaults: hooks and tests in, standalone CRDs out",
			want: []nelmclient.TemplateObject{tmplPreInstall, tmplTest, tmplTestLegacy, tmplConfigMap, tmplSub},
		},
		{
			name:   "include_crds",
			mutate: func(m *templateModel) { m.IncludeCRDs = types.BoolValue(true) },
			want:   testTemplateRender(),
		},
		{
			name:   "skip_tests drops every hook with a test event",
			mutate: func(m *templateModel) { m.SkipTests = types.BoolValue(true) },
			want:   []nelmclient.TemplateObject{tmplPreInstall, tmplConfigMap, tmplSub},
		},
		{
			name:   "show_only one file keeps all its objects",
			mutate: func(m *templateModel) { m.ShowOnly = stringList("templates/hooks.yaml") },
			want:   []nelmclient.TemplateObject{tmplPreInstall, tmplTest, tmplTestLegacy},
		},
		{
			name: "show_only keeps nelm's order, not the patterns'",
			mutate: func(m *templateModel) {
				m.ShowOnly = stringList("charts/sub/templates/*", "templates/configmap.yaml")
			},
			want: []nelmclient.TemplateObject{tmplConfigMap, tmplSub},
		},
		{
			name:   "show_only star does not cross a slash",
			mutate: func(m *templateModel) { m.ShowOnly = stringList("templates/*") },
			want:   []nelmclient.TemplateObject{tmplPreInstall, tmplTest, tmplTestLegacy, tmplConfigMap},
		},
		{
			name: "show_only with include_crds matches crds/",
			mutate: func(m *templateModel) {
				m.IncludeCRDs = types.BoolValue(true)
				m.ShowOnly = stringList("crds/*.yaml")
			},
			want: []nelmclient.TemplateObject{tmplCRD},
		},
		{
			name: "show_only with skip_tests",
			mutate: func(m *templateModel) {
				m.SkipTests = types.BoolValue(true)
				m.ShowOnly = stringList("templates/hooks.yaml")
			},
			want: []nelmclient.TemplateObject{tmplPreInstall},
		},
		{
			// helm_template skips empty and null show_only elements, so a
			// list built from an empty variable filters nothing.
			name:   "show_only empty entry filters nothing",
			mutate: func(m *templateModel) { m.ShowOnly = stringList("") },
			want:   []nelmclient.TemplateObject{tmplPreInstall, tmplTest, tmplTestLegacy, tmplConfigMap, tmplSub},
		},
		{
			name: "show_only null entry filters nothing",
			mutate: func(m *templateModel) {
				m.ShowOnly = types.ListValueMust(types.StringType, []attr.Value{types.StringNull()})
			},
			want: []nelmclient.TemplateObject{tmplPreInstall, tmplTest, tmplTestLegacy, tmplConfigMap, tmplSub},
		},
		{
			name: "show_only empty and null entries next to a pattern",
			mutate: func(m *templateModel) {
				m.ShowOnly = types.ListValueMust(types.StringType, []attr.Value{
					types.StringValue(""), types.StringNull(), types.StringValue("templates/configmap.yaml"),
				})
			},
			want: []nelmclient.TemplateObject{tmplConfigMap},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := baseTestTemplateModel()
			if tt.mutate != nil {
				tt.mutate(&model)
			}

			got := mustReadTemplate(t, &fakeTemplateClient{objs: testTemplateRender()}, model)

			if want := templateDocs(inNamespace(t, "default", tt.want...)...); got.Manifest.ValueString() != want {
				t.Errorf("manifest =\n%s\nwant\n%s", got.Manifest.ValueString(), want)
			}

			wantKeys := map[string]bool{}
			for _, obj := range tt.want {
				wantKeys[strings.TrimPrefix(obj.Path, "app/")] = true
			}

			gotKeys := slices.Sorted(maps.Keys(stringMap(t, got.Manifests)))
			if want := slices.Sorted(maps.Keys(wantKeys)); !slices.Equal(gotKeys, want) {
				t.Errorf("manifests keys = %v, want %v", gotKeys, want)
			}
		})
	}
}

func TestTemplateRead_ShowOnlyErrors(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(m *templateModel)
		wantSummary string
		wantPath    path.Path
	}{
		{
			name:        "no such template",
			mutate:      func(m *templateModel) { m.ShowOnly = stringList("templates/*.yaml", "templates/nope.yaml") },
			wantSummary: "Template Not Found",
			wantPath:    path.Root("show_only").AtListIndex(1),
		},
		{
			name:        "crds/ without include_crds",
			mutate:      func(m *templateModel) { m.ShowOnly = stringList("crds/widget.yaml") },
			wantSummary: "Template Not Found",
			wantPath:    path.Root("show_only").AtListIndex(0),
		},
		{
			name: "skip_tests keeps a file's other hooks",
			mutate: func(m *templateModel) {
				m.SkipTests = types.BoolValue(true)
				m.ShowOnly = stringList("templates/configmap.yaml", "templates/hook*.yaml")
			},
			wantSummary: "",
		},
		{
			name:        "the chart name is not part of the path",
			mutate:      func(m *templateModel) { m.ShowOnly = stringList("app/templates/configmap.yaml") },
			wantSummary: "Template Not Found",
			wantPath:    path.Root("show_only").AtListIndex(0),
		},
		{
			name:        "malformed pattern",
			mutate:      func(m *templateModel) { m.ShowOnly = stringList("templates/[") },
			wantSummary: "Invalid show_only pattern",
			wantPath:    path.Root("show_only").AtListIndex(0),
		},
		{
			name: "an ignored empty entry keeps the list index of the next",
			mutate: func(m *templateModel) {
				m.ShowOnly = types.ListValueMust(types.StringType, []attr.Value{
					types.StringValue(""), types.StringNull(), types.StringValue("templates/nope.yaml"),
				})
			},
			wantSummary: "Template Not Found",
			wantPath:    path.Root("show_only").AtListIndex(2),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := baseTestTemplateModel()
			tt.mutate(&model)

			got, diags := readTemplate(t, &fakeTemplateClient{objs: testTemplateRender()}, model)

			if tt.wantSummary == "" {
				if diags.HasError() {
					t.Fatalf("Read: unexpected error diagnostics: %v", diags)
				}

				if want := templateDocs(inNamespace(t, "default", tmplPreInstall, tmplConfigMap)...); got.Manifest.ValueString() != want {
					t.Errorf("manifest =\n%s\nwant\n%s", got.Manifest.ValueString(), want)
				}

				return
			}

			errs := diags.Errors()
			if len(errs) != 1 {
				t.Fatalf("errors = %v, want exactly one %q", diags, tt.wantSummary)
			}

			if errs[0].Summary() != tt.wantSummary {
				t.Errorf("summary = %q, want %q", errs[0].Summary(), tt.wantSummary)
			}

			withPath, ok := errs[0].(diag.DiagnosticWithPath)
			if !ok || !withPath.Path().Equal(tt.wantPath) {
				t.Errorf("error path = %v, want %s", errs[0], tt.wantPath)
			}
		})
	}
}

// TestTemplateRead_SkipTestsHookEvents: skip_tests reads the helm.sh/hook
// events as Helm's manifest sorter does (split on ",", trimmed, lower-cased),
// so it drops exactly the hooks helm template --skip-tests and helm_template
// drop.
func TestTemplateRead_SkipTestsHookEvents(t *testing.T) {
	tests := []struct {
		hook    string
		dropped bool
	}{
		{hook: "test", dropped: true},
		{hook: "test-success", dropped: true},
		{hook: "Test", dropped: true},
		{hook: "TEST-SUCCESS", dropped: true},
		{hook: "pre-install, TEST-SUCCESS", dropped: true},
		{hook: " pre-install ,\tTest\n", dropped: true},
		{hook: "pre-install", dropped: false},
		{hook: "pre-install,post-upgrade", dropped: false},
		{hook: "testing", dropped: false},
		{hook: "test-failure", dropped: false},
	}

	for _, tt := range tests {
		t.Run(tt.hook, func(t *testing.T) {
			hook := templateObject("templates/hooks.yaml", "hook", tt.hook, false)

			model := baseTestTemplateModel()
			model.SkipTests = types.BoolValue(true)

			got := mustReadTemplate(t, &fakeTemplateClient{objs: []nelmclient.TemplateObject{hook, tmplConfigMap}}, model)

			want := []nelmclient.TemplateObject{hook, tmplConfigMap}
			if tt.dropped {
				want = want[1:]
			}

			if want := templateDocs(inNamespace(t, "default", want...)...); got.Manifest.ValueString() != want {
				t.Errorf("manifest =\n%s\nwant\n%s", got.Manifest.ValueString(), want)
			}
		})
	}
}

// TestTemplateRead_SlashFormPaths: nelm reports a crds/ object's path with
// the OS separator (a Windows render: app\crds\widget.yaml), so manifests
// keys and show_only use the path's slash form, and a show_only "*" never
// crosses a "/"; manifest keeps the "# Source:" path as nelm prints it.
func TestTemplateRead_SlashFormPaths(t *testing.T) {
	winCRD := tmplCRD
	winCRD.Path = `app\crds\widget.yaml`

	winSubCRD := templateObject("charts/sub/crds/gizmo.yaml", "gizmos.example.com", "", true)
	winSubCRD.Path = `app\charts\sub\crds\gizmo.yaml`

	nested := templateObject("templates/sub/nested.yaml", "nested", "", false)

	render := []nelmclient.TemplateObject{winCRD, winSubCRD, tmplConfigMap, nested}

	tests := []struct {
		name     string
		showOnly []string
		want     []nelmclient.TemplateObject
		wantKeys []string
	}{
		{
			name:     "no show_only",
			want:     render,
			wantKeys: []string{"charts/sub/crds/gizmo.yaml", "crds/widget.yaml", "templates/configmap.yaml", "templates/sub/nested.yaml"},
		},
		{
			name:     "crds glob",
			showOnly: []string{"crds/*.yaml"},
			want:     []nelmclient.TemplateObject{winCRD},
			wantKeys: []string{"crds/widget.yaml"},
		},
		{
			name:     "exact crds path",
			showOnly: []string{"crds/widget.yaml"},
			want:     []nelmclient.TemplateObject{winCRD},
			wantKeys: []string{"crds/widget.yaml"},
		},
		{
			name:     "subchart crds glob",
			showOnly: []string{"charts/*/crds/*.yaml"},
			want:     []nelmclient.TemplateObject{winSubCRD},
			wantKeys: []string{"charts/sub/crds/gizmo.yaml"},
		},
		{
			name:     "star does not cross a slash",
			showOnly: []string{"templates/*"},
			want:     []nelmclient.TemplateObject{tmplConfigMap},
			wantKeys: []string{"templates/configmap.yaml"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := baseTestTemplateModel()
			model.IncludeCRDs = types.BoolValue(true)

			if tt.showOnly != nil {
				model.ShowOnly = stringList(tt.showOnly...)
			}

			got := mustReadTemplate(t, &fakeTemplateClient{objs: render}, model)

			if want := templateDocs(inNamespace(t, "default", tt.want...)...); got.Manifest.ValueString() != want {
				t.Errorf("manifest =\n%s\nwant\n%s", got.Manifest.ValueString(), want)
			}

			if gotKeys := slices.Sorted(maps.Keys(stringMap(t, got.Manifests))); !slices.Equal(gotKeys, tt.wantKeys) {
				t.Errorf("manifests keys = %v, want %v", gotKeys, tt.wantKeys)
			}
		})
	}
}

func TestTemplateRead_CRDsAlwaysListed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(m *templateModel)
	}{
		{name: "include_crds unset"},
		{name: "include_crds", mutate: func(m *templateModel) { m.IncludeCRDs = types.BoolValue(true) }},
		{name: "show_only elsewhere", mutate: func(m *templateModel) { m.ShowOnly = stringList("templates/configmap.yaml") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := baseTestTemplateModel()
			if tt.mutate != nil {
				tt.mutate(&model)
			}

			got := mustReadTemplate(t, &fakeTemplateClient{objs: testTemplateRender()}, model)

			if crds := listStrings(t, got.CRDs); !slices.Equal(crds, []string{tmplCRD.YAML}) {
				t.Errorf("crds = %q, want only the CRD's YAML %q", crds, tmplCRD.YAML)
			}
		})
	}
}

func TestTemplateRead_IDAndNamespace(t *testing.T) {
	tests := []struct {
		name      string
		namespace types.String
		wantNS    string
	}{
		{name: "namespace defaults to default", namespace: types.StringNull(), wantNS: "default"},
		{name: "configured namespace", namespace: types.StringValue("apps"), wantNS: "apps"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := baseTestTemplateModel()
			model.Namespace = tt.namespace

			client := &fakeTemplateClient{objs: testTemplateRender()}
			got := mustReadTemplate(t, client, model)

			if got.Namespace.ValueString() != tt.wantNS {
				t.Errorf("namespace = %v, want %q", got.Namespace, tt.wantNS)
			}

			if want := tt.wantNS + "/my-release"; got.ID.ValueString() != want {
				t.Errorf("id = %v, want %q", got.ID, want)
			}

			if len(client.specs) != 1 || client.specs[0].Namespace != tt.wantNS || client.specs[0].Name != "my-release" {
				t.Errorf("Template specs = %+v, want one for my-release in %s", client.specs, tt.wantNS)
			}
		})
	}
}

func TestTemplateRead_OutputsFormat(t *testing.T) {
	t.Run("manifest and manifests", func(t *testing.T) {
		got := mustReadTemplate(t, &fakeTemplateClient{objs: testTemplateRender()}, baseTestTemplateModel())

		// Every ConfigMap carries the release namespace nelm stripped.
		wantManifest := "---\n# Source: app/templates/hooks.yaml\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  annotations:\n    helm.sh/hook: pre-install\n  name: pre-install\n  namespace: default\n" +
			"---\n# Source: app/templates/hooks.yaml\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  annotations:\n    helm.sh/hook: test\n  name: test\n  namespace: default\n" +
			"---\n# Source: app/templates/hooks.yaml\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  annotations:\n    helm.sh/hook: pre-install, test-success\n  name: test-legacy\n  namespace: default\n" +
			"---\n# Source: app/templates/configmap.yaml\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n  namespace: default\n" +
			"---\n# Source: app/charts/sub/templates/configmap.yaml\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app-sub\n  namespace: default\n"
		if got.Manifest.ValueString() != wantManifest {
			t.Errorf("manifest =\n%s\nwant\n%s", got.Manifest.ValueString(), wantManifest)
		}

		wantManifests := map[string]string{
			"templates/hooks.yaml":                templateDocs(inNamespace(t, "default", tmplPreInstall, tmplTest, tmplTestLegacy)...),
			"templates/configmap.yaml":            templateDocs(inNamespace(t, "default", tmplConfigMap)...),
			"charts/sub/templates/configmap.yaml": templateDocs(inNamespace(t, "default", tmplSub)...),
		}
		if gotManifests := stringMap(t, got.Manifests); !maps.Equal(gotManifests, wantManifests) {
			t.Errorf("manifests = %q, want %q", gotManifests, wantManifests)
		}
	})

	t.Run("nothing rendered: empty, known, not null", func(t *testing.T) {
		got := mustReadTemplate(t, &fakeTemplateClient{}, baseTestTemplateModel())

		for name, v := range map[string]attr.Value{"manifest": got.Manifest, "manifests": got.Manifests, "crds": got.CRDs} {
			if v.IsNull() || v.IsUnknown() {
				t.Errorf("%s = %v, want a known empty value", name, v)
			}
		}

		if got.Manifest.ValueString() != "" || len(got.Manifests.Elements()) != 0 || len(got.CRDs.Elements()) != 0 {
			t.Errorf("manifest = %v, manifests = %v, crds = %v, want all empty", got.Manifest, got.Manifests, got.CRDs)
		}
	})
}

// kindObject is a rendered object of chart "app" of kind apiVersion/kind named
// name from template file (relative to the chart root), in namespace (none
// when empty), its YAML as nelm encodes it.
func kindObject(t *testing.T, file, apiVersion, kind, name, namespace string) nelmclient.TemplateObject {
	t.Helper()

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
	}}

	if namespace != "" {
		obj.SetNamespace(namespace)
	}

	return nelmclient.TemplateObject{Path: "app/" + file, Object: obj, YAML: mustManifestYAML(t, obj)}
}

func mustManifestYAML(t *testing.T, obj *unstructured.Unstructured) string {
	t.Helper()

	y, err := nelmclient.ManifestYAML(obj)
	if err != nil {
		t.Fatalf("ManifestYAML: %v", err)
	}

	return y
}

// manifestNamespaces decodes every document of manifest into "<Kind>/<name>"
// -> its metadata.namespace ("" when it has none).
func manifestNamespaces(t *testing.T, manifest string) map[string]string {
	t.Helper()

	out := map[string]string{}

	for _, doc := range strings.Split(manifest, "---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}

		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
			t.Fatalf("decode manifest document: %v\n%s", err, doc)
		}

		out[obj.GetKind()+"/"+obj.GetName()] = obj.GetNamespace()
	}

	return out
}

// TestTemplateRead_RestoresReleaseNamespace: nelm strips metadata.namespace
// where it equals the release namespace, so Read sets it back on every object
// without one whose kind is namespace-scoped — offline from the built-in
// kinds and the CRDs in the render, with validate = true from the cluster —
// leaving crds/ objects, other namespaces and cluster-scoped kinds alone. A
// kind it cannot resolve keeps no namespace, with one warning.
func TestTemplateRead_RestoresReleaseNamespace(t *testing.T) {
	gvkConfigMap := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	gvkJob := schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}
	gvkClusterRole := schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}
	gvkGadget := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Gadget"}
	gvkMonitor := schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"}

	gadgetCRD := kindObject(t, "crds/gadget.yaml", "apiextensions.k8s.io/v1", "CustomResourceDefinition", "gadgets.example.com", "")
	gadgetCRD.Object.Object["spec"] = map[string]any{
		"group":    "example.com",
		"names":    map[string]any{"kind": "Gadget", "plural": "gadgets"},
		"scope":    "Namespaced",
		"versions": []any{map[string]any{"name": "v1", "served": true, "storage": true}},
	}
	gadgetCRD.YAML = mustManifestYAML(t, gadgetCRD.Object)
	gadgetCRD.StandaloneCRD = true

	// A non-CRD object in crds/ is still a crds/ object.
	crdsAccount := kindObject(t, "crds/account.yaml", "v1", "ServiceAccount", "crds-account", "")
	crdsAccount.StandaloneCRD = true

	hook := kindObject(t, "templates/hook.yaml", "batch/v1", "Job", "hook", "")
	hook.Hook = true
	hook.Object.SetAnnotations(map[string]string{"helm.sh/hook": "post-install"})
	hook.YAML = mustManifestYAML(t, hook.Object)

	render := func() []nelmclient.TemplateObject {
		return []nelmclient.TemplateObject{
			gadgetCRD,
			crdsAccount,
			hook,
			kindObject(t, "templates/objects.yaml", "v1", "ConfigMap", "no-ns", ""),
			kindObject(t, "templates/objects.yaml", "v1", "ConfigMap", "other-ns", "other"),
			kindObject(t, "templates/objects.yaml", "rbac.authorization.k8s.io/v1", "ClusterRole", "cluster", ""),
			kindObject(t, "templates/gadget.yaml", "example.com/v1", "Gadget", "gadget", ""),
			kindObject(t, "templates/monitor.yaml", "monitoring.coreos.com/v1", "ServiceMonitor", "monitor", ""),
		}
	}

	// The fake cluster serves the built-in kinds and, in some cases, the
	// ServiceMonitor; never the Gadget, whose CRD only the chart has.
	builtins := map[schema.GroupVersionKind]bool{gvkConfigMap: true, gvkJob: true, gvkClusterRole: false}
	withMonitor := maps.Clone(builtins)
	withMonitor[gvkMonitor] = true

	// asked is every kind Read looks up with validate = true, in the
	// render's order: crds/ objects and objects with a namespace are not
	// looked up.
	asked := []schema.GroupVersionKind{gvkJob, gvkConfigMap, gvkClusterRole, gvkGadget, gvkMonitor}

	restored := map[string]string{
		"CustomResourceDefinition/gadgets.example.com": "",
		"ServiceAccount/crds-account":                  "",
		"Job/hook":                                     "apps",
		"ConfigMap/no-ns":                              "apps",
		"ConfigMap/other-ns":                           "other",
		"ClusterRole/cluster":                          "",
		"Gadget/gadget":                                "apps",
	}

	withNS := func(ns string) map[string]string {
		m := maps.Clone(restored)
		m["ServiceMonitor/monitor"] = ns

		return m
	}

	tests := []struct {
		name        string
		validate    bool
		served      map[schema.GroupVersionKind]bool
		showOnly    []string
		want        map[string]string
		wantAsked   []schema.GroupVersionKind
		wantWarning string
	}{
		{
			name:        "offline: built-in kinds and the chart's CRDs, never the client",
			served:      withMonitor,
			want:        withNS(""),
			wantWarning: "validate = true",
		},
		{
			name:     "offline: show_only leaves the unknown kind out, so no warning",
			showOnly: []string{"crds/*", "templates/hook.yaml", "templates/objects.yaml", "templates/gadget.yaml"},
			want:     restored,
		},
		{
			name:      "validate: the cluster serves the ServiceMonitor",
			validate:  true,
			served:    withMonitor,
			want:      withNS("apps"),
			wantAsked: asked,
		},
		{
			name:        "validate: the cluster does not serve the ServiceMonitor",
			validate:    true,
			served:      builtins,
			want:        withNS(""),
			wantAsked:   asked,
			wantWarning: "does not serve",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := baseTestTemplateModel()
			model.Namespace = types.StringValue("apps")
			model.IncludeCRDs = types.BoolValue(true)
			model.Validate = types.BoolValue(tt.validate)

			if tt.showOnly != nil {
				model.ShowOnly = stringList(tt.showOnly...)
			}

			objs := render()
			client := &fakeTemplateClient{objs: objs, served: tt.served}

			renderedNS := make([]string, len(objs))
			for i, obj := range objs {
				renderedNS[i] = obj.Object.GetNamespace()
			}

			got, diags := readTemplate(t, client, model)
			if diags.HasError() {
				t.Fatalf("Read: unexpected error diagnostics: %v", diags)
			}

			if gotNS := manifestNamespaces(t, got.Manifest.ValueString()); !maps.Equal(gotNS, tt.want) {
				t.Errorf("namespaces in manifest = %v, want %v", gotNS, tt.want)
			}

			if !slices.Equal(client.scoped, tt.wantAsked) {
				t.Errorf("client scope lookups = %v, want %v", client.scoped, tt.wantAsked)
			}

			warnings := diags.Warnings()

			switch {
			case tt.wantWarning == "" && len(warnings) != 0:
				t.Errorf("warnings = %v, want none", warnings)
			case tt.wantWarning != "":
				if len(warnings) != 1 {
					t.Fatalf("warnings = %v, want exactly one", warnings)
				}

				for _, want := range []string{gvkMonitor.String(), tt.wantWarning, `metadata.namespace = "apps"`} {
					if !strings.Contains(warnings[0].Detail(), want) {
						t.Errorf("warning detail = %q, want it to mention %q", warnings[0].Detail(), want)
					}
				}

				if tt.validate && strings.Contains(warnings[0].Detail(), "validate = true") {
					t.Errorf("warning detail = %q suggests validate = true to a validate = true render", warnings[0].Detail())
				}
			}

			// Read changes copies: the render it was handed keeps nelm's
			// namespaces. An unchanged object keeps nelm's YAML byte for
			// byte; a restored one is ManifestYAML of the object with the
			// namespace set.
			for i, obj := range objs {
				if ns := obj.Object.GetNamespace(); ns != renderedNS[i] {
					t.Errorf("Read set namespace %q on the render's own %s %q", ns, obj.Object.GetKind(), obj.Object.GetName())
				}

				wantNS, selected := tt.want[obj.Object.GetKind()+"/"+obj.Object.GetName()]
				if !selected {
					continue
				}

				wantYAML := obj.YAML
				if wantNS != renderedNS[i] {
					restored := obj.Object.DeepCopy()
					restored.SetNamespace(wantNS)
					wantYAML = mustManifestYAML(t, restored)
				}

				if doc := "# Source: " + obj.Path + "\n" + wantYAML; !strings.Contains(got.Manifest.ValueString(), doc) {
					t.Errorf("manifest has no document\n%s\nin\n%s", doc, got.Manifest.ValueString())
				}
			}

			// The crds output stays the render's.
			if crds := listStrings(t, got.CRDs); !slices.Equal(crds, []string{gadgetCRD.YAML, crdsAccount.YAML}) {
				t.Errorf("crds = %q, want the crds/ objects as rendered", crds)
			}
		})
	}
}

// TestTemplateRead_ScopeLookupError: with validate = true, a RESTMapper
// failure other than an unserved kind fails the read instead of leaving the
// namespace out, scrubbed of set_sensitive values like any nelm error.
func TestTemplateRead_ScopeLookupError(t *testing.T) {
	const secret = "hunter2-in-a-discovery-error"

	model := baseTestTemplateModel()
	model.Validate = types.BoolValue(true)
	model.SetSensitive = []setModel{
		{Name: types.StringValue("db.password"), Value: types.StringValue(secret), Type: types.StringNull()},
	}

	client := &fakeTemplateClient{
		objs:     testTemplateRender(),
		scopeErr: errors.New("discovery failed near " + secret),
	}

	_, diags := readTemplate(t, client, model)

	errs := diags.Errors()
	if len(errs) != 1 || errs[0].Summary() != "nelm_template: failed to look up a resource kind" {
		t.Fatalf("errors = %v, want exactly the scope lookup error", diags)
	}

	if strings.Contains(errs[0].Detail(), secret) {
		t.Errorf("diagnostic carries the set_sensitive value in cleartext: %s", errs[0].Detail())
	}

	for _, want := range []string{"/v1, Kind=ConfigMap", "app/templates/hooks.yaml", "discovery failed near (sensitive value redacted)"} {
		if !strings.Contains(errs[0].Detail(), want) {
			t.Errorf("detail = %q, want it to mention %q", errs[0].Detail(), want)
		}
	}
}

// TestTemplateRead_RefreshesKindCache: the client's RESTMapper is cached for
// the life of the provider process, so a validate = true Read refreshes it
// (ResetKindCache) once, before its first lookup: a custom resource whose CRD
// was installed after the mapper was built (earlier in the same run) and
// that the chart does not define still gets the release namespace, without
// a warning. An offline Read never touches the client.
func TestTemplateRead_RefreshesKindCache(t *testing.T) {
	gvkConfigMap := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	gvkWidget := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}

	tests := []struct {
		name        string
		validate    bool
		want        map[string]string
		wantResets  int
		wantAsked   []schema.GroupVersionKind
		wantWarning bool
	}{
		{
			name:       "validate: refreshed once, before the lookups",
			validate:   true,
			want:       map[string]string{"ConfigMap/app": "apps", "Widget/widget": "apps"},
			wantResets: 1,
			wantAsked:  []schema.GroupVersionKind{gvkConfigMap, gvkWidget},
		},
		{
			name:        "offline: never the client",
			want:        map[string]string{"ConfigMap/app": "apps", "Widget/widget": ""},
			wantWarning: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := baseTestTemplateModel()
			model.Namespace = types.StringValue("apps")
			model.Validate = types.BoolValue(tt.validate)

			client := &fakeTemplateClient{
				objs: []nelmclient.TemplateObject{
					kindObject(t, "templates/configmap.yaml", "v1", "ConfigMap", "app", ""),
					kindObject(t, "templates/widget.yaml", "example.com/v1", "Widget", "widget", ""),
				},
				served:           map[schema.GroupVersionKind]bool{gvkConfigMap: true},
				servedAfterReset: map[schema.GroupVersionKind]bool{gvkWidget: true},
			}

			got, diags := readTemplate(t, client, model)
			if diags.HasError() {
				t.Fatalf("Read: unexpected error diagnostics: %v", diags)
			}

			if gotNS := manifestNamespaces(t, got.Manifest.ValueString()); !maps.Equal(gotNS, tt.want) {
				t.Errorf("namespaces in manifest = %v, want %v", gotNS, tt.want)
			}

			if client.resets != tt.wantResets {
				t.Errorf("ResetKindCache calls = %d, want %d", client.resets, tt.wantResets)
			}

			if !slices.Equal(client.scoped, tt.wantAsked) {
				t.Errorf("client scope lookups = %v, want %v", client.scoped, tt.wantAsked)
			}

			if warnings := diags.Warnings(); (len(warnings) != 0) != tt.wantWarning {
				t.Errorf("warnings = %v, want a warning: %v", warnings, tt.wantWarning)
			}
		})
	}
}

// TestResetKindCache_SendsNothing: the real client's ResetKindCache only
// drops a RESTMapper it has already built. It builds none and sends no
// request, on a client with a cluster and on the placeholder clients alike.
func TestResetKindCache_SendsNothing(t *testing.T) {
	ambient := ambientCluster(t)

	cluster := newFakeCluster(t)
	kubeconfig := filepath.Join(t.TempDir(), "config")
	writeKubeconfig(t, kubeconfig, "test", map[string]string{"test": cluster.srv.URL})

	for _, client := range []*nelmclient.Client{
		nelmclient.NewClient(nelmclient.Config{KubeConfigPaths: []string{kubeconfig}}),
		nelmclient.NewNoConnectionClient(nil, errors.New("the nelm provider configuration names no cluster")),
		nelmclient.NewUnknownConfigClient(nil, true),
	} {
		client.ResetKindCache()
	}

	if got := append(cluster.hits(), ambient.hits()...); len(got) != 0 {
		t.Errorf("ResetKindCache sent %v, want no request", got)
	}
}

// TestTemplateRead_ScrubsSensitiveError: nelm echoes --set arguments in its
// errors, so a failed render must not carry a set_sensitive value into the
// diagnostic.
func TestTemplateRead_ScrubsSensitiveError(t *testing.T) {
	const secret = "hunter2-not-json"

	model := baseTestTemplateModel()
	model.SetSensitive = []setModel{
		{Name: types.StringValue("db.password"), Value: types.StringValue(secret), Type: types.StringValue("json")},
	}

	client := &fakeTemplateClient{err: errors.New("chart render: failed parsing --set-json data db.password=" + secret)}

	_, diags := readTemplate(t, client, model)

	errs := diags.Errors()
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly one", diags)
	}

	if strings.Contains(errs[0].Detail(), secret) || strings.Contains(errs[0].Summary(), secret) {
		t.Errorf("diagnostic carries the set_sensitive value in cleartext: %s: %s", errs[0].Summary(), errs[0].Detail())
	}

	if !strings.Contains(errs[0].Detail(), "db.password=(sensitive value redacted)") {
		t.Errorf("detail = %q, want the redacted --set-json argument", errs[0].Detail())
	}
}

// TestTemplateRead_MissingDependencies: a chart whose dependencies are not
// vendored fails the read on the chart attribute with helm_template's
// summary, nelm's own error (which names them, scrubbed like any nelm error)
// and the way out.
func TestTemplateRead_MissingDependencies(t *testing.T) {
	const secret = "missing-deps-sensitive"

	model := baseTestTemplateModel()
	model.SetSensitive = []setModel{
		{Name: types.StringValue("db.password"), Value: types.StringValue(secret), Type: types.StringValue("string")},
	}

	nelmErr := errors.New("chart render: render chart: found in Chart.yaml, but missing in charts/ directory: common (" + secret + ")")
	client := &fakeTemplateClient{err: fmt.Errorf("%w: %w", nelmclient.ErrChartDependenciesMissing, nelmErr)}

	_, diags := readTemplate(t, client, model)

	errs := diags.Errors()
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly one", diags)
	}

	if got := errs[0].Summary(); got != "Missing chart dependencies" {
		t.Errorf("summary = %q, want %q", got, "Missing chart dependencies")
	}

	withPath, ok := errs[0].(diag.DiagnosticWithPath)
	if !ok || !withPath.Path().Equal(path.Root("chart")) {
		t.Errorf("diagnostic is not on the chart attribute: %v", errs[0])
	}

	detail := errs[0].Detail()

	for _, want := range []string{"missing in charts/ directory: common", "helm dependency build"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail = %q, want it to contain %q", detail, want)
		}
	}

	if strings.Contains(detail, secret) {
		t.Errorf("detail carries the set_sensitive value in cleartext: %q", detail)
	}
}

// TestTemplateRead_ClusterRefusals: the placeholder clients refuse a render
// they cannot do (validate = true without a cluster, or an oci:// chart while
// the registries are not known yet) with data-source wording. These run the
// real nelmclient clients: each refuses before calling nelm. The ambient
// kubeconfig trap counts any request a leaked zero Config would send to
// ~/.kube/config's current-context, and the oci:// charts name a registry
// that never resolves (RFC 2606 .invalid), so a refusal that regressed fails
// offline. A local chart on the registries-unknown client renders
// (TestTemplateRead_RealClientOfflineNamespaces).
func TestTemplateRead_ClusterRefusals(t *testing.T) {
	ambient := ambientCluster(t)

	noCluster := errors.New("the nelm provider configuration names no cluster")

	tests := []struct {
		name        string
		client      *nelmclient.Client
		validate    bool
		chart       string
		repository  string
		wantSummary string
		wantPath    path.Path
		wantDetail  string
	}{
		{
			name:        "no connection, validate",
			client:      nelmclient.NewNoConnectionClient(nil, noCluster),
			validate:    true,
			wantSummary: "No Kubernetes connection configured",
			wantPath:    path.Root("validate"),
			wantDetail:  noCluster.Error(),
		},
		{
			name:        "configuration unknown, validate",
			client:      nelmclient.NewUnknownConfigClient(nil, true),
			validate:    true,
			wantSummary: "Provider configuration not known at plan time",
			wantPath:    path.Root("validate"),
			wantDetail:  "validate = false",
		},
		{
			name:        "registries unknown, offline oci:// chart",
			client:      nelmclient.NewUnknownConfigClient(nil, false),
			chart:       "oci://registry.invalid/charts/app",
			wantSummary: "Provider configuration not known at plan time",
			wantDetail:  "registries",
		},
		{
			name:        "registries unknown, offline chart from an oci:// repository",
			client:      nelmclient.NewUnknownConfigClient(nil, false),
			chart:       "app",
			repository:  "oci://registry.invalid/charts",
			wantSummary: "Provider configuration not known at plan time",
			wantDetail:  "registries",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := baseTestTemplateModel()
			model.Validate = types.BoolValue(tt.validate)

			if tt.chart != "" {
				model.Chart = types.StringValue(tt.chart)
			}

			if tt.repository != "" {
				model.Repository = types.StringValue(tt.repository)
			}

			_, diags := readTemplate(t, tt.client, model)

			errs := diags.Errors()
			if len(errs) != 1 {
				t.Fatalf("errors = %v, want exactly one %q", diags, tt.wantSummary)
			}

			if errs[0].Summary() != tt.wantSummary {
				t.Errorf("summary = %q, want %q", errs[0].Summary(), tt.wantSummary)
			}

			if !strings.Contains(errs[0].Detail(), tt.wantDetail) {
				t.Errorf("detail = %q, want it to mention %q", errs[0].Detail(), tt.wantDetail)
			}

			withPath, ok := errs[0].(diag.DiagnosticWithPath)
			switch noPath := len(tt.wantPath.Steps()) == 0; {
			case noPath && ok:
				t.Errorf("error path = %s, want none", withPath.Path())
			case !noPath && (!ok || !withPath.Path().Equal(tt.wantPath)):
				t.Errorf("error = %v, want it on %s", errs[0], tt.wantPath)
			}
		})
	}

	if got := ambient.hits(); len(got) != 0 {
		t.Fatalf("~/.kube/config's current-context was contacted: %v", got)
	}
}

// TestTemplateRead_SpecAndOptions pins what Read hands Template: the
// ReleaseSpec nelm_release's toReleaseSpec builds from the same inputs (chart
// normalization, set/set_sensitive translation) and the render options.
func TestTemplateRead_SpecAndOptions(t *testing.T) {
	ctx := context.Background()

	chartDir := t.TempDir()

	model := baseTestTemplateModel()
	model.Namespace = types.StringValue("apps")
	model.Chart = types.StringValue(chartDir)
	model.Version = types.StringValue("1.2.3")
	model.Values = stringList("replicaCount: 2", "image:\n  tag: v1\n")
	model.Set = []setModel{
		{Name: types.StringValue("image.tag"), Value: types.StringValue("v2"), Type: types.StringNull()},
		{Name: types.StringValue("port"), Value: types.StringValue("8080"), Type: types.StringValue("string")},
		{Name: types.StringValue("auth.password"), Value: types.StringValue("readable"), Type: types.StringValue("literal")},
	}
	model.SetSensitive = []setModel{
		{Name: types.StringValue("auth.password"), Value: types.StringValue("s3cret"), Type: types.StringValue("")},
		{Name: types.StringValue("tls"), Value: types.StringValue(`{"key":"k"}`), Type: types.StringValue("json")},
	}

	t.Run("offline", func(t *testing.T) {
		m := model
		m.KubeVersion = types.StringValue("1.30.0")
		m.APIVersions = stringList("example.com/v1", "monitoring.coreos.com/v1")

		client := &fakeTemplateClient{}
		mustReadTemplate(t, client, m)

		if len(client.specs) != 1 {
			t.Fatalf("Template calls = %d, want 1", len(client.specs))
		}

		wantSpec, diags := m.releaseModel().toReleaseSpec(ctx)
		if diags.HasError() {
			t.Fatalf("toReleaseSpec: %v", diags)
		}

		gotSpec := client.specs[0]
		if gotSpec.Chart != wantSpec.Chart || gotSpec.Version != "1.2.3" || gotSpec.Namespace != "apps" {
			t.Errorf("spec chart/version/namespace = %q/%q/%q, want %q/1.2.3/apps", gotSpec.Chart, gotSpec.Version, gotSpec.Namespace, wantSpec.Chart)
		}

		for name, pair := range map[string][2][]string{
			"ValuesYAML": {gotSpec.ValuesYAML, {"replicaCount: 2", "image:\n  tag: v1\n"}},
			"Set":        {gotSpec.Set, {"image.tag=v2", "auth.password=s3cret"}},
			"SetString":  {gotSpec.SetString, {"port=8080"}},
			"SetLiteral": {gotSpec.SetLiteral, nil},
			"SetJSON":    {gotSpec.SetJSON, {`tls={"key":"k"}`}},
		} {
			if !slices.Equal(pair[0], pair[1]) {
				t.Errorf("spec.%s = %q, want %q", name, pair[0], pair[1])
			}
		}

		wantOpts := nelmclient.TemplateOptions{KubeVersion: "1.30.0", APIVersions: []string{"example.com/v1", "monitoring.coreos.com/v1"}}
		if got := client.opts[0]; got.Remote || got.KubeVersion != wantOpts.KubeVersion || !slices.Equal(got.APIVersions, wantOpts.APIVersions) {
			t.Errorf("options = %+v, want %+v", got, wantOpts)
		}

		if client.timeouts[0] != defaultReadTimeout {
			t.Errorf("timeout = %s, want the default %s", client.timeouts[0], defaultReadTimeout)
		}
	})

	t.Run("validate, timeouts.read", func(t *testing.T) {
		m := model
		m.Validate = types.BoolValue(true)
		m.Timeouts = timeouts.Value{Object: types.ObjectValueMust(templateTimeoutsAttrTypes, map[string]attr.Value{
			"read": types.StringValue("2m"),
		})}

		client := &fakeTemplateClient{}
		mustReadTemplate(t, client, m)

		if got := client.opts[0]; !got.Remote || got.KubeVersion != "" || len(got.APIVersions) != 0 {
			t.Errorf("options = %+v, want a remote render without capabilities overrides", got)
		}

		if client.timeouts[0] != 2*time.Minute {
			t.Errorf("timeout = %s, want 2m", client.timeouts[0])
		}
	})

	t.Run("relative chart path is made absolute", func(t *testing.T) {
		m := model
		m.Chart = types.StringValue("../../testdata/charts/template")

		client := &fakeTemplateClient{}
		mustReadTemplate(t, client, m)

		want, err := filepath.Abs("../../testdata/charts/template")
		if err != nil {
			t.Fatal(err)
		}

		if client.specs[0].Chart != want {
			t.Errorf("spec.Chart = %q, want %q", client.specs[0].Chart, want)
		}
	})
}

func TestTemplateValidateConfig(t *testing.T) {
	tests := []struct {
		name        string
		kubeVersion types.String
		validate    types.Bool
		wantErr     bool
	}{
		{name: "kube_version offline", kubeVersion: types.StringValue("1.30.0"), validate: types.BoolValue(false)},
		{name: "kube_version, validate unset", kubeVersion: types.StringValue("1.30.0"), validate: types.BoolNull()},
		{name: "validate without kube_version", kubeVersion: types.StringNull(), validate: types.BoolValue(true)},
		{name: "kube_version with validate", kubeVersion: types.StringValue("1.30.0"), validate: types.BoolValue(true), wantErr: true},
		{name: "unknown kube_version with validate", kubeVersion: types.StringUnknown(), validate: types.BoolValue(true)},
		{name: "kube_version with unknown validate", kubeVersion: types.StringValue("1.30.0"), validate: types.BoolUnknown()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			model := baseTestTemplateModel()
			model.KubeVersion = tt.kubeVersion
			model.Validate = tt.validate
			// An unknown set must not trip the validation up.
			model.Values = types.ListUnknown(types.StringType)

			var resp datasource.ValidateConfigResponse
			(&templateDataSource{}).ValidateConfig(ctx, datasource.ValidateConfigRequest{Config: templateConfig(t, ctx, model)}, &resp)

			if !tt.wantErr {
				if resp.Diagnostics.HasError() {
					t.Fatalf("unexpected error diagnostics: %v", resp.Diagnostics)
				}

				return
			}

			errs := resp.Diagnostics.Errors()
			if len(errs) != 1 {
				t.Fatalf("errors = %v, want exactly one", resp.Diagnostics)
			}

			withPath, ok := errs[0].(diag.DiagnosticWithPath)
			if !ok || !withPath.Path().Equal(path.Root("kube_version")) {
				t.Errorf("error = %v, want it on kube_version", errs[0])
			}
		})
	}
}

func TestTemplateConfigure(t *testing.T) {
	ctx := context.Background()

	t.Run("nil provider data is a no-op", func(t *testing.T) {
		d := &templateDataSource{}

		var resp datasource.ConfigureResponse
		d.Configure(ctx, datasource.ConfigureRequest{}, &resp)

		if resp.Diagnostics.HasError() || d.client != nil {
			t.Fatalf("diags = %v, client = %v, want neither", resp.Diagnostics, d.client)
		}
	})

	t.Run("client", func(t *testing.T) {
		d := &templateDataSource{}
		client := nelmclient.NewNoConnectionClient(nil, errors.New("no cluster"))

		var resp datasource.ConfigureResponse
		d.Configure(ctx, datasource.ConfigureRequest{ProviderData: client}, &resp)

		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected error diagnostics: %v", resp.Diagnostics)
		}

		if got, ok := d.client.(*nelmclient.Client); !ok || got != client {
			t.Errorf("client = %#v, want the provider's client", d.client)
		}
	})

	t.Run("wrong type", func(t *testing.T) {
		var resp datasource.ConfigureResponse
		(&templateDataSource{}).Configure(ctx, datasource.ConfigureRequest{ProviderData: "nope"}, &resp)

		if got := diagSummaries(resp.Diagnostics, diag.SeverityError); !slices.Equal(got, []string{"Unexpected Data Source Configure Type"}) {
			t.Errorf("errors = %v, want the configure type error", got)
		}
	})
}

// TestTemplateDataSource_Registered: the provider serves nelm_template, and
// its schema passes the framework's own implementation checks.
func TestTemplateDataSource_Registered(t *testing.T) {
	ctx := context.Background()

	var names []string

	for _, newDS := range (&nelmProvider{}).DataSources(ctx) {
		var resp datasource.MetadataResponse
		newDS().Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: "nelm"}, &resp)
		names = append(names, resp.TypeName)
	}

	if !slices.Contains(names, "nelm_template") {
		t.Errorf("data sources = %v, want nelm_template", names)
	}

	if diags := templateDataSourceSchema(ctx).ValidateImplementation(ctx); diags.HasError() {
		t.Errorf("schema: %v", diags)
	}
}

// TestTemplateRead_RealClientOffline renders testdata/charts/template with the
// real nelm through the client a provider block that names no cluster gets:
// an offline render needs none.
func TestTemplateRead_RealClientOffline(t *testing.T) {
	if err := nelmclient.Init(context.Background()); err != nil {
		t.Fatalf("nelmclient.Init: %v", err)
	}

	client := nelmclient.NewNoConnectionClient(nil, errors.New("the nelm provider configuration names no cluster"))

	model := baseTestTemplateModel()
	model.Chart = types.StringValue("../../testdata/charts/template")
	model.KubeVersion = types.StringValue("1.30.0")
	model.APIVersions = stringList("example.com/v1")
	model.SkipTests = types.BoolValue(true)
	model.SetSensitive = []setModel{
		{Name: types.StringValue("secret.password"), Value: types.StringValue("rendered-in-cleartext"), Type: types.StringNull()},
	}

	got := mustReadTemplate(t, client, model)

	if got.ID.ValueString() != "default/my-release" {
		t.Errorf("id = %v, want default/my-release", got.ID)
	}

	manifests := stringMap(t, got.Manifests)

	wantKeys := []string{"charts/sub/templates/configmap.yaml", "templates/configmap.yaml", "templates/hooks.yaml", "templates/secret.yaml"}
	if keys := slices.Sorted(maps.Keys(manifests)); !slices.Equal(keys, wantKeys) {
		t.Errorf("manifests keys = %v, want %v", keys, wantKeys)
	}

	for _, want := range []string{
		"# Source: template/templates/configmap.yaml\n",
		`kubeVersion: v1.30.0`,
		`hasWidgetAPI: "true"`,
		`isUpgrade: "false"`,
		"name: my-release-sub",
		"name: my-release-pre-install",
		// nelm folds stringData into data.
		"password: " + base64.StdEncoding.EncodeToString([]byte("rendered-in-cleartext")),
	} {
		if !strings.Contains(got.Manifest.ValueString(), want) {
			t.Errorf("manifest has no %q:\n%s", want, got.Manifest.ValueString())
		}
	}

	for _, unwanted := range []string{"my-release-test", "kind: CustomResourceDefinition"} {
		if strings.Contains(got.Manifest.ValueString(), unwanted) {
			t.Errorf("manifest has %q (skip_tests / include_crds unset):\n%s", unwanted, got.Manifest.ValueString())
		}
	}

	// manifest is manifests' values in nelm's order: every value is a run of
	// manifest's documents.
	for key, value := range manifests {
		if !strings.HasPrefix(value, "---\n# Source: template/"+key+"\n") || !strings.Contains(got.Manifest.ValueString(), value) {
			t.Errorf("manifests[%q] is not a run of manifest's documents:\n%s", key, value)
		}
	}

	crds := listStrings(t, got.CRDs)
	if len(crds) != 1 || !strings.Contains(crds[0], "name: widgets.example.com") || strings.Contains(crds[0], "# Source:") {
		t.Errorf("crds = %q, want the one Widget CRD's YAML without a Source line", crds)
	}
}

// TestTemplateRead_RealClientOfflineNamespaces renders testdata/charts/namespaces
// offline with the real nelm, which strips the release namespace, and checks
// that Read restores it on every namespace-scoped object without one (hooks
// and custom resources whose CRD ships in crds/ included), leaves another
// namespace, cluster-scoped kinds and crds/ alone, and warns about the
// ServiceMonitor, whose kind neither Kubernetes nor the chart defines. It
// runs on both clients a provider block without a usable cluster gets: one
// that names no cluster, and one whose configuration (registries included)
// is not known yet, which renders a local chart all the same.
func TestTemplateRead_RealClientOfflineNamespaces(t *testing.T) {
	if err := nelmclient.Init(context.Background()); err != nil {
		t.Fatalf("nelmclient.Init: %v", err)
	}

	tests := []struct {
		name   string
		client *nelmclient.Client
	}{
		{name: "no connection", client: nelmclient.NewNoConnectionClient(nil, errors.New("the nelm provider configuration names no cluster"))},
		{name: "configuration and registries unknown", client: nelmclient.NewUnknownConfigClient(nil, false)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := baseTestTemplateModel()
			model.Namespace = types.StringValue("apps")
			model.Chart = types.StringValue("../../testdata/charts/namespaces")
			model.IncludeCRDs = types.BoolValue(true)

			got, diags := readTemplate(t, tt.client, model)
			if diags.HasError() {
				t.Fatalf("Read: unexpected error diagnostics: %v", diags)
			}

			want := map[string]string{
				"CustomResourceDefinition/gadgets.example.com": "",
				"Job/my-release-hook":                          "apps",
				"ServiceAccount/my-release-release-ns":         "apps",
				"ConfigMap/my-release-no-ns":                   "apps",
				"ConfigMap/my-release-other-ns":                "other",
				"ClusterRole/my-release-cluster":               "",
				"Gadget/my-release-gadget":                     "apps",
				"ServiceMonitor/my-release-monitor":            "",
			}
			if gotNS := manifestNamespaces(t, got.Manifest.ValueString()); !maps.Equal(gotNS, want) {
				t.Errorf("namespaces in manifest = %v, want %v", gotNS, want)
			}

			warnings := diags.Warnings()
			if len(warnings) != 1 || !strings.Contains(warnings[0].Detail(), "monitoring.coreos.com/v1, Kind=ServiceMonitor") {
				t.Fatalf("warnings = %v, want exactly one naming the ServiceMonitor kind", warnings)
			}

			for _, unwanted := range []string{"Gadget", "ConfigMap", "Job"} {
				if strings.Contains(warnings[0].Detail(), "Kind="+unwanted) {
					t.Errorf("warning detail = %q names the resolved kind %s", warnings[0].Detail(), unwanted)
				}
			}

			crds := listStrings(t, got.CRDs)
			if len(crds) != 1 || !strings.Contains(crds[0], "name: gadgets.example.com") || strings.Contains(crds[0], "namespace:") {
				t.Errorf("crds = %q, want the Gadget CRD as rendered, without a namespace", crds)
			}
		})
	}
}

// TestTemplateSchema_NamespaceNotEmpty pins that an explicit empty namespace
// is rejected: nelm would render it as its "stub-namespace" sentinel, and the
// release namespace could not be restored into the objects.
func TestTemplateSchema_NamespaceNotEmpty(t *testing.T) {
	ctx := context.Background()

	var resp datasource.SchemaResponse
	(&templateDataSource{}).Schema(ctx, datasource.SchemaRequest{}, &resp)

	attr, ok := resp.Schema.Attributes["namespace"].(dsschema.StringAttribute)
	if !ok {
		t.Fatalf("namespace attribute is %T, want a StringAttribute", resp.Schema.Attributes["namespace"])
	}

	for _, tt := range []struct {
		value   string
		wantErr bool
	}{{value: "", wantErr: true}, {value: "apps"}} {
		var diags diag.Diagnostics

		for _, v := range attr.Validators {
			vresp := validator.StringResponse{}
			v.ValidateString(ctx, validator.StringRequest{Path: path.Root("namespace"), ConfigValue: types.StringValue(tt.value)}, &vresp)
			diags.Append(vresp.Diagnostics...)
		}

		if diags.HasError() != tt.wantErr {
			t.Errorf("namespace %q: errors = %v, want error %v", tt.value, diags, tt.wantErr)
		}
	}
}
