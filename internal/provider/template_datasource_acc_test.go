package provider_test

// template_datasource_acc_test.go covers the nelm_template data source on
// testdata/charts/template and testdata/charts/namespaces: offline renders
// under a provider block that names no cluster at all (no cluster guard
// needed, nothing touches a cluster: clearKubeEnv also points HOME at an
// empty directory, so even a fail-closed regression finds no kubeconfig;
// resource.Test still skips without TF_ACC), nelm_release refusing that same
// block, and validate = true renders against the pinned local cluster with
// the same harness and triple safety guard as release_resource_test.go
// (provider_test.go).

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"sigs.k8s.io/yaml"
)

// templateAddr is the Terraform address every config below declares its
// nelm_template under.
const templateAddr = "data.nelm_template.test"

// emptyProviderBlock names no cluster: no connection attribute and, with
// clearKubeEnv, no KUBE_* environment variable to fill one in.
const emptyProviderBlock = `provider "nelm" {}
`

// templateCRD is the standalone CRD of the template chart (crds/widget.yaml).
// Uninstalling a release never deletes the CRDs of its chart's crds/
// directory (Helm semantics), so a test that installs the chart deletes it.
const templateCRD = "widgets.example.com"

// templateChartPath returns the absolute path to testdata/charts/template.
func templateChartPath(t *testing.T) string {
	t.Helper()

	abs, err := filepath.Abs("../../testdata/charts/template")
	if err != nil {
		t.Fatalf("could not resolve testdata chart path: %v", err)
	}

	return abs
}

// namespacesChartPath returns the absolute path to testdata/charts/namespaces.
func namespacesChartPath(t *testing.T) string {
	t.Helper()

	abs, err := filepath.Abs("../../testdata/charts/namespaces")
	if err != nil {
		t.Fatalf("could not resolve testdata chart path: %v", err)
	}

	return abs
}

// clearKubeEnv unsets, for the rest of the test, the environment variables
// that fill in an empty provider block's connection (KUBE_CONFIG_PATHS,
// KUBE_CONFIG_PATH, KUBE_CTX), so emptyProviderBlock names no cluster whatever
// the shell running the suite exports. The provider runs in the test process,
// so it sees this environment. t.Setenv restores the old values afterwards.
//
// It also points the home directory and KUBECACHEDIR at empty directories:
// these tests' only guard is the fail-closed behavior under test (the
// validate = true step), and nelm's defaults resolve a zero connection to
// ~/.kube/config's current-context (never $KUBECONFIG), so a regression must
// land on a kubeconfig that does not exist, not the operator's. The home
// directory is HOME and, for Windows, USERPROFILE (os.UserHomeDir, which
// nelm asks) with HOMEDRIVE/HOMEPATH unset (client-go's homedir.HomeDir
// tries them too); os.UserHomeDir is then checked, so a platform it misses
// fails here instead of reaching the real one. KUBERNETES_SERVICE_HOST and
// KUBERNETES_SERVICE_PORT are unset too, so that client-go never falls back
// to the in-cluster config of a pod the suite runs in. KUBERNETES_MASTER,
// client-go's default server for a kubeconfig that names none, is read once
// at process start, so it cannot be cleared: a test process started with it
// fails here.
//
// The rest of the harness does not read HOME: terraform-plugin-testing finds
// the CLI via TF_ACC_TERRAFORM_PATH, PATH or an install under
// TF_ACC_TEMP_DIR (or the OS temp directory), the provider is reattached
// in-process (no CLI configuration or plugin directory needed), and the test
// binary runs no go command, so a GOCACHE or GOMODCACHE derived from HOME
// does not matter. A terraform on PATH that is a version-manager shim
// needing HOME (asdf's, for one) needs TF_ACC_TERRAFORM_PATH set to the real
// binary. It returns the empty home directory.
func clearKubeEnv(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("KUBECACHEDIR", t.TempDir())

	for _, k := range []string{
		"KUBE_CONFIG_PATHS", "KUBE_CONFIG_PATH", "KUBE_CTX",
		"HOMEDRIVE", "HOMEPATH",
		"KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT",
	} {
		t.Setenv(k, "")

		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unset %s: %v", k, err)
		}
	}

	if got, err := os.UserHomeDir(); err != nil || got != home {
		t.Fatalf("home directory not isolated: os.UserHomeDir() = %q, %v; want %q", got, err, home)
	}

	if server := clientcmd.ClusterDefaults.Server; server != "http://localhost:8080" {
		t.Fatalf("KUBERNETES_MASTER is set in this test process (client-go's default server is %q); "+
			"unset it before running the offline acceptance tests", server)
	}

	return home
}

