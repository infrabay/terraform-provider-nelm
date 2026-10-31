package nelmclient

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/werf/nelm/pkg/action"
	"github.com/werf/nelm/pkg/common"
	"github.com/werf/nelm/pkg/helm/pkg/werf/helmopts"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// templateChart returns the absolute path of testdata/charts/template, the
// Template tests' fixture chart: a standalone CRD, a pre-install hook, a helm
// test Pod, a subchart, a Secret, and a ConfigMap echoing the capabilities and
// release state its templates see.
func templateChart(t *testing.T) string {
	t.Helper()

	chart, err := filepath.Abs("../../testdata/charts/template")
	if err != nil {
		t.Fatalf("abs chart path: %v", err)
	}

	return chart
}

// templateObjectSummary is the part of a TemplateObject the order tests
// compare.
type templateObjectSummary struct {
	Path          string
	Kind          string
	Name          string
	Hook          bool
	StandaloneCRD bool
}

func summarizeTemplateObjects(objs []TemplateObject) []templateObjectSummary {
	out := make([]templateObjectSummary, 0, len(objs))
	for _, obj := range objs {
		out = append(out, templateObjectSummary{
			Path:          obj.Path,
			Kind:          obj.Object.GetKind(),
			Name:          obj.Object.GetName(),
			Hook:          obj.Hook,
			StandaloneCRD: obj.StandaloneCRD,
		})
	}

	return out
}

// findTemplateObject returns the object of objs with the given kind and name,
// failing the test when there is none.
func findTemplateObject(t *testing.T, objs []TemplateObject, kind, name string) TemplateObject {
	t.Helper()

	for _, obj := range objs {
		if obj.Object.GetKind() == kind && obj.Object.GetName() == name {
			return obj
		}
	}

	t.Fatalf("no %s %q among %d rendered objects: %+v", kind, name, len(objs), summarizeTemplateObjects(objs))

	return TemplateObject{}
}

// TestTemplate_Offline renders the fixture chart offline on every kind of
// Client the provider hands out — a configured one, the no-connection one,
// and the unknown-config placeholder whose registries are known — and checks
// every object comes back (the standalone CRD, both hooks, the helm test Pod
// included, and the subchart's object) with nelm's path and flags, in nelm's
// order: standalone CRDs, hooks, then the rest, each group sorted by kind and
// name. An offline render never contacts a cluster: HOME and KUBECONFIG point
// at a kubeconfig whose current-context is a counting fake API server.
func TestTemplate_Offline(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	hits := trapAmbientKubeconfig(t)
	chart := templateChart(t)

	// A crds/ path carries the OS separator (helm's CRDObjects uses
	// filepath.Join), a templates/ path "/" on every OS (the engine uses
	// path.Join).
	want := []templateObjectSummary{
		{Path: filepath.Join("template", "crds", "widget.yaml"), Kind: "CustomResourceDefinition", Name: "widgets.example.com", StandaloneCRD: true},
		{Path: "template/templates/hooks.yaml", Kind: "ConfigMap", Name: "tmpl-pre-install", Hook: true},
		{Path: "template/templates/hooks.yaml", Kind: "Pod", Name: "tmpl-test", Hook: true},
		{Path: "template/charts/sub/templates/configmap.yaml", Kind: "ConfigMap", Name: "tmpl-sub"},
		{Path: "template/templates/configmap.yaml", Kind: "ConfigMap", Name: "tmpl-template"},
		{Path: "template/templates/secret.yaml", Kind: "Secret", Name: "tmpl-template"},
	}

	tests := []struct {
		name   string
		client *Client
	}{
		{name: "configured", client: NewClient(Config{})},
		{name: "no connection", client: NewNoConnectionClient(nil, errTestNoConnection)},
		{name: "unknown config", client: NewUnknownConfigClient(nil, true)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := ReleaseSpec{Name: "tmpl", Namespace: "default", Chart: chart}

			objs, err := tt.client.Template(ctx, spec, TemplateOptions{}, 30*time.Second)
			if err != nil {
				t.Fatalf("Template: %v", err)
			}

			if got := summarizeTemplateObjects(objs); !slices.Equal(got, want) {
				t.Errorf("rendered objects:\n got %+v\nwant %+v", got, want)
			}

			for _, obj := range objs {
				if obj.YAML == "" {
					t.Errorf("%s %s from %s: empty YAML", obj.Object.GetKind(), obj.Object.GetName(), obj.Path)
				}
			}
		})
	}

	if n := hits.Load(); n != 0 {
		t.Fatalf("an offline Template sent %d request(s) to the ambient ~/.kube/config cluster", n)
	}
}

