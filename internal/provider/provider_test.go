package provider_test

// provider_test.go is the acceptance-test HARNESS. release_resource_test.go
// (same package) and the other *_acc_test.go files hold the actual scenario
// Test functions; everything below is shared plumbing: the protocol-v6
// provider factory, the triple cluster-safety guard, and small kubectl/helm
// helpers the scenarios use to create out-of-band drift/fixtures and to clean
// up after themselves.
//
// CLUSTER SAFETY: the machine this runs on may have its kubeconfig
// current-context pointed at a remote or production cluster. Every helper here
// that shells out to kubectl/helm passes "--context=<pinned test context>" and
// "--kubeconfig=<testKubeconfigPath>" EXPLICITLY and never relies on the
// ambient current-context or $KUBECONFIG; every Terraform provider block a
// test config builds sets kube_context and kube_config_paths to that same
// pinned context and file explicitly (see providerBlock below). The pinned
// context comes from NELM_TEST_KUBE_CONTEXT ("kind-nelm-acc" via the
// GNUmakefile default and in CI, or any other local context such as
// "orbstack") and testAccPreCheck hard-verifies it resolves, in that file, to
// a LOCAL (127.0.0.1, ::1 or localhost) API server before anything runs.

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/infrabay/terraform-provider-nelm/internal/provider"
)

// resourceAddr is the Terraform address every scenario's Config declares its
// single nelm_release under. Kept as one constant so helpers that build
// TestCheckFunc/StateCheck values don't have to repeat the string.
const resourceAddr = "nelm_release.test"

// testAccProtoV6ProviderFactories wires the real provider (protocol v6,
// terraform-plugin-framework) into terraform-plugin-testing's harness under
// the "nelm" key, matching the provider's own Metadata().TypeName ("nelm" ->
// resource type "nelm_release") and every test Config's
// required_providers/provider block below.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"nelm": providerserver.NewProtocol6WithError(provider.New("test")()),
}

// testKubeContext returns the kubeconfig context name this suite is pinned
// to: the value of NELM_TEST_KUBE_CONTEXT ("kind-nelm-acc" via the
// GNUmakefile default, as in CI; any other local context such as "orbstack"
// works too). Empty means "not opted in" and testAccPreCheck hard-fails: the
// Go code itself deliberately has no default.
func testKubeContext() string {
	return os.Getenv("NELM_TEST_KUBE_CONTEXT")
}

// testKubeconfigPath returns the ONE kubeconfig file the whole suite reads:
// ~/.kube/config ("" if the home directory cannot be determined). The
// provider block passes it as kube_config_paths and kubectl/helm get it via
// --kubeconfig, so the safety guard, the fixtures and the provider under test
// all resolve NELM_TEST_KUBE_CONTEXT from the same file. Otherwise kubectl and
// helm would honour an ambient $KUBECONFIG (merging every file it lists) that
// the provider never reads, and the guard could vet one cluster while the
// tests deploy to a same-named context in another.
func testKubeconfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}

	return filepath.Join(home, ".kube", "config")
}

// testAccPreCheck is the triple local-cluster safety guard (GNUmakefile
// testacc target, DEVELOPMENT.md): it MUST hard-fail (t.Fatal, not t.Skip)
// unless ALL of the following hold, so acceptance tests can never silently
// no-op against -- or worse, actually run against -- a real cluster:
//
//  1. TF_ACC is set (resource.Test's own gate already skips before PreCheck
//     runs when this is unset; checked again here defensively in case this
//     function is ever invoked outside resource.Test).
//  2. NELM_TEST_KUBE_CONTEXT is set -- an explicit, separate opt-in pin
//     (distinct from TF_ACC) naming the ONLY context this suite is allowed
//     to touch. There is deliberately no default.
//  3. That kubeconfig context exists in testKubeconfigPath() AND its
//     cluster's server host is exactly 127.0.0.1, ::1 or localhost
//     (localContextServer: reads that one file only, no $KUBECONFIG, no
//     cluster I/O). This is the real safety net: no managed/cloud cluster
//     endpoint is ever a loopback host.
//
// This is deliberately independent of (and in addition to) every test
// Config's own explicit `kube_context = ...` (never current-context), and it
// checks the same file the provider block points kube_config_paths at.
func testAccPreCheck(t *testing.T) {
	t.Helper()

	if os.Getenv("TF_ACC") == "" {
		t.Fatal("nelm_release acceptance tests require TF_ACC=1 (see GNUmakefile's testacc target)")
	}

	kubeCtx := testKubeContext()
	if kubeCtx == "" {
		t.Fatal(
			"nelm_release acceptance tests require NELM_TEST_KUBE_CONTEXT=<local kube context> as an " +
				"explicit opt-in pin (safety guard against accidentally running against this machine's " +
				"actual kubeconfig current-context, which may be a production cluster); e.g. " +
				"NELM_TEST_KUBE_CONTEXT=kind-nelm-acc",
		)
	}

	kubeconfig := testKubeconfigPath()
	if kubeconfig == "" {
		t.Fatal("could not determine the home directory holding the acceptance-test kubeconfig (~/.kube/config)")
	}

	if _, err := localContextServer(kubeconfig, kubeCtx); err != nil {
		t.Fatalf("%v; refusing to run acceptance tests", err)
	}
}

