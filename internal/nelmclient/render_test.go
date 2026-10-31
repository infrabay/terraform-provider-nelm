package nelmclient

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/werf/nelm/pkg/action"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// TestRender_DoesNotWriteToStdout is the regression test for nelm's
// ChartRender printing every rendered manifest — Secret data included — to
// os.Stdout: it ignores OutputNoPrint and prints unless OutputFilePath is
// set. Inside a plugin process os.Stdout is a pipe that Terraform core logs
// at WARN ("unexpected data: <provider>:stdout=..."), so with TF_LOG on, every
// plan wrote base64 Secret data (and any set_sensitive value a chart renders)
// into CI logs. Render runs on every non-destroy plan, so this is checked
// through the real Client.Render against a fake API server, which must see
// requests: Render shares chartRender with the offline Template, and a
// Render that silently went offline would still pass the stdout check while
// planning with nelm's default capabilities and a stub lookup. It swaps the
// process-global os.Stdout, so it must not run in parallel.
func TestRender_DoesNotWriteToStdout(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var requests atomic.Int64

	c := NewClient(fakeKubeConfigWith(t, func(http.ResponseWriter, *http.Request) bool {
		requests.Add(1)
		return false
	}))

	chart, err := filepath.Abs("../../testdata/charts/basic")
	if err != nil {
		t.Fatalf("abs chart path: %v", err)
	}

	const password = "render-stdout-probe-password"

	spec := ReleaseSpec{
		Name:      "stdout",
		Namespace: "default",
		Chart:     chart,
		Set:       []string{"secret.password=" + password},
	}

	var (
		objs      []*unstructured.Unstructured
		renderErr error
	)

	out := captureStdout(t, func() {
		objs, renderErr = c.Render(ctx, spec, 30*time.Second)
	})

	if renderErr != nil {
		t.Fatalf("Render: %v", renderErr)
	}

	if len(out) != 0 {
		t.Fatalf("Render wrote %d bytes to the process stdout (want 0):\n%s", len(out), out)
	}

	if requests.Load() == 0 {
		t.Fatal("Render sent no request to the fake API server: it rendered offline")
	}

	// The redirect must only move nelm's printout, not thin the result: the
	// Secret still comes back, rendered with the value passed in.
	want := base64.StdEncoding.EncodeToString([]byte(password))

	for _, obj := range objs {
		if obj.GetKind() != "Secret" {
			continue
		}

		got, _, _ := unstructured.NestedString(obj.Object, "data", "password")
		if got != want {
			t.Fatalf("rendered Secret data.password = %q, want %q", got, want)
		}

		return
	}

	t.Fatalf("Render returned no Secret among %d objects", len(objs))
}

// writeTestChart writes a one-template chart named name into a temp dir and
// returns its absolute path.
func writeTestChart(t *testing.T, name, template string) string {
	t.Helper()

	chart := filepath.Join(t.TempDir(), name)

	if err := os.MkdirAll(filepath.Join(chart, "templates"), 0o700); err != nil {
		t.Fatal(err)
	}

	chartYAML := "apiVersion: v2\nname: " + name + "\nversion: 0.1.0\n"

	if err := os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte(chartYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(chart, "templates", "slow.yaml"), []byte(template), 0o600); err != nil {
		t.Fatal(err)
	}

	return chart
}

// awaitRender wraps chartRenderAction for the rest of the test and returns a
// channel closed once the render the test starts has returned, abandoned or
// not. A test that abandons a render must wait for it before it ends: the
// render goes on reading process globals, os.Stdout among them, which later
// tests swap (captureStdout), and nothing else orders those reads before the
// swaps.
func awaitRender(t *testing.T) <-chan struct{} {
	t.Helper()

	finished := make(chan struct{})
	orig := chartRenderAction

	var once sync.Once

	chartRenderAction = func(ctx context.Context, opts action.ChartRenderOptions) (*action.ChartRenderResultV2, error) {
		defer once.Do(func() { close(finished) })
		return orig(ctx, opts)
	}

	t.Cleanup(func() {
		select {
		case <-finished:
		case <-time.After(time.Minute):
			t.Error("the abandoned render did not finish within a minute")
		}

		chartRenderAction = orig
	})

	return finished
}

