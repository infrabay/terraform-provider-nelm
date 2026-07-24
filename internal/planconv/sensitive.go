package planconv

import (
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/werf/nelm/pkg/resource"
)

// sensitivePathsFor returns the jsonpath-style sensitive paths that
// NormalizeUnstructured must redact for a resource of the given
// GroupVersionKind carrying the given annotations.
//
// It is a thin, pure wrapper around nelm's own resource.GetSensitiveInfo
// (which honors the werf.io/sensitive / werf.io/sensitive-paths annotations
// and nelm's default "Secrets are sensitive" rule), with two deliberate
// overrides:
//
//  1. A core/v1 Secret's data/stringData is redacted UNCONDITIONALLY — even
//     when a (possibly third-party) chart sets werf.io/sensitive: "false" to
//     opt out of nelm's own redaction. nelm's CLI diff is ephemeral, but this
//     provider persists the resources map durably into terraform.tfstate and
//     prints it in `terraform plan` output/CI logs, so honoring an in-chart
//     opt-out would write trivially-decodable base64 credentials to durable
//     state. A Secret's data is sensitive by definition; the opt-out is
//     ignored here on purpose. This also subsumes nelm's default-Secret case
//     below (whose HideAll skeleton we would otherwise have to override):
//     returning explicit paths routes them through RedactSensitiveData to
//     deterministic "<hidden N sensitive bytes, hash sha256[:12]>"
//     placeholders, so a changed key/value still shows as drift, nothing
//     leaks, and the collapse-to-skeleton HideAll shape (which would hide a
//     changed key alongside a changed value) is avoided.
//
//  2. Non-Secret FullySensitive kinds (there are none built into nelm today,
//     but a future custom werf.io/sensitive=true on any kind without
//     werf.io/sensitive-paths would produce one) keep GetSensitiveInfo's
//     HideAll skeleton: it is the safe default for a kind with no known field
//     layout, and the resulting NormalizeUnstructured output is still
//     deterministic and free of cleartext.
func sensitivePathsFor(gvk schema.GroupVersionKind, annotations map[string]string) []string {
	info := resource.GetSensitiveInfo(gvk.GroupKind(), annotations)

	// Core/v1 Secret: always redact data/stringData, regardless of any
	// werf.io/sensitive opt-out annotation. Guarded on the empty (core) group
	// so a custom CRD that merely happens to be named "Secret" is not swept in.
	if gvk.Group == "" && gvk.Kind == "Secret" {
		paths := []string{"data.*", "stringData.*"}

		// Also honor an explicit werf.io/sensitive-paths annotation IN ADDITION
		// (never the HideAll skeleton — we keep field-level visibility). nelm's
		// GetSensitiveInfo returns custom, non-HideAll paths only when that
		// annotation is present; its default/opt-in Secret answer is HideAll,
		// which FullySensitive() detects and we skip. Without this a Secret that
		// used werf.io/sensitive-paths to redact a non-data field (e.g. an
		// annotation) would leak that field.
		if info.IsSensitive && !info.FullySensitive() {
			paths = append(paths, info.SensitivePaths...)
		}

		return paths
	}

	if !info.IsSensitive {
		return nil
	}

	return info.SensitivePaths
}