// TestTemplate_OfflineCapabilities checks what an offline render's templates
// see: KubeVersion as .Capabilities.KubeVersion (normalized to a full
// version, nelm's own default when empty), APIVersions in
// .Capabilities.APIVersions, and a first install (nelm reads no release
// history offline, so never an upgrade, always revision 1).
func TestTemplate_OfflineCapabilities(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	isolateHome(t)

	c := NewClient(Config{})
	spec := ReleaseSpec{Name: "tmpl", Namespace: "default", Chart: templateChart(t)}

	tests := []struct {
		name string
		opts TemplateOptions
		want map[string]string
	}{
		{
			name: "defaults",
			opts: TemplateOptions{},
			want: map[string]string{
				"kubeVersion":  "v" + common.DefaultLocalKubeVersion,
				"hasWidgetAPI": "false",
				"isUpgrade":    "false",
				"revision":     "1",
			},
		},
		{
			name: "kube version and api versions",
			opts: TemplateOptions{KubeVersion: "v1.30", APIVersions: []string{"example.com/v1"}},
			want: map[string]string{
				"kubeVersion":  "v1.30.0",
				"hasWidgetAPI": "true",
				"isUpgrade":    "false",
				"revision":     "1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs, err := c.Template(ctx, spec, tt.opts, 30*time.Second)
			if err != nil {
				t.Fatalf("Template: %v", err)
			}

			cm := findTemplateObject(t, objs, "ConfigMap", "tmpl-template")

			for key, want := range tt.want {
				got, _, _ := unstructured.NestedString(cm.Object.Object, "data", key)
				if got != want {
					t.Errorf("data.%s = %q, want %q", key, got, want)
				}
			}
		})
	}
}

// TestTemplate_YAMLMatchesNelmPrintout pins TemplateObject.YAML to what
// `nelm chart render` prints: the data source promises a manifest
// byte-identical to that printout and builds it as "---\n# Source: <Path>\n"
// + YAML per object. The reference is action.ChartRender's own printout of
// the same chart with the same options, written to a file, with
// ShowStandaloneCRDs so the crds/ object is printed too. The values hold a
// multi-line string, so the comparison covers YAML block scalars.
func TestTemplate_YAMLMatchesNelmPrintout(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	isolateHome(t)

	chart := templateChart(t)
	values := "message: |\n  line one\n  line two: \"quoted\"\n"
	opts := TemplateOptions{KubeVersion: "v1.30", APIVersions: []string{"example.com/v1"}}

	objs, err := NewClient(Config{}).Template(ctx, ReleaseSpec{
		Name:       "tmpl",
		Namespace:  "default",
		Chart:      chart,
		ValuesYAML: []string{values},
	}, opts, 30*time.Second)
	if err != nil {
		t.Fatalf("Template: %v", err)
	}

	var got strings.Builder
	for _, obj := range objs {
		got.WriteString("---\n# Source: " + obj.Path + "\n" + obj.YAML)

		// The data source re-encodes the objects it changes with
		// ManifestYAML: an unchanged object must come out as YAML.
		if y, err := ManifestYAML(obj.Object); err != nil || y != obj.YAML {
			t.Errorf("%s %s: ManifestYAML = %q, %v; want YAML %q", obj.Object.GetKind(), obj.Object.GetName(), y, err, obj.YAML)
		}
	}

	dir := t.TempDir()
	valuesFile := filepath.Join(dir, "values.yaml")

	if err := os.WriteFile(valuesFile, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}

	printout := filepath.Join(dir, "render.yaml")

	if _, err := action.ChartRender(ctx, action.ChartRenderOptions{
		ValuesOptions:      common.ValuesOptions{ValuesFiles: []string{valuesFile}},
		Chart:              chart,
		ExtraAPIVersions:   opts.APIVersions,
		LocalKubeVersion:   opts.KubeVersion,
		OutputFilePath:     printout,
		ReleaseName:        "tmpl",
		ReleaseNamespace:   "default",
		ShowStandaloneCRDs: true,
		TempDirPath:        dir,
	}); err != nil {
		t.Fatalf("action.ChartRender: %v", err)
	}

	want, err := os.ReadFile(printout)
	if err != nil {
		t.Fatalf("read nelm's printout: %v", err)
	}

	if got.String() != string(want) {
		t.Errorf("Template objects do not concatenate to nelm's printout:\n--- got\n%s\n--- want\n%s", got.String(), want)
	}

	// Both renders must have taken the values, or the block scalar is not
	// covered at all.
	if !strings.Contains(string(want), "line one") {
		t.Errorf("nelm's printout does not carry the multi-line value:\n%s", want)
	}
}

// TestTemplate_DoesNotWriteToStdout is TestRender_DoesNotWriteToStdout for
// Template: the data source renders on every plan and refresh, and without
// OutputFilePath ChartRender prints every object — Secret data included —
// into the plugin's stdout pipe, which Terraform core logs at WARN. The
// fixture chart declares its subchart as a dependency without a Chart.lock,
// so this also covers the warning nelm's chart loader prints for that (Init
// clears it). It swaps the process-global os.Stdout, so it must not run in
// parallel.
func TestTemplate_DoesNotWriteToStdout(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	isolateHome(t)

	const password = "template-stdout-probe-password"

	spec := ReleaseSpec{
		Name:      "stdout",
		Namespace: "default",
		Chart:     templateChart(t),
		Set:       []string{"secret.password=" + password},
	}

	var (
		objs        []TemplateObject
		templateErr error
	)

	out := captureStdout(t, func() {
		objs, templateErr = NewClient(Config{}).Template(ctx, spec, TemplateOptions{}, 30*time.Second)
	})

	if templateErr != nil {
		t.Fatalf("Template: %v", templateErr)
	}

	if len(out) != 0 {
		t.Fatalf("Template wrote %d bytes to the process stdout (want 0):\n%s", len(out), out)
	}

	// The redirect must only move nelm's printout, not thin the result: the
	// Secret still comes back, rendered with the value passed in (nelm folds
	// the template's stringData into data).
	secret := findTemplateObject(t, objs, "Secret", "stdout-template")
	want := base64.StdEncoding.EncodeToString([]byte(password))

	if got, _, _ := unstructured.NestedString(secret.Object.Object, "data", "password"); got != want {
		t.Errorf("rendered Secret data.password = %q, want %q", got, want)
	}

	if !strings.Contains(secret.YAML, want) {
		t.Errorf("rendered Secret YAML does not carry the value passed in:\n%s", secret.YAML)
	}
}

