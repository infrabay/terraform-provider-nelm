package nelmclient

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// loadFixtureErr reads a captured-live error-shape fixture (see
// testdata/errors/README, T-fixtures) and wraps its full combined-output text
// as an error, the same shape IsClusterUnreachable/IsReleaseNotFound would
// see via err.Error() in production (nelm actions return errors built with
// %w around their own descriptive messages; the fixtures capture the fully
// rendered chain).
func loadFixtureErr(t *testing.T, name string) error {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("testdata", "errors", name))
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}

	return errors.New(string(b))
}

func TestIsClusterUnreachable(t *testing.T) {
	tests := []struct {
		fixture string
		want    bool
	}{
		{"unreachable_cluster.txt", true},
		{"bad_chart_ref.txt", false},
		{"remote_chart_no_featgate.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			err := loadFixtureErr(t, tt.fixture)

			if got := IsClusterUnreachable(err); got != tt.want {
				t.Fatalf("IsClusterUnreachable(%s) = %v, want %v", tt.fixture, got, tt.want)
			}
		})
	}
}

func TestIsClusterUnreachable_Nil(t *testing.T) {
	if IsClusterUnreachable(nil) {
		t.Fatal("IsClusterUnreachable(nil) = true, want false")
	}
}

func TestIsReleaseNotFound(t *testing.T) {
	notFound := &notFoundErrStub{}
	if IsReleaseNotFound(notFound) {
		t.Fatal("IsReleaseNotFound matched an unrelated error type")
	}

	if IsReleaseNotFound(nil) {
		t.Fatal("IsReleaseNotFound(nil) = true, want false")
	}
}

// notFoundErrStub is an unrelated error type used to prove IsReleaseNotFound
// doesn't false-positive on arbitrary errors; the real action.ReleaseNotFoundError
// case is exercised end-to-end by acceptance tests (no cluster available here).
type notFoundErrStub struct{}

func (*notFoundErrStub) Error() string { return "release not found (unrelated stub type)" }
