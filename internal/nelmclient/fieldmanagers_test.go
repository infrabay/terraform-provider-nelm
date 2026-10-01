package nelmclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// The field managers of two hashicorp/helm versions (several provider
// versions can have written one object). The tests pair them with nelm's own
// "helm" entries and with unrelated writers that must survive untouched.
const (
	tfHelm302 = "terraform-provider-helm_v3.0.2_x5"
	tfHelm311 = "terraform-provider-helm_v3.1.1_x5"
)

func mfEntry(manager string, op metav1.ManagedFieldsOperationType, apiVersion, subresource, fields string, ts time.Time) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{
		Manager:     manager,
		Operation:   op,
		APIVersion:  apiVersion,
		Time:        &metav1.Time{Time: ts},
		FieldsType:  "FieldsV1",
		FieldsV1:    &metav1.FieldsV1{Raw: []byte(fields)},
		Subresource: subresource,
	}
}

// mustJSONEqual compares two FieldsV1 documents semantically (key order and
// whitespace are irrelevant to a field set).
func mustJSONEqual(t *testing.T, what string, got []byte, want string) {
	t.Helper()

	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: got invalid JSON %s: %v", what, got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("%s: want invalid JSON %s: %v", what, want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("%s:\n got: %s\nwant: %s", what, got, want)
	}
}

func findEntry(entries []metav1.ManagedFieldsEntry, manager string, op metav1.ManagedFieldsOperationType, apiVersion string) (metav1.ManagedFieldsEntry, int) {
	var (
		found metav1.ManagedFieldsEntry
		n     int
	)

	for _, e := range entries {
		if e.Manager == manager && e.Operation == op && e.APIVersion == apiVersion && e.Subresource == "" {
			found = e
			n++
		}
	}

	return found, n
}