// TestTemplate_Refusals checks which renders a clusterless Client refuses,
// and that it refuses before nelm runs: an error from nelm would be a chart
// or pull error rather than the refusal (the local chart does not exist, the
// registry host does not resolve), and the ambient kubeconfig trap counts
// any request. A remote render needs the cluster, so it fails with the
// client's own error; an offline one needs only the registries, and only
// for an oci:// chart, so only the placeholder whose registries are unknown
// refuses it, and only for an oci:// reference — a full one, or an oci://
// repository with a bare chart name, which NormalizeChartRef folds into one.
func TestTemplate_Refusals(t *testing.T) {
	hits := trapAmbientKubeconfig(t)

	ctx := context.Background()
	localChart := filepath.Join(t.TempDir(), "no-such-chart")

	tests := []struct {
		name       string
		client     *Client
		chart      string
		repository string
		remote     bool
		want       error
	}{
		{
			name:   "remote, no connection",
			client: NewNoConnectionClient(nil, errTestNoConnection),
			chart:  localChart,
			remote: true,
			want:   errTestNoConnection,
		},
		{
			name:   "remote, unknown config",
			client: NewUnknownConfigClient(nil, true),
			chart:  localChart,
			remote: true,
			want:   ErrConfigUnknown,
		},
		{
			name:   "offline, unknown config and registries, oci:// chart",
			client: NewUnknownConfigClient(nil, false),
			chart:  "oci://registry.invalid/charts/app",
			want:   ErrConfigUnknown,
		},
		{
			name:       "offline, unknown config and registries, oci:// repository",
			client:     NewUnknownConfigClient(nil, false),
			chart:      "app",
			repository: "oci://registry.invalid/charts",
			want:       ErrConfigUnknown,
		},
		{
			name:   "remote, unknown config and registries",
			client: NewUnknownConfigClient(nil, false),
			chart:  localChart,
			remote: true,
			want:   ErrConfigUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := ReleaseSpec{
				Name:          "app",
				Namespace:     "default",
				Chart:         tt.chart,
				Repository:    tt.repository,
				StorageDriver: "secret",
			}

			_, err := tt.client.Template(ctx, spec, TemplateOptions{Remote: tt.remote}, 0)
			if err != tt.want {
				t.Errorf("Template: err = %v, want %v", err, tt.want)
			}
		})
	}

	if n := hits.Load(); n != 0 {
		t.Fatalf("a refused Template sent %d request(s) to the ambient ~/.kube/config cluster", n)
	}
}