// TestChartRender_TimeoutBoundsTheRender is the regression test for an
// operation timeout that did not bound the render itself: nelm's ChartRender
// takes a ctx, but neither helm's template engine nor a remote render's
// lookup watches it, so a slow template held Render (nelm_release's plan and
// read) and Template (nelm_template's read) until it finished, whatever the
// timeout said. Each case renders a temp chart whose template is still
// running when the timeout fires — an offline Template of a long nested
// range, and a Render whose lookup the fake API server holds until the test
// ends — and must return promptly with the deadline error. Before the fix the
// first one returned success after the whole range, and the second failed
// only once the 10s kube request timeout cut the lookup off.
func TestChartRender_TimeoutBoundsTheRender(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// How long after the timeout the error may come back. It comes back as
	// the timeout fires, so this is only slack for a loaded machine, and
	// still well under the 10s the held lookup took before the fix.
	const promptly = 5 * time.Second

	tests := []struct {
		name    string
		timeout time.Duration
		render  func(t *testing.T, timeout time.Duration) error
	}{
		{
			// 4M empty iterations: about 0.1s on a fast machine without
			// the race detector (over 20 times the timeout), and well over a
			// second with it, which the test then waits out (awaitRender).
			name:    "offline Template, long range",
			timeout: 5 * time.Millisecond,
			render: func(t *testing.T, timeout time.Duration) error {
				isolateHome(t)

				chart := writeTestChart(t, "spin", "{{- range until 2000 }}{{- range until 2000 }}{{- end }}{{- end }}\n")
				awaitRender(t)

				spec := ReleaseSpec{Name: "spin", Namespace: "default", Chart: chart}

				_, err := NewNoConnectionClient(nil, errTestNoConnection).Template(ctx, spec, TemplateOptions{}, timeout)

				return err
			},
		},
		{
			name:    "Render, lookup held by the API server",
			timeout: 500 * time.Millisecond,
			render: func(t *testing.T, timeout time.Duration) error {
				release := make(chan struct{})

				c := NewClient(fakeKubeConfigWith(t, func(w http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path != "/api/v1/namespaces/default/configmaps/held" {
						return false
					}

					select {
					case <-release:
					case <-r.Context().Done():
					}

					w.WriteHeader(http.StatusNotFound)

					return true
				}))

				chart := writeTestChart(t, "held", "{{- $held := lookup \"v1\" \"ConfigMap\" \"default\" \"held\" }}\n")

				// Cleanups run last-registered first: release the held
				// lookup, wait for the abandoned render, then (registered
				// by fakeKubeConfigWith) close the fake API server.
				awaitRender(t)
				t.Cleanup(func() { close(release) })

				spec := ReleaseSpec{Name: "held", Namespace: "default", Chart: chart}

				_, err := c.Render(ctx, spec, timeout)

				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			err := tt.render(t, tt.timeout)
			elapsed := time.Since(start)

			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want the deadline error", err)
			}

			if want := "chart render: " + context.DeadlineExceeded.Error(); !strings.HasPrefix(err.Error(), want) {
				t.Errorf("err = %v, want it to start with %q", err, want)
			}

			if elapsed > tt.timeout+promptly {
				t.Errorf("returned %s after a %s timeout, want promptly", elapsed, tt.timeout)
			}
		})
	}
}

// TestChartRender_PanicIsAnError checks a panic in nelm's render comes back
// as an error instead of crashing the process: runChartRender runs the render
// on its own goroutine, where nothing else would recover it.
func TestChartRender_PanicIsAnError(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	isolateHome(t)

	orig := chartRenderAction
	chartRenderAction = func(context.Context, action.ChartRenderOptions) (*action.ChartRenderResultV2, error) {
		panic("test: nelm panicked")
	}
	t.Cleanup(func() { chartRenderAction = orig })

	spec := ReleaseSpec{Name: "tmpl", Namespace: "default", Chart: templateChart(t)}

	_, err := NewNoConnectionClient(nil, errTestNoConnection).Template(ctx, spec, TemplateOptions{}, 30*time.Second)
	if err == nil {
		t.Fatal("Template: err = nil, want the recovered panic")
	}

	if want := "chart render: recovered from a panic: test: nelm panicked"; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("Template: err = %v, want it to start with %q", err, want)
	}

	// The stack names the panicking function, so the error alone can be
	// triaged like the crash it replaces.
	if want := "TestChartRender_PanicIsAnError"; !strings.Contains(err.Error(), want) {
		t.Errorf("Template: err = %v, want the panic's stack (naming %s)", err, want)
	}
}

