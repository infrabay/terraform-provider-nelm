package planconv

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/werf/nelm/pkg/plan"
	"github.com/werf/nelm/pkg/resource"
	"github.com/werf/nelm/pkg/resource/spec"
)

// bracket is a readable ScrubString placeholder for the tests below.
func bracket(s string) string { return "[" + s + "]" }

func TestScrubString(t *testing.T) {
	cases := []struct {
		name    string
		s       string
		secrets []string
		want    string
	}{
		{"no secret", "plain text", []string{"hunter2"}, "plain text"},
		{"every occurrence", "a=hunter2 b=hunter2", []string{"hunter2"}, "a=[hunter2] b=[hunter2]"},
		// F45: the shorter value used to be replaced first when listed
		// first, leaving the rest of the longer one ("-replica-Kx9") behind.
		{"contained, shorter first", "dsn=S3cret-replica-Kx9;", []string{"S3cret", "S3cret-replica-Kx9"}, "dsn=[S3cret-replica-Kx9];"},
		{"contained, longer first", "dsn=S3cret-replica-Kx9;", []string{"S3cret-replica-Kx9", "S3cret"}, "dsn=[S3cret-replica-Kx9];"},
		{"partial overlap", "xxABCDEFyy", []string{"ABCD", "CDEF"}, "xx[ABCDEF]yy"},
		{"adjacent", "ABCDEFGH", []string{"ABCD", "EFGH"}, "[ABCDEFGH]"},
		{"overlapping occurrences", "aaaaa", []string{"aaaa"}, "[aaaaa]"},
		{"duplicated secret", "k=hunter2", []string{"hunter2", "hunter2"}, "k=[hunter2]"},
		{"empty secret ignored", "k=v", []string{""}, "k=v"},
		// A placeholder is never searched itself: the second secret occurs
		// in the first one's placeholder, not in s.
		{"placeholder not rescanned", "k=secret-value", []string{"secret-value", "["}, "k=[secret-value]"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ScrubString(tc.s, tc.secrets, bracket); got != tc.want {
				t.Errorf("ScrubString(%q, %q) = %q, want %q", tc.s, tc.secrets, got, tc.want)
			}
		})
	}
}

// TestSecretPlaceholderMatchesNelm pins the placeholder to nelm's own Secret
// redaction format, which the docs describe for both.
func TestSecretPlaceholderMatchesNelm(t *testing.T) {
	const value = "postgres://app:P4ss@10.0.0.5/db"

	redacted := resource.RedactSensitiveData(&unstructured.Unstructured{Object: map[string]interface{}{
		"data": map[string]interface{}{"x": value},
	}}, []string{"data.x"})

	want, _, _ := unstructured.NestedString(redacted.Object, "data", "x")
	if got := secretPlaceholder(value); got != want {
		t.Fatalf("secretPlaceholder = %q, nelm redacts the same value to %q", got, want)
	}
}

func TestScrubSecrets(t *testing.T) {
	const secret = `p&ss<"word">`

	obj := map[string]interface{}{
		"data": map[string]interface{}{
			"url":         "https://user:" + secret + "@host",
			secret + ".k": "key",
		},
		"args":     []interface{}{"--password=" + secret, int64(42), true, nil},
		"short":    "on",
		"replicas": int64(3),
	}
	orig := deepCopyJSON(obj)

	got := ScrubSecrets(obj, []string{secret, "on"})

	if !reflect.DeepEqual(obj, orig) {
		t.Fatalf("ScrubSecrets modified its input: %v", obj)
	}

	ph := secretPlaceholder(secret)
	want := map[string]interface{}{
		"data": map[string]interface{}{
			"url":     "https://user:" + ph + "@host",
			ph + ".k": "key",
		},
		"args": []interface{}{"--password=" + ph, int64(42), true, nil},
		// Shorter than MinSecretLength: left alone.
		"short":    "on",
		"replicas": int64(3),
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ScrubSecrets =\n%v\nwant\n%v", got, want)
	}

	if out := ScrubSecrets(obj, nil); !reflect.DeepEqual(out, obj) {
		t.Fatalf("ScrubSecrets without secrets changed the object: %v", out)
	}
}

func deepCopyJSON(m map[string]interface{}) map[string]interface{} {
	return (&unstructured.Unstructured{Object: m}).DeepCopy().Object
}

// sensitiveChart renders a set_sensitive value (secret) into non-Secret
// objects the way charts do: a container env value, ConfigMap data — under a
// key that embeds it too — and a CronJob command-line flag.
func sensitiveChart(secret string) []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		{Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]interface{}{"name": "api"},
			"spec": map[string]interface{}{
				"template": map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{
					map[string]interface{}{
						"name":  "api",
						"image": "api:1.0.0",
						"env":   []interface{}{map[string]interface{}{"name": "DATABASE_URL", "value": secret}},
					},
				}}},
			},
		}},
		{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "api"},
			"data": map[string]interface{}{
				"config.yaml":   "db:\n  url: " + secret + "\n",
				secret + ".pem": "cert",
			},
		}},
		{Object: map[string]interface{}{
			"apiVersion": "batch/v1",
			"kind":       "CronJob",
			"metadata":   map[string]interface{}{"name": "backup"},
			"spec": map[string]interface{}{"jobTemplate": map[string]interface{}{"spec": map[string]interface{}{
				"template": map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{
					map[string]interface{}{"name": "backup", "args": []interface{}{"--url=" + secret}},
				}}},
			}}},
		}},
	}
}

