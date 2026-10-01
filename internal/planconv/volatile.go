package planconv

import (
	"fmt"
	"reflect"
	"sort"
)

// Volatility is what two independent renders of the same chart with the same
// configuration (two BuildRenderedResources maps) disagree on. A template
// that calls randAlphaNum, uuidv4, genCA, now, ... — directly, or behind a
// lookup guard whose object does not exist yet — produces a different
// manifest on every render, and Terraform re-runs ModifyPlan at apply and
// requires every KNOWN planned value to come out identical. A key whose two
// renders differ therefore must never be planned as a known render.
type Volatility struct {
	// masks holds, per key whose two renders differ, which parts of the
	// object differ: true for a whole subtree, a map or slice of sub-masks
	// otherwise (nil where the renders agree).
	masks map[string]interface{}
	// unstable lists the keys only one of the two renders has: the chart's
	// set of objects itself changes from render to render.
	unstable []string
}

// CompareRenders compares two BuildRenderedResources maps of the same chart
// and configuration and reports which keys are volatile, and where.
func CompareRenders(a, b map[string]string) (Volatility, error) {
	v := Volatility{masks: map[string]interface{}{}}

	for key, av := range a {
		bv, ok := b[key]
		if !ok {
			v.unstable = append(v.unstable, key)
			continue
		}

		if av == bv {
			continue
		}

		am, err := unmarshalCanonical(av)
		if err != nil {
			return Volatility{}, fmt.Errorf("planconv: CompareRenders: unmarshal %s: %w", key, err)
		}

		bm, err := unmarshalCanonical(bv)
		if err != nil {
			return Volatility{}, fmt.Errorf("planconv: CompareRenders: unmarshal %s: %w", key, err)
		}

		if mask := diffMask(am, bm); mask != nil {
			v.masks[key] = mask
		}
	}

	for key := range b {
		if _, ok := a[key]; !ok {
			v.unstable = append(v.unstable, key)
		}
	}

	sort.Strings(v.unstable)

	return v, nil
}

// Keys returns the volatile keys, sorted.
func (v Volatility) Keys() []string {
	keys := make([]string, 0, len(v.masks))
	for key := range v.masks {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

// Unstable returns the keys only one of the two renders produced, sorted.
func (v Volatility) Unstable() []string {
	return v.unstable
}

// EqualOutside reports whether prior and rendered — a prior (stored or live)
// value and a render of the volatile key — are identical everywhere except
// in the parts of the object that differ from render to render. Equal means
// the object only "changed" where every render changes it, which is not a
// change of the chart's output. A key that is not volatile compares in
// full. An empty or unparsable prior never compares equal.
func (v Volatility) EqualOutside(key, prior, rendered string) (bool, error) {
	if prior == "" {
		return false, nil
	}

	pm, perr := unmarshalCanonical(prior)
	if perr != nil {
		// A prior that is not a JSON object (a placeholder persisted when no
		// value could be read) is simply not equal.
		return false, nil
	}

	rm, err := unmarshalCanonical(rendered)
	if err != nil {
		return false, fmt.Errorf("planconv: EqualOutside: unmarshal rendered %s: %w", key, err)
	}

	return equalOutside(pm, rm, v.masks[key]), nil
}

// diffMask returns nil where a and b are equal, true where they differ as a
// whole (a scalar, a type mismatch, or a list whose length differs), and a
// map or slice of sub-masks where only some of their elements differ. A map
// key present on one side only is masked as a whole.
func diffMask(a, b interface{}) interface{} {
	switch av := a.(type) {
	case map[string]interface{}:
		bv, ok := b.(map[string]interface{})
		if !ok {
			return true
		}

		out := map[string]interface{}{}

		for k, x := range av {
			y, ok := bv[k]
			if !ok {
				out[k] = true
				continue
			}

			if m := diffMask(x, y); m != nil {
				out[k] = m
			}
		}

		for k := range bv {
			if _, ok := av[k]; !ok {
				out[k] = true
			}
		}

		if len(out) == 0 {
			return nil
		}

		return out

	case []interface{}:
		bv, ok := b.([]interface{})
		if !ok || len(av) != len(bv) {
			return true
		}

		out := make([]interface{}, len(av))
		differs := false

		for i := range av {
			if m := diffMask(av[i], bv[i]); m != nil {
				out[i] = m
				differs = true
			}
		}

		if !differs {
			return nil
		}

		return out

	default:
		if reflect.DeepEqual(a, b) {
			return nil
		}

		return true
	}
}

// equalOutside reports whether x and y are deeply equal everywhere mask does
// not cover (see diffMask for the mask's shape).
func equalOutside(x, y, mask interface{}) bool {
	switch m := mask.(type) {
	case bool:
		return m

	case map[string]interface{}:
		xm, ok := x.(map[string]interface{})
		if !ok {
			return false
		}

		ym, ok := y.(map[string]interface{})
		if !ok {
			return false
		}

		for k, xv := range xm {
			sub := m[k]
			if sub == true {
				continue
			}

			yv, ok := ym[k]
			if !ok || !equalOutside(xv, yv, sub) {
				return false
			}
		}

		for k := range ym {
			if _, ok := xm[k]; !ok && m[k] != true {
				return false
			}
		}

		return true

	case []interface{}:
		xa, ok := x.([]interface{})
		if !ok {
			return false
		}

		ya, ok := y.([]interface{})
		if !ok || len(xa) != len(ya) {
			return false
		}

		for i := range xa {
			var sub interface{}
			if i < len(m) {
				sub = m[i]
			}

			if !equalOutside(xa[i], ya[i], sub) {
				return false
			}
		}

		return true

	default:
		return reflect.DeepEqual(x, y)
	}
}