// localContextServer resolves kubeCtx in the kubeconfig file at
// kubeconfigPath -- that file only, never merged with $KUBECONFIG and never
// via current-context -- and returns its cluster's API server URL, the server
// the provider connects to for kube_config_paths = [kubeconfigPath] plus
// kube_context = kubeCtx. It resolves the CONTEXT's cluster reference (which
// can differ from the context's own name), not a same-named cluster entry. It
// errors unless the server's host is exactly a loopback name: a prefix test
// would also accept https://localhost.example.com or https://127.0.0.1.nip.io.
func localContextServer(kubeconfigPath, kubeCtx string) (string, error) {
	cfg, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return "", fmt.Errorf("could not load kubeconfig %s: %w", kubeconfigPath, err)
	}

	kctx, ok := cfg.Contexts[kubeCtx]
	if !ok {
		return "", fmt.Errorf("kubeconfig context %q does not exist in %s", kubeCtx, kubeconfigPath)
	}

	cluster, ok := cfg.Clusters[kctx.Cluster]
	if !ok || cluster.Server == "" {
		return "", fmt.Errorf("kubeconfig context %q references cluster %q, which has no server URL in %s", kubeCtx, kctx.Cluster, kubeconfigPath)
	}

	server, err := url.Parse(cluster.Server)
	if err != nil {
		return "", fmt.Errorf("kubeconfig context %q has an unparsable server URL %q: %w", kubeCtx, cluster.Server, err)
	}

	switch server.Hostname() {
	case "127.0.0.1", "::1", "localhost":
		return cluster.Server, nil
	}

	return "", fmt.Errorf(
		"kubeconfig context %q resolves to cluster %q with server %q, which is not a local endpoint "+
			"(expected host 127.0.0.1, ::1 or localhost) and may be a real cluster",
		kubeCtx, kctx.Cluster, cluster.Server,
	)
}

// providerBlock is the provider configuration every scenario's Config
// embeds. kube_context and kube_config_paths are always explicit -- never the
// ambient current-context, never an env-dependent kubeconfig -- per the
// cluster safety rule above, so the provider reads exactly the file
// testAccPreCheck vetted.
func providerBlock() string {
	return fmt.Sprintf(`provider "nelm" {
  kube_config_paths = [%q]
  kube_context      = %q
}
`, testKubeconfigPath(), testKubeContext())
}

// chartPath returns the absolute path to testdata/charts/basic. Tests run
// with the package directory (internal/provider) as their working directory,
// so the relative path is always "../../testdata/charts/basic"; Terraform
// configs need an absolute path to survive being written into a scratch
// working directory elsewhere (terraform-plugin-testing copies/writes test
// configs into per-step temp dirs).
func chartPath(t *testing.T) string {
	t.Helper()

	abs, err := filepath.Abs("../../testdata/charts/basic")
	if err != nil {
		t.Fatalf("could not resolve testdata chart path: %v", err)
	}

	return abs
}

// uniqueNamespace returns a namespace name of the form "tfnelm-acc-<tag>-<ts>"
// -- unique per test run, short enough to stay well under Kubernetes' 63-char
// limit, and immediately recognizable as an acceptance-test artifact on the
// test cluster if cleanup is ever interrupted (e.g. by a panic).
func uniqueNamespace(tag string) string {
	return fmt.Sprintf("tfnelm-acc-%s-%d", tag, time.Now().UnixNano())
}

// kubectl runs kubectl against the pinned test context in the suite's
// kubeconfig file ONLY, explicitly, never relying on the ambient
// current-context or $KUBECONFIG (cluster safety rule). It is a bare function
// (no *testing.T) so it can be used from resource.TestCheckFunc /
// CheckDestroy closures, which only receive a *terraform.State.
func kubectl(args ...string) (string, error) {
	kubeconfig := testKubeconfigPath()
	if kubeconfig == "" {
		// An empty --kubeconfig would make kubectl fall back to $KUBECONFIG.
		return "", errors.New("kubectl: no acceptance-test kubeconfig path (home directory unknown)")
	}

	full := append([]string{"--kubeconfig=" + kubeconfig, "--context=" + testKubeContext()}, args...)

	out, err := exec.Command("kubectl", full...).CombinedOutput()

	return string(out), err
}