// TestHandOverHelmProviderEntries is the F08 regression test for the rename
// itself: objects written by hashicorp/helm's helm_release carry
// "terraform-provider-helm_*"/Update entries that nelm's Helm 3 hand-over
// never recognized, so fields the chart stopped rendering stayed live after
// migrating to nelm_release. Every such main-resource Update entry must come
// out as "helm"/Update (the manager nelm folds into its own Apply entry).
func TestHandOverHelmProviderEntries(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)

	apply := metav1.ManagedFieldsOperationApply
	update := metav1.ManagedFieldsOperationUpdate

	t.Run("no helm_release entry is a no-op", func(t *testing.T) {
		in := []metav1.ManagedFieldsEntry{
			mfEntry("helm", apply, "v1", "", `{"f:data":{"f:A":{}}}`, t0),
			mfEntry("kube-controller-manager", update, "v1", "", `{"f:metadata":{"f:annotations":{"f:x":{}}}}`, t0),
		}

		out, changed, err := handOverHelmProviderEntries(in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if changed {
			t.Fatal("changed = true for an object without helm_release entries")
		}
		if !reflect.DeepEqual(out, in) {
			t.Fatalf("entries modified:\n got: %+v\nwant: %+v", out, in)
		}
	})

	t.Run("a lone entry is renamed with its fields", func(t *testing.T) {
		in := []metav1.ManagedFieldsEntry{
			mfEntry(tfHelm311, update, "v1", "", `{"f:data":{"f:A":{},"f:FEATURE_X":{}}}`, t0),
			mfEntry("kube-controller-manager", update, "v1", "", `{"f:metadata":{"f:annotations":{"f:x":{}}}}`, t0),
		}

		out, changed, err := handOverHelmProviderEntries(in)
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v, want changed=true err=nil", changed, err)
		}
		if len(out) != 2 {
			t.Fatalf("got %d entries, want 2: %+v", len(out), out)
		}

		helm, n := findEntry(out, "helm", update, "v1")
		if n != 1 {
			t.Fatalf(`want exactly one "helm"/Update entry, got %d: %+v`, n, out)
		}
		mustJSONEqual(t, "helm/Update fields", helm.FieldsV1.Raw, `{"f:data":{"f:A":{},"f:FEATURE_X":{}}}`)

		if _, n := findEntry(out, "kube-controller-manager", update, "v1"); n != 1 {
			t.Fatalf("unrelated manager dropped: %+v", out)
		}
	})

	t.Run("fields are unioned into an existing helm/Update entry", func(t *testing.T) {
		// A label key with "/" exercises JSON-pointer escaping in the union.
		in := []metav1.ManagedFieldsEntry{
			mfEntry(tfHelm311, update, "v1", "", `{"f:data":{"f:FEATURE_X":{}},"f:metadata":{"f:labels":{"f:app.kubernetes.io/name":{}}}}`, t1),
			mfEntry("helm", update, "v1", "", `{"f:data":{"f:A":{}}}`, t0),
			mfEntry("helm", apply, "v1", "", `{"f:data":{"f:A":{}}}`, t0),
		}

		out, changed, err := handOverHelmProviderEntries(in)
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v, want changed=true err=nil", changed, err)
		}

		helm, n := findEntry(out, "helm", update, "v1")
		if n != 1 {
			t.Fatalf(`want exactly one "helm"/Update entry, got %d: %+v`, n, out)
		}
		mustJSONEqual(t, "helm/Update fields", helm.FieldsV1.Raw,
			`{"f:data":{"f:A":{},"f:FEATURE_X":{}},"f:metadata":{"f:labels":{"f:app.kubernetes.io/name":{}}}}`)
		if !helm.Time.Equal(&metav1.Time{Time: t1}) {
			t.Errorf("merged entry time = %v, want the later %v", helm.Time, t1)
		}

		if _, n := findEntry(out, "helm", apply, "v1"); n != 1 {
			t.Fatalf(`"helm"/Apply entry must be left alone: %+v`, out)
		}

		// The caller's slice (the live object's managedFields) is untouched.
		mustJSONEqual(t, "input helm/Update fields", in[1].FieldsV1.Raw, `{"f:data":{"f:A":{}}}`)
		if in[0].Manager != tfHelm311 {
			t.Errorf("input entry renamed in place: %q", in[0].Manager)
		}
	})

	t.Run("several provider versions fold into one entry", func(t *testing.T) {
		in := []metav1.ManagedFieldsEntry{
			mfEntry(tfHelm302, update, "v1", "", `{"f:data":{"f:FEATURE_X":{}}}`, t0),
			mfEntry(tfHelm311, update, "v1", "", `{"f:data":{"f:A":{}}}`, t1),
		}

		out, changed, err := handOverHelmProviderEntries(in)
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v, want changed=true err=nil", changed, err)
		}
		if len(out) != 1 {
			t.Fatalf("got %d entries, want 1: %+v", len(out), out)
		}

		mustJSONEqual(t, "helm/Update fields", out[0].FieldsV1.Raw, `{"f:data":{"f:A":{},"f:FEATURE_X":{}}}`)
	})

	t.Run("apiVersions stay apart", func(t *testing.T) {
		in := []metav1.ManagedFieldsEntry{
			mfEntry("helm", update, "apps/v1beta2", "", `{"f:spec":{"f:replicas":{}}}`, t0),
			mfEntry(tfHelm311, update, "apps/v1", "", `{"f:spec":{"f:template":{}}}`, t0),
		}

		out, changed, err := handOverHelmProviderEntries(in)
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v, want changed=true err=nil", changed, err)
		}

		v1, n := findEntry(out, "helm", update, "apps/v1")
		if n != 1 {
			t.Fatalf(`want one "helm"/Update apps/v1 entry: %+v`, out)
		}
		mustJSONEqual(t, "apps/v1 fields", v1.FieldsV1.Raw, `{"f:spec":{"f:template":{}}}`)

		beta, n := findEntry(out, "helm", update, "apps/v1beta2")
		if n != 1 {
			t.Fatalf(`want one "helm"/Update apps/v1beta2 entry: %+v`, out)
		}
		mustJSONEqual(t, "apps/v1beta2 fields", beta.FieldsV1.Raw, `{"f:spec":{"f:replicas":{}}}`)
	})

	t.Run("subresource and Apply entries are left alone", func(t *testing.T) {
		in := []metav1.ManagedFieldsEntry{
			mfEntry(tfHelm311, update, "v1", "status", `{"f:status":{}}`, t0),
			mfEntry(tfHelm311, apply, "v1", "", `{"f:data":{"f:A":{}}}`, t0),
		}

		out, changed, err := handOverHelmProviderEntries(in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if changed || !reflect.DeepEqual(out, in) {
			t.Fatalf("changed=%v out=%+v, want the entries untouched", changed, out)
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		in := []metav1.ManagedFieldsEntry{
			mfEntry(tfHelm302, update, "v1", "", `{"f:data":{"f:FEATURE_X":{}}}`, t0),
			mfEntry("helm", update, "v1", "", `{"f:data":{"f:A":{}}}`, t0),
		}

		once, _, err := handOverHelmProviderEntries(in)
		if err != nil {
			t.Fatalf("first pass: %v", err)
		}

		twice, changed, err := handOverHelmProviderEntries(once)
		if err != nil {
			t.Fatalf("second pass: %v", err)
		}
		if changed || !reflect.DeepEqual(twice, once) {
			t.Fatalf("second pass changed=%v out=%+v, want a no-op", changed, twice)
		}
	})
}

// fakeAPIServer is a minimal Kubernetes API server for the dynamic client: it
// serves GET and merge-PATCH of stored objects by URL path, enforcing the
// resourceVersion precondition a real API server enforces on a patch that
// carries one.
type fakeAPIServer struct {
	t *testing.T

	mu      sync.Mutex
	objects map[string]map[string]any
	patches map[string]int

	// beforePatch, if set for a path, runs once (under mu) before that
	// path's next PATCH is evaluated: a concurrent writer, or a failure to
	// inject (a non-zero code is written back as a Status error).
	beforePatch map[string]func(obj map[string]any) (code int, msg string)

	// blockGets makes GET wait until the client gives up on the request.
	blockGets bool
}

func newFakeAPIServer(t *testing.T) (*fakeAPIServer, dynamic.Interface) {
	t.Helper()

	f := &fakeAPIServer{
		t:           t,
		objects:     map[string]map[string]any{},
		patches:     map[string]int{},
		beforePatch: map[string]func(map[string]any) (int, string){},
	}

	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)

	dyn, err := dynamic.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}

	return f, dyn
}

