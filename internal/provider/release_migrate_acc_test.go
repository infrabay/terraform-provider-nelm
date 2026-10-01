package provider_test

// release_migrate_acc_test.go covers handing a release managed by
// hashicorp/helm's helm_release over to nelm_release without uninstalling it,
// both ways docs/guides/migrating-from-helm_release.md documents: removed +
// import (Terraform 1.7+) and moved (Terraform 1.8+, ResourceWithMoveState).
// The helm provider is pinned to the same local test context as nelm (same
// triple safety guard, provider_test.go).

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// helmExternalProviders downloads hashicorp/helm for the helm_release side.
var helmExternalProviders = map[string]resource.ExternalProvider{
	"helm": {Source: "hashicorp/helm", VersionConstraint: "~> 3.0"},
}

// helmProviderBlock configures hashicorp/helm for the pinned test context in
// the suite's kubeconfig file, explicitly (never the ambient current-context
// or $KUBECONFIG), like providerBlock.
func helmProviderBlock() string {
	return fmt.Sprintf(`provider "helm" {
  kubernetes = {
    config_path    = %q
    config_context = %q
  }
}
`, testKubeconfigPath(), testKubeContext())
}

// helmReleaseConfig is the "before" configuration: the release managed by
// helm_release.
func helmReleaseConfig(name, namespace, chart string) string {
	return fmt.Sprintf(`%s
resource "helm_release" "test" {
  name             = %q
  namespace        = %q
  chart            = %q
  create_namespace = true
  max_history      = 5
}
`, helmProviderBlock(), name, namespace, chart)
}

// migratedConfig is the "after" configuration: handover blocks plus the
// nelm_release with the same inputs.
func migratedConfig(name, namespace, chart, handover string) string {
	return fmt.Sprintf(`%s
%s
%s

resource "nelm_release" "test" {
  name                  = %q
  namespace             = %q
  chart                 = %q
  release_history_limit = 5
}
`, providerBlock(), helmProviderBlock(), handover, name, namespace, chart)
}

// expectNothingDestroyedOrCreated fails a plan that deletes or creates
// anything: a handover must only forget, import, move or update.
type expectNothingDestroyedOrCreated struct{}

func (expectNothingDestroyedOrCreated) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	for _, rc := range req.Plan.ResourceChanges {
		if actions := rc.Change.Actions; actions.Delete() || actions.Create() || actions.Replace() {
			resp.Error = fmt.Errorf("%s: planned actions %v, but a helm_release handover must not create or destroy anything", rc.Address, actions)
			return
		}
	}
}

// testAccHelmReleaseHandover installs a release with helm_release, hands it
// over to nelm_release with the blocks handover(namespace, name) returns,
// and checks that nothing was uninstalled.
func testAccHelmReleaseHandover(t *testing.T, tag string, handover func(namespace, name string) string) {
	t.Helper()

	namespace := uniqueNamespace(tag)
	name := tag

	chart := chartPath(t)
	after := migratedConfig(name, namespace, chart, handover(namespace, name))

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		ExternalProviders:        helmExternalProviders,
		CheckDestroy:             testAccCheckReleaseDestroyed(namespace, name),
		Steps: []resource.TestStep{
			{
				Config: helmReleaseConfig(name, namespace, chart),
			},
			{
				Config: after,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{expectNothingDestroyedOrCreated{}},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("nelm_release.test", tfjsonpath.New("id"), knownvalue.StringExact(namespace+"/"+name)),
					statecheck.ExpectKnownValue("nelm_release.test", tfjsonpath.New("status"), knownvalue.StringExact("deployed")),
				},
			},
			{
				// The handover kept the release: still installed, and its
				// nelm_release is stable.
				PreConfig: func() { assertReleaseStored(t, namespace, name) },
				Config:    after,
				PlanOnly:  true,
			},
		},
	})
}

// TestAccReleaseResource_migrateRemovedImport is the guide's one-apply
// recipe: removed { destroy = false } + import + the same inputs.
func TestAccReleaseResource_migrateRemovedImport(t *testing.T) {
	testAccHelmReleaseHandover(t, "migimp", func(namespace, name string) string {
		return fmt.Sprintf(`
removed {
  from = helm_release.test

  lifecycle {
    destroy = false
  }
}

import {
  to = nelm_release.test
  id = "%s/%s"
}
`, namespace, name)
	})
}

// TestAccReleaseResource_migrateMoved hands the release over with a moved
// block (nelm_release's MoveState for hashicorp/helm helm_release).
func TestAccReleaseResource_migrateMoved(t *testing.T) {
	testAccHelmReleaseHandover(t, "migmv", func(string, string) string {
		return `
moved {
  from = helm_release.test
  to   = nelm_release.test
}
`
	})
}
