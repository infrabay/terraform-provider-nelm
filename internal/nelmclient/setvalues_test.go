package nelmclient

import (
	"slices"
	"testing"
)

// TestSetValueStrings: what a chart renders is the value as nelm parses it,
// which differs from the value as written for escapes, lists and JSON.
func TestSetValueStrings(t *testing.T) {
	cases := []struct {
		name    string
		setType string
		arg     string
		want    []string
	}{
		{"auto", "", "db.password=hunter2", []string{"hunter2"}},
		{"escaped comma", "auto", `db.password=p\,ss`, []string{"p,ss"}},
		{"list", "", "hosts={one,two}", []string{"one", "two"}},
		{"further assignment", "", "a=one,b=two", []string{"one", "two"}},
		{"number", "", "pin=123456", []string{"123456"}},
		{"boolean left out", "", "enabled=true", nil},
		{"string keeps booleans", "string", "enabled=true", []string{"true"}},
		{"literal", "literal", `db.password=p\,ss={x}`, []string{`p\,ss={x}`}},
		{"json", "json", `creds={"user":"app","password":"p@ss","port":5432,"tls":true}`, []string{"app", "p@ss", "5432"}},
		{"json string", "json", `token="abc\"def"`, []string{`abc"def`}},
		{"syntax error keeps what was parsed", "", "a=one,b", []string{"one"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SetValueStrings(tc.setType, tc.arg)
			slices.Sort(got)

			want := slices.Clone(tc.want)
			slices.Sort(want)

			if !slices.Equal(got, want) {
				t.Errorf("SetValueStrings(%q, %q) = %q, want %q", tc.setType, tc.arg, got, want)
			}
		})
	}
}

// TestStoredSetValueStrings: the strings a release's stored values hold at
// the positions a set argument assigns — what a revision installed with
// another value for those names (a failed rotation) rendered — and nothing
// from anywhere else in the values.
func TestStoredSetValueStrings(t *testing.T) {
	stored := map[string]interface{}{
		"db":    map[string]interface{}{"password": "rotated-pass", "user": "app"},
		"hosts": []interface{}{"one-new", "two-new"},
		"creds": map[string]interface{}{
			"user": "app", "password": "p@ss-new", "port": float64(6432), "tls": true,
		},
		"pin":     float64(654321),
		"enabled": true,
		"nested":  map[string]interface{}{"a": map[string]interface{}{"deep": "secret-value"}},
	}

	cases := []struct {
		name    string
		setType string
		arg     string
		want    []string
	}{
		{"path", "", "db.password=hunter2", []string{"rotated-pass"}},
		{"list", "", "hosts={one,two}", []string{"one-new", "two-new"}},
		{"json object", "json", `creds={"password":"p@ss","port":5432,"tls":false}`, []string{"p@ss-new", "6432"}},
		{"number", "", "pin=123456", []string{"654321"}},
		{"boolean left out", "string", "enabled=false", nil},
		{"absent path", "", "db.token=abcdef", nil},
		{"structure differs", "", "nested.a=flat", nil},
		{"deeper path", "", "nested.a.deep=old", []string{"secret-value"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StoredSetValueStrings(tc.setType, tc.arg, stored)
			slices.Sort(got)

			want := slices.Clone(tc.want)
			slices.Sort(want)

			if !slices.Equal(got, want) {
				t.Errorf("StoredSetValueStrings(%q, %q) = %q, want %q", tc.setType, tc.arg, got, want)
			}
		})
	}

	if got := StoredSetValueStrings("", "db.password=x", nil); got != nil {
		t.Errorf("StoredSetValueStrings with no stored values = %q, want none", got)
	}
}
