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
