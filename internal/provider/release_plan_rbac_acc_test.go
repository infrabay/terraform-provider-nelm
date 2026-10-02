package provider_test

// release_plan_rbac_acc_test.go covers planning an existing release with an
// identity that may read, but not patch, some of the kinds the chart renders
// (issue #8). Same harness and triple safety guard as
// release_resource_test.go (provider_test.go). The planner step connects
// with the inline host/token/cluster_ca_certificate of a ServiceAccount
// instead of the pinned kubeconfig context, but host is that context's server
// URL, which testAccPreCheck has already vetted to be a loopback endpoint.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"k8s.io/client-go/tools/clientcmd"
)

// plannerSA is the ServiceAccount, in the release namespace, the planner
// step connects as.
const plannerSA = "tf-planner"

// lazyString is a config.Variable whose value is read when the harness
// writes the step's variables, which happens after the step's PreConfig. A
// ServiceAccount token can only be minted once the ServiceAccount exists,
// that is after the TestCase (and every step's Config) has been built.
type lazyString func() string

func (f lazyString) MarshalJSON() ([]byte, error) {
	v := f()
	if v == "" {
		return nil, errors.New("test variable read before the step's PreConfig set it")
	}

	return json.Marshal(v)
}

// inlineConnection is a provider connection given by host, token and
// cluster_ca_certificate instead of a kubeconfig context.
type inlineConnection struct {
	host, token, caCert string
}

// variables returns the step ConfigVariables inlineReleaseConfig reads,
// resolved only when the harness writes them (after PreConfig).
func (c *inlineConnection) variables() config.Variables {
	return config.Variables{
		"host":                   lazyString(func() string { return c.host }),
		"token":                  lazyString(func() string { return c.token }),
		"cluster_ca_certificate": lazyString(func() string { return c.caCert }),
	}
}

// inlineReleaseConfig is releaseConfig with the provider connected through
// the inlineConnection variables, never a kubeconfig file or context.
func inlineReleaseConfig(name, namespace, chart string) string {
	return fmt.Sprintf(`variable "host" {
  type = string
}

variable "token" {
  type      = string
  sensitive = true
}

variable "cluster_ca_certificate" {
  type = string
}

provider "nelm" {
  host                   = var.host
  token                  = var.token
  cluster_ca_certificate = var.cluster_ca_certificate
}

resource "nelm_release" "test" {
  name      = %q
  namespace = %q
  chart     = %q
}
`, name, namespace, chart)
}

// localContextCA returns the PEM CA bundle of kubeCtx's cluster in the
// kubeconfig file at kubeconfigPath (that file only, like
// localContextServer).
func localContextCA(kubeconfigPath, kubeCtx string) (string, error) {
	cfg, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return "", fmt.Errorf("could not load kubeconfig %s: %w", kubeconfigPath, err)
	}

	kctx, ok := cfg.Contexts[kubeCtx]
	if !ok {
		return "", fmt.Errorf("kubeconfig context %q does not exist in %s", kubeCtx, kubeconfigPath)
	}

	cluster, ok := cfg.Clusters[kctx.Cluster]
	if !ok {
		return "", fmt.Errorf("kubeconfig context %q references cluster %q, which is missing from %s", kubeCtx, kctx.Cluster, kubeconfigPath)
	}

	if len(cluster.CertificateAuthorityData) > 0 {
		return string(cluster.CertificateAuthorityData), nil
	}

	if cluster.CertificateAuthority != "" {
		pem, err := os.ReadFile(cluster.CertificateAuthority)
		if err != nil {
			return "", fmt.Errorf("read CA file of kubeconfig context %q: %w", kubeCtx, err)
		}

		return string(pem), nil
	}

	return "", fmt.Errorf("kubeconfig context %q has no cluster CA certificate in %s", kubeCtx, kubeconfigPath)
}

