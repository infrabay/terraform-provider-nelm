package nelmclient

import (
	"context"
	"slices"
	"testing"

	"github.com/werf/nelm/pkg/common"
	"github.com/werf/nelm/pkg/kube/fake"
	"github.com/werf/nelm/pkg/plan"
	"github.com/werf/nelm/pkg/resource"
	"github.com/werf/nelm/pkg/resource/spec"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestInstallTrackingOptions is the F09 regression test for `wait`: Install
// used to hard-code TrackingOptions{NoProgressTablePrint: true}, so nelm's
// final readiness tracking always ran and helm_release's wait = false had no
// equivalent. The zero-value spec must keep waiting (fail-safe default).
func TestInstallTrackingOptions(t *testing.T) {
	if got := installTrackingOptions(ReleaseSpec{}); got.NoFinalTracking || !got.NoProgressTablePrint {
		t.Errorf("zero-value spec (wait = true): got %+v, want NoFinalTracking=false NoProgressTablePrint=true", got)
	}

	if got := installTrackingOptions(ReleaseSpec{NoFinalTracking: true}); !got.NoFinalTracking || !got.NoProgressTablePrint {
		t.Errorf("wait = false: got %+v, want NoFinalTracking=true NoProgressTablePrint=true", got)
	}
}

// TestWaitFalseTrackingSemantics pins what `wait = false` actually skips, as
// documented in docs/resources/release.md and docs/KNOWN_LIMITATIONS.md, by
// feeding installTrackingOptions' NoFinalTracking into nelm's own (offline)
// plan builder. NoFinalTracking only squashes readiness tracking that no
// later resource operation depends on, so it is NOT helm's wait = false:
// resources ahead of a post-install hook or a later weight group are still
// awaited, and a post-install hook without a hook-succeeded delete policy is
// not. A nelm upgrade that changes any of this fails here, so the docs cannot
// silently drift from the behavior.
func TestWaitFalseTrackingSemantics(t *testing.T) {
	deployment := testWorkload("Deployment", "apps/v1", "web", nil)

	tests := []struct {
		name       string
		objs       []*unstructured.Unstructured
		wantWait   []string // tracked with wait = true
		wantNoWait []string // tracked with wait = false
	}{
		{
			name:       "plain chart: nothing is awaited",
			objs:       []*unstructured.Unstructured{deployment},
			wantWait:   []string{"web"},
			wantNoWait: nil,
		},
		{
			name: "pre-install hook is still awaited, the main resources are not",
			objs: []*unstructured.Unstructured{
				testWorkload("Job", "batch/v1", "migrate", map[string]string{"helm.sh/hook": "pre-install,pre-upgrade"}),
				deployment,
			},
			wantWait:   []string{"migrate", "web"},
			wantNoWait: []string{"migrate"},
		},
		{
			name: "main resources are awaited ahead of a post-install hook; the hook itself is not",
			objs: []*unstructured.Unstructured{
				deployment,
				testWorkload("Job", "batch/v1", "smoke", map[string]string{"helm.sh/hook": "post-install,post-upgrade"}),
			},
			wantWait:   []string{"smoke", "web"},
			wantNoWait: []string{"web"},
		},
		{
			name: "a hook-succeeded delete policy keeps the post-install hook awaited",
			objs: []*unstructured.Unstructured{
				deployment,
				testWorkload("Job", "batch/v1", "smoke", map[string]string{
					"helm.sh/hook":               "post-install,post-upgrade",
					"helm.sh/hook-delete-policy": "before-hook-creation,hook-succeeded",
				}),
			},
			wantWait:   []string{"smoke", "web"},
			wantNoWait: []string{"smoke", "web"},
		},
		{
			name: "an earlier weight group is still awaited",
			objs: []*unstructured.Unstructured{
				testWorkload("StatefulSet", "apps/v1", "db", map[string]string{"werf.io/weight": "-10"}),
				deployment,
			},
			wantWait:   []string{"db", "web"},
			wantNoWait: []string{"db"},
		},
		{
			// The per-resource escape hatch (OnDelete StatefulSets, CRs that
			// never report ready): never tracked, whatever wait says.
			name: "werf.io/track-termination-mode NonBlocking is never awaited",
			objs: []*unstructured.Unstructured{
				testWorkload("StatefulSet", "apps/v1", "queue", map[string]string{"werf.io/track-termination-mode": "NonBlocking"}),
				deployment,
			},
			wantWait:   []string{"web"},
			wantNoWait: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, wait := range []bool{true, false} {
				want := tt.wantWait
				if !wait {
					want = tt.wantNoWait
				}

				got := trackedInInstallPlan(t, tt.objs, installTrackingOptions(ReleaseSpec{NoFinalTracking: !wait}))
				if !slices.Equal(got, want) {
					t.Errorf("wait = %v: readiness-tracked resources = %v, want %v", wait, got, want)
				}
			}
		})
	}
}

