package nelmclient

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// NormalizeChartRef converts local chart references into absolute paths
// before they ever reach nelm, mirroring nelm's own local-vs-remote chart
// classification (pkg/chart/chart_render.go isLocalChart:
// filepath.IsAbs(path) || HasPrefix(path, "..") || HasPrefix(path, ".")).
//
// repository is the resource's `repository` attribute. A non-empty repository
// makes the reference UNAMBIGUOUSLY REMOTE (a chart name to resolve against
// that repo), so the ref passes through untouched — critically, WITHOUT the
// exists-on-disk localization below. Otherwise a same-named directory in the
// Terraform process's working directory (a nested module, a vendored copy)
// would silently hijack the reference into a local absolute path, nelm would
// classify it local and never consult the repository, and the wrong chart —
// or an attacker-placed directory — would be planned and installed with no
// diagnostic. A local-path-shaped chart (absolute, "./", "../") combined with
// a repository is contradictory and rejected outright.
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
func NormalizeChartRef(chart, repository string) (string, error) {
	if chart == "" {
		return "", fmt.Errorf("chart reference must not be empty")
	}

	if repository != "" {
		if filepath.IsAbs(chart) || isExplicitRelative(chart) {
			return "", fmt.Errorf(
				"chart %q is a local path but repository %q is set; a repository makes the chart a remote name — remove one of them",
				chart, repository,
			)
		}

		return chart, nil
	}

	if filepath.IsAbs(chart) {
		return chart, nil
	}

	if isExplicitRelative(chart) {
		return absolutize(chart)
	}

	if _, err := os.Stat(chart); err == nil {
		return absolutize(chart)
	}

	// Not local by any signal (bare relative name that doesn't exist on
	// disk, oci:// URL, repo/chart reference, ...): pass through as-is. Nelm
	// classifies this as remote (chart_render.go isLocalChart); never
	// absolutize it, or a legitimate remote ref would be mangled into a
	// nonexistent local path.
	return chart, nil
}

// isExplicitRelative reports whether chart is unambiguously a relative
// filesystem reference by nelm's own convention: it begins with "./" or
// "../", or is exactly "." or "..".
func isExplicitRelative(chart string) bool {
	return chart == "." || chart == ".." ||
		strings.HasPrefix(chart, "./") || strings.HasPrefix(chart, "../")
}

func absolutize(chart string) (string, error) {
	abs, err := filepath.Abs(chart)
	if err != nil {
		return "", fmt.Errorf("absolutize local chart reference %q: %w", chart, err)
	}

	return abs, nil
}
