package nelmclient

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/werf/nelm/pkg/common"
	"github.com/werf/nelm/pkg/kube/fake"
	"github.com/werf/nelm/pkg/plan"
	"github.com/werf/nelm/pkg/resource"
	"github.com/werf/nelm/pkg/resource/spec"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

const artifactReleaseName, artifactReleaseNamespace = "vpa", "kube-system"

// forbiddenPatch is the API server's answer to a (dry-run) server-side apply
// by an identity that may read, but not patch, the resource: issue #8's GKE
// roles/editor without any in-cluster RBAC binding, which carries only
// container.clusterRoles.get/list and container.clusterRoleBindings.get/list.
func forbiddenPatch(action clienttesting.PatchAction) error {
	gr := action.GetResource().GroupResource()

	return apierrors.NewForbidden(gr, action.GetName(), errors.New(
		`User "ci@example.iam.gserviceaccount.com" cannot patch resource "`+gr.Resource+
			`" in API group "`+gr.Group+`" at the cluster scope`))
}

// writeNelmPlanArtifact plans desired over live against nelm's fake
// (offline) cluster exactly the way action.ReleasePlanInstall does
// (plan.BuildResourceInfos, plan.BuildPlan, plan.CalculatePlannedChanges)
// and writes the plan artifact with nelm's own plan.WritePlanArtifact,
// unencrypted (the provider never sets a secret key). Every dry-run
// server-side apply of a resource named in forbidden (plural resource
// names, e.g. "clusterroles") fails with forbiddenPatch, as it does on the
// real API server. It returns the artifact path and the installable
// resource infos the artifact carries.
func writeNelmPlanArtifact(t *testing.T, live, desired []*unstructured.Unstructured, forbidden ...string) (string, []*plan.InstallableResourceInfo) {
	t.Helper()

	ctx := context.Background()

	cf, err := fake.NewClientFactory(ctx)
	if err != nil {
		t.Fatalf("fake client factory: %v", err)
	}

	installable := func(src *unstructured.Unstructured) *resource.InstallableResource {
		obj := src.DeepCopy()

		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["meta.helm.sh/release-name"] = artifactReleaseName
		annotations["meta.helm.sh/release-namespace"] = artifactReleaseNamespace
		obj.SetAnnotations(annotations)
		obj.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm"})

		res, err := resource.NewInstallableResource(
			spec.NewResourceSpec(obj, artifactReleaseNamespace, spec.ResourceSpecOptions{StoreAs: common.StoreAsRegular}),
			nil, artifactReleaseNamespace, cf, resource.InstallableResourceOptions{},
		)
		if err != nil {
			t.Fatalf("installable resource %s: %v", obj.GetName(), err)
		}

		return res
	}

	dyn, ok := cf.Dynamic().(*dynamicfake.FakeDynamicClient)
	if !ok {
		t.Fatalf("nelm's fake dynamic client is a %T, want *dynamicfake.FakeDynamicClient", cf.Dynamic())
	}

	// The release's objects as a previous install left them. The fake serves
	// every kind as namespaced, so they live in the release namespace.
	for _, src := range live {
		obj := installable(src).Unstruct.DeepCopy()
		obj.SetNamespace(artifactReleaseNamespace)

		gvk := obj.GroupVersionKind()

		mapping, err := cf.Mapper().RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			t.Fatalf("rest mapping for %s: %v", gvk, err)
		}

		if err := dyn.Tracker().Create(mapping.Resource, obj, artifactReleaseNamespace); err != nil {
			t.Fatalf("seed live %s: %v", obj.GetName(), err)
		}
	}

	for _, res := range forbidden {
		dyn.PrependReactor("patch", res, func(action clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, forbiddenPatch(action.(clienttesting.PatchAction))
		})
	}

	var resources []*resource.InstallableResource
	for _, obj := range desired {
		resources = append(resources, installable(obj))
	}

	deployType := common.DeployTypeUpgrade
	if len(live) == 0 {
		deployType = common.DeployTypeInitial
	}

	infos, delInfos, err := plan.BuildResourceInfos(ctx, deployType, artifactReleaseName, artifactReleaseNamespace,
		resources, nil, false, cf, plan.BuildResourceInfosOptions{NetworkParallelism: 1})
	if err != nil {
		t.Fatalf("build resource infos: %v", err)
	}

	installPlan, err := plan.BuildPlan(infos, delInfos, nil, plan.BuildPlanOptions{NoFinalTracking: true})
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	changes, err := plan.CalculatePlannedChanges(infos, delInfos)
	if err != nil {
		t.Fatalf("calculate planned changes: %v", err)
	}

	path := filepath.Join(t.TempDir(), "plan.artifact")

	artifact := &plan.PlanArtifact{
		APIVersion: plan.PlanArtifactSchemeVersion,
		Data: &plan.PlanArtifactData{
			Options:                  runtimeOptions(ReleaseSpec{StorageDriver: "secret"}),
			Plan:                     installPlan,
			Changes:                  changes,
			InstallableResourceInfos: infos,
		},
		DeployType: deployType,
		Release: plan.PlanArtifactRelease{
			Name:      artifactReleaseName,
			Namespace: artifactReleaseNamespace,
			Revision:  2,
		},
		Timestamp: time.Now().UTC(),
	}

	if err := plan.WritePlanArtifact(ctx, artifact, path, "", ""); err != nil {
		t.Fatalf("write plan artifact: %v", err)
	}

	return path, infos
}

