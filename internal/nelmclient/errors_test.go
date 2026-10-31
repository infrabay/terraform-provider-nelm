package nelmclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

// loadFixtureErr reads a captured-live error-shape fixture (captured by
// scripts/smoke/errorshapes, see scripts/smoke/README.md) and wraps its full
// combined-output text as an error, the same shape
// IsClusterUnreachable/IsReleaseNotFound would see via err.Error() in
// production (nelm actions return errors built with %w around their own
// descriptive messages; the fixtures capture the fully rendered chain).
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

// yamlSourceErrStub stands in for goccy/go-yaml's pretty errors (the module
// is only an indirect dependency): Error() includes the source lines,
// FormatError(_, false) does not. TestRenderErrors_WithholdManifests covers
// the real one.
type yamlSourceErrStub struct{}

func (yamlSourceErrStub) Error() string { return yamlSourceErrStub{}.FormatError(false, true) }

func (yamlSourceErrStub) FormatError(_, inclSource bool) string {
	msg := "[6:13] mapping value is not allowed in this context"
	if inclSource {
		msg += "\n   5 | stringData:\n>  6 |   password: aHVudGVyMg==: x\n                   ^\n"
	}

	return msg
}

// TestRedactManifestErr pins what redactManifestErr withholds from a render
// error: the manifest a missing-kind decode error quotes, and the source
// lines of a YAML error, on the chains nelm builds (pkg/chart/chart_render.go,
// pkg/resource/spec/resource_spec.go) and when a wrapper hides the decode
// error from the chain. Every wrapper's text stays; an error with nothing to
// redact comes back as is.
func TestRedactManifestErr(t *testing.T) {
	const payload = "aHVudGVyMg=="

	missingKind := runtime.NewMissingKindErr(`{"apiVersion":"v1","data":{"password":"` + payload + `"},"kind":null}`)
	decode := fmt.Errorf("construct resource spec for %q: %w", "app/templates/secret.yaml",
		fmt.Errorf("decode resource (file: %q): %w", "app/templates/secret.yaml", missingKind))
	syntax := fmt.Errorf("parse YAML resource #1 for %q: %w", "app/templates/secret.yaml", yamlSourceErrStub{})

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "missing kind",
			err:  fmt.Errorf("render chart: %w", decode),
			want: `render chart: construct resource spec for "app/templates/secret.yaml": decode resource (file: "app/templates/secret.yaml"): ` + missingKindWithheld,
		},
		{
			name: "missing kind behind %v",
			err:  fmt.Errorf("render chart: %v", decode),
			want: `render chart: construct resource spec for "app/templates/secret.yaml": decode resource (file: "app/templates/secret.yaml"): ` + missingKindWithheld,
		},
		{
			name: "missing kind joined",
			err:  errors.Join(decode, errors.New("another failure")),
			want: `construct resource spec for "app/templates/secret.yaml": decode resource (file: "app/templates/secret.yaml"): ` + missingKindWithheld + "\nanother failure",
		},
		{
			name: "YAML source lines",
			err:  fmt.Errorf("render chart: %w", syntax),
			want: `render chart: parse YAML resource #1 for "app/templates/secret.yaml": [6:13] mapping value is not allowed in this context`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactManifestErr(tt.err)

			if got.Error() != tt.want {
				t.Errorf("redactManifestErr:\n got %q\nwant %q", got.Error(), tt.want)
			}

			if strings.Contains(got.Error(), payload) {
				t.Errorf("redactManifestErr kept the manifest content: %q", got.Error())
			}
		})
	}

	t.Run("nothing to redact", func(t *testing.T) {
		err := fmt.Errorf("render chart: %w", context.DeadlineExceeded)

		if got := redactManifestErr(err); got != err {
			t.Errorf("redactManifestErr = %v, want the error itself", got)
		}

		if redactManifestErr(nil) != nil {
			t.Error("redactManifestErr(nil) != nil")
		}
	})
}

// TestRedactManifestErr_KeepsClassification checks that a redacted error
// still classifies like the error it redacts: a timeout, ErrConfigUnknown or
// a goccy error joined to or wrapping the render error stays matchable with
// errors.Is and errors.As, while its message withholds the manifest.
func TestRedactManifestErr_KeepsClassification(t *testing.T) {
	const payload = "aHVudGVyMg=="

	missingKind := runtime.NewMissingKindErr(`{"data":{"password":"` + payload + `"}}`)
	err := redactManifestErr(fmt.Errorf("render chart: %w", errors.Join(missingKind, context.DeadlineExceeded, ErrConfigUnknown)))

	if strings.Contains(err.Error(), payload) {
		t.Fatalf("redacted error still quotes the manifest: %q", err.Error())
	}

	for _, target := range []error{context.DeadlineExceeded, ErrConfigUnknown} {
		if !errors.Is(err, target) {
			t.Errorf("errors.Is(redacted, %v) = false, want true", target)
		}
	}

	var stub yamlSourceErrStub
	if !errors.As(redactManifestErr(fmt.Errorf("render chart: %w", yamlSourceErrStub{})), &stub) {
		t.Error("errors.As(redacted, yamlSourceErrStub) = false, want true")
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