// TestTemplate_UnknownRegistriesNonOCI checks that the placeholder whose
// registries are unknown still renders offline what needs no registry
// credentials: a local chart, and a chart from a classic repository (which
// authenticates with its own basic auth), here a local one that must have
// served both its index and the chart archive.
func TestTemplate_UnknownRegistriesNonOCI(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	hits := trapAmbientKubeconfig(t)
	c := NewUnknownConfigClient(nil, false)

	t.Run("local chart", func(t *testing.T) {
		spec := ReleaseSpec{Name: "tmpl", Namespace: "default", Chart: templateChart(t)}

		objs, err := c.Template(ctx, spec, TemplateOptions{}, 30*time.Second)
		if err != nil {
			t.Fatalf("Template: %v", err)
		}

		findTemplateObject(t, objs, "ConfigMap", "tmpl-template")
	})

	t.Run("classic repository", func(t *testing.T) {
		const index = "apiVersion: v1\nentries:\n  app:\n  - apiVersion: v2\n    name: app\n    version: 1.0.0\n    urls:\n    - app-1.0.0.tgz\n"

		archive := chartArchive(t, "app", "1.0.0", "classic-repository")

		var (
			mu     sync.Mutex
			served []string
		)

		repo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			served = append(served, r.URL.Path)
			mu.Unlock()

			switch r.URL.Path {
			case "/index.yaml":
				_, _ = w.Write([]byte(index))
			case "/app-1.0.0.tgz":
				_, _ = w.Write(archive)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(repo.Close)

		spec := ReleaseSpec{Name: "app", Namespace: "default", Chart: "app", Repository: repo.URL, Version: "1.0.0"}

		objs, err := c.Template(ctx, spec, TemplateOptions{}, 30*time.Second)
		if err != nil {
			t.Fatalf("Template: %v", err)
		}

		cm := findTemplateObject(t, objs, "ConfigMap", "app")
		if got, _, _ := unstructured.NestedString(cm.Object.Object, "data", "source"); got != "classic-repository" {
			t.Errorf("data.source = %q, want %q", got, "classic-repository")
		}

		mu.Lock()
		defer mu.Unlock()

		for _, path := range []string{"/index.yaml", "/app-1.0.0.tgz"} {
			if !slices.Contains(served, path) {
				t.Errorf("the chart repository never served %s; requests: %v", path, served)
			}
		}
	})

	if n := hits.Load(); n != 0 {
		t.Fatalf("an offline Template sent %d request(s) to the ambient ~/.kube/config cluster", n)
	}
}

// TestTemplate_Remote renders the fixture chart with Remote against a fake
// API server (validate = true): the capabilities are the cluster's (its
// /version, not nelm's offline default), and the release storage is nelm's
// in-memory driver whatever ReleaseSpec.StorageDriver says, so the templates
// see a first install at revision 1 and no release Secret is ever listed —
// helm_template's semantics, deterministic, and no Secret RBAC needed.
func TestTemplate_Remote(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var (
		mu    sync.Mutex
		paths []string
	)

	c := NewClient(fakeKubeConfigWith(t, func(_ http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()

		paths = append(paths, r.URL.Path)

		return false
	}))

	spec := ReleaseSpec{Name: "tmpl", Namespace: "default", Chart: templateChart(t), StorageDriver: "secret"}

	objs, err := c.Template(ctx, spec, TemplateOptions{Remote: true}, 30*time.Second)
	if err != nil {
		t.Fatalf("Template: %v", err)
	}

	cm := findTemplateObject(t, objs, "ConfigMap", "tmpl-template")

	for key, want := range map[string]string{
		"kubeVersion": "v1.29.3",
		"isUpgrade":   "false",
		"revision":    "1",
	} {
		got, _, _ := unstructured.NestedString(cm.Object.Object, "data", key)
		if got != want {
			t.Errorf("data.%s = %q, want %q", key, got, want)
		}
	}

	mu.Lock()
	defer mu.Unlock()

	if !slices.Contains(paths, "/version") {
		t.Errorf("the fake API server never served /version; requests: %v", paths)
	}

	for _, path := range paths {
		if strings.HasSuffix(path, "/secrets") {
			t.Errorf("a remote Template read the release storage (%s); requests: %v", path, paths)
		}
	}
}

// TestTemplate_StripsReleaseNamespace pins what nelm does to namespaces,
// which the data source corrects: it removes metadata.namespace from every
// object whose namespace equals the release namespace — written by the
// template or not, a custom resource of a kind the chart does not define
// included — and keeps any other namespace. The data source restores the
// release namespace on the namespace-scoped objects; if nelm ever stops
// stripping (or strips more), this fails first.
func TestTemplate_StripsReleaseNamespace(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	isolateHome(t)

	chart, err := filepath.Abs("../../testdata/charts/namespaces")
	if err != nil {
		t.Fatalf("abs chart path: %v", err)
	}

	spec := ReleaseSpec{Name: "ns", Namespace: "apps", Chart: chart}

	objs, err := NewClient(Config{}).Template(ctx, spec, TemplateOptions{}, 30*time.Second)
	if err != nil {
		t.Fatalf("Template: %v", err)
	}

	tests := []struct {
		kind, name, wantNamespace string
	}{
		{kind: "CustomResourceDefinition", name: "gadgets.example.com"},
		{kind: "Job", name: "ns-hook"},
		{kind: "ServiceAccount", name: "ns-release-ns"},
		{kind: "ConfigMap", name: "ns-no-ns"},
		{kind: "ConfigMap", name: "ns-other-ns", wantNamespace: "other"},
		{kind: "ClusterRole", name: "ns-cluster"},
		{kind: "Gadget", name: "ns-gadget"},
		{kind: "ServiceMonitor", name: "ns-monitor"},
	}

	if len(objs) != len(tests) {
		t.Errorf("rendered %d objects, want %d: %+v", len(objs), len(tests), summarizeTemplateObjects(objs))
	}

	for _, tt := range tests {
		obj := findTemplateObject(t, objs, tt.kind, tt.name)

		if got := obj.Object.GetNamespace(); got != tt.wantNamespace {
			t.Errorf("%s %s: metadata.namespace = %q, want %q", tt.kind, tt.name, got, tt.wantNamespace)
		}

		if tt.wantNamespace == "" && strings.Contains(obj.YAML, "namespace:") {
			t.Errorf("%s %s: YAML carries a namespace:\n%s", tt.kind, tt.name, obj.YAML)
		}
	}
}

// writeChartFiles writes files (chart-relative path to content) under dir,
// creating the directories they need.
func writeChartFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))

		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTemplate_CRDsDirectory checks StandaloneCRD means "from a crds/
