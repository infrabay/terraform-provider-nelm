package nelmclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	helmrelease "github.com/werf/nelm/pkg/helm/pkg/release"
	helmtime "github.com/werf/nelm/pkg/helm/pkg/time"
	"github.com/werf/nelm/pkg/release"
)

// testRelease builds a stored release record the way Helm/nelm write one.
func testRelease(name string, revision int, status helmrelease.Status, lastDeployed time.Time) *helmrelease.Release {
	return &helmrelease.Release{
		Name:      name,
		Namespace: "apps",
		Version:   revision,
		Info: &helmrelease.Info{
			Status:       status,
			LastDeployed: helmtime.Time{Time: lastDeployed},
		},
	}
}

// TestSummarizeHistory pins History's classification to nelm's own: Deployed
// is exactly nelm's "upgrade, not install" condition (a deployed or
// superseded revision since the last uninstall), and Revision/Status/
// LastDeployed describe the LAST record, where Helm's pending-* lock lives.
func TestSummarizeHistory(t *testing.T) {
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		rels         []*helmrelease.Release
		want         ReleaseHistory
		wantExists   bool
		wantPending  bool
		wantDeployed bool
	}{
		{
			name: "no records",
			want: ReleaseHistory{},
		},
		{
			name:       "failed first install only (recovery must stay possible)",
			rels:       []*helmrelease.Release{testRelease("app", 1, helmrelease.StatusFailed, t0)},
			want:       ReleaseHistory{Revision: 1, Status: "failed", LastDeployed: t0},
			wantExists: true,
		},
		{
			name:         "deployed",
			rels:         []*helmrelease.Release{testRelease("app", 1, helmrelease.StatusDeployed, t0)},
			want:         ReleaseHistory{Revision: 1, Status: "deployed", LastDeployed: t0, Deployed: true},
			wantExists:   true,
			wantDeployed: true,
		},
		{
			// A failed upgrade over a deployed release: the last record is
			// failed, but the superseded revision's objects are live and nelm
			// would upgrade, so it is Deployed.
			name: "failed upgrade over a deployed revision",
			rels: []*helmrelease.Release{
				testRelease("app", 2, helmrelease.StatusFailed, t0.Add(time.Hour)),
				testRelease("app", 1, helmrelease.StatusSuperseded, t0),
			},
			want:         ReleaseHistory{Revision: 2, Status: "failed", LastDeployed: t0.Add(time.Hour), Deployed: true},
			wantExists:   true,
			wantDeployed: true,
		},
		{
			// helm uninstall --keep-history: nothing is deployed any more.
			name: "uninstalled with kept history",
			rels: []*helmrelease.Release{
				testRelease("app", 1, helmrelease.StatusSuperseded, t0),
				testRelease("app", 2, helmrelease.StatusUninstalled, t0.Add(time.Hour)),
			},
			want:       ReleaseHistory{Revision: 2, Status: "uninstalled", LastDeployed: t0.Add(time.Hour)},
			wantExists: true,
		},
		{
			name: "pending-rollback on top of a deployed release",
			rels: []*helmrelease.Release{
				testRelease("app", 1, helmrelease.StatusDeployed, t0),
				testRelease("app", 2, helmrelease.StatusPendingRollback, t0.Add(time.Minute)),
			},
			want:         ReleaseHistory{Revision: 2, Status: "pending-rollback", LastDeployed: t0.Add(time.Minute), Deployed: true},
			wantExists:   true,
			wantPending:  true,
			wantDeployed: true,
		},
		{
			name:        "pending-install of a first install",
			rels:        []*helmrelease.Release{testRelease("app", 1, helmrelease.StatusPendingInstall, t0)},
			want:        ReleaseHistory{Revision: 1, Status: "pending-install", LastDeployed: t0},
			wantExists:  true,
			wantPending: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizeHistory(release.NewHistory(tt.rels, "app", nil, release.HistoryOptions{}))

			if *got != tt.want {
				t.Errorf("summarizeHistory = %+v, want %+v", *got, tt.want)
			}
			if got.Exists() != tt.wantExists {
				t.Errorf("Exists() = %v, want %v", got.Exists(), tt.wantExists)
			}
			if got.IsPending() != tt.wantPending {
				t.Errorf("IsPending() = %v, want %v", got.IsPending(), tt.wantPending)
			}
			if got.Deployed != tt.wantDeployed {
				t.Errorf("Deployed = %v, want %v", got.Deployed, tt.wantDeployed)
			}
		})
	}
}

