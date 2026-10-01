package planconv

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/werf/nelm/pkg/resource"
)

// TestNormalize_SecretRedactedDespiteSensitiveFalse is the Phase D regression
// test for the werf.io/sensitive:"false" leak (finding #6): a core/v1 Secret
// carrying that opt-out annotation must STILL be redacted. Unlike nelm's
// ephemeral CLI diff, this provider persists the normalized resources map into
// durable terraform.tfstate and prints it in `terraform plan` output, so
// honoring an in-chart opt-out would write trivially-decodable base64
// credentials to durable state.
//
// The object is hand-built (not a captured-live golden) on purpose: this
// exercises redaction policy, not server-side-defaulting normalization.
func TestNormalize_SecretRedactedDespiteSensitiveFalse(t *testing.T) {
	const secretVal = "c3VwZXItc2VjcmV0LXBhc3N3b3Jk" // base64("super-secret-password")

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]interface{}{
			"name":        "db",
			"annotations": map[string]interface{}{"werf.io/sensitive": "false"},
		},
		"data": map[string]interface{}{"password": secretVal},
	}}

	got, err := NormalizeUnstructured(obj)
	if err != nil {
		t.Fatalf("NormalizeUnstructured: %v", err)
	}

	if strings.Contains(got, secretVal) {
		t.Fatalf("Secret data leaked in cleartext despite the redaction override: %s", got)
	}
	if strings.Contains(got, "super-secret") {
		t.Fatalf("decoded Secret data leaked: %s", got)
	}
	if !strings.Contains(got, "hidden") || !strings.Contains(got, "sensitive bytes") {
		t.Fatalf("expected a deterministic hidden-bytes placeholder, got: %s", got)
	}
}

// TestNormalize_SecretCustomSensitivePathsUnioned checks that a Secret's
// unconditional data/stringData redaction is UNIONED with (not replaced by) an
// explicit werf.io/sensitive-paths annotation: both the Secret data and the
// custom non-data path must be redacted. Regression guard for the re-review
// finding that the Secret short-circuit was bypassing GetSensitiveInfo's
// custom paths entirely.
func TestNormalize_SecretCustomSensitivePathsUnioned(t *testing.T) {
	const dataVal = "cGFzc3dvcmQ="              // base64("password")
	const tokenVal = "super-secret-token-value" // redacted via custom path

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]interface{}{
			"name": "db",
			"annotations": map[string]interface{}{
				"werf.io/sensitive-paths": "metadata.annotations.token",
				"token":                   tokenVal,
			},
		},
		"data": map[string]interface{}{"password": dataVal},
	}}

	got, err := NormalizeUnstructured(obj)
	if err != nil {
		t.Fatalf("NormalizeUnstructured: %v", err)
	}

	if strings.Contains(got, dataVal) {
		t.Fatalf("Secret data leaked despite the union redaction: %s", got)
	}
	if strings.Contains(got, tokenVal) {
		t.Fatalf("custom werf.io/sensitive-paths field leaked (union dropped the custom path): %s", got)
	}
}

// TestNormalize_MalformedSensitiveAnnotationsDoNotPanic guards the recover in
// NormalizeUnstructured: a LIVE object (which bypasses nelm's chart-time
// validation) carrying a malformed werf.io/sensitive or werf.io/sensitive-paths
// annotation must yield an error, not panic and crash the provider plugin.
func TestNormalize_MalformedSensitiveAnnotationsDoNotPanic(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"non-bool werf.io/sensitive": {"werf.io/sensitive": "definitely"},
		"malformed sensitive-paths":  {"werf.io/sensitive-paths": "[[[not-a-jsonpath"},
	}

	for name, annos := range cases {
		annos := annos
		t.Run(name, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "x", "annotations": annos},
				"data":       map[string]interface{}{"k": "v"},
			}}

			// Must not panic.
			_, err := NormalizeUnstructured(obj)
			if err == nil {
				t.Fatalf("expected an error for a malformed sensitivity annotation, got nil")
			}
			if !strings.Contains(err.Error(), "recovered from a panic") {
				t.Fatalf("expected a recovered-panic error, got: %v", err)
			}
		})
	}
}

