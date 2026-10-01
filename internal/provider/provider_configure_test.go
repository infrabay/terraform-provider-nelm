package provider

// Offline unit tests for nelmProvider.Configure (review findings F04, F14,
// F27): which cluster a provider configuration targets, how it fails closed,
// and what resources get while the configuration is still Unknown. The
// "clusters" are local httptest servers; HOME, KUBECACHEDIR and every
// kubeconfig env var are pinned per test, so neither the real ~/.kube/config
// nor its current-context is ever read.

import (
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
)

// --- helpers ---------------------------------------------------------------

// isolateKubeEnv points HOME (and so the default ~/.kube/config) and nelm's
// discovery cache at fresh temp dirs and clears every env var that could
// select a kubeconfig, returning the fake home directory.
func isolateKubeEnv(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECACHEDIR", t.TempDir())

	for _, k := range []string{"KUBECONFIG", envKubeConfigPaths, envKubeConfigPath, envKubeContext} {
		t.Setenv(k, "")
	}

	return home
}

// fakeCluster is a minimal TLS Kubernetes API server holding no releases: it
// answers the version probe and an empty release-secret list, and records
// every request so a test can assert which "cluster" a client talked to.
type fakeCluster struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string
	tokens   []string
}

func newFakeCluster(t *testing.T) *fakeCluster {
	t.Helper()

	fc := &fakeCluster{}
	fc.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fc.mu.Lock()
		fc.requests = append(fc.requests, r.Method+" "+r.URL.Path)
		fc.tokens = append(fc.tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		fc.mu.Unlock()

		status, body := http.StatusNotFound, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`

		switch {
		case r.URL.Path == "/version":
			status, body = http.StatusOK, `{"major":"1","minor":"30","gitVersion":"v1.30.0"}`
		case strings.HasSuffix(r.URL.Path, "/secrets"):
			status, body = http.StatusOK, `{"kind":"SecretList","apiVersion":"v1","metadata":{},"items":[]}`
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(fc.srv.Close)

	return fc
}

// ambientCluster isolates the kube environment (isolateKubeEnv) and makes a
// fresh fakeCluster the current-context of the fake ~/.kube/config: the
// cluster nelm would fall back to if a zero connection Config ever reached
// it. A test that must not touch any cluster asserts this one saw no
// requests, so a regression fails offline instead of reaching the operator's
// real current-context.
func ambientCluster(t *testing.T) *fakeCluster {
	t.Helper()

	home := isolateKubeEnv(t)
	fc := newFakeCluster(t)
	writeKubeconfig(t, filepath.Join(home, ".kube", "config"), "ambient", map[string]string{"ambient": fc.srv.URL})

	return fc
}

func (fc *fakeCluster) hits() []string {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	return append([]string(nil), fc.requests...)
}

func (fc *fakeCluster) sawToken(token string) bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	for _, got := range fc.tokens {
		if got == token {
			return true
		}
	}

	return false
}

func (fc *fakeCluster) caPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fc.srv.Certificate().Raw}))
}

// writeKubeconfig writes a kubeconfig at p with one context per entry of
// servers (context name -> server URL, each with its own same-named cluster
// and user) and the given current-context.
func writeKubeconfig(t *testing.T, p, currentContext string, servers map[string]string) {
	t.Helper()

	var b strings.Builder

	b.WriteString("apiVersion: v1\nkind: Config\nclusters:\n")
	for name, server := range servers {
		fmt.Fprintf(&b, "- name: %s\n  cluster:\n    server: %s\n    insecure-skip-tls-verify: true\n", name, server)
	}

	b.WriteString("contexts:\n")
	for name := range servers {
		fmt.Fprintf(&b, "- name: %s\n  context:\n    cluster: %s\n    user: %s\n", name, name, name)
	}

	b.WriteString("users:\n")
	for name := range servers {
		fmt.Fprintf(&b, "- name: %s\n  user:\n    token: %s-token\n", name, name)
	}

	fmt.Fprintf(&b, "current-context: %s\n", currentContext)

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func str(s string) tftypes.Value {
	return tftypes.NewValue(tftypes.String, s)
}

func strList(ss ...string) tftypes.Value {
	elems := make([]tftypes.Value, len(ss))
	for i, s := range ss {
		elems[i] = str(s)
	}

	return tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, elems)
}

// homePaths resolves a test's env value for variable k: the kubeconfig path
// variables (KUBECONFIG included) hold OS-list-separated paths relative to
// home (a "~/..." entry stays literal, for the provider to expand); anything
// else is used as is.
func homePaths(home, k, v string) string {
	if k != envKubeConfigPaths && k != envKubeConfigPath && k != "KUBECONFIG" {
		return v
	}

	parts := filepath.SplitList(v)
	for i, p := range parts {
		if !strings.HasPrefix(p, "~") {
			parts[i] = filepath.Join(home, p)
		}
	}

	return strings.Join(parts, string(os.PathListSeparator))
}

// configure runs the real nelmProvider.Configure against a provider block
// with the given attributes set and every other attribute null (attrs == nil
// is `provider "nelm" {}`, or the implicit empty default provider Terraform
// instantiates for a module call without a providers mapping).
func configure(t *testing.T, attrs map[string]tftypes.Value, caps provider.ConfigureProviderClientCapabilities) provider.ConfigureResponse {
	t.Helper()

	ctx := context.Background()
	p := &nelmProvider{version: "test"}

	var sresp provider.SchemaResponse
	p.Schema(ctx, provider.SchemaRequest{}, &sresp)

	objType, ok := sresp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	if !ok {
		t.Fatalf("provider schema type = %T, want tftypes.Object", sresp.Schema.Type().TerraformType(ctx))
	}

	vals := make(map[string]tftypes.Value, len(objType.AttributeTypes))
	for name, typ := range objType.AttributeTypes {
		vals[name] = tftypes.NewValue(typ, nil)
	}

	for name, v := range attrs {
		if _, ok := objType.AttributeTypes[name]; !ok {
			t.Fatalf("configure: provider schema has no attribute %q", name)
		}

		vals[name] = v
	}

	req := provider.ConfigureRequest{
		Config:             tfsdk.Config{Schema: sresp.Schema, Raw: tftypes.NewValue(objType, vals)},
		ClientCapabilities: caps,
	}

	var resp provider.ConfigureResponse
	p.Configure(ctx, req, &resp)

	return resp
}

// configuredClient asserts Configure succeeded with a real (non-placeholder)
// client and returns it.
func configuredClient(t *testing.T, resp provider.ConfigureResponse) *nelmclient.Client {
	t.Helper()

	if resp.Diagnostics.HasError() {
		t.Fatalf("Configure: unexpected error diagnostics: %v", resp.Diagnostics)
	}

	c, ok := resp.ResourceData.(*nelmclient.Client)
	if !ok {
		t.Fatalf("ResourceData = %T, want *nelmclient.Client", resp.ResourceData)
	}

	if c.ConfigUnknown() {
		t.Fatal("ResourceData is the unknown-config placeholder, want a configured client")
	}

	return c
}

// requireError asserts diags holds exactly one error with the given summary
// (and, when at is non-nil, attached to that attribute path).
func requireError(t *testing.T, diags diag.Diagnostics, summary string, at *path.Path) {
	t.Helper()

	errs := diags.Errors()
	if len(errs) != 1 {
		t.Fatalf("got %d error diagnostics, want 1 (%q): %v", len(errs), summary, diags)
	}

	if errs[0].Summary() != summary {
		t.Fatalf("error summary = %q, want %q (detail: %s)", errs[0].Summary(), summary, errs[0].Detail())
	}

	if at == nil {
		return
	}

	withPath, ok := errs[0].(diag.DiagnosticWithPath)
	if !ok || !withPath.Path().Equal(*at) {
		t.Fatalf("error %q is not attached to %s: %v", summary, at, errs[0])
	}
}

func pathPtr(p path.Path) *path.Path {
	return &p
}

// --- F04: an empty configuration fails closed --------------------------------

// TestConfigure_NoConnectionSource_FailsClosed is the F04 regression test: a
// provider block that names no cluster used to configure successfully and
// silently target ~/.kube/config's current-context (here a "production"
// context, as a developer's machine may have) — and $KUBECONFIG did not
// change that.
// Every such shape must now be a Configure error with no client.
func TestConfigure_NoConnectionSource_FailsClosed(t *testing.T) {
	tests := []struct {
		name  string
		attrs map[string]tftypes.Value
		env   map[string]string
	}{
		{name: "empty provider block"},
		{
			// $KUBECONFIG is never read: an ambient value must not turn an
			// empty block into a working (and possibly wrong) connection.
			name: "KUBECONFIG alone is not a source",
			env:  map[string]string{"KUBECONFIG": "staging.yaml"},
		},
		{
			// An ambient KUBE_CTX naming the prod context of ~/.kube/config
			// must not make the implicit empty provider connect there.
			name: "KUBE_CTX alone is not a source",
			env:  map[string]string{envKubeContext: "gke_prod"},
		},
		{name: "empty host", attrs: map[string]tftypes.Value{"host": str("")}},
		{name: "empty kube_context", attrs: map[string]tftypes.Value{"kube_context": str("")}},
		{name: "only empty kube_config_paths entries", attrs: map[string]tftypes.Value{"kube_config_paths": strList("", "")}},
		{name: "empty kube_config_base64", attrs: map[string]tftypes.Value{"kube_config_base64": str("")}},
		{
			// An explicit (even empty) attribute wins over the environment.
			name:  "explicit empty kube_config_paths is not seeded from KUBE_CONFIG_PATH",
			attrs: map[string]tftypes.Value{"kube_config_paths": strList()},
			env:   map[string]string{envKubeConfigPath: "staging.yaml"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := isolateKubeEnv(t)
			writeKubeconfig(t, filepath.Join(home, ".kube", "config"), "gke_prod", map[string]string{"gke_prod": "https://prod.invalid"})
			writeKubeconfig(t, filepath.Join(home, "staging.yaml"), "gke_staging", map[string]string{"gke_staging": "https://staging.invalid"})

			for k, v := range tt.env {
				t.Setenv(k, homePaths(home, k, v))
			}

			resp := configure(t, tt.attrs, provider.ConfigureProviderClientCapabilities{})

			requireError(t, resp.Diagnostics, "No Kubernetes connection configured", nil)

			if resp.ResourceData != nil {
				t.Fatalf("ResourceData = %T, want nil on a failed Configure", resp.ResourceData)
			}
		})
	}
}

// --- F04: env seeding, "~" expansion, file validation --------------------------

func TestResolveKubeconfig(t *testing.T) {
	sep := string(os.PathListSeparator)

	paths := func(ps ...string) types.List {
		elems := make([]attr.Value, len(ps))
		for i, p := range ps {
			elems[i] = types.StringValue(p)
		}

		return types.ListValueMust(types.StringType, elems)
	}

	tests := []struct {
		name      string
		model     providerModel
		env       map[string]string // see homePaths
		noDefault bool              // no ~/.kube/config in the fake home

		wantPaths   []string // relative to the fake home
		wantContext string
		wantBase64  string
		wantErr     string // error summary; "" means success
		wantErrAt   *path.Path
	}{
		{
			name:      "KUBE_CONFIG_PATH seeds kube_config_paths",
			env:       map[string]string{envKubeConfigPath: "a.yaml"},
			wantPaths: []string{"a.yaml"},
		},
		{
			name:      "KUBE_CONFIG_PATHS is a list and wins over KUBE_CONFIG_PATH",
			env:       map[string]string{envKubeConfigPaths: "a.yaml" + sep + "b.yaml", envKubeConfigPath: "c.yaml"},
			wantPaths: []string{"a.yaml", "b.yaml"},
		},
		{
			name:      "kube_config_paths wins over the environment",
			model:     providerModel{KubeConfigPaths: paths("~/b.yaml")},
			env:       map[string]string{envKubeConfigPaths: "a.yaml", envKubeConfigPath: "a.yaml"},
			wantPaths: []string{"b.yaml"},
		},
		{
			name:        "KUBE_CTX seeds kube_context",
			env:         map[string]string{envKubeConfigPath: "a.yaml", envKubeContext: "gke_staging"},
			wantPaths:   []string{"a.yaml"},
			wantContext: "gke_staging",
		},
		{
			name:        "kube_context wins over KUBE_CTX",
			model:       providerModel{KubeContext: types.StringValue("gke_dev")},
			env:         map[string]string{envKubeConfigPath: "a.yaml", envKubeContext: "gke_staging"},
			wantPaths:   []string{"a.yaml"},
			wantContext: "gke_dev",
		},
		{
			// A path copied verbatim from a helm provider config_path.
			name:      "a leading ~ is expanded",
			model:     providerModel{KubeConfigPaths: paths("~/.kube/config")},
			wantPaths: []string{".kube/config"},
		},
		{
			name:      "~ in KUBE_CONFIG_PATHS is expanded too",
			env:       map[string]string{envKubeConfigPaths: "~/a.yaml" + sep + "~/b.yaml"},
			wantPaths: []string{"a.yaml", "b.yaml"},
		},
		{
			// The documented `provider "nelm" { kube_context = "..." }` shape:
			// the context names the cluster, looked up in ~/.kube/config.
			name:        "kube_context alone uses ~/.kube/config",
			model:       providerModel{KubeContext: types.StringValue("gke_dev")},
			wantPaths:   []string{".kube/config"},
			wantContext: "gke_dev",
		},
		{
			// Unlike the kube_context attribute, an env var may be exported
			// ambiently; hashicorp/helm ignores KUBE_CTX without a path too.
			name:    "KUBE_CTX alone names no cluster",
			env:     map[string]string{envKubeContext: "gke_dev"},
			wantErr: "No Kubernetes connection configured",
		},
		{
			name:        "KUBE_CTX selects the context in kube_config_base64",
			model:       providerModel{KubeConfigBase64: types.StringValue("Zm9v")},
			env:         map[string]string{envKubeContext: "gke_dev"},
			wantContext: "gke_dev",
			wantBase64:  "Zm9v",
		},
		{
			name:        "kube_config_base64 takes precedence and skips path resolution",
			model:       providerModel{KubeConfigBase64: types.StringValue("Zm9v"), KubeContext: types.StringValue("gke_dev")},
			env:         map[string]string{envKubeConfigPath: "missing.yaml"},
			wantContext: "gke_dev",
			wantBase64:  "Zm9v",
		},
		{
			// nelm's loader skips a missing file silently and lands on
			// http://localhost:8080 (or on another file's current-context).
			name:      "a missing kube_config_paths file is an attribute error",
			model:     providerModel{KubeConfigPaths: paths("~/a.yaml", "~/missing.yaml")},
			wantErr:   "Invalid kubeconfig path",
			wantErrAt: pathPtr(path.Root("kube_config_paths")),
		},
		{
			name:    "a missing KUBE_CONFIG_PATH file is an error",
			env:     map[string]string{envKubeConfigPath: "missing.yaml"},
			wantErr: "Invalid kubeconfig path",
		},
		{
			name:    "a directory is not a kubeconfig",
			env:     map[string]string{envKubeConfigPath: ".kube"},
			wantErr: "Invalid kubeconfig path",
		},
		{
			name:      "kube_context alone without ~/.kube/config is an error",
			model:     providerModel{KubeContext: types.StringValue("gke_dev")},
			noDefault: true,
			wantErr:   "Invalid kubeconfig path",
		},
		{
			name:    "nothing configured",
			wantErr: "No Kubernetes connection configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := isolateKubeEnv(t)

			for _, f := range []string{"a.yaml", "b.yaml", "c.yaml"} {
				writeKubeconfig(t, filepath.Join(home, f), "ctx", map[string]string{"ctx": "https://" + f + ".invalid"})
			}

			if !tt.noDefault {
				writeKubeconfig(t, filepath.Join(home, ".kube", "config"), "ctx", map[string]string{"ctx": "https://default.invalid"})
			}

			for k, v := range tt.env {
				t.Setenv(k, homePaths(home, k, v))
			}

			var cfg nelmclient.Config

			diags := resolveKubeconfig(context.Background(), tt.model, &cfg)

			if tt.wantErr != "" {
				requireError(t, diags, tt.wantErr, tt.wantErrAt)
				return
			}

			if diags.HasError() {
				t.Fatalf("resolveKubeconfig: unexpected error diagnostics: %v", diags)
			}

			var wantPaths []string
			for _, p := range tt.wantPaths {
				wantPaths = append(wantPaths, filepath.Join(home, p))
			}

			if strings.Join(cfg.KubeConfigPaths, sep) != strings.Join(wantPaths, sep) {
				t.Errorf("KubeConfigPaths = %q, want %q", cfg.KubeConfigPaths, wantPaths)
			}

			if cfg.KubeContext != tt.wantContext {
				t.Errorf("KubeContext = %q, want %q", cfg.KubeContext, tt.wantContext)
			}

			if cfg.KubeConfigBase64 != tt.wantBase64 {
				t.Errorf("KubeConfigBase64 = %q, want %q", cfg.KubeConfigBase64, tt.wantBase64)
			}
		})
	}
}

// --- F04: the configured client really talks to the configured cluster ------

// TestConfigure_TargetsConfiguredCluster drives Configure and then a real
// nelm release read (nelmclient.Get) against two fake API servers: "prod" is
// the current-context of ~/.kube/config, "staging" is what the provider
// configuration names. Before F04, the env-seeded cases read prod (and the
// release, absent there, would have been dropped from state).
func TestConfigure_TargetsConfiguredCluster(t *testing.T) {
	tests := []struct {
		name  string
		attrs func(staging *fakeCluster) map[string]tftypes.Value
		env   map[string]string // see homePaths
		token string            // bearer token staging must have seen
	}{
		{
			name:  "KUBE_CONFIG_PATH",
			env:   map[string]string{envKubeConfigPath: "staging.yaml"},
			token: "gke_staging-token",
		},
		{
			name:  "KUBE_CONFIG_PATHS + KUBE_CTX",
			env:   map[string]string{envKubeConfigPaths: "both.yaml", envKubeContext: "gke_staging"},
			token: "gke_staging-token",
		},
		{
			name: "kube_context alone (looked up in ~/.kube/config)",
			attrs: func(*fakeCluster) map[string]tftypes.Value {
				return map[string]tftypes.Value{"kube_context": str("gke_staging")}
			},
			token: "gke_staging-token",
		},
		{
			// Inline host is a standalone connection: the kubeconfig
			// attributes and env next to it are ignored, never validated.
			name: "inline host ignores kubeconfig settings",
			attrs: func(staging *fakeCluster) map[string]tftypes.Value {
				return map[string]tftypes.Value{
					"host":                   str(strings.TrimPrefix(staging.srv.URL, "https://")),
					"token":                  str("inline-token"),
					"cluster_ca_certificate": str(staging.caPEM()),
					"kube_context":           str("gke_prod"),
					"kube_config_paths":      strList("/nonexistent/kubeconfig"),
				}
			},
			env:   map[string]string{envKubeConfigPath: "missing.yaml", envKubeContext: "gke_prod"},
			token: "inline-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := isolateKubeEnv(t)
			prod, staging := newFakeCluster(t), newFakeCluster(t)

			both := map[string]string{"gke_prod": prod.srv.URL, "gke_staging": staging.srv.URL}
			writeKubeconfig(t, filepath.Join(home, ".kube", "config"), "gke_prod", both)
			writeKubeconfig(t, filepath.Join(home, "both.yaml"), "gke_prod", both)
			writeKubeconfig(t, filepath.Join(home, "staging.yaml"), "gke_staging", map[string]string{"gke_staging": staging.srv.URL})

			for k, v := range tt.env {
				t.Setenv(k, homePaths(home, k, v))
			}

			var attrs map[string]tftypes.Value
			if tt.attrs != nil {
				attrs = tt.attrs(staging)
			}

			c := configuredClient(t, configure(t, attrs, provider.ConfigureProviderClientCapabilities{}))

			_, err := c.Get(context.Background(), "app", "default", "secret", 30*time.Second)
			if !nelmclient.IsReleaseNotFound(err) {
				t.Fatalf("Get: err = %v, want release-not-found from the staging fake", err)
			}

			if got := prod.hits(); len(got) != 0 {
				t.Fatalf("~/.kube/config's current-context (prod) was contacted: %v", got)
			}

			if len(staging.hits()) == 0 {
				t.Fatal("the configured cluster (staging) was never contacted")
			}

			if !staging.sawToken(tt.token) {
				t.Fatalf("staging never saw bearer token %q", tt.token)
			}
		})
	}
}

// --- F14: an Unknown configuration defers instead of failing the plan --------

func TestConfigure_UnknownConfig_ReturnsPlaceholder(t *testing.T) {
	unknownString := tftypes.NewValue(tftypes.String, tftypes.UnknownValue)

	registryType := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"url": tftypes.String, "username": tftypes.String, "password": tftypes.String,
	}}

	tests := []struct {
		name  string
		attrs map[string]tftypes.Value
	}{
		{
			// The GKE cluster is created in the same run.
			name: "host unknown",
			attrs: map[string]tftypes.Value{
				"host":                   unknownString,
				"token":                  str("t"),
				"cluster_ca_certificate": unknownString,
			},
		},
		{
			name:  "kube_config_paths element unknown",
			attrs: map[string]tftypes.Value{"kube_config_paths": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{unknownString})},
		},
		{
			name: "registries password unknown",
			attrs: map[string]tftypes.Value{
				"kube_context": str("ctx"),
				"registries": tftypes.NewValue(tftypes.List{ElementType: registryType}, []tftypes.Value{
					tftypes.NewValue(registryType, map[string]tftypes.Value{
						"url": str("oci://example.invalid"), "username": str("u"), "password": unknownString,
					}),
				}),
			},
		},
	}

	for _, tt := range tests {
		for _, deferral := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deferral_allowed=%t", tt.name, deferral), func(t *testing.T) {
				// No kubeconfig and no env at all: resolving the connection
				// now would fail, so success proves it is not attempted.
				isolateKubeEnv(t)

				resp := configure(t, tt.attrs, provider.ConfigureProviderClientCapabilities{DeferralAllowed: deferral})

				if resp.Diagnostics.HasError() {
					t.Fatalf("Configure: unexpected error diagnostics: %v", resp.Diagnostics)
				}

				c, ok := resp.ResourceData.(*nelmclient.Client)
				if !ok || !c.ConfigUnknown() {
					t.Fatalf("ResourceData = %#v, want the unknown-config placeholder client", resp.ResourceData)
				}

				// The framework rejects a Deferred response the client did
				// not announce support for.
				switch {
				case deferral && (resp.Deferred == nil || resp.Deferred.Reason != provider.DeferredReasonProviderConfigUnknown):
					t.Fatalf("Deferred = %v, want reason ProviderConfigUnknown", resp.Deferred)
				case !deferral && resp.Deferred != nil:
					t.Fatalf("Deferred = %v, want nil when the client does not allow deferral", resp.Deferred)
				}
			})
		}
	}
}

// TestModifyPlan_UnknownProviderConfig covers the resource side of F14: a NEW
// release degrades to an Unknown diff with a warning (apply computes it with
// the real configuration), while a release already in state cannot be
// planned without its cluster and fails with nelmclient.ErrConfigUnknown.
func TestModifyPlan_UnknownProviderConfig(t *testing.T) {
	ambient := ambientCluster(t)

	ctx := context.Background()
	plan := buildPlan(t, ctx, baseTestReleaseModel())
	r := &releaseResource{client: nelmclient.NewUnknownConfigClient()}

	t.Cleanup(func() {
		if got := ambient.hits(); len(got) != 0 {
			t.Errorf("~/.kube/config's current-context was contacted: %v", got)
		}
	})

	t.Run("create degrades with a warning", func(t *testing.T) {
		req := resource.ModifyPlanRequest{
			Plan:  plan,
			State: tfsdk.State{Raw: tftypes.NewValue(plan.Raw.Type(), nil), Schema: plan.Schema},
		}
		resp := &resource.ModifyPlanResponse{Plan: plan}

		r.ModifyPlan(ctx, req, resp)

		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected error diagnostics: %v", resp.Diagnostics)
		}

		if w := resp.Diagnostics.Warnings(); len(w) != 1 || w[0].Summary() != "Provider configuration not known at plan time" {
			t.Fatalf("warnings = %v, want one \"Provider configuration not known at plan time\"", w)
		}

		var resources types.Map
		assertUnknownAttr(t, ctx, resp.Plan, "resources", &resources)

		var status types.String
		assertUnknownAttr(t, ctx, resp.Plan, "status", &status)

		var revision types.Int64
		assertUnknownAttr(t, ctx, resp.Plan, "revision", &revision)

		var metadata types.Object
		assertUnknownAttr(t, ctx, resp.Plan, "metadata", &metadata)
	})

	t.Run("existing release is a hard error", func(t *testing.T) {
		req := resource.ModifyPlanRequest{Plan: plan, State: tfsdk.State(plan)}
		resp := &resource.ModifyPlanResponse{Plan: plan}

		r.ModifyPlan(ctx, req, resp)

		requireError(t, resp.Diagnostics, "nelm_release plan failed", nil)

		if !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), nelmclient.ErrConfigUnknown.Error()) {
			t.Fatalf("error detail = %q, want it to explain the unknown provider configuration", resp.Diagnostics.Errors()[0].Detail())
		}
	})
}

// TestRead_UnknownProviderConfig_KeepsState: refreshing an existing release
// while the provider configuration is Unknown must fail, never drop the
// release from state (helm_release's behavior on an unreachable cluster) and
// never read some other cluster.
func TestRead_UnknownProviderConfig_KeepsState(t *testing.T) {
	// Without the guard, the zero Config reads this (empty) cluster, finds
	// no release and drops it from state without any error.
	ambient := ambientCluster(t)

	ctx := context.Background()
	plan := buildPlan(t, ctx, baseTestReleaseModel())
	state := tfsdk.State(plan)

	r := &releaseResource{client: nelmclient.NewUnknownConfigClient()}
	resp := &resource.ReadResponse{State: state}

	r.Read(ctx, resource.ReadRequest{State: state}, resp)

	if got := ambient.hits(); len(got) != 0 {
		t.Fatalf("~/.kube/config's current-context was contacted: %v", got)
	}

	requireError(t, resp.Diagnostics, "Failed to read nelm release", nil)

	if !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), nelmclient.ErrConfigUnknown.Error()) {
		t.Fatalf("error detail = %q, want it to explain the unknown provider configuration", resp.Diagnostics.Errors()[0].Detail())
	}

	if resp.State.Raw.IsNull() {
		t.Fatal("Read removed the release from state")
	}
}

// --- F27: Configure guards, parsing and wiring --------------------------------

func TestConfigure_InlineConnectionGuards(t *testing.T) {
	tests := []struct {
		name    string
		attrs   map[string]tftypes.Value
		summary string
		at      path.Path
	}{
		{
			// The dangerous real-world shape: a private cluster's empty
			// public_endpoint next to a perfectly good token.
			name:    "empty host with token",
			attrs:   map[string]tftypes.Value{"host": str(""), "token": str("t"), "kube_context": str("ctx")},
			summary: "Inline connection attributes require host",
			at:      path.Root("host"),
		},
		{
			name:    "null host with cluster_ca_certificate",
			attrs:   map[string]tftypes.Value{"cluster_ca_certificate": str("pem"), "kube_context": str("ctx")},
			summary: "Inline connection attributes require host",
			at:      path.Root("host"),
		},
		{
			name:    "null host with insecure",
			attrs:   map[string]tftypes.Value{"insecure": tftypes.NewValue(tftypes.Bool, true), "kube_context": str("ctx")},
			summary: "Inline connection attributes require host",
			at:      path.Root("host"),
		},
		{
			name:    "null host with tls_server_name",
			attrs:   map[string]tftypes.Value{"tls_server_name": str("kubernetes.default"), "kube_context": str("ctx")},
			summary: "Inline connection attributes require host",
			at:      path.Root("host"),
		},
		{
			name:    "host without token",
			attrs:   map[string]tftypes.Value{"host": str("10.0.0.1")},
			summary: "Inline connection requires token",
			at:      path.Root("token"),
		},
		{
			name:    "host with empty token",
			attrs:   map[string]tftypes.Value{"host": str("10.0.0.1"), "token": str("")},
			summary: "Inline connection requires token",
			at:      path.Root("token"),
		},
		{
			name:    "unparsable kube_request_timeout",
			attrs:   map[string]tftypes.Value{"kube_request_timeout": str("bogus"), "kube_context": str("ctx")},
			summary: "Invalid kube_request_timeout",
			at:      path.Root("kube_request_timeout"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := isolateKubeEnv(t)
			writeKubeconfig(t, filepath.Join(home, ".kube", "config"), "ctx", map[string]string{"ctx": "https://ctx.invalid"})

			resp := configure(t, tt.attrs, provider.ConfigureProviderClientCapabilities{})

			requireError(t, resp.Diagnostics, tt.summary, &tt.at)

			if resp.ResourceData != nil {
				t.Fatalf("ResourceData = %T, want nil on a failed Configure", resp.ResourceData)
			}
		})
	}
}

func TestConfigure_InlineHostWithRegistries(t *testing.T) {
	isolateKubeEnv(t)

	registryType := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"url": tftypes.String, "username": tftypes.String, "password": tftypes.String,
	}}

	// A typical GKE + Artifact Registry configuration (inline endpoint,
	// token and CA): no kubeconfig exists anywhere, and none is needed.
	resp := configure(t, map[string]tftypes.Value{
		"host":                   str("34.1.2.3"),
		"token":                  str("access-token"),
		"cluster_ca_certificate": str("-----BEGIN CERTIFICATE-----\n-----END CERTIFICATE-----\n"),
		"kube_qps":               tftypes.NewValue(tftypes.Number, 50),
		"kube_burst":             tftypes.NewValue(tftypes.Number, 200),
		"kube_request_timeout":   str("30s"),
		"registries": tftypes.NewValue(tftypes.List{ElementType: registryType}, []tftypes.Value{
			tftypes.NewValue(registryType, map[string]tftypes.Value{
				"url": str("oci://us-central1-docker.pkg.dev"), "username": str("oauth2accesstoken"), "password": str("access-token"),
			}),
		}),
	}, provider.ConfigureProviderClientCapabilities{})

	configuredClient(t, resp)
}

func TestNormalizeHost(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"10.0.0.1", "https://10.0.0.1"},
		{"34.1.2.3:443", "https://34.1.2.3:443"},
		{"https://10.0.0.1", "https://10.0.0.1"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
	}

	for _, tt := range tests {
		if got := normalizeHost(tt.in); got != tt.want {
			t.Errorf("normalizeHost(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDurationValidator(t *testing.T) {
	tests := []struct {
		name    string
		value   types.String
		wantErr bool
	}{
		{"valid", types.StringValue("30s"), false},
		{"empty means unset", types.StringValue(""), false},
		{"null", types.StringNull(), false},
		{"unknown", types.StringUnknown(), false},
		{"invalid", types.StringValue("30"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validator.StringRequest{Path: path.Root("kube_request_timeout"), ConfigValue: tt.value}

			var resp validator.StringResponse
			durationValidator{}.ValidateString(context.Background(), req, &resp)

			if resp.Diagnostics.HasError() != tt.wantErr {
				t.Fatalf("HasError = %v, want %v: %v", resp.Diagnostics.HasError(), tt.wantErr, resp.Diagnostics)
			}
		})
	}
}