// directory", whatever the kind (helm's file-based rule): a crds/ file with
// a CustomResourceDefinition and a ServiceAccount marks both, and an object
// from templates/ neither. nelm's own printout would still show that
// ServiceAccount without ShowStandaloneCRDs; the data source follows helm.
func TestTemplate_CRDsDirectory(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	isolateHome(t)

	chart := filepath.Join(t.TempDir(), "crds")

	files := map[string]string{
		"Chart.yaml": "apiVersion: v2\nname: crds\nversion: 0.1.0\n",
		"crds/crds.yaml": `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: gizmos.example.com
spec:
  group: example.com
  names:
    kind: Gizmo
    plural: gizmos
    singular: gizmo
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: from-crds
`,
		"templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: from-templates\n",
	}

	writeChartFiles(t, chart, files)

	objs, err := NewClient(Config{}).Template(ctx, ReleaseSpec{Name: "crds", Namespace: "default", Chart: chart}, TemplateOptions{}, 30*time.Second)
	if err != nil {
		t.Fatalf("Template: %v", err)
	}

	// The crds/ path carries the OS separator (helm's CRDObjects uses
	// filepath.Join), the templates/ path "/" on every OS (the engine uses
	// path.Join).
	crdsPath := filepath.Join("crds", "crds", "crds.yaml")

	want := []templateObjectSummary{
		{Path: crdsPath, Kind: "CustomResourceDefinition", Name: "gizmos.example.com", StandaloneCRD: true},
		{Path: crdsPath, Kind: "ServiceAccount", Name: "from-crds", StandaloneCRD: true},
		{Path: "crds/templates/configmap.yaml", Kind: "ConfigMap", Name: "from-templates"},
	}

	if got := summarizeTemplateObjects(objs); !slices.Equal(got, want) {
		t.Errorf("rendered objects:\n got %+v\nwant %+v", got, want)
	}
}

// TestTemplate_DependenciesMustBeVendored pins that Template renders a local
// chart only from the dependencies vendored in its charts/ directory, as
// helm_template does without dependency_update: whatever its Chart.lock says
// and whatever the provider's registries, nothing is downloaded (a local
// registry counts every request), a dependency charts/ lacks fails the read
// with ErrChartDependenciesMissing, and the chart directory is only read.
// nelm_release's Render, by contrast, keeps nelm's own behavior and builds a
// locked dependency charts/ lacks.
func TestTemplate_DependenciesMustBeVendored(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var pulls atomic.Int64

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pulls.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(registry.Close)

	repo := "oci://" + strings.TrimPrefix(registry.URL, "http://") + "/charts"
	chartYAML := "apiVersion: v2\nname: app\nversion: 0.1.0\ndependencies:\n- name: common\n  version: 1.0.0\n  repository: " + repo + "\n"
	lock := "dependencies:\n- name: common\n  repository: " + repo + "\n  version: 1.0.0\ndigest: sha256:0\ngenerated: \"2026-01-01T00:00:00Z\"\n"

	tests := []struct {
		name  string
		files map[string]string
		// wantMissing expects ErrChartDependenciesMissing, else a render
		// holding the dependency's ConfigMap.
		wantMissing bool
	}{
		{
			name:        "locked, not vendored",
			files:       map[string]string{"Chart.yaml": chartYAML, "Chart.lock": lock},
			wantMissing: true,
		},
		{
			name:        "not locked, not vendored",
			files:       map[string]string{"Chart.yaml": chartYAML},
			wantMissing: true,
		},
		{
			name:  "locked, vendored archive",
			files: map[string]string{"Chart.yaml": chartYAML, "Chart.lock": lock, "charts/common-1.0.0.tgz": string(chartArchive(t, "common", "1.0.0", "vendored"))},
		},
		{
			// nelm's loader only counts an archive as vendored and would
			// build this one; helm counts the unpacked chart too.
			name: "locked, vendored unpacked",
			files: map[string]string{
				"Chart.yaml":                             chartYAML,
				"Chart.lock":                             lock,
				"charts/common/Chart.yaml":               "apiVersion: v2\nname: common\nversion: 1.0.0\n",
				"charts/common/templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: common\ndata:\n  source: vendored\n",
			},
		},
		{
			name: "vendored archive ignored by .helmignore",
			files: map[string]string{
				"Chart.yaml":              chartYAML,
				"Chart.lock":              lock,
				"charts/common-1.0.0.tgz": string(chartArchive(t, "common", "1.0.0", "vendored")),
				".helmignore":             "*.tgz\n",
			},
			wantMissing: true,
		},
		{
			// A file:// dependency is not loaded from its path either:
			// like helm, it must be copied into charts/ first.
			name: "file:// dependency not vendored",
			files: map[string]string{
				"Chart.yaml":                         "apiVersion: v2\nname: app\nversion: 0.1.0\ndependencies:\n- name: common\n  version: 1.0.0\n  repository: file://../common\n",
				"Chart.lock":                         "dependencies:\n- name: common\n  repository: file://../common\n  version: 1.0.0\ndigest: sha256:0\ngenerated: \"2026-01-01T00:00:00Z\"\n",
				"../common/Chart.yaml":               "apiVersion: v2\nname: common\nversion: 1.0.0\n",
				"../common/templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: common\n",
			},
			wantMissing: true,
		},
		{
			// A v1 chart locks its requirements.yaml in requirements.lock,
			// which nelm's loader builds from just like Chart.lock.
			name: "v1 chart, requirements.lock, not vendored",
			files: map[string]string{
				"Chart.yaml":        "apiVersion: v1\nname: app\nversion: 0.1.0\n",
				"requirements.yaml": "dependencies:\n- name: common\n  version: 1.0.0\n  repository: " + repo + "\n",
				"requirements.lock": lock,
			},
			wantMissing: true,
		},
	}

	clients := map[string]*Client{
		"registries known":   NewClient(Config{Registries: []RegistryAuth{{URL: "registry.invalid", Username: "test", Password: "test"}}}),
		"registries unknown": NewUnknownConfigClient(nil, false),
	}

	for _, tt := range tests {
		for clientName, c := range clients {
			t.Run(tt.name+"/"+clientName, func(t *testing.T) {
				isolateHome(t)
				pulls.Store(0)

				chart := filepath.Join(t.TempDir(), "app")
				writeChartFiles(t, chart, tt.files)

				spec := ReleaseSpec{Name: "app", Namespace: "default", Chart: chart}

				objs, err := c.Template(ctx, spec, TemplateOptions{}, 30*time.Second)

				switch {
				case tt.wantMissing:
					if !errors.Is(err, ErrChartDependenciesMissing) || !strings.Contains(err.Error(), "common") {
						t.Errorf("Template: err = %v, want ErrChartDependenciesMissing naming common", err)
					}
				case err != nil:
					t.Errorf("Template: %v", err)
				default:
					rendered := false
					for _, obj := range objs {
						rendered = rendered || obj.Path == "app/charts/common/templates/configmap.yaml"
					}

					if !rendered {
						t.Errorf("the vendored dependency rendered nothing: %+v", summarizeTemplateObjects(objs))
					}
				}

				if n := pulls.Load(); n != 0 {
					t.Errorf("Template sent %d requests to the dependency's registry, want 0", n)
				}

				if _, err := os.Stat(filepath.Join(chart, "Chart.lock")); tt.files["Chart.lock"] != "" && err != nil {
					t.Errorf("the chart's Chart.lock is gone: %v", err)
				}

				if _, ok := tt.files["charts/common-1.0.0.tgz"]; !ok {
					if _, err := os.Stat(filepath.Join(chart, "charts", "common-1.0.0.tgz")); err == nil {
						t.Error("Template wrote a dependency archive into the chart directory")
					}
				}
			})
		}
	}

	t.Run("nelm_release keeps building locked dependencies", func(t *testing.T) {
		// The build refreshes the Helm repositories and caches under
		// ~/.werf: of a throwaway HOME, not the user's.
		isolateHome(t)
		pulls.Store(0)

		c := NewClient(fakeKubeConfig(t))

		chart := filepath.Join(t.TempDir(), "app")
		writeChartFiles(t, chart, map[string]string{"Chart.yaml": chartYAML, "Chart.lock": lock})

		_, _ = c.Render(ctx, ReleaseSpec{Name: "app", Namespace: "default", Chart: chart}, 30*time.Second)

		if pulls.Load() == 0 {
			t.Error("Render sent no request to the dependency's registry: nelm_release no longer builds locked dependencies")
		}
	})
}