// TestNormalize_RedactionIgnoresNelmFeatureGates is the planconv half of the
// feature-gate pinning fix: nelm's GetSensitiveInfo switches
// werf.io/sensitive: "true" and the default Secret answer from HideAll to
// data.*/stringData.* when NELM_FEAT_PREVIEW_V2 or NELM_FEAT_FIELD_SENSITIVE
// is "true". Passed straight through, that exposed an annotated CR's spec in
// cleartext and redacted Secret data twice (a placeholder of a placeholder),
// so the stored resources map depended on the machine running the plan.
// nelmclient.Init pins the gates; this checks sensitivePathsFor does not need
// it, in a test binary where nelm still reads the environment.
func TestNormalize_RedactionIgnoresNelmFeatureGates(t *testing.T) {
	const (
		secretData = "c3VwZXItc2VjcmV0" // base64("super-secret"), 16 bytes
		crPassword = "grafana-db-pass-XYZ"
		token      = "custom-path-token"
	)

	objs := map[string]*unstructured.Unstructured{
		"plain Secret": {Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]interface{}{"name": "db"},
			"data":       map[string]interface{}{"password": secretData},
		}},
		"Secret with sensitive-paths": {Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]interface{}{
				"name": "db",
				"annotations": map[string]interface{}{
					"werf.io/sensitive-paths": "metadata.annotations.token",
					"token":                   token,
				},
			},
			"data": map[string]interface{}{"password": secretData},
		}},
		"werf.io/sensitive CR": {Object: map[string]interface{}{
			"apiVersion": "grafana.integreatly.org/v1beta1",
			"kind":       "GrafanaDatasource",
			"metadata": map[string]interface{}{
				"name":        "pg",
				"annotations": map[string]interface{}{"werf.io/sensitive": "true"},
			},
			"spec": map[string]interface{}{
				"datasource": map[string]interface{}{
					"secureJsonData": map[string]interface{}{"password": crPassword},
				},
			},
		}},
		"ConfigMap with sensitive-paths": {Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":        "cfg",
				"annotations": map[string]interface{}{"werf.io/sensitive-paths": "data.token"},
			},
			"data": map[string]interface{}{"token": token, "plain": "visible"},
		}},
	}

	normalizeAll := func(t *testing.T) map[string]string {
		t.Helper()

		out := make(map[string]string, len(objs))

		for name, obj := range objs {
			got, err := NormalizeUnstructured(obj)
			if err != nil {
				t.Fatalf("NormalizeUnstructured(%s): %v", name, err)
			}

			out[name] = got
		}

		return out
	}

	t.Setenv("NELM_FEAT_PREVIEW_V2", "")
	t.Setenv("NELM_FEAT_FIELD_SENSITIVE", "")

	want := normalizeAll(t)

	for name, got := range want {
		for _, secret := range []string{secretData, crPassword, token} {
			if strings.Contains(got, secret) {
				t.Fatalf("%s: %q leaked with no feature gate set: %s", name, secret, got)
			}
		}
	}

	if !strings.Contains(want["plain Secret"], "hidden 16 sensitive bytes") {
		t.Fatalf("plain Secret: want a single-redaction placeholder of the 16-byte value, got: %s", want["plain Secret"])
	}

	for _, env := range []string{"NELM_FEAT_PREVIEW_V2", "NELM_FEAT_FIELD_SENSITIVE"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, "true")

			// Precondition: nothing in this test binary pins nelm's gates, so
			// nelm itself must now give its v2 answer — otherwise the
			// comparison below would prove nothing.
			if info := resource.GetSensitiveInfo(schema.GroupKind{Kind: "Secret"}, nil); info.FullySensitive() {
				t.Fatalf("precondition: %s=true did not change nelm's default Secret answer; are feature gates pinned in this test binary?", env)
			}

			got := normalizeAll(t)

			for name := range objs {
				if got[name] != want[name] {
					t.Errorf("%s: output depends on %s:\n got: %s\nwant: %s", name, env, got[name], want[name])
				}
			}
		})
	}
}
