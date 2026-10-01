package provider_test

// release_volatile_acc_test.go covers charts whose render changes on every
// render — testdata/charts/volatile: a lookup-guarded generated password, a
// rollme annotation and a deploy-date timestamp — their replacement, and
// diff_mode = "none" for a chart that can never converge. Same harness and
// triple safety guard as release_resource_test.go (provider_test.go).

import (
	"path/filepath"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// volatileChartPath returns the absolute path to testdata/charts/volatile.
func volatileChartPath(t *testing.T) string {
	t.Helper()

	abs, err := filepath.Abs("../../testdata/charts/volatile")
	if err != nil {
		t.Fatalf("could not resolve testdata chart path: %v", err)
	}

	return abs
}

// TestAccReleaseResource_volatileChart: the first install of a chart that
// generates a password and stamps random/time annotations used to abort
// with "Provider produced inconsistent final plan" (the plan and apply
// phases render different values), and every later apply did too. Now the
// install applies, the next plan is empty (no perpetual rollout), and an
// unrelated change reinstalls it with those objects known after apply.
func TestAccReleaseResource_volatileChart(t *testing.T) {
	namespace := uniqueNamespace("vol")
	const name = "vol"

	chart := volatileChartPath(t)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckReleaseDestroyed(namespace, name),
		Steps: []resource.TestStep{
			{
				// The harness re-plans after the apply and fails the step
				// unless that plan is empty.
				Config: releaseConfig(name, namespace, chart, ""),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("status"), knownvalue.StringExact("deployed")),
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("revision"), knownvalue.Int64Exact(1)),
				},
			},
			{
				Config: releaseConfig(name, namespace, chart, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config: releaseConfig(name, namespace, chart, `  set = [{ name = "configMap.message", value = "changed" }]`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(resourceAddr, plancheck.ResourceActionUpdate),
						plancheck.ExpectUnknownValue(resourceAddr,
							tfjsonpath.New("resources").AtMapKey("apps/v1/Deployment/"+namespace+"/"+name+"-volatile")),
					},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("revision"), knownvalue.Int64Exact(2)),
				},
			},
		},
	})
}

// TestAccReleaseResource_replaceLookupChart: replacing a release whose chart
// reads its own live objects with lookup (the volatile chart's Secret keeps
// its generated password once it exists) used to uninstall the release and
// then abort with "Provider produced inconsistent final plan": the plan
// rendered the password lookup found on the old release's Secret, the
// apply-time re-plan, after the destroy, a newly generated one. While the
// replaced release is live, the create's resources are now known after apply.
// Covers a taint and a destroy-first release_storage_driver change (whose
// replaced release lives in the other backend).
func TestAccReleaseResource_replaceLookupChart(t *testing.T) {
	namespace := uniqueNamespace("vol-replace")
	const name = "vol-replace"

	chart := volatileChartPath(t)
	configmap := `  release_storage_driver = "configmap"`

	replaced := []plancheck.PlanCheck{
		plancheck.ExpectResourceAction(resourceAddr, plancheck.ResourceActionDestroyBeforeCreate),
		plancheck.ExpectUnknownValue(resourceAddr, tfjsonpath.New("resources")),
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckReleaseDestroyed(namespace, name),
		Steps: []resource.TestStep{
			{
				Config: releaseConfig(name, namespace, chart, ""),
			},
			{
				Config:           releaseConfig(name, namespace, chart, ""),
				Taint:            []string{resourceAddr},
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: replaced},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("status"), knownvalue.StringExact("deployed")),
					// The destroy purged the release history: a fresh install.
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("revision"), knownvalue.Int64Exact(1)),
				},
			},
			{
				Config:           releaseConfig(name, namespace, chart, configmap),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: replaced},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("release_storage_driver"), knownvalue.StringExact("configmap")),
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("status"), knownvalue.StringExact("deployed")),
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("revision"), knownvalue.Int64Exact(1)),
				},
			},
		},
	})
}

// TestAccReleaseResource_diffModeNone: a .Release.Revision annotation renders
// the next revision against the live one on every plan, so under
// diff_mode = "full" the release never converges; "none" plans it like
// helm_release does — empty until an input changes.
func TestAccReleaseResource_diffModeNone(t *testing.T) {
	namespace := uniqueNamespace("nodiff")
	const name = "nodiff"

	chart := volatileChartPath(t)

	revisionSet := `  set = [{ name = "revisionAnnotation", value = "true" }]`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckReleaseDestroyed(namespace, name),
		Steps: []resource.TestStep{
			{
				Config: releaseConfig(name, namespace, chart, revisionSet+"\n  diff_mode = \"none\""),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("status"), knownvalue.StringExact("deployed")),
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("diff_mode"), knownvalue.StringExact("none")),
				},
			},
			{
				Config: releaseConfig(name, namespace, chart, revisionSet+"\n  diff_mode = \"none\""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Back to "full": every plan now renders revision N+1 against
				// the live N.
				Config:             releaseConfig(name, namespace, chart, revisionSet),
				ExpectNonEmptyPlan: true,
			},
		},
	})
}