// TestClearKubeEnv_IsolatesHome pins clearKubeEnv on every platform, without
// TF_ACC: whatever home and in-cluster variables the shell exports, both
// os.UserHomeDir (nelm's ~/.kube/config) and client-go's homedir.HomeDir
// (its discovery cache) resolve to the empty directory, and client-go sees
// no in-cluster service.
func TestClearKubeEnv_IsolatesHome(t *testing.T) {
	operator := t.TempDir()

	for k, v := range map[string]string{
		"HOME":                    operator,
		"USERPROFILE":             operator,
		"HOMEDRIVE":               "C:",
		"HOMEPATH":                `\Users\operator`,
		"KUBE_CONFIG_PATHS":       filepath.Join(operator, ".kube", "config"),
		"KUBERNETES_SERVICE_HOST": "10.0.0.1",
		"KUBERNETES_SERVICE_PORT": "443",
	} {
		t.Setenv(k, v)
	}

	home := clearKubeEnv(t)

	if got := homedir.HomeDir(); got != home {
		t.Errorf("homedir.HomeDir() = %q, want the empty home %q", got, home)
	}

	if got := os.Getenv("USERPROFILE"); got != home {
		t.Errorf("USERPROFILE = %q, want the empty home %q", got, home)
	}

	for _, k := range []string{"HOMEDRIVE", "HOMEPATH", "KUBE_CONFIG_PATHS", "KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT"} {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("%s = %q, want it unset", k, v)
		}
	}
}

// templateConfig is a nelm_template config for chart under the provider
// block provider, plus extra HCL inside the data block.
func templateConfig(provider, name, chart, extra string) string {
	return fmt.Sprintf(`%s
data "nelm_template" "test" {
  name  = %q
  chart = %q
%s
}
`, provider, name, chart, extra)
}

// subManifest is the manifests entry of the subchart's ConfigMap for release
// name in namespace: what `nelm chart render` prints for it, plus the release
// namespace nelm strips and the data source restores.
func subManifest(name, namespace string) string {
	return fmt.Sprintf(`---
# Source: template/charts/sub/templates/configmap.yaml
apiVersion: v1
data:
  from: subchart
kind: ConfigMap
metadata:
  annotations: {}
  labels: {}
  name: %s-sub
  namespace: %s
`, name, namespace)
}

// sourceLine matches the "# Source: <path>" line nelm puts before every
// document it prints.
var sourceLine = regexp.MustCompile(`(?m)^# Source: (.+)$`)

// expectSources checks a manifest or manifests entry: its documents come from
// the template files want, one entry per document, in that order.
func expectSources(want ...string) knownvalue.Check {
	return knownvalue.StringFunc(func(manifest string) error {
		var got []string
		for _, m := range sourceLine.FindAllStringSubmatch(manifest, -1) {
			got = append(got, m[1])
		}

		if !slices.Equal(got, want) {
			return fmt.Errorf("documents rendered from %q, want %q", got, want)
		}

		return nil
	})
}

// manifestData decodes the data of the first document of a manifests entry
// ("---\n# Source: <path>\n<yaml>...") and checks that it is a kind.
func manifestData(doc, kind string) (map[string]string, error) {
	var obj struct {
		Kind string            `json:"kind"`
		Data map[string]string `json:"data"`
	}

	if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
		return nil, fmt.Errorf("decode manifest: %w\n%s", err, doc)
	}

	if obj.Kind != kind {
		return nil, fmt.Errorf("manifest holds a %q, want a %s:\n%s", obj.Kind, kind, doc)
	}

	return obj.Data, nil
}