// sensitiveScoper keys sensitiveChart's kinds.
func sensitiveScoper() fakeScoper {
	return fakeScoper{namespaced: map[schema.GroupVersionKind]bool{
		gvkDeployment: true,
		gvkConfigMap:  true,
		{Group: "batch", Version: "v1", Kind: "CronJob"}: true,
	}}
}

// asLive is obj as the API server returns it: namespace, runtime metadata,
// ownership metadata and server-side defaulting added.
func asLive(obj *unstructured.Unstructured) *unstructured.Unstructured {
	live := installed(obj)
	live.SetUID("0b6c1d2e")
	live.SetResourceVersion("4242")
	live.Object["status"] = map[string]interface{}{"observedGeneration": int64(1)}

	if obj.GetKind() == "Deployment" {
		_ = unstructured.SetNestedField(live.Object, int64(600), "spec", "progressDeadlineSeconds")
		_ = unstructured.SetNestedField(live.Object, "ClusterFirst", "spec", "template", "spec", "dnsPolicy")
	}

	return live
}

// TestBuildResources_ScrubsSecretsOnBothSides is the F06 regression test: a
// set_sensitive value a chart renders into a non-Secret object went into the
// planned "resources" values (ModifyPlan's render) and the live ones (Read)
// verbatim, so every plan printed it and state stored it. Both sides now
// carry the deterministic placeholder instead, and still compare equal when
// nothing changed (a scrubbed map key included), while a rotated value still
// shows as a change.
func TestBuildResources_ScrubsSecretsOnBothSides(t *testing.T) {
	const secret = "postgres://app:P4ss@10.0.0.5/db"

	secrets := []string{secret}
	scoper := sensitiveScoper()

	rendered, err := BuildRenderedResources(sensitiveChart(secret), "ns", scoper, secrets)
	if err != nil {
		t.Fatalf("BuildRenderedResources: %v", err)
	}

	var live []*unstructured.Unstructured
	for _, obj := range sensitiveChart(secret) {
		live = append(live, asLive(obj))
	}

	refreshed, err := BuildLiveResources(live, "ns", scoper, rendered, secrets)
	if err != nil {
		t.Fatalf("BuildLiveResources: %v", err)
	}

	if len(rendered) != 3 {
		t.Fatalf("rendered = %v, want 3 objects", rendered)
	}

	// encoding/json writes the placeholder's angle brackets as \u003c/\u003e.
	placeholder := strings.Trim(secretPlaceholder(secret), "<>")

	for key, value := range rendered {
		for side, v := range map[string]string{"planned": value, "live": refreshed[key]} {
			if strings.Contains(v, "P4ss") {
				t.Errorf("%s %s carries the set_sensitive value in cleartext: %s", side, key, v)
			}

			if !strings.Contains(v, placeholder) {
				t.Errorf("%s %s lacks the placeholder %q: %s", side, key, placeholder, v)
			}
		}

		if refreshed[key] != value {
			t.Errorf("%s: live and planned differ although nothing changed (phantom diff):\nlive:    %s\nplanned: %s", key, refreshed[key], value)
		}
	}

	rotated, err := BuildRenderedResources(sensitiveChart(secret+"-v2"), "ns", scoper, []string{secret + "-v2"})
	if err != nil {
		t.Fatalf("BuildRenderedResources(rotated): %v", err)
	}

	for key, value := range rotated {
		if value == rendered[key] {
			t.Errorf("%s: a rotated set_sensitive value must still show as a change", key)
		}
	}
}

// TestBuildPlannedResources_ScrubsFallbacks: the arms that normalize nelm's
// After themselves (no rendered entry) scrub it too.
func TestBuildPlannedResources_ScrubsFallbacks(t *testing.T) {
	const secret = "sk_live_TOPSECRET_123"

	cm := sensitiveChart(secret)[1]
	prior, err := NormalizeUnstructured(cm, []string{secret})
	if err != nil {
		t.Fatalf("NormalizeUnstructured: %v", err)
	}

	for _, typ := range []string{"create", "update"} {
		t.Run(typ, func(t *testing.T) {
			change := &plan.ResourceChange{
				Type:         typ,
				ResourceMeta: &spec.ResourceMeta{Name: "api", GroupVersionKind: gvkConfigMap},
				Before:       installed(cm),
				After:        installed(cm),
			}

			out, _, err := BuildPlannedResources(map[string]string{"v1/ConfigMap/ns/api": prior}, []*plan.ResourceChange{change}, "ns", sensitiveScoper(), nil, []string{secret})
			if err != nil {
				t.Fatalf("BuildPlannedResources: %v", err)
			}

			if v := out["v1/ConfigMap/ns/api"]; strings.Contains(v, "TOPSECRET") || v != prior {
				t.Errorf("planned = %s, want the scrubbed %s", v, prior)
			}
		})
	}
}
