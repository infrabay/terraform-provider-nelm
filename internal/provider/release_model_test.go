package provider

import (
	"context"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// TestToReleaseSpec_SetSensitiveWinsOnKeyConflict is the regression test for
// the set/set_sensitive precedence bug: because nelm merges the four --set*
// categories in a FIXED order (json, set, string, literal — last wins per key)
// rather than in the order the provider appends them, a non-sensitive `set`
// entry of a later category (here type="literal") used to override a colliding
// set_sensitive entry of an earlier category (type="auto"/set), silently
// deploying the public value in place of the secret and violating the schema's
// documented "set_sensitive wins on key conflicts" contract.
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

// sensitiveModel is a releaseModel with the given set_sensitive entries
// (name/value/type triples).
func sensitiveModel(entries ...[3]string) releaseModel {
	m := baseTestReleaseModel()

	for _, e := range entries {
		m.SetSensitive = append(m.SetSensitive, setModel{
			Name:  types.StringValue(e[0]),
			Value: types.StringValue(e[1]),
			Type:  types.StringValue(e[2]),
		})
	}

	return m
}

// TestScrubSensitive is the regression test for overlapping values:
// scrubSensitive replaced the set_sensitive values one after another in list
// order, so a shorter value listed before a longer one that contains it broke
// the longer one's match, and the rest of it reached the diagnostic in
// cleartext.
func TestScrubSensitive(t *testing.T) {
	const redacted = "(sensitive value redacted)"

	cases := []struct {
		name    string
		entries [][3]string
		in      string
		want    string
	}{
		{
			name:    "contained value listed first",
			entries: [][3]string{{"db.password", "S3cret", ""}, {"db.dsn", "S3cret-replica-Kx9", ""}},
			in:      "failed parsing --set-json data db.dsn=S3cret-replica-Kx9: invalid character",
			want:    "failed parsing --set-json data db.dsn=" + redacted + ": invalid character",
		},
		{
			name:    "contained value listed last",
			entries: [][3]string{{"db.dsn", "S3cret-replica-Kx9", ""}, {"db.password", "S3cret", ""}},
			in:      "db.dsn=S3cret-replica-Kx9 db.password=S3cret",
			want:    "db.dsn=" + redacted + " db.password=" + redacted,
		},
		{
			name:    "duplicated value",
			entries: [][3]string{{"a", "hunter2", ""}, {"b", "hunter2", ""}},
			in:      "a=hunter2,b=hunter2",
			want:    "a=" + redacted + ",b=" + redacted,
		},
		{
			name:    "empty value",
			entries: [][3]string{{"a", "", ""}},
			in:      "nothing to hide",
			want:    "nothing to hide",
		},
		{
			name:    "short value is still scrubbed from diagnostics",
			entries: [][3]string{{"pin", "42x", ""}},
			in:      "pin=42x",
			want:    "pin=" + redacted,
		},
		{
			name:    "quoted echo of a JSON value",
			entries: [][3]string{{"creds", `{"password":"p@ss\\w0rd"}`, "json"}},
			in:      `parse JSON set "creds={\"password\":\"p@ss\\\\w0rd\"}": unexpected end`,
			want:    `parse JSON set "creds=` + redacted + `": unexpected end`,
		},
		{
			name:    "value as nelm parsed it",
			entries: [][3]string{{"auth.token", `tok\,en-123`, "string"}},
			in:      `Invalid value: "tok,en-123"`,
			want:    `Invalid value: "` + redacted + `"`,
		},
		{
			name:    "base64 echo",
			entries: [][3]string{{"auth.token", "token-123", ""}},
			in:      "data.token: dG9rZW4tMTIz", // base64("token-123"), fake; gitleaks:allow
			want:    "data.token: " + redacted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sensitiveModel(tc.entries...).scrubSensitive(tc.in); got != tc.want {
				t.Errorf("scrubSensitive(%q) =\n%q\nwant\n%q", tc.in, got, tc.want)
			}
		})
	}
}

// TestToReleaseSpec_OCIRepositoryFoldedIntoChart is the regression test for
// the split OCI form: helm_release's OCI form (repository = "oci://host/path",
// chart = "name") used to reach nelm as ChartRepoURL = "oci://host/path",
// which nelm fetches as a classic index.yaml repository, so every plan failed.
// The spec handed to nelm must carry the joined oci:// reference and NO
// repository URL, while a classic https repository keeps working unchanged.
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
			repository: "oci://us-central1-docker.pkg.dev/my-project/charts",
			wantChart:  "oci://us-central1-docker.pkg.dev/my-project/charts/app",
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
	m.Chart = types.StringValue("oci://us-central1-docker.pkg.dev/my-project/charts/app")
	m.Repository = types.StringValue("oci://us-central1-docker.pkg.dev/my-project/charts")

	if _, diags := m.toReleaseSpec(ctx); !diags.HasError() {
		t.Error("toReleaseSpec: want an error for an oci:// chart combined with a repository")
	}
}

// TestToReleaseSpec_WaitMapsToNoFinalTracking is the regression test for the
// `wait` attribute: only an explicit wait = false may skip nelm's final
// readiness tracking. A null wait (state written before the attribute existed,
// or a hand-built model) must keep waiting, not inherit
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

func TestSensitiveValues(t *testing.T) {
	m := sensitiveModel(
		[3]string{"short", "abc", ""},
		[3]string{"creds", `{"user":"app-user","password":"p\"ss","port":5432,"tls":true}`, "json"},
	)

	got := m.sensitiveValues()

	for _, want := range []string{
		"abc", // as written: still scrubbed from diagnostics
		`{"user":"app-user","password":"p\"ss","port":5432,"tls":true}`,
		"app-user", `p"ss`, "5432", // parsed out of the JSON
		`p\"ss`,        // quote / toJson
		"cCJzcw==",     // b64enc
		"YXBwLXVzZXI=", // b64enc
	} {
		if !slices.Contains(got, want) {
			t.Errorf("sensitiveValues() = %q, missing %q", got, want)
		}
	}

	for _, unwanted := range []string{"YWJj", "true"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("sensitiveValues() = %q, must not contain %q", got, unwanted)
		}
	}
}