// expectData checks a manifests entry: its first document is a kind whose
// data holds want (other keys are not checked).
func expectData(kind string, want map[string]string) knownvalue.Check {
	return knownvalue.StringFunc(func(doc string) error {
		data, err := manifestData(doc, kind)
		if err != nil {
			return err
		}

		for _, k := range slices.Sorted(maps.Keys(want)) {
			if got, ok := data[k]; !ok || got != want[k] {
				return fmt.Errorf("%s data[%q] = %q, want %q", kind, k, got, want[k])
			}
		}

		return nil
	})
}

// manifestNamespaces decodes every document of a manifest into
// "<Kind>/<name>" -> its metadata.namespace ("" when it has none).
func manifestNamespaces(manifest string) (map[string]string, error) {
	out := map[string]string{}

	for _, doc := range regexp.MustCompile(`(?m)^---$`).Split(manifest, -1) {
		var obj struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}

		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			return nil, fmt.Errorf("decode manifest: %w\n%s", err, doc)
		}

		if obj.Kind != "" {
			out[obj.Kind+"/"+obj.Metadata.Name] = obj.Metadata.Namespace
		}
	}

	return out, nil
}

// checkNamespaces checks that a manifest holds exactly the objects of want,
// "<Kind>/<name>" -> the namespace it must carry ("" for none).
func checkNamespaces(manifest string, want map[string]string) error {
	got, err := manifestNamespaces(manifest)
	if err != nil {
		return err
	}

	if !maps.Equal(got, want) {
		return fmt.Errorf("namespaces in manifest = %v, want %v", got, want)
	}

	return nil
}

// namespacesChartObjects is "<Kind>/<name>" -> namespace of every object
// testdata/charts/namespaces renders for release name in namespace, its CRD
// included, as nelm_template returns them: the release namespace on every
// namespace-scoped object without one (nelm strips it from those that set
// it), none on cluster-scoped kinds, another namespace kept. monitorNS is
// the ServiceMonitor's, a kind only a cluster that serves it can scope.
func namespacesChartObjects(name, namespace, monitorNS string) map[string]string {
	return map[string]string{
		"CustomResourceDefinition/gadgets.example.com": "",
		"Job/" + name + "-hook":                        namespace,
		"ServiceAccount/" + name + "-release-ns":       namespace,
		"ConfigMap/" + name + "-no-ns":                 namespace,
		"ConfigMap/" + name + "-other-ns":              "other",
		"ClusterRole/" + name + "-cluster":             "",
		"Gadget/" + name + "-gadget":                   namespace,
		"ServiceMonitor/" + name + "-monitor":          monitorNS,
	}
}

// clusterGitVersion returns the pinned test cluster's version as its API
// server reports it (/version gitVersion): what nelm renders into
// .Capabilities.KubeVersion.Version for a cluster-backed render.
func clusterGitVersion() (string, error) {
	out, err := kubectl("get", "--raw", "/version")
	if err != nil {
		return "", fmt.Errorf("kubectl get --raw /version: %w\n%s", err, out)
	}

	var v struct {
		GitVersion string `json:"gitVersion"`
	}

	if err := json.Unmarshal([]byte(out), &v); err != nil || v.GitVersion == "" {
		return "", fmt.Errorf("no gitVersion in the cluster's /version (%v):\n%s", err, out)
	}

	return v.GitVersion, nil
}

// clusterServesServiceMonitor reports whether the pinned test cluster serves
// monitoring.coreos.com ServiceMonitors (prometheus-operator's CRD installed).
func clusterServesServiceMonitor() (bool, error) {
	out, err := kubectl("api-resources", "--api-group=monitoring.coreos.com", "-o", "name")
	if err != nil {
		return false, fmt.Errorf("kubectl api-resources: %w\n%s", err, out)
	}

	return slices.Contains(strings.Fields(out), "servicemonitors.monitoring.coreos.com"), nil
}

