package nelmclient

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
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
