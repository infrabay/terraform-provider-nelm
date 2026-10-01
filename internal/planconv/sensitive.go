package planconv

import (
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/werf/nelm/pkg/common"
	"github.com/werf/nelm/pkg/resource"
	"github.com/werf/nelm/pkg/resource/spec"
)

// sensitivePathsFor returns the jsonpath-style sensitive paths that
// NormalizeUnstructured must redact for a resource of the given
// GroupVersionKind carrying the given annotations.
//
// It is a thin, pure wrapper around nelm's own resource.GetSensitiveInfo
// (which honors the werf.io/sensitive / werf.io/sensitive-paths annotations
// and nelm's default "Secrets are sensitive" rule), with three deliberate
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
//     but a werf.io/sensitive=true on any kind without werf.io/sensitive-paths
//     produces one) keep the HideAll skeleton: it is the safe default for a
//     kind with no known field layout, and the resulting NormalizeUnstructured
//     output is still deterministic and free of cleartext.
//
//  3. The answer never depends on nelm's feature gates. GetSensitiveInfo's
//     werf.io/sensitive=true and default-Secret answers flip from HideAll to
//     data.*/stringData.* under the field-sensitive / preview-v2 gates, which
//     would expose a sensitive CR's spec and double-redact Secret data.
//     nelmclient.Init pins those gates off; this function does not rely on
//     it: only paths from an explicit werf.io/sensitive-paths annotation
//     (whose handling no gate changes) are taken from nelm verbatim.
func sensitivePathsFor(gvk schema.GroupVersionKind, annotations map[string]string) []string {
	info := resource.GetSensitiveInfo(gvk.GroupKind(), annotations)
	custom := hasSensitivePathsAnnotation(annotations)

	// Core/v1 Secret: always redact data/stringData, regardless of any
	// werf.io/sensitive opt-out annotation. Guarded on the empty (core) group
	// so a custom CRD that merely happens to be named "Secret" is not swept in.
	if gvk.Group == "" && gvk.Kind == "Secret" {
		paths := []string{"data.*", "stringData.*"}

		// Also honor an explicit werf.io/sensitive-paths annotation IN ADDITION
		// (never the HideAll skeleton — we keep field-level visibility). Only
		// that annotation's paths are unioned: nelm's own default answer for a
		// Secret is either HideAll or, under a v2 gate, data.*/stringData.*
		// again, which would redact the placeholders a second time. Without
		// this a Secret that used werf.io/sensitive-paths to redact a non-data
		// field (e.g. an annotation) would leak that field.
		if custom {
			paths = append(paths, info.SensitivePaths...)
		}

		return paths
	}

	if !info.IsSensitive {
		return nil
	}

	if custom {
		return info.SensitivePaths
	}

	// werf.io/sensitive: "true" without werf.io/sensitive-paths: the v1
	// HideAll answer, whatever nelm's gates would make of it.
	return []string{resource.HideAll}
}

// hasSensitivePathsAnnotation reports whether annotations carry a
// werf.io/sensitive-paths annotation that GetSensitiveInfo answers from (the
// same lookup and non-empty-parse test nelm applies, so the two cannot
// disagree about which branch produced the answer).
func hasSensitivePathsAnnotation(annotations map[string]string) bool {
	_, value, found := spec.FindAnnotationOrLabelByKeyPattern(annotations, common.AnnotationKeyPatternSensitivePaths)

	return found && len(resource.ParseSensitivePaths(value)) > 0
}