// TestAccTemplateDataSource_Offline renders the template chart under a
// provider block that names no cluster, which used to fail the provider's
// Configure for every config: set/values, kube_version, api_versions,
// include_crds, skip_tests and show_only across steps, plus the two
// failures — a show_only pattern matching no template, and validate = true,
// which needs the cluster this block does not name.
func TestAccTemplateDataSource_Offline(t *testing.T) {
	clearKubeEnv(t)

	const name = "tmpl"

	chart := templateChartPath(t)
	cfg := func(extra string) string { return templateConfig(emptyProviderBlock, name, chart, extra) }

	const sensitiveValue = "s3cr3t-template-value"

	values := `  values = [<<-YAML
    message: from values
    secret:
      password: from-values
    YAML
  ]
`

	// The template files of the chart's documents, as "# Source:" names
	// them: a crds/ path carries the OS separator (Helm's CRDObjects uses
	// filepath.Join), templates/ paths are slash-separated on every OS (the
	// engine uses path.Join). The manifests keys are slash-separated
	// everywhere (chartRelativePath).
	crdsDoc := filepath.FromSlash("template/crds/widget.yaml")

	const (
		hooksDoc  = "template/templates/hooks.yaml"
		subDoc    = "template/charts/sub/templates/configmap.yaml"
		configDoc = "template/templates/configmap.yaml"
		secretDoc = "template/templates/secret.yaml"
	)

	// crds lists the chart's one standalone CRD (YAML only, no "---" or
	// "# Source:" line) whatever include_crds and show_only select.
	crds := statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("crds"), knownvalue.ListExact([]knownvalue.Check{
		knownvalue.StringRegexp(regexp.MustCompile(`^apiVersion: apiextensions\.k8s\.io/v1\nkind: CustomResourceDefinition\n(?s:.*)\n  name: widgets\.example\.com\n`)),
	}))

	manifests := func(key string, check knownvalue.Check) statecheck.StateCheck {
		return statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("manifests").AtMapKey(key), check)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Defaults: hooks (the test one too) and the regular
				// objects in nelm's order, the CRD only in crds, the
				// chart's own values, a first install.
				Config: cfg(""),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("id"), knownvalue.StringExact("default/"+name)),
					statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("namespace"), knownvalue.StringExact("default")),
					statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("manifest"), expectSources(hooksDoc, hooksDoc, subDoc, configDoc, secretDoc)),
					statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("manifests"), knownvalue.MapExact(map[string]knownvalue.Check{
						"templates/hooks.yaml":                expectSources(hooksDoc, hooksDoc),
						"charts/sub/templates/configmap.yaml": knownvalue.StringExact(subManifest(name, "default")),
						"templates/configmap.yaml":            expectSources(configDoc),
						"templates/secret.yaml":               expectSources(secretDoc),
					})),
					manifests("templates/configmap.yaml", expectData("ConfigMap", map[string]string{
						"message":      "hello from the template test chart",
						"hasWidgetAPI": "false",
						"isUpgrade":    "false",
						"revision":     "1",
					})),
					manifests("templates/secret.yaml", expectData("Secret", map[string]string{
						"password": base64.StdEncoding.EncodeToString([]byte("changeme")),
					})),
					crds,
				},
			},
			{
				// values and set_sensitive (the outputs are cleartext),
				// offline capabilities, another namespace.
				Config: cfg(values + fmt.Sprintf(`  namespace    = "tmpl-ns"
  kube_version = "v1.30"
  api_versions = ["example.com/v1"]

  set_sensitive = [{ name = "secret.password", value = %q }]
`, sensitiveValue)),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("id"), knownvalue.StringExact("tmpl-ns/"+name)),
					statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("manifest"), expectSources(hooksDoc, hooksDoc, subDoc, configDoc, secretDoc)),
					manifests("templates/configmap.yaml", expectData("ConfigMap", map[string]string{
						"message":      "from values",
						"kubeVersion":  "v1.30.0",
						"hasWidgetAPI": "true",
						"isUpgrade":    "false",
						"revision":     "1",
					})),
					manifests("templates/secret.yaml", expectData("Secret", map[string]string{
						"password": base64.StdEncoding.EncodeToString([]byte(sensitiveValue)),
					})),
					crds,
				},
			},
			{
				// set wins over values; include_crds puts the CRD first,
				// skip_tests drops the helm test Pod but keeps the other
				// hook.
				Config: cfg(values + `  set = [{ name = "message", value = "from set" }]

  include_crds = true
  skip_tests   = true
`),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("manifest"), expectSources(crdsDoc, hooksDoc, subDoc, configDoc, secretDoc)),
					statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("manifests"), knownvalue.MapSizeExact(5)),
					manifests("crds/widget.yaml", expectSources(crdsDoc)),
					manifests("templates/hooks.yaml", expectSources(hooksDoc)),
					manifests("templates/hooks.yaml", expectData("ConfigMap", map[string]string{"phase": "pre-install"})),
					manifests("templates/configmap.yaml", expectData("ConfigMap", map[string]string{"message": "from set"})),
					crds,
				},
			},
			{
				Config:      cfg(`  show_only = ["templates/configmap.yaml", "templates/missing.yaml"]`),
				ExpectError: regexp.MustCompile(`Template\s+Not\s+Found`),
			},
			{
				// The data source reports the provider's own no-connection
				// detail instead of falling back to any kubeconfig.
				Config:      cfg(`  validate = true`),
				ExpectError: regexp.MustCompile(`No\s+Kubernetes\s+connection\s+configured|names\s+no\s+cluster`),
			},
			{
				// show_only selects by chart-relative path or glob; the
				// documents keep nelm's order, not the patterns'. Last, so
				// the post-test destroy runs against a config that works.
				Config: cfg(`  show_only = ["templates/configmap.yaml", "charts/*/templates/*.yaml"]`),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("manifest"), expectSources(subDoc, configDoc)),
					statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("manifests"), knownvalue.MapExact(map[string]knownvalue.Check{
						"charts/sub/templates/configmap.yaml": knownvalue.StringExact(subManifest(name, "default")),
						"templates/configmap.yaml":            expectSources(configDoc),
					})),
					crds,
				},
			},
		},
	})
}