// mustKubectl is kubectl plus a t.Fatalf on error, for setup/fixture code
// (PreConfig closures) where a failure should abort the test immediately with
// full command output attached.
func mustKubectl(t *testing.T, args ...string) string {
	t.Helper()

	out, err := kubectl(args...)
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return out
}

// scaleDeployment performs the out-of-band `kubectl scale` acceptance scenario
// 3 (drift) needs: a live mutation the next `terraform plan` must detect via
// the resources map diff (Read always live-reads resources, never the
// stored/rendered manifest, precisely so this kind of drift is visible).
func scaleDeployment(t *testing.T, namespace, deployment string, replicas int) {
	t.Helper()

	mustKubectl(t, "scale", "deployment/"+deployment, "-n", namespace, fmt.Sprintf("--replicas=%d", replicas))
}

// helmInstallOOB installs chartDir as release name/namespace using the real,
// locally installed helm CLI (v4.2.3, never nelm/Terraform) -- acceptance
// scenario 6 (import), which also checks whether nelm's vendored helm v3
// release-storage reader can read what a real, current helm v4 CLI writes.
// --kubeconfig and --kube-context are explicit here for the same
// cluster-safety reason as every other helper in this file.
func helmInstallOOB(t *testing.T, namespace, name, chartDir string) {
	t.Helper()

	kubeconfig := testKubeconfigPath()
	if kubeconfig == "" {
		t.Fatal("helm install: no acceptance-test kubeconfig path (home directory unknown)")
	}

	cmd := exec.Command("helm", "install", name, chartDir,
		"--namespace", namespace,
		"--create-namespace",
		"--kubeconfig", kubeconfig,
		"--kube-context", testKubeContext(),
		"--wait",
		"--timeout", "60s",
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm install (out-of-band fixture setup for %s/%s) failed: %v\n%s", namespace, name, err, out)
	}
}

// testAccCheckReleaseDestroyed is the CheckDestroy every scenario that
// actually creates a release uses. It asserts the release secret + namespace
// are gone (and deletes the namespace, since nelm Uninstall leaves it):
//
//  1. Assert no release record remains in either storage backend
//     (releaseRecordsLeft): a release that moved to release_storage_driver
//     = "configmap" leaves ConfigMaps, not Secrets, and the namespace
//     deletion below would hide them.
//  2. Delete the namespace -- ReleaseUninstall always runs with
//     DeleteReleaseNamespace:false, so nothing else will ever
//     clean this up -- and confirm it is actually gone afterwards, so a
//     stuck namespace-terminating condition fails the test loudly instead of
//     leaking silently.
func testAccCheckReleaseDestroyed(namespace, name string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		if left := releaseRecordsLeft(kubectl, namespace, name); len(left) > 0 {
			return fmt.Errorf("nelm_release CheckDestroy: release record(s) for %s/%s still exist after destroy:\n%s", namespace, name, strings.Join(left, "\n"))
		}

		if out, err := kubectl("delete", "namespace", namespace, "--ignore-not-found", "--wait=true", "--timeout=180s"); err != nil {
			return fmt.Errorf("nelm_release CheckDestroy: failed to delete namespace %q for cleanup: %w\n%s", namespace, err, out)
		}

		if out, err := kubectl("get", "namespace", namespace); err == nil {
			return fmt.Errorf("nelm_release CheckDestroy: namespace %q still present after cleanup delete:\n%s", namespace, out)
		}

		return nil
	}
}

// releaseRecordsLeft lists, through get (kubectl), the release records left
// for release name in namespace in both storage backends: the Secrets and
// the ConfigMaps labeled "owner=helm,name=<name>", the convention nelm's
// vendored helm v3 storage/driver/secrets.go and cfgmaps.go share whatever
// release_storage_driver spelling selected them. A lookup error (e.g. the
// namespace is already gone) counts as no records left, not a failure.
func releaseRecordsLeft(get func(args ...string) (string, error), namespace, name string) []string {
	var left []string

	for _, kind := range []string{"secret", "configmap"} {
		out, err := get("get", kind, "-n", namespace, "-l", fmt.Sprintf("owner=helm,name=%s", name), "-o", "name")
		if err != nil {
			continue
		}

		if s := strings.TrimSpace(out); s != "" {
			left = append(left, s)
		}
	}

	return left
}

