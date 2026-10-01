package nelmclient

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/werf/nelm/pkg/action"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// renderFixtureOffline renders a fixture chart with nelm's own chart-render
// action, offline: Remote false builds no kube client (lookup returns
// nothing, as on a first install), and HOME/KUBECONFIG point at empty temp
// files so no real kubeconfig or registry credential is ever read.
func renderFixtureOffline(t *testing.T, chart string) map[string]*unstructured.Unstructured {
	t.Helper()

	dir := t.TempDir()
	ctx, buf := captureCtx(context.Background())

	result, err := action.ChartRender(ctx, action.ChartRenderOptions{
		Chart:            chart,
		OutputFilePath:   filepath.Join(dir, "rendered.yaml"),
		OutputNoPrint:    true,
		ReleaseName:      "app",
		ReleaseNamespace: "apps",
		Remote:           false,
		TempDirPath:      dir,
	})
	if err != nil {
		t.Fatalf("render %s: %v", chart, tailErr(err, buf))
	}

	out := map[string]*unstructured.Unstructured{}
	for _, res := range result.Resources {
		out[res.Unstruct.GetKind()] = res.Unstruct
	}

	return out
}

// TestVolatileFixtureRendersDifferentlyEveryTime pins what
// testdata/charts/volatile stands for: rendered twice with the same inputs
// by nelm's own template engine, its lookup-guarded generated password (on a
// first install), its rollme annotation and its deploy-date timestamp come
// out different, while its ConfigMap is identical. The provider's
// volatility tests (release_volatile_test.go) model exactly this chart.
func TestVolatileFixtureRendersDifferentlyEveryTime(t *testing.T) {
	home := t.TempDir()
	kubeconfig := filepath.Join(home, "kubeconfig")

	if err := os.WriteFile(kubeconfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", kubeconfig)

	chart, err := filepath.Abs("../../testdata/charts/volatile")
	if err != nil {
		t.Fatal(err)
	}

	first := renderFixtureOffline(t, chart)
	second := renderFixtureOffline(t, chart)

	for _, kind := range []string{"ConfigMap", "Secret", "Deployment"} {
		if first[kind] == nil || second[kind] == nil {
			t.Fatalf("the fixture chart did not render a %s", kind)
		}
	}

	if !reflect.DeepEqual(first["ConfigMap"].Object, second["ConfigMap"].Object) {
		t.Errorf("ConfigMap renders differently:\n%v\n%v", first["ConfigMap"].Object, second["ConfigMap"].Object)
	}

	differs := func(obj string, fields ...string) {
		t.Helper()

		a, _, _ := unstructured.NestedString(first[obj].Object, fields...)
		b, _, _ := unstructured.NestedString(second[obj].Object, fields...)

		if a == "" || a == b {
			t.Errorf("%s %v = %q and %q, want two different values", obj, fields, a, b)
		}
	}

	differs("Secret", "data", "password")
	differs("Deployment", "spec", "template", "metadata", "annotations", "rollme")
	differs("Deployment", "spec", "template", "metadata", "annotations", "deploy-date")

	unstructured.RemoveNestedField(first["Deployment"].Object, "spec", "template", "metadata", "annotations")
	unstructured.RemoveNestedField(second["Deployment"].Object, "spec", "template", "metadata", "annotations")

	if !reflect.DeepEqual(first["Deployment"].Object, second["Deployment"].Object) {
		t.Errorf("the Deployment differs outside its volatile annotations:\n%v\n%v", first["Deployment"].Object, second["Deployment"].Object)
	}
}
