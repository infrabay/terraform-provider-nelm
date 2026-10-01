package nelmclient

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// NormalizeChartRef resolves the resource's chart/repository pair into the
// chart reference and chart repository URL handed to nelm (Chart and
// ChartRepoConnectionOptions.ChartRepoURL), converting local chart references
// into absolute paths before they ever reach nelm, mirroring nelm's own
// local-vs-remote chart classification (pkg/chart/chart_render.go
// isLocalChart: filepath.IsAbs(path) || HasPrefix(path, "..") ||
// HasPrefix(path, ".")). The result is idempotent: feeding (chartRef,
// repoURL) back in returns them unchanged, so toReleaseSpec and the Client
// methods can both normalize.
//
// repository is the resource's `repository` attribute. A non-empty repository
// makes the reference UNAMBIGUOUSLY REMOTE (a chart name to resolve against
// that repo), so the ref passes through untouched — critically, WITHOUT the
// exists-on-disk localization below. Otherwise a same-named directory in the
// Terraform process's working directory (a nested module, a vendored copy)
// would silently hijack the reference into a local absolute path, nelm would
// classify it local and never consult the repository, and the wrong chart —
// or an attacker-placed directory — would be planned and installed with no
// diagnostic. A local-path-shaped chart (absolute, "./", "../") or a full
// oci:// chart combined with a repository is contradictory and rejected
// outright.
//
// An oci:// repository is helm_release's OCI form (repository =
// "oci://host/path", chart = "name"). nelm treats any ChartRepoURL as a
// classic index.yaml repository — for oci:// that becomes an OCI pull of
// "host/path/index.yaml", failing with "... is not a valid chart repository
// or cannot be reached: object required" — so the pair is folded into the
// single reference "oci://host/path/name" with an empty repository URL,
// exactly as hashicorp/helm does (resource_helm_release.go, registry.IsOCI).
//
// With no repository set: a bare relative directory name (no leading "./" or
// "../") is classified REMOTE by nelm even when a directory of that name
// exists in the current working directory — that classification would
// silently change out from under a long-lived provider process whose cwd is
// not the chart's original directory (testdata/errors/bad_chart_ref.txt /
// remote_chart_no_featgate.txt capture nelm's resulting errors when this
// happens unintentionally). To avoid ever handing nelm an ambiguous bare
// relative name that was actually meant to be a local chart, this function
// absolutizes any relative reference that currently resolves to something on
// disk. Anything else (oci://..., repo/chart, a relative path that does not
// exist) passes through untouched as a remote reference.
func NormalizeChartRef(chart, repository string) (chartRef, repoURL string, err error) {
	if chart == "" {
		return "", "", fmt.Errorf("chart reference must not be empty")
	}

	if repository != "" {
		if filepath.IsAbs(chart) || isExplicitRelative(chart) {
			return "", "", fmt.Errorf(
				"chart %q is a local path but repository %q is set; a repository makes the chart a remote name — remove one of them",
				chart, repository,
			)
		}

		if isOCI(chart) {
			return "", "", fmt.Errorf(
				"chart %q is a full oci:// reference but repository %q is set; remove repository, or set chart to the bare chart name",
				chart, repository,
			)
		}

		if isOCI(repository) {
			ref, err := joinOCIRef(repository, chart)
			if err != nil {
				return "", "", err
			}

			return ref, "", nil
		}

		return chart, repository, nil
	}

	if filepath.IsAbs(chart) {
		return chart, "", nil
	}

	if isExplicitRelative(chart) {
		abs, err := absolutize(chart)
		return abs, "", err
	}

	if _, err := os.Stat(chart); err == nil {
		abs, err := absolutize(chart)
		return abs, "", err
	}

	// Not local by any signal (bare relative name that doesn't exist on
	// disk, oci:// URL, repo/chart reference, ...): pass through as-is. Nelm
	// classifies this as remote (chart_render.go isLocalChart); never
	// absolutize it, or a legitimate remote ref would be mangled into a
	// nonexistent local path.
	return chart, "", nil
}

// isExplicitRelative reports whether chart is unambiguously a relative
// filesystem reference by nelm's own convention: it begins with "./" or
// "../", or is exactly "." or "..".
func isExplicitRelative(chart string) bool {
	return chart == "." || chart == ".." ||
		strings.HasPrefix(chart, "./") || strings.HasPrefix(chart, "../")
}

// isOCI reports whether ref uses the oci:// scheme (case-insensitively, like
// url.Parse's scheme handling).
func isOCI(ref string) bool {
	return strings.HasPrefix(strings.ToLower(ref), "oci://")
}

// joinOCIRef appends chart to an oci:// repository URL's path, e.g.
// ("oci://host/path/", "app") -> "oci://host/path/app". url.Parse lowercases
// the scheme, so a mixed-case "OCI://" repository still yields the canonical
// "oci://" reference nelm classifies as remote.
func joinOCIRef(repository, chart string) (string, error) {
	u, err := url.Parse(repository)
	if err != nil {
		return "", fmt.Errorf("parse oci:// repository %q: %w", repository, err)
	}

	if u.Host == "" {
		return "", fmt.Errorf("oci:// repository %q has no registry host", repository)
	}

	u.Path = path.Join(u.Path, chart)

	return u.String(), nil
}

func absolutize(chart string) (string, error) {
	abs, err := filepath.Abs(chart)
	if err != nil {
		return "", fmt.Errorf("absolutize local chart reference %q: %w", chart, err)
	}

	return abs, nil
}
