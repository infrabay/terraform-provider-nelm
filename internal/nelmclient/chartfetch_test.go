package nelmclient

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// chartArchive builds a packaged chart (<name>/Chart.yaml plus one ConfigMap
// template whose data.source is source) the way `helm package` lays it out.
func chartArchive(t *testing.T, name, version, source string) []byte {
	t.Helper()

	files := map[string]string{
		name + "/Chart.yaml": fmt.Sprintf("apiVersion: v2\nname: %s\nversion: %s\n", name, version),
		name + "/templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n" +
			"data:\n  source: " + source + "\n",
	}

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for path, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: path, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatalf("tar header %s: %v", path, err)
		}

		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar write %s: %v", path, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	return buf.Bytes()
}

// chartRepoServer serves a classic helm chart repository holding one chart
// version: /index.yaml and the archive it points to. archiveHandler, when
// non-nil, replaces the archive download.
func chartRepoServer(t *testing.T, name, version string, archive []byte, archiveHandler http.HandlerFunc) *httptest.Server {
	t.Helper()

	archiveName := fmt.Sprintf("%s-%s.tgz", name, version)
	index := fmt.Sprintf("apiVersion: v1\nentries:\n  %s:\n  - apiVersion: v2\n    name: %s\n    version: %s\n    urls:\n    - %s\n",
		name, name, version, archiveName)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.yaml":
			_, _ = w.Write([]byte(index))
		case "/" + archiveName:
			if archiveHandler != nil {
				archiveHandler(w, r)
				return
			}

			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	return srv
}

// renderedSource renders chart "app" 1.0.0 from repoURL through Client.Render
// and returns the data.source of its ConfigMap.
func renderedSource(ctx context.Context, c *Client, repoURL string) (string, error) {
	objs, err := c.Render(ctx, ReleaseSpec{
		Name:       "app",
		Namespace:  "default",
		Chart:      "app",
		Repository: repoURL,
		Version:    "1.0.0",
	}, 30*time.Second)
	if err != nil {
		return "", err
	}

	for _, obj := range objs {
		if obj.GetKind() == "ConfigMap" {
			source, _, _ := unstructured.NestedString(obj.Object, "data", "source")
			return source, nil
		}
	}

	return "", fmt.Errorf("no ConfigMap among %d rendered objects", len(objs))
}

// TestRender_RemoteChartsDoNotShareADownloadPath is the regression test for
// remote charts being downloaded to a cache path keyed only by
// <name>-<version>.tgz: nelm wrote every remote chart into the shared helm
// repository cache and re-opened that path to load it, so two concurrent
// operations fetching different charts that share a name and version (the
// same leaf name and tag in two registries or repositories) could each load
// the other's archive and plan or install the wrong chart without any error.
// Each operation now downloads into its own directory; nothing lands in the
// shared cache.
func TestRender_RemoteChartsDoNotShareADownloadPath(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	c := NewClient(fakeKubeConfig(t))

	repos := map[string]string{
		chartRepoServer(t, "app", "1.0.0", chartArchive(t, "app", "1.0.0", "repo-a"), nil).URL: "repo-a",
		chartRepoServer(t, "app", "1.0.0", chartArchive(t, "app", "1.0.0", "repo-b"), nil).URL: "repo-b",
	}

	var wg sync.WaitGroup

	errs := make(chan error, 16)

	for i := 0; i < 8; i++ {
		for repoURL, want := range repos {
			wg.Add(1)

			go func() {
				defer wg.Done()

				got, err := renderedSource(ctx, c, repoURL)
				if err != nil {
					errs <- fmt.Errorf("render from %s: %w", repoURL, err)
					return
				}

				if got != want {
					errs <- fmt.Errorf("chart from %s rendered as %q, want %q (another repository's archive was loaded)", repoURL, got, want)
				}
			}()
		}
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	cache := os.Getenv("HELM_REPOSITORY_CACHE")

	entries, err := os.ReadDir(cache)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read helm repository cache: %v", err)
	}

	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tgz") {
			t.Errorf("chart archive %s was downloaded into the shared helm repository cache %s", e.Name(), cache)
		}
	}
}

func TestIsLocalChartRef(t *testing.T) {
	cases := map[string]bool{
		"/charts/app":                    true,
		"./charts/app":                   true,
		"../charts/app":                  true,
		".":                              true,
		"oci://registry.example/c/app":   false,
		"https://charts.example/app.tgz": false,
		"bitnami/postgresql":             false,
		"app":                            false,
	}

	for ref, want := range cases {
		if got := isLocalChartRef(ref); got != want {
			t.Errorf("isLocalChartRef(%q) = %v, want %v", ref, got, want)
		}
	}
}

// stallingHandler accepts a request and then sends nothing until release is
// closed, the client goes away, or 20s pass: a chart server that stalls
// mid-request.
func stallingHandler(release <-chan struct{}) http.HandlerFunc {
	return func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
	}
}

