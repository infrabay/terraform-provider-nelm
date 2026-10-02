package nelmclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
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
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/infrabay/terraform-provider-nelm/internal/planconv"
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
// (offline) cluster the way action.ReleasePlanInstall does
// (plan.BuildResourceInfos, plan.BuildPlan, plan.CalculatePlannedChanges)
// and writes the plan artifact as it does, with nelm's own
// plan.WritePlanArtifact, unencrypted (the provider never sets a secret
// key); only the release and its release infos are left out of the data. Every patch of a
// resource named in forbidden (plural resource names, e.g. "clusterroles"),
// which while planning is the dry-run server-side apply of an existing
// object, fails with forbiddenPatch, as it does on the real API server. It
// returns the artifact path and the installable resource infos the artifact
// carries.
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

	deployType, revision := common.DeployTypeUpgrade, 2
	if len(live) == 0 {
		deployType, revision = common.DeployTypeInitial, 1
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
			Revision:  revision,
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
// failed with "read plan artifact: decode artifact data json: ..."; it now
// reads the artifact with readPlanArtifact, which never decodes
// installableResourceInfos.
func TestPlanArtifact_DryApplyErrorIssue8(t *testing.T) {
	path, infos := writeNelmPlanArtifact(t, vpaObjects(), vpaObjects(), "clusterroles", "clusterrolebindings")

	// wantReasons: what nelm puts in each failed object's change
	// (planned_changes.go buildInstChanges), by IDHuman.
	wantReasons := map[string]string{}

	for _, info := range infos {
		if info.DryApplyErr == nil {
			continue
		}

		if !apierrors.IsForbidden(info.DryApplyErr) {
			t.Errorf("precondition: %s dry-apply error is not a Kubernetes forbidden StatusError: %v", info.IDHuman(), info.DryApplyErr)
		}

		wantReasons[info.IDHuman()] = "error: " + info.DryApplyErr.Error()
	}

	if len(wantReasons) != 2 {
		t.Fatalf("precondition: %d objects' dry-run apply failed, want 2 (the ClusterRole and the ClusterRoleBinding)", len(wantReasons))
	}

	// nelm's own reader still fails on it. When this starts to pass, werf/nelm
	// has fixed the DryApplyErr serialization upstream: readPlanArtifact may
	// then be dropped for plan.ReadPlanArtifact again (update this test).
	if _, err := plan.ReadPlanArtifact(context.Background(), path, "", ""); err == nil {
		t.Errorf("nelm's plan.ReadPlanArtifact now reads an artifact with a dry-apply error (issue #8 fixed upstream?)")
	} else if !issue8Err.MatchString(err.Error()) {
		t.Errorf("nelm's plan.ReadPlanArtifact error = %v, want one matching %s (issue #8)", err, issue8Err)
	}

	res, err := readPlanArtifact(path)
	if err != nil {
		t.Fatalf("readPlanArtifact: %v", err)
	}

	if res.DeployType != DeployTypeUpgrade {
		t.Errorf("DeployType = %q, want %q", res.DeployType, DeployTypeUpgrade)
	}

	// The ServiceAccount is unchanged (no change); the two RBAC objects are
	// blind applies whose Reason carries the forbidden error.
	gotReasons := map[string]string{}

	for _, change := range res.Changes {
		if change.Type != "blind apply" {
			t.Errorf("%s: change type %q, want \"blind apply\"", change.ResourceMeta.IDHuman(), change.Type)
		}

		gotReasons[change.ResourceMeta.IDHuman()] = change.Reason
	}

	if !maps.Equal(gotReasons, wantReasons) {
		t.Errorf("blind-apply reasons = %q, want %q", gotReasons, wantReasons)
	}

	for id, reason := range gotReasons {
		if !strings.Contains(reason, `is forbidden: User "ci@example.iam.gserviceaccount.com" cannot patch resource`) {
			t.Errorf("%s: reason %q does not carry the API server's forbidden message", id, reason)
		}
	}
}

// rbacScoper answers like a real RESTMapper for the kinds of vpaObjects.
type rbacScoper struct{}

func (rbacScoper) IsNamespaced(gvk schema.GroupVersionKind) (bool, error) {
	switch gvk.Kind {
	case "ClusterRole", "ClusterRoleBinding":
		return false, nil
	default:
		return true, nil
	}
}

// TestPlanArtifact_DryApplyErrorPlansBlindApplyWarnings follows issue #8's
// artifact on to the planned "resources" map, as ModifyPlan builds it from
// Plan's result: the objects whose dry run was refused are unchanged, so the
// plan has no changes, and each is a blind-apply warning (ModifyPlan's
// "nelm_release: blind apply for <key>") carrying the forbidden error.
func TestPlanArtifact_DryApplyErrorPlansBlindApplyWarnings(t *testing.T) {
	path, _ := writeNelmPlanArtifact(t, vpaObjects(), vpaObjects(), "clusterroles", "clusterrolebindings")

	res, err := readPlanArtifact(path)
	if err != nil {
		t.Fatalf("readPlanArtifact: %v", err)
	}

	// The prior state: what the previous apply rendered and Read stored.
	rendered, err := planconv.BuildRenderedResources(vpaObjects(), artifactReleaseNamespace, rbacScoper{}, nil)
	if err != nil {
		t.Fatalf("BuildRenderedResources: %v", err)
	}

	planned, warnings, err := planconv.BuildPlannedResources(rendered, res.Changes, artifactReleaseNamespace, rbacScoper{}, rendered, nil)
	if err != nil {
		t.Fatalf("BuildPlannedResources: %v", err)
	}

	if !maps.Equal(planned, rendered) {
		t.Errorf("planned resources = %v, want the prior value (no changes)", planned)
	}

	gotWarnings := map[string]string{}
	for _, w := range warnings {
		gotWarnings[w.Resource] = w.Reason
	}

	for _, key := range []string{
		"rbac.authorization.k8s.io/v1/ClusterRole//vpa-admission-controller",
		"rbac.authorization.k8s.io/v1/ClusterRoleBinding//vpa-admission-controller",
	} {
		if reason, ok := gotWarnings[key]; !ok || !strings.Contains(reason, "is forbidden") {
			t.Errorf("warning for %s = %q (present: %v), want a blind-apply warning carrying the forbidden error", key, reason, ok)
		}
	}

	if len(warnings) != 2 {
		t.Errorf("warnings = %v, want exactly the two blind applies", warnings)
	}
}

// TestPlanArtifact_RoundTrip: on artifacts nelm's own reader can read,
// readPlanArtifact returns exactly the deploy type and changes it returns —
// for a plan written here without dry-apply errors, and for the artifacts
// the smoke harness captured from real clusters (first install, no change,
// drift, delete, Secret).
func TestPlanArtifact_RoundTrip(t *testing.T) {
	configMap := func(message string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "vpa-config", "namespace": artifactReleaseNamespace},
			"data":       map[string]any{"message": message},
		}}
	}

	service := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   map[string]any{"name": "vpa-webhook", "namespace": artifactReleaseNamespace},
		"spec":       map[string]any{"ports": []any{map[string]any{"port": int64(443)}}},
	}}

	sa := vpaObjects()[0]

	generated, _ := writeNelmPlanArtifact(t,
		[]*unstructured.Unstructured{sa, configMap("hello")},
		[]*unstructured.Unstructured{sa, configMap("hello again"), service},
	)

	captured, err := filepath.Glob("../planconv/testdata/lifecycle/*.artifact.json.gz")
	if err != nil || len(captured) == 0 {
		t.Fatalf("captured lifecycle artifacts: %v, %v", captured, err)
	}

	captured = append(captured, "../planconv/testdata/secrets/secret_plan.artifact.json.gz")

	for _, path := range append([]string{generated}, captured...) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			want, err := plan.ReadPlanArtifact(context.Background(), path, "", "")
			if err != nil {
				t.Fatalf("plan.ReadPlanArtifact: %v", err)
			}

			got, err := readPlanArtifact(path)
			if err != nil {
				t.Fatalf("readPlanArtifact: %v", err)
			}

			if got.DeployType != string(want.DeployType) {
				t.Errorf("DeployType = %q, want %q", got.DeployType, want.DeployType)
			}

			if !reflect.DeepEqual(got.Changes, want.Data.Changes) {
				t.Errorf("Changes differ from plan.ReadPlanArtifact's:\n got: %s\nwant: %s", mustJSON(t, got.Changes), mustJSON(t, want.Data.Changes))
			}

			if path == generated {
				var types []string
				for _, change := range got.Changes {
					types = append(types, change.Type+" "+change.ResourceMeta.IDHuman())
				}

				slices.Sort(types)

				if wantTypes := []string{"create Service/vpa-webhook", "update ConfigMap/vpa-config"}; !slices.Equal(types, wantTypes) {
					t.Errorf("precondition: generated plan changes = %q, want %q", types, wantTypes)
				}
			}
		})
	}
}

