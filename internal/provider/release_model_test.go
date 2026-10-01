package provider

import (
	"context"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// TestToReleaseSpec_SetSensitiveWinsOnKeyConflict is the Phase D regression
// test for the set/set_sensitive precedence bug: because nelm merges the four
// --set* categories in a FIXED order (json, set, string, literal — last wins
// per key) rather than in the order the provider appends them, a non-sensitive
// `set` entry of a later category (here type="literal") used to override a
// colliding set_sensitive entry of an earlier category (type="auto"/set),
// silently deploying the public value in place of the secret and violating the
// schema's documented "set_sensitive wins on key conflicts" contract.
//
// toReleaseSpec now drops any non-sensitive `set` entry whose name also
// appears in set_sensitive, making the set_sensitive entry the sole writer for
// that key so it wins regardless of category order.
func TestToReleaseSpec_SetSensitiveWinsOnKeyConflict(t *testing.T) {
	ctx := context.Background()

	m := baseTestReleaseModel()
	m.Set = []setModel{
		// Conflicts with a set_sensitive entry AND sorts into a later merge
		// category (literal) than it (auto/set) — the exact reversal that
		// leaked the public value pre-fix.
		{Name: types.StringValue("auth.password"), Value: types.StringValue("readable"), Type: types.StringValue("literal")},
		// A non-conflicting entry that must be preserved untouched.
		{Name: types.StringValue("image.tag"), Value: types.StringValue("v1"), Type: types.StringValue("")},
	}
	m.SetSensitive = []setModel{
		{Name: types.StringValue("auth.password"), Value: types.StringValue("s3cret"), Type: types.StringValue("")},
	}

	spec, diags := m.toReleaseSpec(ctx)
	if diags.HasError() {
		t.Fatalf("toReleaseSpec: unexpected diagnostics: %v", diags)
	}

	// The colliding non-sensitive value must not survive in ANY category, so
	// the set_sensitive value is the only writer for auth.password.
	for name, cat := range map[string][]string{
		"Set":        spec.Set,
		"SetString":  spec.SetString,
		"SetLiteral": spec.SetLiteral,
		"SetJSON":    spec.SetJSON,
	} {
		if slices.Contains(cat, "auth.password=readable") {
			t.Errorf("category %s still carries the dropped non-sensitive conflict auth.password=readable: %v", name, cat)
		}
	}

	// The set_sensitive entry (type "auto" -> Set) is present and wins.
	if !slices.Contains(spec.Set, "auth.password=s3cret") {
		t.Errorf("spec.Set is missing the winning set_sensitive value auth.password=s3cret: %v", spec.Set)
	}

	// The non-conflicting `set` entry must be preserved.
	if !slices.Contains(spec.Set, "image.tag=v1") {
		t.Errorf("non-conflicting set entry image.tag=v1 was wrongly dropped: %v", spec.Set)
	}
}

// TestToReleaseSpec_OCIRepositoryFoldedIntoChart is the F16 regression test:
// helm_release's OCI form (repository = "oci://host/path", chart = "name")
// used to reach nelm as ChartRepoURL = "oci://host/path", which nelm fetches
// as a classic index.yaml repository, so every plan failed. The spec handed
// to nelm must carry the joined oci:// reference and NO repository URL, while
// a classic https repository keeps working unchanged.
func TestToReleaseSpec_OCIRepositoryFoldedIntoChart(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name       string
		chart      string
		repository string
		wantChart  string
		wantRepo   string
	}{
		{
			name:       "oci:// repository + chart name",
			chart:      "app",
			repository: "oci://us-central1-docker.pkg.dev/my-project/helm",
			wantChart:  "oci://us-central1-docker.pkg.dev/my-project/helm/app",
			wantRepo:   "",
		},
		{
			name:       "classic https repository is passed through",
			chart:      "ingress-nginx",
			repository: "https://kubernetes.github.io/ingress-nginx",
			wantChart:  "ingress-nginx",
			wantRepo:   "https://kubernetes.github.io/ingress-nginx",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := baseTestReleaseModel()
			m.Chart = types.StringValue(tt.chart)
			m.Repository = types.StringValue(tt.repository)

			spec, diags := m.toReleaseSpec(ctx)
			if diags.HasError() {
				t.Fatalf("toReleaseSpec: unexpected diagnostics: %v", diags)
			}

			if spec.Chart != tt.wantChart || spec.Repository != tt.wantRepo {
				t.Errorf("spec chart/repository = %q / %q, want %q / %q", spec.Chart, spec.Repository, tt.wantChart, tt.wantRepo)
			}
		})
	}

	// A full oci:// chart AND a repository is contradictory: an attribute
	// error on chart, not a garbage joined reference.
	m := baseTestReleaseModel()
	m.Chart = types.StringValue("oci://us-central1-docker.pkg.dev/my-project/helm/app")
	m.Repository = types.StringValue("oci://us-central1-docker.pkg.dev/my-project/helm")

	if _, diags := m.toReleaseSpec(ctx); !diags.HasError() {
		t.Error("toReleaseSpec: want an error for an oci:// chart combined with a repository")
	}
}

// TestToReleaseSpec_WaitMapsToNoFinalTracking is the F09 regression test for
// the `wait` attribute: only an explicit wait = false may skip nelm's final
// readiness tracking. A null wait (state written before the attribute
// existed, or a hand-built model) must keep waiting, not inherit
// types.Bool.ValueBool()'s false.
func TestToReleaseSpec_WaitMapsToNoFinalTracking(t *testing.T) {
	ctx := context.Background()

	for _, tt := range []struct {
		name string
		wait types.Bool
		want bool
	}{
		{"wait = true", types.BoolValue(true), false},
		{"wait = false", types.BoolValue(false), true},
		{"wait null", types.BoolNull(), false},
		{"wait unknown", types.BoolUnknown(), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := baseTestReleaseModel()
			m.Wait = tt.wait

			spec, diags := m.toReleaseSpec(ctx)
			if diags.HasError() {
				t.Fatalf("toReleaseSpec: unexpected diagnostics: %v", diags)
			}

			if spec.NoFinalTracking != tt.want {
				t.Errorf("spec.NoFinalTracking = %v, want %v", spec.NoFinalTracking, tt.want)
			}
		})
	}
}