// TestAccTemplateDataSource_OfflineNamespaces renders testdata/charts/namespaces
// under a provider block that names no cluster: nelm strips the release
// namespace, and the data source puts it back on every namespace-scoped
// object, from the built-in kinds and the Gadget CRD in the chart's crds/.
// The ServiceMonitor's kind is neither, so it keeps no namespace (with a
// warning, which terraform-plugin-testing cannot assert;
// TestTemplateRead_RealClientOfflineNamespaces does).
func TestAccTemplateDataSource_OfflineNamespaces(t *testing.T) {
	clearKubeEnv(t)

	const name = "ns"

	chart := namespacesChartPath(t)
	cfg := func(extra string) string { return templateConfig(emptyProviderBlock, name, chart, extra) }

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: cfg(`  namespace    = "apps"
  include_crds = true
`),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("manifest"), knownvalue.StringFunc(func(manifest string) error {
						return checkNamespaces(manifest, namespacesChartObjects(name, "apps", ""))
					})),
					statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("crds"), knownvalue.ListExact([]knownvalue.Check{
						knownvalue.StringFunc(func(crd string) error {
							return checkNamespaces(crd, map[string]string{"CustomResourceDefinition/gadgets.example.com": ""})
						}),
					})),
				},
			},
			{
				// The default namespace is restored like any other.
				Config: cfg(`  show_only = ["templates/objects.yaml"]`),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("manifests").AtMapKey("templates/objects.yaml"),
						knownvalue.StringFunc(func(manifest string) error {
							want := namespacesChartObjects(name, "default", "")
							delete(want, "CustomResourceDefinition/gadgets.example.com")
							delete(want, "Job/"+name+"-hook")

							return checkNamespaces(manifest, want)
						})),
				},
			},
		},
	})
}

