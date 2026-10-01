//go:build smoke

package smokelib

import (
	"path/filepath"
	"runtime"
)

// RepoRoot returns the absolute path to the terraform-provider-nelm repo
// root, computed from this source file's own location (runtime.Caller)
// rather than the process's current working directory — so every smoke
// program behaves identically whether invoked via `go run
// -tags smoke ./scripts/smoke/<prog>` from the repo root or from elsewhere.
// This file lives at <repoRoot>/scripts/smoke/smokelib/paths.go.
func RepoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("smokelib.RepoRoot: runtime.Caller failed to resolve source location")
	}

	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

// BasicChartPath returns the absolute path to testdata/charts/basic — an
// ABSOLUTE path, never a bare relative one, matching the provider's chart
// reference rule (internal/nelmclient/chartref.go: a bare relative chart name
// must NEVER reach nelm un-absolutized).
func BasicChartPath() string {
	return filepath.Join(RepoRoot(), "testdata", "charts", "basic")
}

// FixtureDir returns <repoRoot>/internal/planconv/testdata/<sub>.
func FixtureDir(sub string) string {
	return filepath.Join(RepoRoot(), "internal", "planconv", "testdata", sub)
}

// NelmclientFixtureDir returns <repoRoot>/internal/nelmclient/testdata/<sub>.
func NelmclientFixtureDir(sub string) string {
	return filepath.Join(RepoRoot(), "internal", "nelmclient", "testdata", sub)
}
