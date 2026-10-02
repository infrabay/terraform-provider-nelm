package provider

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/werf/nelm/pkg/plan"
	"github.com/werf/nelm/pkg/resource/spec"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
)

// TestModifyPlan_DryApplyForbiddenWarnsBlindApply is the provider side of
// issue #8 (nelmclient's TestPlanArtifact_DryApplyErrorIssue8 covers reading
// such a plan back from nelm's artifact): a plan identity that may read but
// not patch clusterroles gets the dry-run apply of the release's ClusterRole
// refused, and nelm plans it as a "blind apply" carrying that error. The
// plan succeeds, the unchanged object plans no change, and the error
// reaches the plan output as a blind-apply warning.
func TestModifyPlan_DryApplyForbiddenWarnsBlindApply(t *testing.T) {
	const roleKey = "rbac.authorization.k8s.io/v1/ClusterRole//my-release-reader"

	installed := map[string]attr.Value{}
	for key, value := range renderedMap(t, renderedObjects()) {
		installed[key] = types.StringValue(value)
	}

	prior := appliedModel(1, "deployed")
	prior.Resources = types.MapValueMust(types.StringType, installed)

	if _, ok := installed[roleKey]; !ok {
		t.Fatalf("precondition: the chart renders no %s", roleKey)
	}

	// The ClusterRole as nelm plans it (its After carries the release
	// ownership metadata), with the error exactly as nelm's kube client
	// wraps the API server's answer.
	role := renderedObjects()[2]
	role.SetAnnotations(map[string]string{
		"meta.helm.sh/release-name":      "my-release",
		"meta.helm.sh/release-namespace": "default",
	})
	role.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm"})

	forbidden := apierrors.NewForbidden(
		k8sschema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "clusterroles"}, "my-release-reader",
		errors.New(`User "ci@example.iam.gserviceaccount.com" cannot patch resource "clusterroles" in API group "rbac.authorization.k8s.io" at the cluster scope`))
	dryApplyErr := fmt.Errorf("server-side dry-run apply resource %q: %w", "ClusterRole/my-release-reader", fmt.Errorf("server-side apply: %w", forbidden))

	client := &fakeReleaseClient{
		renderObjs: renderedObjects(),
		planResult: &nelmclient.PlanResult{
			DeployType: nelmclient.DeployTypeUpgrade,
			Changes: []*plan.ResourceChange{{
				Type:         "blind apply",
				Reason:       fmt.Sprintf("error: %s", dryApplyErr),
				ResourceMeta: spec.NewResourceMetaFromUnstructured(role, "default", "templates/clusterrole.yaml"),
				After:        role,
			}},
		},
	}

	// Nothing changed in the configuration: Terraform proposes the prior state.
	resp := runModifyPlan(t, client, prior, &prior)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	assertOneDiag(t, resp.Diagnostics, diag.SeverityWarning, "nelm_release: blind apply for "+roleKey)

	for _, d := range resp.Diagnostics {
		if d.Severity() == diag.SeverityWarning && !strings.Contains(d.Detail(), `"my-release-reader" is forbidden: User "ci@example.iam.gserviceaccount.com" cannot patch resource "clusterroles"`) {
			t.Errorf("warning detail %q does not carry the dry-run apply error", d.Detail())
		}
	}

	if got := plannedResourcesValue(t, resp.Plan); !got.Equal(prior.Resources) {
		t.Errorf("resources = %v, want the prior value (no change)", got)
	}

	assertComputedUnknown(t, resp.Plan, false)
}
