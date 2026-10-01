package provider_test

// release_replace_acc_test.go covers the lifecycle-safety acceptance
// scenarios: Create refusing to adopt an existing release (G3.1/F07),
// replacement of a tainted release (F03), a namespace move reusing the
// replaced release's cluster-scoped object names (F03), and a
// create_before_destroy replacement failing safe instead of uninstalling the
// release (G3.1). Same
// harness and triple safety guard as release_resource_test.go
// (provider_test.go).

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// releaseConfig is a minimal nelm_release config for the basic chart plus
// extra HCL inside the resource block.
func releaseConfig(name, namespace, chart, extra string) string {
	return fmt.Sprintf(`%s
resource "nelm_release" "test" {
  name      = %q
  namespace = %q
  chart     = %q
%s
}
`, providerBlock(), name, namespace, chart, extra)
}

// assertReleaseStored fails the test unless release-storage records for
// namespace/name exist, i.e. the release was NOT uninstalled.
func assertReleaseStored(t *testing.T, namespace, name string) {
	t.Helper()

	out := mustKubectl(t, "get", "secret", "-n", namespace, "-l", fmt.Sprintf("owner=helm,name=%s", name), "-o", "name")
	if strings.TrimSpace(out) == "" {
		t.Fatalf("release %s/%s has no release-storage Secrets: it was uninstalled", namespace, name)
	}
}

// TestAccReleaseResource_createRefusesExistingRelease: a nelm_release created
// for a release that already exists (here installed out-of-band by helm, as
// in a helm_release migration that forgot the import) must fail instead of
// silently adopting it; adopt_existing = true is the explicit opt-in.
func TestAccReleaseResource_createRefusesExistingRelease(t *testing.T) {
	namespace := uniqueNamespace("adopt")
	const name = "adopt"

	chart := chartPath(t)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckReleaseDestroyed(namespace, name),
		Steps: []resource.TestStep{
			{
				PreConfig:   func() { helmInstallOOB(t, namespace, name, chart) },
				Config:      releaseConfig(name, namespace, chart, ""),
				ExpectError: regexp.MustCompile(`already exists`),
			},
			{
				PreConfig: func() { assertReleaseStored(t, namespace, name) },
				Config:    releaseConfig(name, namespace, chart, "  adopt_existing = true"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("status"), knownvalue.StringExact("deployed")),
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("adopt_existing"), knownvalue.Bool(true)),
				},
			},
		},
	})
}

// TestAccReleaseResource_taintReplace: replacing a release (taint, the same
// path as -replace and a failed-create retry) destroys it and installs it
// again in one apply. It used to abort with "Provider produced inconsistent
// final plan" after the uninstall, because the create's planned resources
// were computed while the old release was still live (F03).
func TestAccReleaseResource_taintReplace(t *testing.T) {
	namespace := uniqueNamespace("taint")
	const name = "taint"

	chart := chartPath(t)
	cfg := releaseConfig(name, namespace, chart, "")

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckReleaseDestroyed(namespace, name),
		Steps: []resource.TestStep{
			{
				Config: cfg,
			},
			{
				Config: cfg,
				Taint:  []string{resourceAddr},
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(resourceAddr, plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("status"), knownvalue.StringExact("deployed")),
					// The destroy purged the release history: a fresh install.
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("revision"), knownvalue.Int64Exact(1)),
				},
			},
		},
	})
}

// TestAccReleaseResource_namespaceMoveReusesClusterObjects: a namespace move
// replaces the release, and the new release reuses the name of a
// cluster-scoped object the old one still owns while the plan runs (a
// fixed-name ClusterRole, as ingress-nginx or cert-manager have). Terraform
// plans that change first against the real prior state, where nelm's
// ownership check used to fail the plan, so the move could not be planned at
// all (F03). It must only warn, and the apply (destroy, then create) must
// hand the ClusterRole to the release in the new namespace.
func TestAccReleaseResource_namespaceMoveReusesClusterObjects(t *testing.T) {
	oldNamespace := uniqueNamespace("move-old")
	newNamespace := uniqueNamespace("move-new")
	const name = "move"

	chart := chartPath(t)
	// Unique per run, identical in both namespaces.
	clusterName := oldNamespace + "-fixed"
	fixedName := fmt.Sprintf("  set = [{ name = \"clusterNameOverride\", value = %q }]", clusterName)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(s *terraform.State) error {
			if err := testAccCheckReleaseDestroyed(newNamespace, name)(s); err != nil {
				return err
			}

			// The move uninstalled the old release; this only cleans up.
			return testAccCheckReleaseDestroyed(oldNamespace, name)(s)
		},
		Steps: []resource.TestStep{
			{
				Config: releaseConfig(name, oldNamespace, chart, fixedName),
			},
			{
				Config: releaseConfig(name, newNamespace, chart, fixedName),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(resourceAddr, plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("namespace"), knownvalue.StringExact(newNamespace)),
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("status"), knownvalue.StringExact("deployed")),
				},
				Check: func(_ *terraform.State) error {
					out, err := kubectl("get", "clusterrole", clusterName, "-o", `jsonpath={.metadata.annotations.meta\.helm\.sh/release-namespace}`)
					if err != nil {
						return fmt.Errorf("get ClusterRole %s: %w\n%s", clusterName, err, out)
					}

					if got := strings.TrimSpace(out); got != newNamespace {
						return fmt.Errorf("ClusterRole %s is owned by the release in namespace %q, want %q", clusterName, got, newNamespace)
					}

					return nil
				},
			},
		},
	})
}

// TestAccReleaseResource_createBeforeDestroyFailsSafe: a create_before_destroy
// replacement of the same release name used to install over the live release
// and then uninstall it as the deposed object, with a green apply (G3.1). The
// create half must now be refused, Terraform keeps the old object, and the
// release stays installed.
func TestAccReleaseResource_createBeforeDestroyFailsSafe(t *testing.T) {
	namespace := uniqueNamespace("cbd")
	const name = "cbd"

	chart := chartPath(t)
	cbd := releaseConfig(name, namespace, chart, "\n  lifecycle {\n    create_before_destroy = true\n  }")

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckReleaseDestroyed(namespace, name),
		Steps: []resource.TestStep{
			{
				Config: cbd,
			},
			{
				Config:      cbd,
				Taint:       []string{resourceAddr},
				ExpectError: regexp.MustCompile(`already exists`),
			},
			{
				// Still installed; without create_before_destroy the (still
				// tainted) release is replaced destroy-first as usual.
				PreConfig: func() { assertReleaseStored(t, namespace, name) },
				Config:    releaseConfig(name, namespace, chart, ""),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("status"), knownvalue.StringExact("deployed")),
				},
			},
		},
	})
}
