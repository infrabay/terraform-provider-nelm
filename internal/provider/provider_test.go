package provider_test

// provider_test.go is the acceptance-test HARNESS (design §6, OWNERS.json
// "acctest"). It replaces the Phase A pre-warm placeholder that used to live
// here. release_resource_test.go (same package, same owner) holds the actual
// scenario Test functions; everything below is shared plumbing: the
// protocol-v6 provider factory, the triple orbstack safety guard, and small
// kubectl/helm helpers the scenarios use to create out-of-band drift/fixtures
// and to clean up after themselves.
//
// CLUSTER SAFETY: the machine this runs on may have its kubeconfig
// current-context pointed at a remote or production cluster. Every helper here
// that shells out to kubectl passes "--context=<pinned test context>" EXPLICITLY
// and never relies on the ambient current-context; every Terraform provider
// block a test config builds sets kube_context to that same pinned context
// explicitly (see providerBlock below). The pinned context comes from
// NELM_TEST_KUBE_CONTEXT ("orbstack" locally via the GNUmakefile default,
// "kind-nelm-acc" in CI) and testAccPreCheck hard-verifies it resolves to a
// LOCAL (127.0.0.1/localhost) API server before anything runs.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

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
// to: the value of NELM_TEST_KUBE_CONTEXT. Empty means "not opted in" and
// testAccPreCheck hard-fails. "orbstack" locally (the GNUmakefile default),
// "kind-nelm-acc" in the CI kind job.
func testKubeContext() string {
	return os.Getenv("NELM_TEST_KUBE_CONTEXT")
}

// testAccPreCheck is the triple local-cluster safety guard (design §6,
// GNUmakefile testacc target, docs/DEVELOPMENT.md): it MUST hard-fail
// (t.Fatal, not t.Skip) unless ALL of the following hold, so acceptance
// tests can never silently no-op against -- or worse, actually run against
// -- a real cluster:
//
//  1. TF_ACC is set (resource.Test's own gate already skips before PreCheck
//     runs when this is unset; checked again here defensively in case this
//     function is ever invoked outside resource.Test).
//  2. NELM_TEST_KUBE_CONTEXT is set -- an explicit, separate opt-in pin
//     (distinct from TF_ACC) naming the ONLY context this suite is allowed
//     to touch. There is deliberately no default.
//  3. That kubeconfig context actually exists AND its cluster.server looks
//     like a LOCAL endpoint (https://127.0.0.1:* / https://localhost:*),
//     resolved via `kubectl config view` (reads the kubeconfig file only,
//     no cluster I/O). This is the real safety net: no managed/cloud
//     cluster endpoint ever looks like localhost.
//
// This is deliberately independent of (and in addition to) every test
// Config's own explicit `kube_context = ...` (never current-context).
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
				"NELM_TEST_KUBE_CONTEXT=orbstack locally or kind-nelm-acc in CI",
		)
	}

	// Resolve the pinned CONTEXT (not just a same-named cluster entry -- a
	// context's cluster reference can differ from its own name) to its
	// cluster name, then that cluster's server URL. Both steps read the
	// kubeconfig file only; kubectl config view never touches the network.
	clusterName, err := kubectl(
		"config", "view", "-o",
		fmt.Sprintf(`jsonpath={.contexts[?(@.name==%q)].context.cluster}`, kubeCtx),
	)
	if err != nil {
		t.Fatalf("could not inspect kubeconfig for context %q: %v\n%s", kubeCtx, err, clusterName)
	}

	clusterName = strings.TrimSpace(clusterName)
	if clusterName == "" {
		t.Fatalf("kubeconfig context %q does not exist; refusing to run acceptance tests", kubeCtx)
	}

	server, err := kubectl(
		"config", "view", "-o",
		fmt.Sprintf(`jsonpath={.clusters[?(@.name==%q)].cluster.server}`, clusterName),
	)
	if err != nil {
		t.Fatalf("could not inspect kubeconfig cluster %q: %v\n%s", clusterName, err, server)
	}

	server = strings.TrimSpace(server)
	if server == "" {
		t.Fatalf("kubeconfig context %q references cluster %q, which has no server URL", kubeCtx, clusterName)
	}

	if !strings.HasPrefix(server, "https://127.0.0.1") && !strings.HasPrefix(server, "https://localhost") {
		t.Fatalf(
			"kubeconfig context %q resolves to cluster %q with server %q, which does not look like a "+
				"local endpoint (expected https://127.0.0.1:* or https://localhost:*); refusing "+
				"to run acceptance tests against what may be a real cluster",
			kubeCtx, clusterName, server,
		)
	}
}

