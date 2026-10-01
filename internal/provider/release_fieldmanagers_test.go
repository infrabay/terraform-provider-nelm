package provider_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// helmProviderManagerPrefix is the field manager hashicorp/helm's
// helm_release writes objects with ("terraform-provider-helm_v<ver>_x5").
const helmProviderManagerPrefix = "terraform-provider-helm"

// TestAccReleaseResource_migrateFromHelmRelease covers the helm_release ->
// nelm_release migration end to end (review finding F08): a release created
// by the real hashicorp/helm provider (Helm 3 SDK, client-side writes under a
// "terraform-provider-helm_*" field manager) moves to nelm_release through
// removed{destroy=false} + import{}, in the same change that drops a
// value-driven ConfigMap key. Without the provider's field-manager hand-over,
// nelm's server-side apply only co-owned that key with the stale manager, so
// it stayed live indefinitely (and invisible to the projected resources
// diff); with it, the very first nelm apply prunes it.
func TestAccReleaseResource_migrateFromHelmRelease(t *testing.T) {
	namespace := uniqueNamespace("mig")
	const name = "mig"

	chart := fieldManagersChartPath(t)
	configMap := name + "-fieldmanagers"

	helmCfg := fmt.Sprintf(`%s
resource "helm_release" "test" {
  name             = %q
  namespace        = %q
  chart            = %q
  create_namespace = true

  set = [
    {
      name  = "data.FEATURE_X"
      value = "on"
    },
  ]
}
`, helmProviderBlock(), name, namespace, chart)

	// The migration change also drops the "set": the first nelm apply renders
	// the ConfigMap without FEATURE_X.
	nelmCfg := fmt.Sprintf(`%s
%s
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

resource "nelm_release" "test" {
  name      = %q
  namespace = %q
  chart     = %q
}
`, helmProviderBlock(), providerBlock(), namespace, name, name, namespace, chart)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		ExternalProviders:        helmExternalProviders,
		CheckDestroy:             testAccCheckReleaseDestroyed(namespace, name),
		Steps: []resource.TestStep{
			{
				// The premise: helm_release wrote FEATURE_X under its own
				// field manager.
				Config: helmCfg,
				Check: func(_ *terraform.State) error {
					cm, err := liveConfigMap(namespace, configMap)
					if err != nil {
						return err
					}

					if got := cm.Data["FEATURE_X"]; got != "on" {
						return fmt.Errorf("helm_release did not render FEATURE_X (got %q)", got)
					}

					if !cm.hasManagerPrefix(helmProviderManagerPrefix) {
						return fmt.Errorf("expected a %s* field manager on the helm_release object, got %v", helmProviderManagerPrefix, cm.managers())
					}

					return nil
				},
			},
			{
				// Migrate and drop FEATURE_X in the same apply. The harness's
				// own post-apply plan also asserts the migrated release
				// converges to an empty plan.
				Config: nelmCfg,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("status"), knownvalue.StringExact("deployed")),
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("revision"), knownvalue.Int64Exact(2)),
				},
				Check: func(_ *terraform.State) error {
					cm, err := liveConfigMap(namespace, configMap)
					if err != nil {
						return err
					}

					if got, ok := cm.Data["FEATURE_X"]; ok {
						return fmt.Errorf("FEATURE_X is still live after the first nelm apply (%q): the helm_release field manager was not handed over", got)
					}

					if got := cm.Data["KEEP"]; got != "1" {
						return fmt.Errorf("KEEP = %q after the first nelm apply, want \"1\"", got)
					}

					if cm.hasManagerPrefix(helmProviderManagerPrefix) {
						return fmt.Errorf("a %s* field manager is left on the migrated object: %v", helmProviderManagerPrefix, cm.managers())
					}

					return nil
				},
			},
		},
	})
}

// fieldManagersChartPath returns the absolute path to
// testdata/charts/fieldmanagers (see chartPath for why it must be absolute).
func fieldManagersChartPath(t *testing.T) string {
	t.Helper()

	abs, err := filepath.Abs("../../testdata/charts/fieldmanagers")
	if err != nil {
		t.Fatalf("could not resolve testdata chart path: %v", err)
	}

	return abs
}

// configMapView is the part of a live ConfigMap the migration test asserts on.
type configMapView struct {
	Data     map[string]string `json:"data"`
	Metadata struct {
		ManagedFields []struct {
			Manager string `json:"manager"`
		} `json:"managedFields"`
	} `json:"metadata"`
}

func (c configMapView) managers() []string {
	out := make([]string, 0, len(c.Metadata.ManagedFields))
	for _, e := range c.Metadata.ManagedFields {
		out = append(out, e.Manager)
	}

	return out
}

func (c configMapView) hasManagerPrefix(prefix string) bool {
	for _, m := range c.managers() {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}

	return false
}

// liveConfigMap reads a ConfigMap from the pinned test context in the suite's
// kubeconfig file (explicitly, like kubectl()). Only stdout is decoded, so a
// kubectl warning on stderr cannot corrupt the JSON. --show-managed-fields is
// required: kubectl >= 1.21 strips metadata.managedFields from -o json output
// otherwise.
func liveConfigMap(namespace, name string) (configMapView, error) {
	var cm configMapView

	kubeconfig := testKubeconfigPath()
	if kubeconfig == "" {
		// An empty --kubeconfig would make kubectl fall back to $KUBECONFIG.
		return cm, errors.New("kubectl: no acceptance-test kubeconfig path (home directory unknown)")
	}

	cmd := exec.Command("kubectl", "--kubeconfig="+kubeconfig, "--context="+testKubeContext(),
		"get", "configmap", name, "-n", namespace, "-o", "json", "--show-managed-fields")

	out, err := cmd.Output()
	if err != nil {
		return cm, fmt.Errorf("kubectl get configmap %s/%s: %w", namespace, name, err)
	}

	if err := json.Unmarshal(out, &cm); err != nil {
		return cm, fmt.Errorf("decode configmap %s/%s: %w", namespace, name, err)
	}

	return cm, nil
}
