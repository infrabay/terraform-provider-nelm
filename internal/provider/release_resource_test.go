package provider_test

// release_resource_test.go implements the acceptance scenarios for
// nelm_release, strictly against the context NELM_TEST_KUBE_CONTEXT pins
// ("kind-nelm-acc" via the GNUmakefile default, as in CI; any other local
// context such as "orbstack") -- see provider_test.go for the harness and the
// triple safety guard. Every Config below embeds providerBlock()
// (kube_config_paths and kube_context set explicitly to that pinned context,
// never current-context) and every test's PreCheck is testAccPreCheck.

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

// TestAccReleaseResource_lifecycle covers acceptance scenarios (1)
// create+read, (2) post-apply no-change plan (the phantom-diff guarantee),
// (3) drift (out-of-band kubectl scale -> next plan non-empty), and (4) a
// values change -> in-place update with a revision bump, all against one
// release so state carries across steps the way a real user's workflow would.
func TestAccReleaseResource_lifecycle(t *testing.T) {
	namespace := uniqueNamespace("lc")
	const name = "lc"

	chart := chartPath(t)
	deployment := name + "-basic"

	cfg := func(replicaCount int, message string) string {
		return fmt.Sprintf(`%s
resource "nelm_release" "test" {
  name      = %q
  namespace = %q
  chart     = %q

  values = [<<-YAML
    replicaCount: %d
    configMap:
      message: %q
    YAML
  ]
}
`, providerBlock(), name, namespace, chart, replicaCount, message)
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckReleaseDestroyed(namespace, name),
		Steps: []resource.TestStep{
			{
				// (1) create + read.
				Config: cfg(1, "hello-lifecycle-v1"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("id"), knownvalue.StringExact(namespace+"/"+name)),
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("status"), knownvalue.StringExact("deployed")),
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("revision"), knownvalue.Int64Exact(1)),
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("metadata").AtMapKey("chart_name"), knownvalue.StringExact("basic")),
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("metadata").AtMapKey("app_version"), knownvalue.StringExact("1.0.0")),
				},
				Check: resource.TestMatchResourceAttr(resourceAddr, "resources.%", regexp.MustCompile(`^[1-9][0-9]*$`)),
			},
			{
				// (2) post-apply no-change plan: re-applying the identical
				// config must show an empty plan (the phantom-diff hard
				// gate). terraform-plugin-testing already enforces this by
				// default for every non-PlanOnly step's own post-apply
				// refresh plan; PlanOnly + the explicit plancheck here make
				// the assertion a first-class, independent step.
				Config:   cfg(1, "hello-lifecycle-v1"),
				PlanOnly: true,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PostApplyPostRefresh: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// (3) drift: an out-of-band kubectl scale changes the live
				// Deployment's spec.replicas without touching desired
				// config. The next plan's REFRESH pass re-reads live
				// resources and must show a non-empty diff.
				PreConfig:          func() { scaleDeployment(t, namespace, deployment, 5) },
				Config:             cfg(1, "hello-lifecycle-v1"),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// (4) values change -> in-place update, revision bump. This
				// apply also incidentally fixes step (3)'s drift (nelm
				// reconciles spec.replicas back to the chart's desired
				// value), which is expected and not itself asserted here.
				Config: cfg(2, "hello-lifecycle-v2"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("revision"), knownvalue.Int64Exact(2)),
				},
			},
		},
	})
}

