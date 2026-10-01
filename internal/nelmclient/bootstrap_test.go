package nelmclient

import (
	"context"
	"os"
	"testing"

	"github.com/werf/nelm/pkg/featgate"
)

// TestInit_PinsEveryFeatureGate is the regression test for nelm feature gates
// following the environment Terraform runs in: an unforced gate reads
// NELM_FEAT_<NAME>, so an exported NELM_FEAT_PREVIEW_V2=true (common where the
// nelm CLI is also used) used to switch werf.io/sensitive redaction to its v2
// data/stringData-only form, among other behaviour changes. After Init only
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
