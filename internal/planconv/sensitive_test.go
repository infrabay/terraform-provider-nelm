package planconv

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

	got, err := NormalizeUnstructured(obj, nil)
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

	got, err := NormalizeUnstructured(obj, nil)
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
			_, err := NormalizeUnstructured(obj, nil)
			if err == nil {
				t.Fatalf("expected an error for a malformed sensitivity annotation, got nil")
			}
			if !strings.Contains(err.Error(), "recovered from a panic") {
				t.Fatalf("expected a recovered-panic error, got: %v", err)
			}
		})
	}
}