func (f *fakeAPIServer) add(path string, obj map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()

	metadata := obj["metadata"].(map[string]any)
	if _, ok := metadata["resourceVersion"]; !ok {
		metadata["resourceVersion"] = "1"
	}

	f.objects[path] = obj
}

func (f *fakeAPIServer) managedFields(path string) []metav1.ManagedFieldsEntry {
	f.mu.Lock()
	defer f.mu.Unlock()

	raw, err := json.Marshal(f.objects[path]["metadata"].(map[string]any)["managedFields"])
	if err != nil {
		f.t.Fatalf("marshal stored managedFields: %v", err)
	}

	var entries []metav1.ManagedFieldsEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		f.t.Fatalf("unmarshal stored managedFields: %v", err)
	}

	return entries
}

func (f *fakeAPIServer) patchCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.patches[path]
}

func writeStatus(w http.ResponseWriter, code int, reason metav1.StatusReason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Message:  msg,
		Reason:   reason,
		Code:     int32(code),
	})
}

func writeObject(w http.ResponseWriter, obj map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(obj)
}

func (f *fakeAPIServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()

	if r.Method == http.MethodGet && f.blockGets {
		f.mu.Unlock()
		<-r.Context().Done()

		return
	}

	defer f.mu.Unlock()

	obj, ok := f.objects[r.URL.Path]

	switch r.Method {
	case http.MethodGet:
		if !ok {
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "not found")
			return
		}

		writeObject(w, obj)

	case http.MethodPatch:
		f.patches[r.URL.Path]++

		if ct := r.Header.Get("Content-Type"); ct != "application/merge-patch+json" {
			f.t.Errorf("PATCH %s: content type %q, want a JSON merge patch", r.URL.Path, ct)
		}
		if fm := r.URL.Query().Get("fieldManager"); fm != "helm" {
			f.t.Errorf("PATCH %s: fieldManager %q, want %q", r.URL.Path, fm, "helm")
		}

		if !ok {
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "not found")
			return
		}

		if hook := f.beforePatch[r.URL.Path]; hook != nil {
			delete(f.beforePatch, r.URL.Path)

			if code, msg := hook(obj); code != 0 {
				writeStatus(w, code, metav1.StatusReasonUnknown, msg)
				return
			}
		}

		// Handler goroutine: report with Errorf, never Fatalf.
		var patch struct {
			Metadata map[string]any `json:"metadata"`
		}

		body, err := io.ReadAll(r.Body)
		if err == nil {
			err = json.Unmarshal(body, &patch)
		}
		if err != nil {
			f.t.Errorf("PATCH %s: unreadable body %s: %v", r.URL.Path, body, err)
			writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())

			return
		}

		for k := range patch.Metadata {
			if k != "managedFields" && k != "resourceVersion" {
				f.t.Errorf("PATCH %s touches metadata.%s; only managedFields may change", r.URL.Path, k)
			}
		}

		metadata := obj["metadata"].(map[string]any)

		// Like a real API server, a patch without a resourceVersion is
		// unconditional.
		if rv, ok := patch.Metadata["resourceVersion"]; ok && rv != metadata["resourceVersion"] {
			writeStatus(w, http.StatusConflict, metav1.StatusReasonConflict, "the object has been modified")
			return
		}

		metadata["managedFields"] = patch.Metadata["managedFields"]
		bumpResourceVersion(metadata)

		writeObject(w, obj)

	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		writeStatus(w, http.StatusMethodNotAllowed, metav1.StatusReasonMethodNotAllowed, "unexpected")
	}
}

