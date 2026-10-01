package nelmclient

import (
	"fmt"

	"github.com/werf/nelm/pkg/helm/pkg/strvals"
)

// SetValueStrings returns the strings a set argument ("name=value") assigns
// once nelm has parsed it: what a chart can actually render, which is not
// always the value as written. setType is the argument's category as
// nelm_release names it — "" or "auto" (ReleaseSpec.Set), "string"
// (SetString), "literal" (SetLiteral), "json" (SetJSON) — and the argument is
// parsed with the same helm strvals parser nelm merges that category with:
// backslash escapes are resolved, "{a,b}" lists and further comma-separated
// assignments are split out, and a JSON value is decoded into its scalars. A
// number is formatted as a template prints it; booleans and nulls are left
// out. Whatever the parser assigned before a syntax error is still returned
// (nelm fails the plan on such an argument anyway).
func SetValueStrings(setType, arg string) []string {
	vals := map[string]interface{}{}

	// The error is deliberately ignored, see above.
	switch setType {
	case "string":
		_ = strvals.ParseIntoString(arg, vals)
	case "literal":
		_ = strvals.ParseLiteralInto(arg, vals)
	case "json":
		_ = strvals.ParseJSON(arg, vals)
	default:
		_ = strvals.ParseInto(arg, vals)
	}

	var out []string

	var collect func(v interface{})
	collect = func(v interface{}) {
		switch val := v.(type) {
		case map[string]interface{}:
			for _, x := range val {
				collect(x)
			}
		case []interface{}:
			for _, x := range val {
				collect(x)
			}
		case string:
			out = append(out, val)
		case int64, float64:
			out = append(out, fmt.Sprint(val))
		}
	}
	collect(vals)

	return out
}
