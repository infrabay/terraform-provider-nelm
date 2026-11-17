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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// TestHandOverHelmProviderEntries is the regression test for the rename
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

	// failGets, if set for a path, fails that many of its next GETs with the
	// 500 kube-apiserver writes when its authorizer fails
	// (writeAuthorizerErr).
	failGets map[string]int
}

func newFakeAPIServer(t *testing.T) (*fakeAPIServer, dynamic.Interface) {
	t.Helper()

	f := &fakeAPIServer{
		t:           t,
		objects:     map[string]map[string]any{},
		patches:     map[string]int{},
		beforePatch: map[string]func(map[string]any) (int, string){},
		failGets:    map[string]int{},
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
		if n := f.failGets[r.URL.Path]; n > 0 {
			f.failGets[r.URL.Path] = n - 1
			writeAuthorizerErr(w, r)

			return
		}

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

		mf := patch.Metadata["managedFields"]
		// Like a real API server, [{}] clears the list.
		if l, ok := mf.([]any); ok && len(l) == 1 {
			if e, ok := l[0].(map[string]any); ok && len(e) == 0 {
				mf = []any{}
			}
		}

		metadata["managedFields"] = mf
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
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"}, meta.RESTScopeNamespace)
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

// TestHandOverHelmProviderFieldManagers covers the cluster side of the
// helm_release field-manager hand-over against a fake API server: only objects
// carrying a helm_release entry are patched (namespaced and cluster-scoped
// alike), unrelated managers survive, missing objects and unserved kinds are
// skipped, and a second run is a no-op.
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

	skipped, err := handOverHelmProviderFieldManagers(context.Background(), testMapper(), dyn, refs, nil)
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
	if _, err := handOverHelmProviderFieldManagers(context.Background(), testMapper(), dyn, refs, nil); err != nil {
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

	skipped, err := handOverHelmProviderFieldManagers(context.Background(), testMapper(), dyn, []ResourceRef{cmRef}, nil)
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

// TestHandOverHelmProviderFieldManagers_TransientGetRetried asserts the
// hand-over's GET is retried like LiveObjects' (getLive): a single 500 from
// GKE's authorizer must not fail the apply before the install runs.
func TestHandOverHelmProviderFieldManagers_TransientGetRetried(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	update := metav1.ManagedFieldsOperationUpdate

	setLiveGetRetryDelay(t, time.Millisecond)

	f, dyn := newFakeAPIServer(t)

	f.add(cmPath, testObject(t, "v1", "ConfigMap", "app", "app",
		mfEntry(tfHelm311, update, "v1", "", `{"f:data":{"f:FEATURE_X":{}}}`, ts),
	))
	f.failGets[cmPath] = 1

	skipped, err := handOverHelmProviderFieldManagers(context.Background(), testMapper(), dyn, []ResourceRef{cmRef}, nil)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("skipped=%v err=%v, want neither", skipped, err)
	}

	f.mu.Lock()
	left := f.failGets[cmPath]
	f.mu.Unlock()

	if left != 0 {
		t.Fatalf("%d injected GET failure(s) never reached the server", left)
	}
	if got := f.patchCount(cmPath); got != 1 {
		t.Errorf("PATCH attempts = %d, want 1", got)
	}
	if _, n := findEntry(f.managedFields(cmPath), "helm", update, "v1"); n != 1 {
		t.Fatalf(`want a "helm"/Update entry after the retried GET: %+v`, f.managedFields(cmPath))
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

	skipped, err := handOverHelmProviderFieldManagers(context.Background(), testMapper(), dyn, []ResourceRef{cmRef, crRef}, nil)
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

	_, err := handOverHelmProviderFieldManagers(context.Background(), testMapper(), dyn, []ResourceRef{cmRef}, nil)
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
	_, err := handOverHelmProviderFieldManagers(ctx, testMapper(), dyn, []ResourceRef{cmRef}, nil)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("hand-over not bounded by its context: took %s", elapsed)
	}
}

// TestIsIdentityOnly pins which manifests leave a server-side apply nothing to
// own: identity fields only, empty values counting as unset.
func TestIsIdentityOnly(t *testing.T) {
	sa := func(extra map[string]any, metadata map[string]any) map[string]any {
		md := map[string]any{"name": "exporter", "namespace": "monitoring"}
		for k, v := range metadata {
			md[k] = v
		}

		obj := map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": md}
		for k, v := range extra {
			obj[k] = v
		}

		return obj
	}

	for _, tc := range []struct {
		name string
		obj  map[string]any
		want bool
	}{
		{"name and namespace only (cadvisor chart 2.3.3)", sa(nil, nil), true},
		{"no namespace", map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": "exporter"}}, true},
		{"empty labels, annotations and secrets", sa(map[string]any{"secrets": []any{}}, map[string]any{"labels": map[string]any{}, "annotations": nil}), true},
		{"a label", sa(nil, map[string]any{"labels": map[string]any{"app": "exporter"}}), false},
		{"an annotation", sa(nil, map[string]any{"annotations": map[string]any{"a": "b"}}), false},
		{"automountServiceAccountToken false", sa(map[string]any{"automountServiceAccountToken": false}, nil), false},
		{"configmap data", map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c"}, "data": map[string]any{"k": "v"}}, false},
		{"configmap with empty data", map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c"}, "data": map[string]any{}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isIdentityOnly(tc.obj); got != tc.want {
				t.Errorf("isIdentityOnly() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDropHelmUpdateEntries asserts only the main-resource Update entries of
// the Helm managers go; nelm's Apply entry, subresource entries and other
// managers stay.
func TestDropHelmUpdateEntries(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	update, apply := metav1.ManagedFieldsOperationUpdate, metav1.ManagedFieldsOperationApply

	entries := []metav1.ManagedFieldsEntry{
		mfEntry(tfHelm311, update, "v1", "", `{"f:metadata":{"f:labels":{}}}`, ts),
		mfEntry("helm", update, "v1", "", `{"f:metadata":{"f:annotations":{}}}`, ts),
		mfEntry("helm", apply, "v1", "", `{}`, ts),
		mfEntry(tfHelm302, update, "v1", "status", `{"f:status":{}}`, ts),
		mfEntry("kube-controller-manager", update, "v1", "", `{"f:secrets":{}}`, ts),
	}

	out, changed := dropHelmUpdateEntries(entries)
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if len(out) != 3 {
		t.Fatalf("got %d entries, want 3 (helm/Apply, the status entry, kube-controller-manager): %+v", len(out), out)
	}
	if _, n := findEntry(out, "helm", update, "v1"); n != 0 {
		t.Errorf(`"helm"/Update not dropped: %+v`, out)
	}
	if _, n := findEntry(out, "helm", apply, "v1"); n != 1 {
		t.Errorf(`"helm"/Apply must stay: %+v`, out)
	}
	if len(entries) != 5 {
		t.Errorf("input modified: %+v", entries)
	}

	kept := []metav1.ManagedFieldsEntry{mfEntry("kube-controller-manager", update, "v1", "", `{}`, ts)}
	if _, changed := dropHelmUpdateEntries(kept); changed {
		t.Error("changed = true with no Helm entry")
	}
}

// TestHandOverHelmProviderFieldManagers_IdentityOnly is the regression test for
// nelm failing the first install after the hand-over with "find apply managed
// fields entry in dry-run result" on an object whose manifest is identity only
// (cadvisor chart 2.3.3's ServiceAccount): such an object loses its Helm
// Update entries instead of having them renamed to "helm"/Update, the list is
// cleared when nothing else is left, other managers survive, and an ordinary
// object of the same release is still renamed.
func TestHandOverHelmProviderFieldManagers_IdentityOnly(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	update := metav1.ManagedFieldsOperationUpdate

	const (
		saPath      = "/api/v1/namespaces/app/serviceaccounts/exporter"
		sharedPath  = "/api/v1/namespaces/app/serviceaccounts/shared"
		helmCLIPath = "/api/v1/namespaces/app/serviceaccounts/cli"
	)

	saRef := ResourceRef{Version: "v1", Kind: "ServiceAccount", Namespace: "app", Name: "exporter"}
	sharedRef := ResourceRef{Version: "v1", Kind: "ServiceAccount", Namespace: "app", Name: "shared"}
	helmCLIRef := ResourceRef{Version: "v1", Kind: "ServiceAccount", Namespace: "app", Name: "cli"}

	f, dyn := newFakeAPIServer(t)

	f.add(saPath, testObject(t, "v1", "ServiceAccount", "app", "exporter",
		mfEntry(tfHelm311, update, "v1", "", `{"f:metadata":{"f:labels":{"f:app.kubernetes.io/managed-by":{}}}}`, ts),
	))
	f.add(sharedPath, testObject(t, "v1", "ServiceAccount", "app", "shared",
		mfEntry(tfHelm311, update, "v1", "", `{"f:metadata":{"f:labels":{}}}`, ts),
		mfEntry("kube-controller-manager", update, "v1", "", `{"f:secrets":{}}`, ts),
	))
	f.add(helmCLIPath, testObject(t, "v1", "ServiceAccount", "app", "cli",
		mfEntry("helm", update, "v1", "", `{"f:metadata":{"f:labels":{}}}`, ts),
	))
	f.add(cmPath, testObject(t, "v1", "ConfigMap", "app", "app",
		mfEntry(tfHelm311, update, "v1", "", `{"f:data":{"f:A":{}}}`, ts),
	))

	manifest := func(name, namespace string) *unstructured.Unstructured {
		md := map[string]any{"name": name}
		if namespace != "" {
			md["namespace"] = namespace
		}

		return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": md}}
	}

	identityOnly := newIdentityOnlyObjects([]*unstructured.Unstructured{
		manifest("exporter", "app"),
		manifest("shared", ""),
		manifest("cli", "app"),
		{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "app"}, "data": map[string]any{"A": "1"}}},
	})

	refs := []ResourceRef{saRef, sharedRef, helmCLIRef, cmRef}

	if _, err := handOverHelmProviderFieldManagers(context.Background(), testMapper(), dyn, refs, identityOnly); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mf := f.managedFields(saPath); len(mf) != 0 {
		t.Errorf("identity-only ServiceAccount: managedFields = %+v, want cleared", mf)
	}
	if mf := f.managedFields(helmCLIPath); len(mf) != 0 {
		t.Errorf(`identity-only ServiceAccount with a Helm CLI "helm"/Update entry: managedFields = %+v, want cleared`, mf)
	}

	shared := f.managedFields(sharedPath)
	if len(shared) != 1 || shared[0].Manager != "kube-controller-manager" {
		t.Errorf("identity-only ServiceAccount with another manager: managedFields = %+v, want only kube-controller-manager", shared)
	}

	if _, n := findEntry(f.managedFields(cmPath), "helm", update, "v1"); n != 1 {
		t.Errorf(`ordinary ConfigMap: want its entry renamed to "helm"/Update: %+v`, f.managedFields(cmPath))
	}

	// Idempotent: nothing left to drop or rename.
	before := f.patchCount(saPath) + f.patchCount(sharedPath) + f.patchCount(helmCLIPath) + f.patchCount(cmPath)
	if _, err := handOverHelmProviderFieldManagers(context.Background(), testMapper(), dyn, refs, identityOnly); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if after := f.patchCount(saPath) + f.patchCount(sharedPath) + f.patchCount(helmCLIPath) + f.patchCount(cmPath); after != before {
		t.Errorf("second run patched again: %d patches, want %d", after, before)
	}
}