// vpaObjects is the RBAC part of issue #8's vertical-pod-autoscaler chart: a
// ServiceAccount and the ClusterRole/ClusterRoleBinding that grant it.
func vpaObjects() []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ServiceAccount",
			"metadata":   map[string]any{"name": "vpa-admission-controller", "namespace": artifactReleaseNamespace},
		}},
		{Object: map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "ClusterRole",
			"metadata":   map[string]any{"name": "vpa-admission-controller"},
			"rules": []any{map[string]any{
				"apiGroups": []any{"autoscaling.k8s.io"},
				"resources": []any{"verticalpodautoscalers"},
				"verbs":     []any{"get", "list", "watch"},
			}},
		}},
		{Object: map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "ClusterRoleBinding",
			"metadata":   map[string]any{"name": "vpa-admission-controller"},
			"roleRef": map[string]any{
				"apiGroup": "rbac.authorization.k8s.io",
				"kind":     "ClusterRole",
				"name":     "vpa-admission-controller",
			},
			"subjects": []any{map[string]any{
				"kind":      "ServiceAccount",
				"name":      "vpa-admission-controller",
				"namespace": artifactReleaseNamespace,
			}},
		}},
	}
}

// issue8Err is the error nelm's own plan.ReadPlanArtifact fails with on an
// artifact in which any object's dry-run apply failed.
var issue8Err = regexp.MustCompile(`^decode artifact data json: json: cannot unmarshal object into Go struct field PlanArtifactData\.installableResourceInfos\.\d+\.dryApplyErr of type error$`)

// TestPlanArtifact_DryApplyErrorIssue8 is the regression test for issue #8:
// when nelm's dry-run server-side apply of an existing object fails (here an
// RBAC forbidden on clusterroles/clusterrolebindings), nelm plans the object
// as a "blind apply" carrying the error in ResourceChange.Reason, and writes
// the error value itself into the artifact's
// installableResourceInfos[].dryApplyErr — as a JSON object nelm's own
// plan.ReadPlanArtifact cannot decode back into an error interface. Plan
// failed with "read plan artifact: decode artifact data json: ...".
func TestPlanArtifact_DryApplyErrorIssue8(t *testing.T) {
	path, infos := writeNelmPlanArtifact(t, vpaObjects(), vpaObjects(), "clusterroles", "clusterrolebindings")

	var failed int

	for _, info := range infos {
		if info.DryApplyErr == nil {
			continue
		}

		failed++

		if !apierrors.IsForbidden(info.DryApplyErr) {
			t.Errorf("precondition: %s dry-apply error is not a Kubernetes forbidden StatusError: %v", info.IDHuman(), info.DryApplyErr)
		}
	}

	if failed != 2 {
		t.Fatalf("precondition: %d objects' dry-run apply failed, want 2 (the ClusterRole and the ClusterRoleBinding)", failed)
	}

	_, err := plan.ReadPlanArtifact(context.Background(), path, "", "")
	if err == nil || !issue8Err.MatchString(err.Error()) {
		t.Fatalf("nelm's plan.ReadPlanArtifact error = %v, want one matching %s (issue #8)", err, issue8Err)
	}
}