// TestRender_StalledChartRepositoryIsBounded is the regression test for
// ModifyPlan's Render hanging on a stalled chart server: no chart-repo
// request timeout was set, so nelm's downloader ran with
// http.Client{Timeout: 0}, and ChartRender (unlike nelm's install/plan
// actions) has no timeout of its own, so timeouts.read could not interrupt
// the archive download and `terraform plan` hung until the CI job was
// killed.
func TestRender_StalledChartRepositoryIsBounded(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	c := NewClient(fakeKubeConfig(t))

	release := make(chan struct{})
	repo := chartRepoServer(t, "app", "1.0.0", nil, stallingHandler(release))
	// Registered after the server's Close, so it runs first and lets the
	// stalled handler return.
	t.Cleanup(func() { close(release) })

	start := time.Now()

	_, err := c.Render(ctx, ReleaseSpec{
		Name:       "app",
		Namespace:  "default",
		Chart:      "app",
		Repository: repo.URL,
		Version:    "1.0.0",
	}, time.Second)

	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from a chart download that never completes")
	}

	if elapsed > 10*time.Second {
		t.Fatalf("Render took %s against a stalled chart repository; its 1s timeout must bound the download", elapsed)
	}
}

// TestFetchChart_StalledOCIRegistryIsBounded covers the OCI half: an OCI pull
// goes through helm's registry client, which has no HTTP timeout at all
// (ChartRepoRequestTimeout bounds only HTTP repositories), so only
// fetchChart's own timeout stops a registry that accepts the connection and
// then never answers.
func TestFetchChart_StalledOCIRegistryIsBounded(t *testing.T) {
	isolateHome(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var (
		mu    sync.Mutex
		conns []net.Conn
	)

	t.Cleanup(func() {
		_ = ln.Close()

		mu.Lock()
		defer mu.Unlock()

		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()

	opDir := t.TempDir()

	registryConfig, err := writeRegistryConfig(opDir, []RegistryAuth{{URL: "registry.invalid", Username: "test", Password: "test"}})
	if err != nil {
		t.Fatalf("writeRegistryConfig: %v", err)
	}

	ref := "oci://" + ln.Addr().String() + "/charts/app"

	start := time.Now()

	_, err = fetchChart(context.Background(), opDir, ref, "1.0.0", chartRepoOptions("", time.Minute), registryConfig, time.Second)

	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from an OCI pull that never completes")
	}

	if elapsed > 10*time.Second {
		t.Fatalf("fetchChart took %s against a stalled OCI registry; its 1s timeout must bound the pull", elapsed)
	}
}

// TestRender_OCIRepositoryIsPulledAsOneReference covers the seam between
// helm_release's OCI form (repository = "oci://host/path", chart = "name"),
// which NormalizeChartRef folds into one oci:// reference with no repository
// URL, and the per-operation chart download: the download must get that
// folded repository URL, never spec.Repository. Given the oci:// repository as
// ChartRepoURL, it looked the chart up as a classic index.yaml repository and
// failed with "get chart URL: ... is not a valid chart repository" before
// ever pulling the chart. The registry here drops every connection, so the
// pull itself fails too, just later and differently.
func TestRender_OCIRepositoryIsPulledAsOneReference(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	c := NewClient(fakeKubeConfig(t))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			_ = conn.Close()
		}
	}()

	_, err = c.Render(ctx, ReleaseSpec{
		Name:       "app",
		Namespace:  "default",
		Chart:      "app",
		Repository: "oci://" + ln.Addr().String() + "/charts",
		Version:    "1.0.0",
	}, 10*time.Second)
	if err == nil {
		t.Fatal("expected an error from a registry that drops every connection")
	}

	if want := fmt.Sprintf("download chart %q", "oci://"+ln.Addr().String()+"/charts/app"); !strings.Contains(err.Error(), want) {
		t.Errorf("Render error does not come from pulling the folded reference (want %q):\n%v", want, err)
	}

	if strings.Contains(err.Error(), "get chart URL") {
		t.Errorf("the oci:// repository was looked up as a classic chart repository:\n%v", err)
	}
}

// TestFetchChart_HelmPanicIsAnError checks a panic in helm's download code
// comes back as an error instead of crashing the process: fetchChart runs the
// download on its own goroutine, where nothing else would recover it. In nelm
// v1.26.2 through v1.27.2 the registry client nil-derefs on a plain-HTTP OCI
// pull (its ClientOptPlainHTTP assumes a custom HTTP client), which makes a
// real one; if nelm fixes that, the pull just fails to connect and this still
// holds.
func TestFetchChart_HelmPanicIsAnError(t *testing.T) {
	isolateHome(t)

	opDir := t.TempDir()

	registryConfig, err := writeRegistryConfig(opDir, []RegistryAuth{{URL: "registry.invalid", Username: "test", Password: "test"}})
	if err != nil {
		t.Fatalf("writeRegistryConfig: %v", err)
	}

	repo := chartRepoOptions("", time.Minute)
	repo.ChartRepoInsecure = true

	_, err = fetchChart(context.Background(), opDir, "oci://127.0.0.1:1/charts/app", "1.0.0", repo, registryConfig, 10*time.Second)
	if err == nil {
		t.Fatal("expected an error")
	}

	t.Logf("fetchChart: %v", err)
}

func TestChartRepoOptions_RequestTimeout(t *testing.T) {
	cases := map[time.Duration]time.Duration{
		0:                maxChartRepoRequestTimeout,
		30 * time.Second: 30 * time.Second,
		10 * time.Minute: maxChartRepoRequestTimeout,
	}

	for op, want := range cases {
		if got := chartRepoOptions("", op).ChartRepoRequestTimeout; got != want {
			t.Errorf("chartRepoOptions(timeout=%s).ChartRepoRequestTimeout = %s, want %s", op, got, want)
		}
	}
}