// TestTemplate_MissingDependenciesClassification checks that only helm's
// CheckDependencies error itself makes ErrChartDependenciesMissing: a
// template that fails with the same words is a render failure like any
// other.
func TestTemplate_MissingDependenciesClassification(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	chart := t.TempDir()
	writeChartFiles(t, chart, map[string]string{
		"Chart.yaml":               "apiVersion: v2\nname: app\nversion: 0.1.0\n",
		"templates/configmap.yaml": `{{ fail "found in Chart.yaml, but missing in charts/ directory: common" }}`,
	})

	_, err := NewClient(Config{}).Template(ctx, ReleaseSpec{Name: "app", Namespace: "default", Chart: chart}, TemplateOptions{}, 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "execution error at (app/templates/configmap.yaml") {
		t.Fatalf("Template: err = %v, want the template's fail", err)
	}

	if errors.Is(err, ErrChartDependenciesMissing) {
		t.Errorf("Template: err = %v, classified as ErrChartDependenciesMissing", err)
	}
}

// TestIsMissingDependencies_JoinedErrors checks that the classification
// finds helm's CheckDependencies error in a joined error's branches too.
func TestIsMissingDependencies_JoinedErrors(t *testing.T) {
	missing := errors.New(missingDependenciesPrefix + "common")
	err := fmt.Errorf("chart render: %w", errors.Join(errors.New("lock conflict"), fmt.Errorf("render chart: %w", missing)))

	if !isMissingDependencies(err) {
		t.Errorf("isMissingDependencies(%q) = false, want true", err)
	}
}

// TestTemplate_ChartMetadataRequired checks that a local chart directory
// whose metadata lacks a name, a version or Chart.yaml altogether fails the
// render with the chart validation error, as nelm's default chart type
// fails it, rather than rendering with the chart stub type's defaults
// ("stubchartname", 1.0.0).
func TestTemplate_ChartMetadataRequired(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	configMap := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"

	tests := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{
			name:    "no Chart.yaml",
			files:   map[string]string{"templates/configmap.yaml": configMap},
			wantErr: "chart.metadata.apiVersion is required",
		},
		{
			name:    "no name",
			files:   map[string]string{"Chart.yaml": "apiVersion: v2\nversion: 0.1.0\n", "templates/configmap.yaml": configMap},
			wantErr: "chart.metadata.name is required",
		},
		{
			name:    "no version",
			files:   map[string]string{"Chart.yaml": "apiVersion: v2\nname: app\n", "templates/configmap.yaml": configMap},
			wantErr: "chart.metadata.version is required",
		},
		{
			name: "Chart.yaml ignored by .helmignore",
			files: map[string]string{
				"Chart.yaml":               "apiVersion: v2\nname: app\nversion: 0.1.0\n",
				".helmignore":              "Chart.yaml\n",
				"templates/configmap.yaml": configMap,
			},
			wantErr: "chart.metadata.apiVersion is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateHome(t)

			chart := filepath.Join(t.TempDir(), "app")
			writeChartFiles(t, chart, tt.files)

			objs, err := NewClient(Config{}).Template(ctx, ReleaseSpec{Name: "app", Namespace: "default", Chart: chart}, TemplateOptions{}, 30*time.Second)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Template: err = %v, want %q; rendered %+v", err, tt.wantErr, summarizeTemplateObjects(objs))
			}
		})
	}
}