// encodeStoredRelease encodes rel the way Helm's Secret storage driver does
// (JSON, gzip, base64) — the value of a release Secret's "release" key.
func encodeStoredRelease(t *testing.T, rel *helmrelease.Release) string {
	t.Helper()

	raw, err := json.Marshal(rel)
	if err != nil {
		t.Fatalf("marshal release: %v", err)
	}

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatalf("gzip release: %v", err)
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("gzip release: %v", err)
	}

	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// fakeAPIClient starts an httptest fake API server answering /version and
// handing every other request to handle, and returns a Client for it. The
// kubeconfig is passed inline (kube_config_base64), so no kubeconfig file and
// no real cluster is ever consulted.
func fakeAPIClient(t *testing.T, handle http.HandlerFunc) *Client {
	t.Helper()

	t.Setenv("KUBECACHEDIR", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/version" {
			_, _ = w.Write([]byte(`{"major":"1","minor":"29","gitVersion":"v1.29.3"}`))
			return
		}

		handle(w, r)
	}))
	t.Cleanup(srv.Close)

	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: fake
  cluster:
    server: %s
contexts:
- name: fake
  context:
    cluster: fake
    user: fake
current-context: fake
users:
- name: fake
  user: {}
`, srv.URL)

	return NewClient(Config{
		KubeConfigBase64: base64.StdEncoding.EncodeToString([]byte(kubeconfig)),
		RequestTimeout:   10 * time.Second,
	})
}

// TestClientHistory_FakeAPIServer drives Client.History end to end — kube
// client construction, nelm's Secret release storage, Helm's record decoding,
// BuildHistory and summarizeHistory — against an httptest fake API server
// serving two stored revisions of a release, one of them a pending-upgrade.
func TestClientHistory_FakeAPIServer(t *testing.T) {
	started := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	secret := func(rel *helmrelease.Release) map[string]any {
		return map[string]any{
			"metadata": map[string]any{
				"name":      fmt.Sprintf("sh.helm.release.v1.%s.v%d", rel.Name, rel.Version),
				"namespace": rel.Namespace,
				"labels": map[string]string{
					"name":    rel.Name,
					"owner":   "helm",
					"status":  string(rel.Info.Status),
					"version": fmt.Sprint(rel.Version),
				},
			},
			"type": "helm.sh/release.v1",
			// []byte fields are base64 in the API's JSON.
			"data": map[string]any{
				"release": base64.StdEncoding.EncodeToString([]byte(encodeStoredRelease(t, rel))),
			},
		}
	}

	secretList := map[string]any{
		"kind":       "SecretList",
		"apiVersion": "v1",
		"metadata":   map[string]any{},
		"items": []any{
			secret(testRelease("app", 1, helmrelease.StatusDeployed, started.Add(-time.Hour))),
			secret(testRelease("app", 2, helmrelease.StatusPendingUpgrade, started)),
		},
	}

	var (
		mu       sync.Mutex
		selector string
	)

	c := fakeAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/apps/secrets" {
			t.Errorf("unexpected API request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)

			return
		}

		mu.Lock()
		selector = r.URL.Query().Get("labelSelector")
		mu.Unlock()

		_ = json.NewEncoder(w).Encode(secretList)
	})

	got, err := c.History(context.Background(), "app", "apps", "secret", 10*time.Second)
	if err != nil {
		t.Fatalf("History: %v", err)
	}

	want := ReleaseHistory{Revision: 2, Status: "pending-upgrade", LastDeployed: started, Deployed: true}
	if *got != want {
		t.Fatalf("History = %+v, want %+v", *got, want)
	}

	mu.Lock()
	defer mu.Unlock()

	for _, term := range []string{"name=app", "owner=helm"} {
		if !strings.Contains(selector, term) {
			t.Errorf("release storage query labelSelector = %q, want it to contain %q", selector, term)
		}
	}
}

// TestClientHistory_ForbiddenIsDetectable: a History read the credentials may
// not make (here: listing ConfigMaps, the other storage backend) must still
// classify as Forbidden through nelm's and Helm's error wrapping — Create
// downgrades exactly that case of its other-backend check to a warning.
func TestClientHistory_ForbiddenIsDetectable(t *testing.T) {
	c := fakeAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/apps/configmaps" {
			t.Errorf("unexpected API request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)

			return
		}

		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,` +
			`"message":"configmaps is forbidden: User \"deployer\" cannot list resource \"configmaps\" in API group \"\" in the namespace \"apps\""}`))
	})

	_, err := c.History(context.Background(), "app", "apps", "configmap", 10*time.Second)
	if err == nil {
		t.Fatal("History: expected an error from a forbidden list")
	}

	if !IsForbidden(err) {
		t.Fatalf("IsForbidden(%v) = false, want true", err)
	}

	if IsForbidden(errors.New("configmaps is forbidden")) {
		t.Error("IsForbidden matched a plain error by its text")
	}
}