// jsonFields maps the JSON names of t's serialized fields to their types.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}

	for i := range t.NumField() {
		f := t.Field(i)

		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" || !f.IsExported() {
			continue
		}

		if name == "" {
			name = f.Name
		}

		fields[name] = f.Type
	}

	return fields
}

// TestPlanArtifactFormat fails loudly when a nelm upgrade changes the parts
// of the plan artifact format readPlanArtifact depends on (it does not use
// nelm's types to decode, so the compiler cannot catch it): the scheme
// version, the top-level fields it reads (name and type), data being
// serialized only as dataRaw, and data's changes field. The write side
// (gzip, dataRaw as a JSON string) is exercised by TestPlanArtifact_RoundTrip
// through nelm's own plan.WritePlanArtifact.
func TestPlanArtifactFormat(t *testing.T) {
	if planArtifactAPIVersion != plan.PlanArtifactSchemeVersion {
		t.Errorf("nelm's plan artifact scheme is %q, readPlanArtifact reads %q", plan.PlanArtifactSchemeVersion, planArtifactAPIVersion)
	}

	nelmArtifact := jsonFields(reflect.TypeFor[plan.PlanArtifact]())

	for name, typ := range jsonFields(reflect.TypeFor[planArtifact]()) {
		if nelmType, ok := nelmArtifact[name]; !ok {
			t.Errorf("nelm's plan.PlanArtifact has no %q JSON field any more", name)
		} else if nelmType != typ {
			t.Errorf("nelm's plan.PlanArtifact field %q is a %s, readPlanArtifact decodes a %s", name, nelmType, typ)
		}
	}

	if f, ok := reflect.TypeFor[plan.PlanArtifact]().FieldByName("Data"); !ok || f.Tag.Get("json") != "-" {
		t.Errorf("nelm's plan.PlanArtifact.Data is no longer serialized only as dataRaw (json tag %q)", f.Tag.Get("json"))
	}

	ours := jsonFields(reflect.TypeFor[planArtifactData]())
	if len(ours) != 1 || ours["changes"] == nil {
		t.Fatalf("planArtifactData fields = %v, this test checks only \"changes\"", ours)
	}

	wantChanges := reflect.TypeFor[[]*plan.ResourceChange]()
	if got := jsonFields(reflect.TypeFor[plan.PlanArtifactData]())["changes"]; got != wantChanges {
		t.Errorf("nelm's plan.PlanArtifactData \"changes\" field is a %v, readPlanArtifact decodes a %s", got, wantChanges)
	}
}

