package nelmclient

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/werf/nelm/pkg/common"
	"github.com/werf/nelm/pkg/kube/fake"
	nelmlog "github.com/werf/nelm/pkg/log"
	"github.com/werf/nelm/pkg/plan"
	"github.com/werf/nelm/pkg/resource"
	"github.com/werf/nelm/pkg/resource/spec"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestCaptureWarningsCtx_KeepsPlannedDiffsOutOfErrors is the regression test
// for Plan folding nelm's resource diffs into its error diagnostics:
// ReleasePlanInstall logs every planned change's unified diff at info level
// before it writes the plan artifact, and nelm diffs a Secret annotated
// werf.io/sensitive: "false" with its data. A plan that then failed (e.g.
// writing the artifact hit ENOSPC) carried that Secret data into the
// "nelm_release plan failed" diagnostic and CI logs. The diff is produced and
// logged exactly as nelm's logPlannedChanges does.
func TestCaptureWarningsCtx_KeepsPlannedDiffsOutOfErrors(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const (
		oldData = "T0xEUEFTUw==" // base64("OLDPASS")
		newData = "TkVXUEFTUw==" // base64("NEWPASS")
	)

	secret := func(data string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]interface{}{
				"name":        "db",
				"namespace":   "default",
				"annotations": map[string]interface{}{"werf.io/sensitive": "false"},
			},
			"data": map[string]interface{}{"password": data},
		}}
	}

	change := &plan.ResourceChange{
		Type:         "update",
		ResourceMeta: spec.NewResourceMetaFromUnstructured(secret(newData), "default", "templates/secret.yaml"),
		Before:       secret(oldData),
		After:        secret(newData),
	}

	uDiff, err := change.UDiff(common.ResourceDiffOptions{DiffContextLines: common.DefaultDiffContextLines})
	if err != nil {
		t.Fatalf("UDiff: %v", err)
	}

	if !strings.Contains(uDiff, newData) {
		t.Fatalf("precondition: nelm's diff of an opted-out Secret no longer shows its data, so this test proves nothing:\n%s", uDiff)
	}

	ctx, buf := captureWarningsCtx(ctx)

	if err := nelmlog.Default.InfoBlockErr(ctx, nelmlog.BlockOptions{BlockTitle: "Update Secret/db"}, func() error {
		nelmlog.Default.Info(ctx, "%s", uDiff)
		return nil
	}); err != nil {
		t.Fatalf("InfoBlockErr: %v", err)
	}

	nelmlog.Default.Warn(ctx, "Chart %q is deprecated", "app:1.0.0")

	got := tailErr(errors.New(`save install plan to "/tmp/plan.artifact": no space left on device`), buf).Error()

	for _, data := range []string{oldData, newData} {
		if strings.Contains(got, data) {
			t.Fatalf("Secret data %q from nelm's planned-change diff reached the error:\n%s", data, got)
		}
	}

	if !strings.Contains(got, `Chart "app:1.0.0" is deprecated`) {
		t.Fatalf("nelm's warnings must still be folded into the error:\n%s", got)
	}
}

// TestTailErr_FoldsABoundedTail checks tailErr folds only the end of a long
// nelm log (where nelm reports what failed), starting on a line boundary,
// and keeps the original error unwrappable.
func TestTailErr_FoldsABoundedTail(t *testing.T) {
	buf := &syncBuffer{}

	for i := 0; i < 5000; i++ {
		_, _ = fmt.Fprintf(buf, "nelm log line %05d\n", i)
	}

	base := errors.New("release install: boom")

	err := tailErr(base, buf)
	if !errors.Is(err, base) {
		t.Fatalf("tailErr must wrap the original error, got: %v", err)
	}

	got := err.Error()

	if len(got) > maxErrTail+256 {
		t.Fatalf("folded %d bytes, want at most about %d", len(got), maxErrTail)
	}

	if !strings.Contains(got, "nelm log line 04999\n") {
		t.Fatalf("the end of the log must be kept:\n%s", got)
	}

	if strings.Contains(got, "nelm log line 00000") {
		t.Fatal("the start of a long log must be dropped")
	}

	_, rest, found := strings.Cut(got, "earlier bytes omitted ...]\n")
	if !found {
		t.Fatalf("expected an omission marker:\n%.300s", got)
	}

	if !strings.HasPrefix(rest, "nelm log line ") {
		t.Fatalf("tail must start on a line boundary, starts with %.40q", rest)
	}

	short := &syncBuffer{}
	_, _ = short.Write([]byte("only line\n"))

	if got := tailErr(base, short).Error(); !strings.HasSuffix(got, "--- nelm output ---\nonly line\n") {
		t.Fatalf("a short log must be folded whole, got: %q", got)
	}
}

// TestInstallTrackingOptions is the regression test for `wait`: Install used
// to hard-code TrackingOptions{NoProgressTablePrint: true}, so nelm's final
// readiness tracking always ran and helm_release's wait = false had no
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
// documented in docs/resources/release.md and
// docs/guides/known-limitations.md, by feeding installTrackingOptions'
// NoFinalTracking into nelm's own (offline) plan builder. NoFinalTracking only squashes readiness tracking that no
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
