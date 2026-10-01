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