// TestTemplate_V1ChartFiles checks that a v1 chart's requirements.lock is in
// its .Files, as helm loads it, although Template never builds the
// dependencies it locks.
func TestTemplate_V1ChartFiles(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	isolateHome(t)

	// Nothing listens on port 1: a build of the locked dependency fails at
	// once.
	repo := "oci://127.0.0.1:1/charts"
	lock := "dependencies:\n- name: common\n  repository: " + repo + "\n  version: 1.0.0\ndigest: sha256:0\ngenerated: \"2026-01-01T00:00:00Z\"\n"

	chart := filepath.Join(t.TempDir(), "app")
	writeChartFiles(t, chart, map[string]string{
		"Chart.yaml":                             "apiVersion: v1\nname: app\nversion: 0.1.0\n",
		"requirements.yaml":                      "dependencies:\n- name: common\n  version: 1.0.0\n  repository: " + repo + "\n",
		"requirements.lock":                      lock,
		"charts/common/Chart.yaml":               "apiVersion: v1\nname: common\nversion: 1.0.0\n",
		"charts/common/templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: common\n",
		"templates/configmap.yaml":               "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: lock\ndata:\n  lock: {{ .Files.Get \"requirements.lock\" | b64enc | quote }}\n",
	})

	objs, err := NewClient(Config{}).Template(ctx, ReleaseSpec{Name: "app", Namespace: "default", Chart: chart}, TemplateOptions{}, 30*time.Second)
	if err != nil {
		t.Fatalf("Template: %v", err)
	}

	got, _, _ := unstructured.NestedString(findTemplateObject(t, objs, "ConfigMap", "lock").Object.Object, "data", "lock")
	if want := base64.StdEncoding.EncodeToString([]byte(lock)); got != want {
		t.Errorf(".Files.Get \"requirements.lock\" = %q (base64), want %q", got, want)
	}
}

// TestTemplate_StubRendersAsDefault pins that a local chart directory renders
// the same under nelm's chart stub type (noDependencyBuild) as under its
// default type, whose branches of nelm's chart loader differ: the objects,
// their paths and storage, and a dump of what the templates see (.Chart,
// .Values, .Files, .Subcharts, .Release, .Capabilities). The charts build
// nothing under the default type either: they have no lock file, or every
// dependency they lock is vendored as an archive.
func TestTemplate_StubRendersAsDefault(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	dump := `apiVersion: v1
kind: ConfigMap
metadata:
  name: dump
data:
  chart: {{ toJson .Chart | quote }}
  values: {{ toJson .Values | quote }}
  files: "{{ range $p, $_ := .Files.Glob "**" }}{{ $p }},{{ end }}"
  requirementsLock: {{ .Files.Get "requirements.lock" | quote }}
  chartLock: {{ .Files.Get "Chart.lock" | quote }}
  basePath: {{ .Template.BasePath }}
  release: {{ toJson .Release | quote }}
  capabilities: {{ .Capabilities.KubeVersion.Version }} {{ .Capabilities.HelmVersion.Version }}
  subcharts: "{{ range $k, $_ := .Subcharts }}{{ $k }},{{ end }}"
  bom: {{ .Files.Get "extra/data.txt" | b64enc | quote }}
`

	// Nothing listens on port 1: a build of a locked dependency fails at
	// once.
	repo := "oci://127.0.0.1:1/charts"
	lock := "dependencies:\n- name: common\n  repository: " + repo + "\n  version: 1.0.0\ndigest: sha256:0\ngenerated: \"2026-01-01T00:00:00Z\"\n"
	archive := string(chartArchive(t, "common", "1.0.0", "vendored"))

	tests := []struct {
		name string
		// files is nil for the fixture chart, templateChart.
		files map[string]string
	}{
		{name: "fixture"},
		{
			name: "v2, Chart.lock, vendored archive",
			files: map[string]string{
				"Chart.yaml":              "apiVersion: v2\nname: app\nversion: 0.1.0\nappVersion: \"2.0\"\ndependencies:\n- name: common\n  version: 1.0.0\n  repository: " + repo + "\n",
				"Chart.lock":              lock,
				"charts/common-1.0.0.tgz": archive,
				"values.yaml":             "a: 1\ncommon:\n  x: y\n",
				"values.schema.json":      `{"type":"object"}`,
				"templates/dump.yaml":     dump,
				"templates/_helpers.tpl":  `{{- define "x" -}}x{{- end -}}`,
				"templates/NOTES.txt":     "notes",
				"templates/tests/t.yaml":  "apiVersion: v1\nkind: Pod\nmetadata:\n  name: t\n  annotations:\n    helm.sh/hook: test\nspec:\n  containers: []\n",
				"crds/crd.yaml":           "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.example.com\n",
				".helmignore":             "ignored.txt\n",
				"ignored.txt":             "ignored",
				"extra/data.txt":          "\xEF\xBB\xBF\xEF\xBB\xBFbom",
			},
		},
		{
			name: "v1, requirements.lock, vendored archive",
			files: map[string]string{
				"Chart.yaml":              "apiVersion: v1\nname: app\nversion: 0.1.0\n",
				"requirements.yaml":       "dependencies:\n- name: common\n  version: 1.0.0\n  repository: " + repo + "\n",
				"requirements.lock":       lock,
				"charts/common-1.0.0.tgz": archive,
				"templates/dump.yaml":     dump,
			},
		},
		{
			name: "v1, no lock, unpacked subchart",
			files: map[string]string{
				"Chart.yaml":                      "apiVersion: v1\nname: app\nversion: 0.1.0\n",
				"requirements.yaml":               "dependencies:\n- name: common\n  version: 1.0.0\n  repository: " + repo + "\n",
				"charts/common/Chart.yaml":        "apiVersion: v2\nname: common\nversion: 1.0.0\n",
				"charts/common/templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: common\n",
				"templates/dump.yaml":             dump,
			},
		},
	}

	render := func(t *testing.T, chart string, prepare func(*action.ChartRenderOptions) error) string {
		t.Helper()

		spec := ReleaseSpec{Name: "app", Namespace: "default", Chart: chart, Set: []string{"a=2"}}

		result, err := NewClient(Config{}).chartRender(ctx, "nelm-template-", spec, 30*time.Second, prepare, func(*action.ChartRenderOptions) {})
		if err != nil {
			t.Fatalf("chartRender: %v", err)
		}

		var b strings.Builder

		for _, res := range result.Resources {
			y, err := ManifestYAML(res.Unstruct)
			if err != nil {
				t.Fatalf("ManifestYAML: %v", err)
			}

			b.WriteString("# " + res.FilePath + " " + string(res.StoreAs) + "\n" + y + "---\n")
		}

		return b.String()
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateHome(t)

			chart := templateChart(t)
			if tt.files != nil {
				chart = filepath.Join(t.TempDir(), "app")
				writeChartFiles(t, chart, tt.files)
			}

			if def, stub := render(t, chart, nil), render(t, chart, noDependencyBuild); def != stub {
				t.Errorf("the chart stub type renders differently:\n--- default type ---\n%s\n--- stub type ---\n%s", def, stub)
			}
		})
	}
}