// TestRenderErrors_WithholdManifests is the regression test for render errors
// quoting the rendered manifest: apimachinery's decoder puts the whole
// manifest of an object without a kind into its error, and goccy/go-yaml
// prints the source lines around a YAML syntax error, so a broken Secret
// template leaked its data into the diagnostic — of an offline Template too,
// which contacts no cluster, and of nelm_release's plan and apply, whose
// resources attribute withholds that data. Every operation that renders the
// chart is checked through the real Client: Template offline, and Render,
// Plan and Install against a fake API server (for Install one that also
// keeps nelm's release-lock ConfigMap, so it gets as far as the render). The
// error must still name the template and what is wrong with it.
func TestRenderErrors_WithholdManifests(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const password = "withheld-probe-password"

	charts := []struct {
		name   string
		secret string
		want   string
	}{
		{
			name:   "missing kind",
			secret: "apiVersion: v1\nkind: {{ .Values.kind }}\nmetadata:\n  name: s\ndata:\n  password: {{ .Values.password | b64enc }}\n",
			want:   "Object 'Kind' is missing in the rendered manifest",
		},
		{
			name:   "YAML syntax error",
			secret: "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\nstringData:\n  password: {{ .Values.password }}: x\n",
			want:   "[6:13] mapping value is not allowed in this context",
		},
	}

	ops := []struct {
		name string
		run  func(t *testing.T, spec ReleaseSpec) error
	}{
		{
			name: "Template",
			run: func(t *testing.T, spec ReleaseSpec) error {
				isolateHome(t)

				_, err := NewNoConnectionClient(nil, errTestNoConnection).Template(ctx, spec, TemplateOptions{}, 30*time.Second)
				return err
			},
		},
		{
			name: "Render",
			run: func(t *testing.T, spec ReleaseSpec) error {
				_, err := NewClient(fakeKubeConfig(t)).Render(ctx, spec, 30*time.Second)
				return err
			},
		},
		{
			name: "Plan",
			run: func(t *testing.T, spec ReleaseSpec) error {
				_, err := NewClient(fakeKubeConfig(t)).Plan(ctx, spec, 30*time.Second)
				return err
			},
		},
		{
			name: "Install",
			run: func(t *testing.T, spec ReleaseSpec) error {
				var (
					mu      sync.Mutex
					objects = map[string][]byte{}
				)

				// An object store just big enough for the release lock:
				// a write keeps the object, a read returns it.
				c := NewClient(fakeKubeConfigWith(t, func(w http.ResponseWriter, r *http.Request) bool {
					mu.Lock()
					defer mu.Unlock()

					switch r.Method {
					case http.MethodPatch, http.MethodPut, http.MethodPost:
						body, err := io.ReadAll(r.Body)
						if err != nil {
							return false
						}

						obj, err := yaml.YAMLToJSON(body)
						if err != nil {
							return false
						}

						objects[r.URL.Path] = obj
					case http.MethodGet:
						if _, ok := objects[r.URL.Path]; !ok {
							return false
						}
					default:
						return false
					}

					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(objects[r.URL.Path])

					return true
				}))

				return c.Install(ctx, spec, 30*time.Second)
			},
		},
	}

	for _, chart := range charts {
		for _, op := range ops {
			t.Run(chart.name+"/"+op.name, func(t *testing.T) {
				dir := t.TempDir()

				writeChartFiles(t, dir, map[string]string{
					"Chart.yaml":            "apiVersion: v2\nname: badchart\nversion: 0.1.0\n",
					"values.yaml":           "password: " + password + "\n",
					"templates/secret.yaml": chart.secret,
				})

				err := op.run(t, ReleaseSpec{Name: "bad", Namespace: "default", Chart: dir, StorageDriver: "secret"})
				if err == nil {
					t.Fatalf("%s of a broken chart: err = nil", op.name)
				}

				msg := err.Error()

				for _, leaked := range []string{password, base64.StdEncoding.EncodeToString([]byte(password))} {
					if strings.Contains(msg, leaked) {
						t.Errorf("the %s error carries the Secret value %q:\n%s", op.name, leaked, msg)
					}
				}

				for _, want := range []string{"badchart/templates/secret.yaml", chart.want} {
					if !strings.Contains(msg, want) {
						t.Errorf("the %s error does not say %q:\n%s", op.name, want, msg)
					}
				}
			})
		}
	}
}