// gzipArtifact is a plan artifact file's bytes: top as gzip-compressed JSON,
// with data (when non-nil) JSON-encoded into dataRaw the way
// plan.WritePlanArtifact stores it.
func gzipArtifact(t *testing.T, top map[string]any, data any) *bytes.Buffer {
	t.Helper()

	if data != nil {
		top["dataRaw"] = string(mustJSON(t, data))
	}

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(top); err != nil {
		t.Fatalf("encode artifact: %v", err)
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}

	return &buf
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return b
}

// TestDecodePlanArtifact: readPlanArtifact ignores everything it does not
// read, whatever its shape, and refuses an artifact it cannot read
// correctly with a clear error instead of planning no changes.
func TestDecodePlanArtifact(t *testing.T) {
	blindApply := &plan.ResourceChange{
		Type:         "blind apply",
		Reason:       "error: boom",
		ResourceMeta: spec.NewResourceMetaFromUnstructured(vpaObjects()[1], artifactReleaseNamespace, ""),
		After:        vpaObjects()[1],
	}

	top := func() map[string]any {
		return map[string]any{
			"apiVersion": "v1",
			"deployType": "Upgrade",
			"encrypted":  false,
			"release":    map[string]any{"name": artifactReleaseName, "namespace": artifactReleaseNamespace, "revision": 2},
			"timestamp":  "2026-10-02T12:00:00Z",
		}
	}

	t.Run("unknown fields and any installableResourceInfos", func(t *testing.T) {
		artifact := top()
		artifact["signature"] = map[string]any{"future": "field"}

		got, err := decodePlanArtifact(gzipArtifact(t, artifact, map[string]any{
			"changes": []*plan.ResourceChange{blindApply},
			"installableResourceInfos": []any{
				map[string]any{"dryApplyErr": map[string]any{"ErrStatus": map[string]any{"reason": "Forbidden", "code": 403}}},
				map[string]any{"dryApplyErr": "server-side dry-run apply: forbidden"},
				map[string]any{"dryApplyErr": 42},
				"not an object",
			},
			"releaseInfos": "anything",
			"future":       []any{true},
		}))
		if err != nil {
			t.Fatalf("decodePlanArtifact: %v", err)
		}

		if got.DeployType != DeployTypeUpgrade {
			t.Errorf("DeployType = %q, want %q", got.DeployType, DeployTypeUpgrade)
		}

		if len(got.Changes) != 1 || got.Changes[0].Type != "blind apply" || got.Changes[0].Reason != "error: boom" ||
			got.Changes[0].ResourceMeta.IDHuman() != "ClusterRole/vpa-admission-controller" {
			t.Errorf("Changes = %s, want the one blind apply", mustJSON(t, got.Changes))
		}
	})

	t.Run("no changes", func(t *testing.T) {
		// nelm writes a plan without changes as "changes": null.
		got, err := decodePlanArtifact(gzipArtifact(t, top(), map[string]any{"changes": nil}))
		if err != nil {
			t.Fatalf("decodePlanArtifact: %v", err)
		}

		if got.Changes != nil {
			t.Errorf("Changes = %v, want none", got.Changes)
		}
	})

	errorCases := map[string]struct {
		artifact io.Reader
		want     string
	}{
		"not gzip": {
			artifact: strings.NewReader(`{"apiVersion":"v1"}`),
			want:     "create gzip reader: ",
		},
		"not JSON": {
			artifact: func() io.Reader {
				var buf bytes.Buffer
				zw := gzip.NewWriter(&buf)
				_, _ = zw.Write([]byte("apiVersion: v1"))
				_ = zw.Close()

				return &buf
			}(),
			want: "decode plan artifact json: ",
		},
		"other scheme": {
			artifact: func() io.Reader {
				a := top()
				a["apiVersion"] = "v2"

				return gzipArtifact(t, a, map[string]any{"changes": nil})
			}(),
			want: `unsupported plan artifact apiVersion "v2", want "v1"`,
		},
		"encrypted": {
			artifact: func() io.Reader {
				a := top()
				a["encrypted"] = true
				a["dataRaw"] = "0a1b2c3d"

				return gzipArtifact(t, a, nil)
			}(),
			want: "plan artifact is encrypted, but the provider never sets a secret key",
		},
		"no data": {
			artifact: gzipArtifact(t, top(), nil),
			want:     "artifact data is empty",
		},
		"no deploy type": {
			artifact: func() io.Reader {
				a := top()
				delete(a, "deployType")

				return gzipArtifact(t, a, map[string]any{"changes": nil})
			}(),
			want: "plan artifact has no deployType",
		},
		"data not JSON": {
			artifact: func() io.Reader {
				a := top()
				a["dataRaw"] = "changes: []"

				return gzipArtifact(t, a, nil)
			}(),
			want: "decode artifact data json: ",
		},
		"no changes field": {
			artifact: gzipArtifact(t, top(), map[string]any{"resourceChanges": []any{}}),
			want:     `artifact data has no "changes" field`,
		},
		"changes not a list": {
			artifact: gzipArtifact(t, top(), map[string]any{"changes": map[string]any{"type": "create"}}),
			want:     "decode artifact data changes: ",
		},
	}

	for name, tt := range errorCases {
		t.Run(name, func(t *testing.T) {
			got, err := decodePlanArtifact(tt.artifact)
			if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Fatalf("decodePlanArtifact = %v, %v; want an error starting with %q", got, err, tt.want)
			}
		})
	}
}