// TestTemplate_SetsNoOptionTheStubIgnores pins that Template loads a local
// chart directory with nelm's chart stub type and sets none of the options
// that type ignores (see noDependencyBuild), offline and remote alike: a
// local chart would render as if the option were unset, unlike an archive.
func TestTemplate_SetsNoOptionTheStubIgnores(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	isolateHome(t)

	// Appended on the render's goroutine, read once Template has returned,
	// which it does only after receiving that goroutine's result.
	var got []action.ChartRenderOptions

	orig := chartRenderAction
	chartRenderAction = func(ctx context.Context, opts action.ChartRenderOptions) (*action.ChartRenderResultV2, error) {
		got = append(got, opts)
		return orig(ctx, opts)
	}
	t.Cleanup(func() { chartRenderAction = orig })

	spec := ReleaseSpec{Name: "tmpl", Namespace: "default", Chart: templateChart(t), Set: []string{"message=set"}}

	renders := []struct {
		client *Client
		opts   TemplateOptions
	}{
		{NewClient(Config{}), TemplateOptions{}},
		{NewClient(Config{}), TemplateOptions{KubeVersion: "1.30.0", APIVersions: []string{"example.com/v1"}}},
		{NewClient(fakeKubeConfig(t)), TemplateOptions{Remote: true, APIVersions: []string{"example.com/v1"}}},
	}

	for _, r := range renders {
		if _, err := r.client.Template(ctx, spec, r.opts, 30*time.Second); err != nil {
			t.Fatalf("Template(%+v): %v", r.opts, err)
		}
	}

	if len(got) != len(renders) {
		t.Fatalf("nelm rendered %d times, want %d", len(got), len(renders))
	}

	for i, opts := range got {
		if opts.LegacyChartType != helmopts.ChartTypeChartStub {
			t.Errorf("render %d: LegacyChartType = %q, want the chart stub type", i, opts.LegacyChartType)
		}

		if opts.ChartAppVersion != "" || opts.DefaultValuesDisable || opts.LegacyExtraValues != nil ||
			opts.DefaultChartAPIVersion != "" || opts.DefaultChartName != "" || opts.DefaultChartVersion != "" {
			t.Errorf("render %d sets an option the chart stub type ignores: ChartAppVersion %q, DefaultValuesDisable %t, "+
				"LegacyExtraValues %v, DefaultChartAPIVersion %q, DefaultChartName %q, DefaultChartVersion %q",
				i, opts.ChartAppVersion, opts.DefaultValuesDisable, opts.LegacyExtraValues,
				opts.DefaultChartAPIVersion, opts.DefaultChartName, opts.DefaultChartVersion)
		}
	}
}