func bumpResourceVersion(metadata map[string]any) {
	rv, _ := strconv.Atoi(metadata["resourceVersion"].(string))
	metadata["resourceVersion"] = strconv.Itoa(rv + 1)
}

// managedFieldsJSON renders entries the way the API server stores them.
func managedFieldsJSON(t *testing.T, entries ...metav1.ManagedFieldsEntry) []any {
	t.Helper()

	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal managedFields: %v", err)
	}

	var out []any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal managedFields: %v", err)
	}

	return out
}

func testObject(t *testing.T, apiVersion, kind, namespace, name string, entries ...metav1.ManagedFieldsEntry) map[string]any {
	t.Helper()

	metadata := map[string]any{"name": name, "managedFields": managedFieldsJSON(t, entries...)}
	if namespace != "" {
		metadata["namespace"] = namespace
	}

	return map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": metadata}
}

func testMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, meta.RESTScopeRoot)

	return mapper
}

const (
	cmPath      = "/api/v1/namespaces/app/configmaps/app"
	plainCMPath = "/api/v1/namespaces/app/configmaps/plain"
	crPath      = "/apis/rbac.authorization.k8s.io/v1/clusterroles/app"
)

var (
	cmRef      = ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: "app", Name: "app"}
	plainCMRef = ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: "app", Name: "plain"}
	crRef      = ResourceRef{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole", Name: "app"}
)