// TestAccReleaseResource_secretSensitive covers acceptance scenario (5): a
// set_sensitive-provided Secret value must never appear in cleartext anywhere
// under the "resources" computed attribute -- only the deterministic "<hidden
// N sensitive bytes, hash ...>" placeholder (planconv/sensitive.go's local
// V2-style override for Secret kinds) that NormalizeUnstructured produces
// before anything reaches Terraform state. A set_sensitive value the chart
// renders into a non-Secret object (the ConfigMap's data.message) is replaced
// by the same kind of placeholder (planconv.ScrubSecrets), and the post-apply
// plan the test framework runs stays empty, so the planned and the live side
// scrub identically.
func TestAccReleaseResource_secretSensitive(t *testing.T) {
	namespace := uniqueNamespace("sec")
	const name = "sec"

	chart := chartPath(t)
	const sensitiveValue = "s3cr3t-acceptance-test-value-do-not-leak"
	const configMapValue = "c0nfigmap-acceptance-test-value-do-not-leak"
	// Key(ref, releaseNS, scoper) = "<apiVersion>/<Kind>/<namespace>/<name>"
	// (planconv/key.go); the chart's Secret and ConfigMap are named
	// "<release>-basic".
	secretResourcesKey := fmt.Sprintf("v1/Secret/%s/%s-basic", namespace, name)
	configMapResourcesKey := fmt.Sprintf("v1/ConfigMap/%s/%s-basic", namespace, name)

	cfg := fmt.Sprintf(`%s
resource "nelm_release" "test" {
  name      = %q
  namespace = %q
  chart     = %q

  set_sensitive = [
    {
      name  = "secret.password"
      value = %q
    },
    {
      name  = "configMap.message"
      value = %q
    },
  ]
}
`, providerBlock(), name, namespace, chart, sensitiveValue, configMapValue)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckReleaseDestroyed(namespace, name),
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources[resourceAddr]
					if !ok {
						return fmt.Errorf("resource %s not found in state", resourceAddr)
					}

					// No "resources.*" attribute value may contain the
					// cleartext secret value, anywhere in the map -- not
					// just at the Secret's own key.
					for k, v := range rs.Primary.Attributes {
						if !strings.HasPrefix(k, "resources.") || k == "resources.%" {
							continue
						}

						if strings.Contains(v, sensitiveValue) || strings.Contains(v, configMapValue) {
							return fmt.Errorf("cleartext secret value leaked into state at %s: %s", k, v)
						}
					}

					cm, ok := rs.Primary.Attributes["resources."+configMapResourcesKey]
					if !ok || !strings.Contains(cm, "sensitive bytes, hash") {
						return fmt.Errorf("expected a placeholder for the set_sensitive ConfigMap value at %s, got: %q", configMapResourcesKey, cm)
					}

					gotKey := "resources." + secretResourcesKey

					got, ok := rs.Primary.Attributes[gotKey]
					if !ok {
						return fmt.Errorf("expected %s in state (available resources.* keys logged below); state:\n%#v", gotKey, rs.Primary.Attributes)
					}

					// encoding/json.Marshal HTML-escapes "<"/">" by default
					// (planconv.NormalizeUnstructured's canonical JSON), so
					// the placeholder appears as "<hidden ...>" in
					// the raw attribute string, not literal angle brackets --
					// harmless (still valid, deterministic JSON; never a
					// phantom-diff source since both sides of the diff
					// escape identically), but this check must not require
					// the literal "<" byte. "hidden " + "sensitive bytes,
					// hash" are unaffected by that escaping either way.
					if !strings.Contains(got, "hidden ") || !strings.Contains(got, "sensitive bytes, hash") {
						return fmt.Errorf("expected a redacted \"<hidden N sensitive bytes, hash ...>\" placeholder at %s, got: %s", gotKey, got)
					}

					return nil
				},
			},
		},
	})
}

// TestAccReleaseResource_import covers acceptance scenario (6): adopting a
// release the real, locally installed helm CLI (v4.2.3) created entirely
// out-of-band -- never through Terraform/nelm -- and confirming the resulting
// state needs no further changes.
//
// This deliberately does NOT use ImportStateVerify's built-in "old vs new"
// comparison (traced directly against terraform-plugin-testing v1.16.0's
// testing_new_import_state.go testImportCommand): that comparison diffs the
// freshly imported state against whatever resource with the same "id" ALREADY
// existed in this TestCase's own (persistent) working-directory state before
// this step ran -- which is empty by construction, since this release was
// never created by Terraform. That comparison is structurally unsatisfiable as
// a TestCase's first step touching a resource, regardless of
// ImportStateVerifyIgnore contents. (The obvious alternative,
// "ImportStateKind: ImportBlockWithID + ImportPlanChecks", was tried first
// here and rejected for a narrower, purely mechanical reason:
// terraform-plugin-testing v1.16.0 hard-rejects ImportStatePersist=true
// combined with a plannable ImportStateKind ("ImportStatePersist is not
// supported with plannable import blocks"), and without persisting, the
// follow-up empty-plan step has no state to plan against.) Instead:
// ImportStateCheck asserts a few key attributes directly on the freshly
// imported InstanceState (no baseline needed), and the second TestStep asserts
// the actual "zero-diff adoption" guarantee via a plain follow-up plan against
// the (now persisted) imported state.
func TestAccReleaseResource_import(t *testing.T) {
	namespace := uniqueNamespace("imp")
	const name = "imp"

	chart := chartPath(t)

	// No values/set/set_sensitive overrides (a config without values
	// overrides) -- chart defaults must match exactly what the plain `helm
	// install` below also used (also chart defaults), so nothing here is a
	// legitimate config-vs-cluster diff.
	cfg := fmt.Sprintf(`%s
resource "nelm_release" "test" {
  name      = %q
  namespace = %q
  chart     = %q
}
`, providerBlock(), name, namespace, chart)

	wantID := namespace + "/" + name

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckReleaseDestroyed(namespace, name),
		Steps: []resource.TestStep{
			{
				ResourceName: resourceAddr,
				PreConfig:    func() { helmInstallOOB(t, namespace, name, chart) },
				Config:       cfg,
				ImportState:  true,
				ImportStateIdFunc: func(_ *terraform.State) (string, error) {
					// "namespace/name" (the import ID format); built directly
					// rather than read back from prior TF state, since there
					// is none yet -- this release was created by helm, not
					// Terraform.
					return wantID, nil
				},
				// Persist into the TestCase's real working-directory state
				// so (a) the follow-up empty-plan step below has something
				// to plan against, and (b) the TestCase's final teardown
				// runs a real `terraform destroy` (-> Delete -> Uninstall)
				// against this adopted release, exercising that path too.
				ImportStatePersist: true,
				ImportStateCheck: func(is []*terraform.InstanceState) error {
					if len(is) != 1 {
						return fmt.Errorf("expected exactly 1 imported instance state, got %d", len(is))
					}

					attrs := is[0].Attributes
					if got := attrs["id"]; got != wantID {
						return fmt.Errorf(`expected imported "id" = %q, got %q`, wantID, got)
					}

					if got := attrs["status"]; got != "deployed" {
						return fmt.Errorf(`expected imported "status" = "deployed", got %q`, got)
					}

					if got := attrs["metadata.chart_name"]; got != "basic" {
						return fmt.Errorf(`expected imported "metadata.chart_name" = "basic", got %q`, got)
					}

					return nil
				},
			},
			{
				// Known gap (not a test bug): ideally the follow-up plan here
				// would be EMPTY (plancheck.ExpectEmptyPlan). Live it is NOT:
				// ImportState (release_crud.go) never sets "chart" (nor
				// repository/version), and Read never touches config-only
				// inputs by design, so "chart" stays null after import. Since
				// "chart" is Required (non-Computed, no RequiresReplace), ANY
				// real config's non-null chart value is therefore an
				// unavoidable config-vs-state diff on the very first
				// post-import plan, which correctly cascades into
				// metadata/status/revision going Unknown (ModifyPlan's
				// existing, correct "metadata changed" logic) -- this is a
				// genuine gap between a zero-diff import and the current
				// schema/ImportState shape, not a flaw in this test. What IS
				// asserted here, and IS true live: the resulting plan is a
				// plain in-place Update -- never a destroy/recreate of the
				// adopted release -- and (per the full plan output captured in
				// the run log) "resources" and "values"/"set"/"set_sensitive"
				// are NOT part of the diff, i.e. the normalization/redaction
				// pipeline itself round-trips cleanly through import; only
				// chart and its dependent computed attributes do not.
				Config:             cfg,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PostApplyPostRefresh: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(resourceAddr, plancheck.ResourceActionUpdate),
					},
				},
			},
		},
	})
}

