package nelmclient

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/werf/nelm/pkg/featgate"
)

// TestInit_PinsEveryFeatureGate is the regression test for nelm feature gates
// following the environment Terraform runs in: an unforced gate reads
// NELM_FEAT_<NAME>, so an exported NELM_FEAT_PREVIEW_V2=true (common where the
// nelm CLI is also used) used to switch werf.io/sensitive redaction to its v2
// data/stringData-only form, among other behavior changes. After Init only
// remote-charts may be on, whatever the environment says.
func TestInit_PinsEveryFeatureGate(t *testing.T) {
	for _, g := range featgate.FeatGates {
		t.Setenv(g.EnvVarName(), "true")
	}

	t.Setenv(featgate.FeatGateRemoteCharts.EnvVarName(), "false")

	if err := Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	for _, g := range featgate.FeatGates {
		want := g == featgate.FeatGateRemoteCharts
		if got := g.Enabled(); got != want {
			t.Errorf("feature gate %q: Enabled() = %v with %s=%q, want %v",
				g.Name, got, g.EnvVarName(), os.Getenv(g.EnvVarName()), want)
		}
	}
}

// TestShutdown_RemovesTempRoot is the regression test for the per-process
// temp root outliving every provider process: per-op directories were
// removed, but nothing removed the tf-nelm-* root itself, so each provider
// process (one per configured provider per plan or apply walk) left one in
// $TMPDIR, plus whatever a timed-out nelm worker wrote after its cleanup.
func TestShutdown_RemovesTempRoot(t *testing.T) {
	if err := Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	root := TempRoot()

	// Init runs once per process: put the root back afterwards so later tests
	// in this binary still have one.
	t.Cleanup(func() {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Errorf("recreate temp root: %v", err)
		}
	})

	// A leftover like the ones a timed-out nelm worker writes after its
	// operation's cleanup already ran.
	dir, _, err := newOpDir("nelm-leftover-")
	if err != nil {
		t.Fatalf("newOpDir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "late.yaml"), []byte("late"), 0o600); err != nil {
		t.Fatalf("write leftover: %v", err)
	}

	Shutdown()

	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("temp root %s still exists after Shutdown (stat: %v)", root, err)
	}
}