// providerBlock is the provider configuration every scenario's Config
// embeds. kube_context is always explicit -- never the ambient
// current-context -- per the cluster safety rule above.
func providerBlock() string {
	return fmt.Sprintf(`provider "nelm" {
  kube_context = %q
}
`, testKubeContext())
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
// orbstack cluster if cleanup is ever interrupted (e.g. by a panic).
func uniqueNamespace(tag string) string {
	return fmt.Sprintf("tfnelm-acc-%s-%d", tag, time.Now().UnixNano())
}

// kubectl runs kubectl against the pinned test context ONLY, explicitly,
// never relying on the ambient current-context (cluster safety rule). It is a
// bare function (no *testing.T) so it can be used from resource.TestCheckFunc
// / CheckDestroy closures, which only receive a *terraform.State.
func kubectl(args ...string) (string, error) {
	full := append([]string{"--context=" + testKubeContext()}, args...)

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

// scaleDeployment performs the out-of-band `kubectl scale` design §6
// scenario 3 (drift) needs: a live mutation the next `terraform plan` must
// detect via the resources map diff (design §2.4 -- Read always live-reads
// resources, never the stored/rendered manifest, precisely so this kind of
// drift is visible).
func scaleDeployment(t *testing.T, namespace, deployment string, replicas int) {
	t.Helper()

	mustKubectl(t, "scale", "deployment/"+deployment, "-n", namespace, fmt.Sprintf("--replicas=%d", replicas))
}

// helmInstallOOB installs chartDir as release name/namespace using the real,
// locally installed helm CLI (v4.2.3, never nelm/Terraform) -- design §6
// scenario 6 (import) and design §7 risk #2 (whether nelm's vendored helm v3
// release-storage reader can read what a real, current helm v4 CLI writes).
// --kube-context is explicit here for the same cluster-safety reason as
// every other helper in this file.
func helmInstallOOB(t *testing.T, namespace, name, chartDir string) {
	t.Helper()

	cmd := exec.Command("helm", "install", name, chartDir,
		"--namespace", namespace,
		"--create-namespace",
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
// actually creates a release uses. Design §6: "CheckDestroy asserts the
// release secret + namespace are gone (delete the namespace in CheckDestroy
// since nelm Uninstall leaves it)":
//
//  1. Assert no release-storage Secret remains (label selector
//     "owner=helm,name=<name>" -- the same convention nelm's vendored helm
//     v3 storage/driver/secrets.go uses, regardless of "secret" vs "secrets"
//     release_storage_driver spelling). A lookup error here (e.g. the
//     namespace is already gone) is treated as "no secrets left", not a
//     failure.
//  2. Delete the namespace -- ReleaseUninstall always runs with
//     DeleteReleaseNamespace:false (design §2.3), so nothing else will ever
//     clean this up -- and confirm it is actually gone afterwards, so a
//     stuck namespace-terminating condition fails the test loudly instead of
//     leaking silently.
func testAccCheckReleaseDestroyed(namespace, name string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		if out, err := kubectl("get", "secret", "-n", namespace, "-l", fmt.Sprintf("owner=helm,name=%s", name), "-o", "name"); err == nil {
			if s := strings.TrimSpace(out); s != "" {
				return fmt.Errorf("nelm_release CheckDestroy: release secret(s) for %s/%s still exist after destroy:\n%s", namespace, name, s)
			}
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