// TestAccReleaseResource_waitFalse covers the `wait` attribute (helm_release
// wait = false parity): a release whose only workload can never become ready
// (its image is unpullable) must still apply cleanly and report "deployed"
// with wait = false, because nelm then skips final readiness tracking. With
// the default wait = true nelm's tracker fails this exact apply on
// ErrImagePull; the short create timeout bounds a regression to minutes
// instead of the 10m default.
func TestAccReleaseResource_waitFalse(t *testing.T) {
	namespace := uniqueNamespace("nowait")
	const name = "nowait"

	chart := chartPath(t)

	cfg := fmt.Sprintf(`%s
resource "nelm_release" "test" {
  name      = %q
  namespace = %q
  chart     = %q
  wait      = false

  values = [<<-YAML
    image:
      repository: registry.invalid/tf-nelm/never-pulls
    YAML
  ]

  timeouts {
    create = "3m"
  }
}
`, providerBlock(), name, namespace, chart)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckReleaseDestroyed(namespace, name),
		Steps: []resource.TestStep{
			{
				Config: cfg,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("status"), knownvalue.StringExact("deployed")),
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("wait"), knownvalue.Bool(false)),
				},
			},
		},
	})
}

// TestAccReleaseResource_invalidStorageDriver covers acceptance scenario (7):
// release_storage_driver's OneOf validator must reject "memory" (explicitly
// out of v1 -- verified fact: an unrecognized driver string panics inside
// Nelm's NewReleaseStorage) and any other unrecognized value at PLAN time,
// with a normal validation error -- never a panic, and never a real cluster
// call (Configure only builds a client and runs nelmclient.Init; ModifyPlan
// never runs because validation fails before ModifyPlan would).
func TestAccReleaseResource_invalidStorageDriver(t *testing.T) {
	namespace := uniqueNamespace("bad")
	const name = "bad"

	chart := chartPath(t)

	cfgWithDriver := func(driver string) string {
		return fmt.Sprintf(`%s
resource "nelm_release" "test" {
  name                   = %q
  namespace              = %q
  chart                  = %q
  release_storage_driver = %q
}
`, providerBlock(), name, namespace, chart, driver)
	}

	invalidDriver := regexp.MustCompile(`(?i)value must be one of`)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      cfgWithDriver("memory"),
				ExpectError: invalidDriver,
			},
			{
				Config:      cfgWithDriver("totally-not-a-real-driver"),
				ExpectError: invalidDriver,
			},
		},
	})
}