// createToken mints a ServiceAccount token with `kubectl create token`,
// pinned to the test context and kubeconfig file exactly like kubectl(). It
// reads stdout only: kubectl's warnings (e.g. a client/server version skew)
// go to stderr and must not end up in the token.
func createToken(t *testing.T, namespace, serviceAccount string) string {
	t.Helper()

	kubeconfig := testKubeconfigPath()
	if kubeconfig == "" {
		t.Fatal("kubectl create token: no acceptance-test kubeconfig path (home directory unknown)")
	}

	var stderr strings.Builder

	cmd := exec.Command("kubectl", "--kubeconfig="+kubeconfig, "--context="+testKubeContext(),
		"create", "token", serviceAccount, "-n", namespace, "--duration=30m")
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kubectl create token %s/%s: %v\n%s", namespace, serviceAccount, err, stderr.String())
	}

	token := strings.TrimSpace(string(out))
	if token == "" || strings.ContainsAny(token, " \n") {
		t.Fatalf("kubectl create token %s/%s printed no single token: %q", namespace, serviceAccount, out)
	}

	return token
}

// setupPlannerIdentity creates the ServiceAccount plannerSA in namespace
// with the access a GKE identity holding IAM roles/editor and no in-cluster
// RBAC binding has to the basic chart's kinds: full access to the namespaced
// kinds, read access to Namespaces, and only get/list on ClusterRoles and
// ClusterRoleBindings (container.clusterRoles.get/list,
// container.clusterRoleBindings.get/list), so nelm's dry-run apply of the
// chart's ClusterRole and ClusterRoleBinding is forbidden. API discovery
// comes from the default system:discovery binding. It returns the inline
// connection of that ServiceAccount to the pinned (loopback) test cluster.
// The cluster-scoped objects are named clusterName; they and the namespace
// are removed by cleanupPlannerFixtures.
func setupPlannerIdentity(t *testing.T, namespace, clusterName string) inlineConnection {
	t.Helper()

	subject := namespace + ":" + plannerSA
	user := "system:serviceaccount:" + subject

	mustKubectl(t, "create", "serviceaccount", plannerSA, "-n", namespace)
	mustKubectl(t, "create", "role", plannerSA, "-n", namespace,
		"--verb=get,list,watch,create,update,patch",
		"--resource=configmaps,secrets,services,deployments.apps")
	mustKubectl(t, "create", "rolebinding", plannerSA, "-n", namespace, "--role="+plannerSA, "--serviceaccount="+subject)
	mustKubectl(t, "create", "clusterrole", clusterName,
		"--verb=get,list",
		"--resource=clusterroles.rbac.authorization.k8s.io,clusterrolebindings.rbac.authorization.k8s.io,namespaces")
	mustKubectl(t, "create", "clusterrolebinding", clusterName, "--clusterrole="+clusterName, "--serviceaccount="+subject)

	// The fixture must model the issue: it can read everything the plan
	// reads and dry-run the namespaced objects, but not patch the
	// cluster-scoped RBAC objects.
	for _, check := range []struct {
		args []string
		want bool
	}{
		{[]string{"get", "secrets", "-n", namespace}, true},
		{[]string{"patch", "deployments.apps", "-n", namespace}, true},
		{[]string{"get", "clusterroles.rbac.authorization.k8s.io"}, true},
		{[]string{"list", "clusterrolebindings.rbac.authorization.k8s.io"}, true},
		{[]string{"patch", "clusterroles.rbac.authorization.k8s.io"}, false},
		{[]string{"patch", "clusterrolebindings.rbac.authorization.k8s.io"}, false},
	} {
		args := append([]string{"auth", "can-i"}, check.args...)
		args = append(args, "--as="+user)

		// can-i exits 0 for "yes" and 1 for "no".
		out, err := kubectl(args...)
		if got := err == nil; got != check.want {
			t.Fatalf("planner fixture: kubectl %s = %v, want %v\n%s", strings.Join(args, " "), got, check.want, out)
		}
	}

	host, err := localContextServer(testKubeconfigPath(), testKubeContext())
	if err != nil {
		t.Fatalf("%v; refusing to connect the planner", err)
	}

	caCert, err := localContextCA(testKubeconfigPath(), testKubeContext())
	if err != nil {
		t.Fatal(err)
	}

	return inlineConnection{
		host:   host,
		token:  createToken(t, namespace, plannerSA),
		caCert: caCert,
	}
}

