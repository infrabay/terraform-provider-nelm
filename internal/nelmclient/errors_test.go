package nelmclient

import (
	"errors"
	"fmt"
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

// TestIsLiveConflict pins the live-conflict classification to the exact
// error chains nelm's ReleasePlanInstall builds (pkg/action/
// release_plan_install.go, pkg/plan/validate.go, pkg/plan/resource_info.go):
// those, and only those, may be downgraded to a warning on a create plan.
func TestIsLiveConflict(t *testing.T) {
	adoption := fmt.Errorf("release plan install: %w",
		fmt.Errorf("remotely validate resources: %w",
			fmt.Errorf("validate adoptable resources: %w",
				errors.New(`adopt "ClusterRole/ingress-nginx": annotation "meta.helm.sh/release-namespace=ingress" must have value "ingress-v2"`))))

	immutable := fmt.Errorf("release plan install: %w",
		fmt.Errorf("build resource infos: %w",
			errors.New(`immutable fields change in resource "Service/web", but recreation is not requested: spec.clusterIP: Invalid value`)))

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"adoption check", adoption, true},
		{"immutable field change", immutable, true},
		{"nil", nil, false},
		{"bad chart reference", loadFixtureErr(t, "bad_chart_ref.txt"), false},
		{"cluster unreachable", loadFixtureErr(t, "unreachable_cluster.txt"), false},
		{"values parse error", errors.New("release plan install: render chart: failed parsing --set data: key has no value"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsLiveConflict(tt.err); got != tt.want {
				t.Fatalf("IsLiveConflict(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestRenderStorageDriver: a first-install render must read NO release
// history (nelm's always-empty in-memory driver), and an ordinary render the
// configured backend.
func TestRenderStorageDriver(t *testing.T) {
	if got := renderStorageDriver(ReleaseSpec{StorageDriver: "secret"}); got != "secret" {
		t.Errorf("renderStorageDriver(plain) = %q, want %q", got, "secret")
	}

	if got := renderStorageDriver(ReleaseSpec{StorageDriver: "secret", RenderAsFirstInstall: true}); got != "memory" {
		t.Errorf("renderStorageDriver(first install) = %q, want %q", got, "memory")
	}
}
