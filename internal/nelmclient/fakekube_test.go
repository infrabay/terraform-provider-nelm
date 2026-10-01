package nelmclient

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The discovery documents fakeKubeConfig serves: just enough for a
// Remote:true ChartRender of testdata/charts/basic (kube-client construction,
// chart capabilities, release-history lookup).
const (
	fakeAPIGroupList = `{"kind":"APIGroupList","apiVersion":"v1","groups":[` +
		`{"name":"apps","versions":[{"groupVersion":"apps/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}},` +
		`{"name":"rbac.authorization.k8s.io","versions":[{"groupVersion":"rbac.authorization.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"rbac.authorization.k8s.io/v1","version":"v1"}}]}`
	fakeCoreV1Resources = `{"kind":"APIResourceList","groupVersion":"v1","resources":[` +
		`{"name":"namespaces","singularName":"namespace","namespaced":false,"kind":"Namespace","verbs":["get","list"]},` +
		`{"name":"configmaps","singularName":"configmap","namespaced":true,"kind":"ConfigMap","verbs":["get","list"]},` +
		`{"name":"secrets","singularName":"secret","namespaced":true,"kind":"Secret","verbs":["get","list"]},` +
		`{"name":"services","singularName":"service","namespaced":true,"kind":"Service","verbs":["get","list"]}]}`
	fakeAppsV1Resources = `{"kind":"APIResourceList","groupVersion":"apps/v1","resources":[` +
		`{"name":"deployments","singularName":"deployment","namespaced":true,"kind":"Deployment","verbs":["get","list"]}]}`
	fakeRBACV1Resources = `{"kind":"APIResourceList","groupVersion":"rbac.authorization.k8s.io/v1","resources":[` +
		`{"name":"clusterroles","singularName":"clusterrole","namespaced":false,"kind":"ClusterRole","verbs":["get","list"]},` +
		`{"name":"clusterrolebindings","singularName":"clusterrolebinding","namespaced":false,"kind":"ClusterRoleBinding","verbs":["get","list"]}]}`
)

// fakeKubeConfig starts an in-process fake kube-apiserver (httptest, TLS)
// that serves discovery and an empty release-storage Secret list, and
// returns a Config pointing at it through an inline kubeconfig. HOME,
// KUBECONFIG, KUBECACHEDIR and helm's cache/config locations are redirected
// into temp dirs first, so nothing can fall back to the developer's real
// kubeconfig (which may hold production contexts) or helm cache. It uses
// t.Setenv, so callers cannot be parallel.
func fakeKubeConfig(t *testing.T) Config {
	t.Helper()

	isolateHome(t)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body string

		switch {
		case r.URL.Path == "/version":
			body = `{"major":"1","minor":"29","gitVersion":"v1.29.3","platform":"linux/amd64"}`
		case r.URL.Path == "/api":
			body = `{"kind":"APIVersions","versions":["v1"],"serverAddressByClientCIDRs":[{"clientCIDR":"0.0.0.0/0","serverAddress":"127.0.0.1"}]}`
		case r.URL.Path == "/apis":
			body = fakeAPIGroupList
		case r.URL.Path == "/api/v1":
			body = fakeCoreV1Resources
		case r.URL.Path == "/apis/apps/v1":
			body = fakeAppsV1Resources
		case r.URL.Path == "/apis/rbac.authorization.k8s.io/v1":
			body = fakeRBACV1Resources
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/secrets"):
			body = `{"kind":"SecretList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[]}`
		}

		w.Header().Set("Content-Type", "application/json")

		if body == "" {
			w.WriteHeader(http.StatusNotFound)
			body = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`
		}

		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	kubeconfig, err := BuildInlineKubeconfig(srv.URL, "fake-token", "", true, "")
	if err != nil {
		t.Fatalf("BuildInlineKubeconfig: %v", err)
	}

	return Config{
		KubeConfigBase64: kubeconfig,
		RequestTimeout:   10 * time.Second,
		// Registry clients then read a per-op config.json holding only this
		// entry, never the developer's ~/.docker/config.json (nelm resolves
		// that default from the real HOME at process start).
		Registries: []RegistryAuth{{URL: "registry.invalid", Username: "test", Password: "test"}},
	}
}

// isolateHome points HOME, the kubeconfig/kube-cache locations and helm's
// cache/config/data homes at a fresh temp dir and returns it.
func isolateHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()

	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", filepath.Join(home, "no-kubeconfig"))
	t.Setenv("KUBECACHEDIR", filepath.Join(home, "kube-cache"))
	t.Setenv("HELM_CACHE_HOME", filepath.Join(home, "helm-cache"))
	t.Setenv("HELM_CONFIG_HOME", filepath.Join(home, "helm-config"))
	t.Setenv("HELM_DATA_HOME", filepath.Join(home, "helm-data"))
	t.Setenv("HELM_REPOSITORY_CACHE", filepath.Join(home, "helm-cache", "repository"))
	t.Setenv("HELM_REPOSITORY_CONFIG", filepath.Join(home, "helm-config", "repositories.yaml"))

	return home
}

// captureStdout swaps os.Stdout for a pipe while fn runs — what go-plugin's
// Serve does for the whole life of a provider process — and returns what was
// written to it.
func captureStdout(t *testing.T, fn func()) []byte {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	var out bytes.Buffer

	done := make(chan struct{})

	go func() {
		_, _ = io.Copy(&out, r)
		close(done)
	}()

	orig := os.Stdout
	os.Stdout = w

	func() {
		defer func() { os.Stdout = orig }()
		fn()
	}()

	_ = w.Close()
	<-done
	_ = r.Close()

	return out.Bytes()
}