// cleanupPlannerFixtures removes everything the planner test can leave
// behind, whether or not the test, its post-test destroy or CheckDestroy
// succeeded: the given cluster-scoped objects (the planner's ClusterRole and
// ClusterRoleBinding, the chart's ClusterRole and ClusterRoleBinding) and the
// namespace (the release storage Secrets, the release's namespaced objects
// and the planner's ServiceAccount, Role and RoleBinding). A failed planner
// step leaves the planner configuration in the working directory, so the
// harness's own destroy then runs as the planner and cannot uninstall the
// cluster-scoped objects.
func cleanupPlannerFixtures(t *testing.T, namespace string, clusterScoped ...string) {
	t.Helper()

	if testKubeContext() == "" {
		// Never reached through testAccPreCheck; an empty --context would
		// make kubectl fall back to the current-context.
		t.Error("cleanup: NELM_TEST_KUBE_CONTEXT is unset, not cleaning up")
		return
	}

	for _, obj := range clusterScoped {
		if out, err := kubectl("delete", obj, "--ignore-not-found", "--wait=true", "--timeout=60s"); err != nil {
			t.Errorf("cleanup: delete %s: %v\n%s", obj, err, out)
		}
	}

	if out, err := kubectl("delete", "namespace", namespace, "--ignore-not-found", "--wait=true", "--timeout=180s"); err != nil {
		t.Errorf("cleanup: delete namespace %s: %v\n%s", namespace, err, out)
	}
}

// TestAccReleaseResource_planWithoutClusterRBACPatch reproduces issue #8: a
// plan of an existing release by an identity that may read but not patch the
// chart's ClusterRole and ClusterRoleBinding (GKE IAM roles/editor) failed
// with "read plan artifact: decode artifact data json: ... dryApplyErr of
// type error", because nelm's plan artifact cannot carry a dry-run apply
// error back. The plan must instead succeed: nelm plans those objects as a
// blind apply (a warning carrying the forbidden error), and since the
// rendered objects equal the live ones the plan has no changes.
func TestAccReleaseResource_planWithoutClusterRBACPatch(t *testing.T) {
	namespace := uniqueNamespace("rbac")
	const name = "rbac"

	chart := chartPath(t)
	// The basic chart's ClusterRole and ClusterRoleBinding (basic.clusterName).
	chartClusterName := fmt.Sprintf("%s-%s-basic", name, namespace)
	plannerClusterName := namespace + "-" + plannerSA

	// Filled by step 2's PreConfig, read when that step writes its variables.
	planner := &inlineConnection{}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)

			// Registered only once the guard passed: it then runs after the
			// harness's post-test destroy and CheckDestroy, also on failure.
			t.Cleanup(func() {
				cleanupPlannerFixtures(t, namespace,
					"clusterrolebinding/"+plannerClusterName,
					"clusterrole/"+plannerClusterName,
					"clusterrolebinding/"+chartClusterName,
					"clusterrole/"+chartClusterName,
				)
			})
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckReleaseDestroyed(namespace, name),
		Steps: []resource.TestStep{
			{
				// Installed by the admin context; the harness's post-apply
				// plan must be empty.
				Config: releaseConfig(name, namespace, chart, ""),
			},
			{
				// The same release, planned as the planner ServiceAccount.
				PreConfig: func() {
					*planner = setupPlannerIdentity(t, namespace, plannerClusterName)
				},
				Config:          inlineReleaseConfig(name, namespace, chart),
				ConfigVariables: planner.variables(),
				PlanOnly:        true,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PostApplyPostRefresh: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Back to the admin context, so that the post-test destroy
				// and CheckDestroy run with it.
				Config:   releaseConfig(name, namespace, chart, ""),
				PlanOnly: true,
			},
		},
	})
}