// TestAccTemplateDataSource_OfflineReleaseRefused: the provider block that is
// enough for an offline nelm_template still names no cluster for
// nelm_release, whose plan must fail with the error the provider's Configure
// used to raise for every config, never fall back to ~/.kube/config's
// current-context.
func TestAccTemplateDataSource_OfflineReleaseRefused(t *testing.T) {
	clearKubeEnv(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`%s
resource "nelm_release" "test" {
  name  = "offline"
  chart = %q
}
`, emptyProviderBlock, templateChartPath(t)),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`No\s+Kubernetes\s+connection\s+configured`),
			},
		},
	})
}

// TestAccTemplateDataSource_Validate: validate = true renders against the
// cluster — its real version instead of nelm's offline default — but never
// reads the release history (nelm's in-memory storage, helm_template's
// semantics): even with a nelm_release of the same chart and name installed
// (revision 1), the render is a first install at revision 1. A second data
// source renders testdata/charts/namespaces against the cluster's
// RESTMapper: the release namespace on every namespace-scoped object, the
// Gadget (not served) scoped from the chart's own CRD, and the ServiceMonitor
// namespaced only if the cluster serves its kind (the pinned kind cluster
// does not, so it keeps none).
func TestAccTemplateDataSource_Validate(t *testing.T) {
	namespace := uniqueNamespace("tmpl")
	const name = "tmpl"

	chart := templateChartPath(t)
	nsChart := namespacesChartPath(t)

	// The chart renders .Release.Revision and .Release.IsUpgrade, which a
	// diff_mode = "full" plan would show changing on every run.
	release := releaseConfig(name, namespace, chart, `  diff_mode = "none"`)

	validated := release + fmt.Sprintf(`
data "nelm_template" "test" {
  name      = %q
  namespace = %q
  chart     = %q
  validate  = true

  depends_on = [nelm_release.test]
}

data "nelm_template" "namespaces" {
  name      = "ns"
  namespace = %q
  chart     = %q
  validate  = true
}
`, name, namespace, chart, namespace, nsChart)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(s *terraform.State) error {
			err := testAccCheckReleaseDestroyed(namespace, name)(s)

			if out, delErr := kubectl("delete", "crd", templateCRD, "--ignore-not-found", "--wait=true", "--timeout=60s"); delErr != nil {
				err = errors.Join(err, fmt.Errorf("delete CRD %s for cleanup: %w\n%s", templateCRD, delErr, out))
			}

			return err
		},
		Steps: []resource.TestStep{
			{
				Config: release,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("status"), knownvalue.StringExact("deployed")),
					statecheck.ExpectKnownValue(resourceAddr, tfjsonpath.New("revision"), knownvalue.Int64Exact(1)),
				},
			},
			{
				// The release has nothing to change, so the data source is
				// read at plan time, with revision 1 stored.
				Config: validated,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("id"), knownvalue.StringExact(namespace+"/"+name)),
					statecheck.ExpectKnownValue(templateAddr, tfjsonpath.New("manifests").AtMapKey("templates/configmap.yaml"),
						knownvalue.StringFunc(func(doc string) error {
							data, err := manifestData(doc, "ConfigMap")
							if err != nil {
								return err
							}

							version, err := clusterGitVersion()
							if err != nil {
								return err
							}

							if got := data["kubeVersion"]; got != version {
								return fmt.Errorf("ConfigMap data.kubeVersion = %q, want the cluster's %q", got, version)
							}

							if data["isUpgrade"] != "false" || data["revision"] != "1" {
								return fmt.Errorf("ConfigMap data.isUpgrade = %q, data.revision = %q, want a first install at revision 1 "+
									"whatever the cluster stores for the release", data["isUpgrade"], data["revision"])
							}

							return nil
						})),
					statecheck.ExpectKnownValue("data.nelm_template.namespaces", tfjsonpath.New("manifest"),
						knownvalue.StringFunc(func(manifest string) error {
							served, err := clusterServesServiceMonitor()
							if err != nil {
								return err
							}

							monitorNS := ""
							if served {
								monitorNS = namespace
							}

							want := namespacesChartObjects("ns", namespace, monitorNS)
							// include_crds is unset.
							delete(want, "CustomResourceDefinition/gadgets.example.com")

							return checkNamespaces(manifest, want)
						})),
				},
			},
		},
	})
}