// TestReleaseRecordsLeft is the offline test of the CheckDestroy lookup: a
// release record left in either backend must fail it.
func TestReleaseRecordsLeft(t *testing.T) {
	const (
		secretRecord    = "secret/sh.helm.release.v1.my-release.v1"
		configMapRecord = "configmap/sh.helm.release.v1.my-release.v2"
	)

	tests := map[string]struct {
		records map[string]string
		errs    map[string]error
		want    []string
	}{
		"none left":        {},
		"secret left":      {records: map[string]string{"secret": secretRecord + "\n"}, want: []string{secretRecord}},
		"configmap left":   {records: map[string]string{"configmap": configMapRecord + "\n"}, want: []string{configMapRecord}},
		"both left":        {records: map[string]string{"secret": secretRecord, "configmap": configMapRecord}, want: []string{secretRecord, configMapRecord}},
		"namespace gone":   {errs: map[string]error{"secret": errors.New("not found"), "configmap": errors.New("not found")}},
		"one lookup fails": {records: map[string]string{"configmap": configMapRecord}, errs: map[string]error{"secret": errors.New("timeout")}, want: []string{configMapRecord}},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var calls [][]string

			get := func(args ...string) (string, error) {
				calls = append(calls, args)
				kind := args[1]

				return tt.records[kind], tt.errs[kind]
			}

			if got := releaseRecordsLeft(get, "ns-1", "my-release"); !slices.Equal(got, tt.want) {
				t.Errorf("releaseRecordsLeft = %q, want %q", got, tt.want)
			}

			for _, args := range calls {
				if want := []string{"get", args[1], "-n", "ns-1", "-l", "owner=helm,name=my-release", "-o", "name"}; !slices.Equal(args, want) {
					t.Errorf("lookup args = %q, want %q", args, want)
				}
			}

			if len(calls) != 2 || calls[0][1] != "secret" || calls[1][1] != "configmap" {
				t.Errorf("lookups = %q, want one per backend (secret, configmap)", calls)
			}
		})
	}
}

// TestLocalContextServer is the offline regression test for the acceptance
// guard (runs without TF_ACC). The guard used to resolve the pinned context
// through `kubectl config view`, which honours $KUBECONFIG while the provider
// never reads it, and accepted any server merely PREFIXED with
// https://127.0.0.1 or https://localhost.
func TestLocalContextServer(t *testing.T) {
	write := func(t *testing.T, name, server string) string {
		t.Helper()

		cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: cluster-of-dev
  cluster:
    server: %s
contexts:
- name: dev
  context:
    cluster: cluster-of-dev
    user: dev
users:
- name: dev
  user: {}
`, server)

		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}

		return p
	}

	tests := []struct {
		name    string
		server  string
		ctx     string
		wantErr bool
	}{
		{name: "127.0.0.1", server: "https://127.0.0.1:6443", ctx: "dev"},
		{name: "localhost", server: "https://localhost:6443", ctx: "dev"},
		{name: "IPv6 loopback", server: "https://[::1]:6443", ctx: "dev"},
		{name: "localhost-prefixed hostname", server: "https://localhost.example.com", ctx: "dev", wantErr: true},
		{name: "127.0.0.1-prefixed hostname", server: "https://127.0.0.1.nip.io:6443", ctx: "dev", wantErr: true},
		{name: "cloud endpoint", server: "https://34.1.2.3", ctx: "dev", wantErr: true},
		{name: "missing context", server: "https://127.0.0.1:6443", ctx: "orbstack", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := localContextServer(write(t, "config", tt.server), tt.ctx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("localContextServer(%q) err = %v, wantErr %v", tt.server, err, tt.wantErr)
			}
		})
	}

	t.Run("KUBECONFIG is not consulted", func(t *testing.T) {
		// kubectl would merge $KUBECONFIG and see dev -> 127.0.0.1; the file
		// the provider is pointed at says dev -> a real cluster.
		t.Setenv("KUBECONFIG", write(t, "kind.yaml", "https://127.0.0.1:6443"))

		if server, err := localContextServer(write(t, "config", "https://34.1.2.3"), "dev"); err == nil {
			t.Fatalf("localContextServer accepted %q via $KUBECONFIG", server)
		}
	})

	t.Run("missing kubeconfig file", func(t *testing.T) {
		if _, err := localContextServer(filepath.Join(t.TempDir(), "missing"), "dev"); err == nil {
			t.Fatal("localContextServer accepted a missing kubeconfig file")
		}
	})
}