// TestHandOverHelmProviderFieldManagers covers the cluster side of the F08
// fix against a fake API server: only objects carrying a helm_release entry
// are patched (namespaced and cluster-scoped alike), unrelated managers
// survive, missing objects and unserved kinds are skipped, and a second run
// is a no-op.
func TestHandOverHelmProviderFieldManagers(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	update := metav1.ManagedFieldsOperationUpdate

	f, dyn := newFakeAPIServer(t)

	f.add(cmPath, testObject(t, "v1", "ConfigMap", "app", "app",
		mfEntry(tfHelm311, update, "v1", "", `{"f:data":{"f:A":{},"f:FEATURE_X":{}}}`, ts),
		mfEntry("kubectl-label", update, "v1", "", `{"f:metadata":{"f:labels":{"f:team":{}}}}`, ts),
	))
	f.add(plainCMPath, testObject(t, "v1", "ConfigMap", "app", "plain",
		mfEntry("helm", metav1.ManagedFieldsOperationApply, "v1", "", `{"f:data":{"f:A":{}}}`, ts),
	))
	f.add(crPath, testObject(t, "rbac.authorization.k8s.io/v1", "ClusterRole", "", "app",
		mfEntry(tfHelm302, update, "rbac.authorization.k8s.io/v1", "", `{"f:rules":{}}`, ts),
	))

	refs := []ResourceRef{
		cmRef,
		plainCMRef,
		crRef,
		{Version: "v1", Kind: "ConfigMap", Namespace: "app", Name: "gone"},
		{Group: "example.com", Version: "v1", Kind: "Widget", Namespace: "app", Name: "unserved"},
	}

	skipped, err := handOverHelmProviderFieldManagers(context.Background(), testMapper(), dyn, refs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("unexpected skipped objects: %v", skipped)
	}

	if got := f.patchCount(cmPath); got != 1 {
		t.Errorf("ConfigMap app patched %d times, want 1", got)
	}
	if got := f.patchCount(crPath); got != 1 {
		t.Errorf("ClusterRole app patched %d times, want 1", got)
	}
	if got := f.patchCount(plainCMPath); got != 0 {
		t.Errorf("ConfigMap plain (no helm_release entry) patched %d times, want 0", got)
	}

	cm := f.managedFields(cmPath)
	if len(cm) != 2 {
		t.Fatalf("ConfigMap app managedFields = %+v, want helm/Update + kubectl-label/Update", cm)
	}
	helm, n := findEntry(cm, "helm", update, "v1")
	if n != 1 {
		t.Fatalf(`ConfigMap app: want one "helm"/Update entry: %+v`, cm)
	}
	mustJSONEqual(t, "ConfigMap app helm/Update fields", helm.FieldsV1.Raw, `{"f:data":{"f:A":{},"f:FEATURE_X":{}}}`)
	if _, n := findEntry(cm, "kubectl-label", update, "v1"); n != 1 {
		t.Fatalf("ConfigMap app: unrelated kubectl-label entry lost: %+v", cm)
	}

	cr := f.managedFields(crPath)
	if _, n := findEntry(cr, "helm", update, "rbac.authorization.k8s.io/v1"); n != 1 || len(cr) != 1 {
		t.Fatalf(`ClusterRole app managedFields = %+v, want a single "helm"/Update entry`, cr)
	}

	// Idempotent: nothing left to hand over, so nothing is patched again.
	if _, err := handOverHelmProviderFieldManagers(context.Background(), testMapper(), dyn, refs); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := f.patchCount(cmPath) + f.patchCount(crPath) + f.patchCount(plainCMPath); got != 2 {
		t.Errorf("second run patched again: %d patches in total, want 2", got)
	}
}

// TestHandOverHelmProviderFieldManagers_ConflictRetried asserts the patch is
// conditional on the resourceVersion it was computed from: a write landing
// between the GET and the PATCH fails it with a Conflict, the hand-over is
// recomputed from a fresh GET, and that writer's own entry survives (an
// unconditional managedFields replace would silently drop it).
func TestHandOverHelmProviderFieldManagers_ConflictRetried(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	update := metav1.ManagedFieldsOperationUpdate

	f, dyn := newFakeAPIServer(t)

	f.add(cmPath, testObject(t, "v1", "ConfigMap", "app", "app",
		mfEntry(tfHelm311, update, "v1", "", `{"f:data":{"f:FEATURE_X":{}}}`, ts),
	))

	f.beforePatch[cmPath] = func(obj map[string]any) (int, string) {
		metadata := obj["metadata"].(map[string]any)
		metadata["managedFields"] = append(metadata["managedFields"].([]any),
			managedFieldsJSON(t, mfEntry("kubectl-annotate", update, "v1", "", `{"f:metadata":{"f:annotations":{"f:note":{}}}}`, ts))...)
		bumpResourceVersion(metadata)

		return 0, ""
	}

	skipped, err := handOverHelmProviderFieldManagers(context.Background(), testMapper(), dyn, []ResourceRef{cmRef})
	if err != nil || len(skipped) != 0 {
		t.Fatalf("skipped=%v err=%v, want neither", skipped, err)
	}

	if got := f.patchCount(cmPath); got != 2 {
		t.Errorf("PATCH attempts = %d, want 2 (one Conflict, one retry)", got)
	}

	cm := f.managedFields(cmPath)
	if _, n := findEntry(cm, "helm", update, "v1"); n != 1 {
		t.Fatalf(`want a "helm"/Update entry after the retry: %+v`, cm)
	}
	if _, n := findEntry(cm, "kubectl-annotate", update, "v1"); n != 1 {
		t.Fatalf("the concurrent writer's entry was overwritten: %+v", cm)
	}
	if _, n := findEntry(cm, tfHelm311, update, "v1"); n != 0 {
		t.Fatalf("helm_release entry still present: %+v", cm)
	}
}

