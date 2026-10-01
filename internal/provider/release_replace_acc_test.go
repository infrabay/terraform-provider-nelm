package provider_test

// release_replace_acc_test.go covers the lifecycle-safety acceptance
// scenarios: Create refusing to adopt an existing release (G3.1/F07),
// replacement of a tainted release (F03), and a create_before_destroy
// replacement failing safe instead of uninstalling the release (G3.1). Same
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