// trackedInInstallPlan builds nelm's initial-install plan for objs against a
// fake (offline) cluster with the given tracking options and returns the
// sorted names of the resources it will readiness-track.
func trackedInInstallPlan(t *testing.T, objs []*unstructured.Unstructured, tracking common.TrackingOptions) []string {
	t.Helper()

	const releaseName, releaseNamespace = "rel", "ns"

	ctx := context.Background()

	cf, err := fake.NewClientFactory(ctx)
	if err != nil {
		t.Fatalf("fake client factory: %v", err)
	}

	var resources []*resource.InstallableResource

	for _, src := range objs {
		obj := src.DeepCopy()

		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["meta.helm.sh/release-name"] = releaseName
		annotations["meta.helm.sh/release-namespace"] = releaseNamespace
		obj.SetAnnotations(annotations)
		obj.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm"})
		obj.SetNamespace(releaseNamespace)

		storeAs := common.StoreAsRegular
		if _, hook := annotations["helm.sh/hook"]; hook {
			storeAs = common.StoreAsHook
		}

		res, err := resource.NewInstallableResource(
			spec.NewResourceSpec(obj, releaseNamespace, spec.ResourceSpecOptions{StoreAs: storeAs}),
			nil, releaseNamespace, cf, resource.InstallableResourceOptions{},
		)
		if err != nil {
			t.Fatalf("installable resource %s: %v", obj.GetName(), err)
		}

		resources = append(resources, res)
	}

	infos, _, err := plan.BuildResourceInfos(ctx, common.DeployTypeInitial, releaseName, releaseNamespace,
		resources, nil, false, cf, plan.BuildResourceInfosOptions{NetworkParallelism: 1})
	if err != nil {
		t.Fatalf("build resource infos: %v", err)
	}

	p, err := plan.BuildPlan(infos, nil, nil, plan.BuildPlanOptions{NoFinalTracking: tracking.NoFinalTracking})
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	var tracked []string

	for _, op := range p.Operations() {
		if cfg, ok := op.Config.(*plan.OperationConfigTrackReadiness); ok {
			tracked = append(tracked, cfg.ResourceMeta.Name)
		}
	}

	slices.Sort(tracked)

	return tracked
}

// testWorkload returns a minimal Deployment/StatefulSet/Job manifest.
func testWorkload(kind, apiVersion, name string, annotations map[string]string) *unstructured.Unstructured {
	podSpec := map[string]any{
		"containers": []any{map[string]any{"name": "app", "image": "app:1"}},
	}

	var workloadSpec map[string]any

	if kind == "Job" {
		podSpec["restartPolicy"] = "Never"
		workloadSpec = map[string]any{"template": map[string]any{"spec": podSpec}}
	} else {
		labels := map[string]any{"app": name}
		workloadSpec = map[string]any{
			"replicas": int64(1),
			"selector": map[string]any{"matchLabels": labels},
			"template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": podSpec},
		}
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
		"spec":       workloadSpec,
	}}
	obj.SetAnnotations(annotations)

	return obj
}