// TestHandOverHelmProviderFieldManagers_WebhookUnavailableSkipped mirrors
// nelm's own managedFields fix: an admission webhook that cannot be reached
// skips that object (reported, retried by a later apply) instead of failing
// the apply, and the remaining objects are still handed over.
func TestHandOverHelmProviderFieldManagers_WebhookUnavailableSkipped(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	update := metav1.ManagedFieldsOperationUpdate

	f, dyn := newFakeAPIServer(t)

	f.add(cmPath, testObject(t, "v1", "ConfigMap", "app", "app",
		mfEntry(tfHelm311, update, "v1", "", `{"f:data":{"f:FEATURE_X":{}}}`, ts),
	))
	f.add(crPath, testObject(t, "rbac.authorization.k8s.io/v1", "ClusterRole", "", "app",
		mfEntry(tfHelm311, update, "rbac.authorization.k8s.io/v1", "", `{"f:rules":{}}`, ts),
	))

	f.beforePatch[cmPath] = func(map[string]any) (int, string) {
		return http.StatusInternalServerError, `Internal error occurred: failed calling webhook "validate.example.com": ` +
			`failed to call webhook: Post "https://webhook.example.svc:443/validate": dial tcp 10.0.0.1:443: connect: connection refused`
	}

	skipped, err := handOverHelmProviderFieldManagers(context.Background(), testMapper(), dyn, []ResourceRef{cmRef, crRef})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "app/app") || !strings.Contains(skipped[0], "failed calling webhook") {
		t.Fatalf("skipped = %v, want exactly the ConfigMap with the webhook error", skipped)
	}

	if _, n := findEntry(f.managedFields(cmPath), tfHelm311, update, "v1"); n != 1 {
		t.Fatal("the skipped ConfigMap must keep its helm_release entry for a later apply")
	}
	if _, n := findEntry(f.managedFields(crPath), "helm", update, "rbac.authorization.k8s.io/v1"); n != 1 {
		t.Fatal("the ClusterRole after the skipped object must still be handed over")
	}
}

// TestHandOverHelmProviderFieldManagers_OtherErrorsFail asserts any other
// patch failure (here RBAC) fails the hand-over, naming the object — the
// install that would follow cannot write the object either.
func TestHandOverHelmProviderFieldManagers_OtherErrorsFail(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	f, dyn := newFakeAPIServer(t)

	f.add(cmPath, testObject(t, "v1", "ConfigMap", "app", "app",
		mfEntry(tfHelm311, metav1.ManagedFieldsOperationUpdate, "v1", "", `{"f:data":{"f:FEATURE_X":{}}}`, ts),
	))

	f.beforePatch[cmPath] = func(map[string]any) (int, string) {
		return http.StatusForbidden, `configmaps "app" is forbidden: User "ci" cannot patch resource "configmaps"`
	}

	_, err := handOverHelmProviderFieldManagers(context.Background(), testMapper(), dyn, []ResourceRef{cmRef})
	if err == nil {
		t.Fatal("expected an error for a forbidden patch")
	}
	if !strings.Contains(err.Error(), "app/app") || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("error does not name the object and cause: %v", err)
	}
}

// TestHandOverHelmProviderFieldManagers_BoundedByContext asserts a hung API
// server cannot stall an apply past its timeout: the caller's deadline cuts
// the in-flight request.
func TestHandOverHelmProviderFieldManagers_BoundedByContext(t *testing.T) {
	f, dyn := newFakeAPIServer(t)
	f.blockGets = true

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := handOverHelmProviderFieldManagers(ctx, testMapper(), dyn, []ResourceRef{cmRef})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("hand-over not bounded by its context: took %s", elapsed)
	}
}
