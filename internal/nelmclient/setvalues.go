package nelmclient

import (
	"encoding/json"
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
		default:
			if s, ok := scalarString(val); ok {
				out = append(out, s)
			}
		}
	}
	collect(parseSetArg(setType, arg))

	return out
}

// StoredSetValueStrings returns the strings values — a release's stored,
// coalesced values (ReleaseInfo.Values) — holds wherever the set argument
// assigns one, i.e. what that release revision rendered there. It differs
// from SetValueStrings(setType, arg) when the revision was installed with
// other values for the same names: an upgrade that failed after rotating a
// set_sensitive value stores (and partly applied) the new one. arg is parsed
// as SetValueStrings parses it, and only the strings and numbers values
// holds at the positions it assigns are returned.
func StoredSetValueStrings(setType, arg string, values map[string]interface{}) []string {
	var out []string

	var walk func(assigned, stored interface{})
	walk = func(assigned, stored interface{}) {
		switch a := assigned.(type) {
		case map[string]interface{}:
			s, ok := stored.(map[string]interface{})
			if !ok {
				return
			}

			for key, x := range a {
				walk(x, s[key])
			}
		case []interface{}:
			s, ok := stored.([]interface{})
			if !ok {
				return
			}

			for i, x := range a {
				if i < len(s) {
					walk(x, s[i])
				}
			}
		default:
			if str, ok := scalarString(stored); ok {
				out = append(out, str)
			}
		}
	}
	walk(parseSetArg(setType, arg), values)

	return out
}

// parseSetArg parses a set argument of setType the way nelm merges that
// category. The error is deliberately ignored: see SetValueStrings.
func parseSetArg(setType, arg string) map[string]interface{} {
	vals := map[string]interface{}{}

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

	return vals
}

// scalarString formats a string or a number as a template prints it; ok is
// false for anything else (booleans, nulls, maps, lists). Parsed arguments
// carry int64/float64 numbers, stored release values (decoded from JSON and
// YAML) float64, int or json.Number ones.
func scalarString(v interface{}) (string, bool) {
	switch val := v.(type) {
	case string:
		return val, true
	case int, int64, float64:
		return fmt.Sprint(val), true
	case json.Number:
		return val.String(), true
	default:
		return "", false
	}
}
