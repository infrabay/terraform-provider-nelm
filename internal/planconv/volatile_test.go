package planconv

import (
	"reflect"
	"testing"
)

// The values below are what testdata/charts/volatile normalizes to: a
// Deployment whose pod template carries a random rollme annotation and a
// deploy timestamp, a Secret with a generated password (redacted to its
// placeholder), and a deterministic ConfigMap.
const (
	volatileDeployA = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"app-volatile"},"spec":{"replicas":1,"template":{"metadata":{"annotations":{"deploy-date":"2026-10-01 13:16:39.737469 +0200 CEST","rollme":"6dWiP"}},"spec":{"containers":[{"image":"nginx:1.27-alpine","name":"volatile"}]}}}}`
	volatileDeployB = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"app-volatile"},"spec":{"replicas":1,"template":{"metadata":{"annotations":{"deploy-date":"2026-10-01 13:16:39.744808 +0200 CEST","rollme":"fsh1x"}},"spec":{"containers":[{"image":"nginx:1.27-alpine","name":"volatile"}]}}}}`
	volatileSecretA = `{"apiVersion":"v1","data":{"password":"<hidden 24 sensitive bytes, hash ab1375303f0e>"},"kind":"Secret","metadata":{"name":"app-volatile"},"type":"Opaque"}`
	volatileSecretB = `{"apiVersion":"v1","data":{"password":"<hidden 24 sensitive bytes, hash 605abbdaa328>"},"kind":"Secret","metadata":{"name":"app-volatile"},"type":"Opaque"}`
	volatileCM      = `{"apiVersion":"v1","data":{"message":"hello"},"kind":"ConfigMap","metadata":{"name":"app-volatile"}}`

	keyDeploy = "apps/v1/Deployment/apps/app-volatile"
	keySecret = "v1/Secret/apps/app-volatile"
	keyCM     = "v1/ConfigMap/apps/app-volatile"
)

func TestCompareRenders(t *testing.T) {
	a := map[string]string{keyDeploy: volatileDeployA, keySecret: volatileSecretA, keyCM: volatileCM}
	b := map[string]string{keyDeploy: volatileDeployB, keySecret: volatileSecretB, keyCM: volatileCM}

	v, err := CompareRenders(a, b)
	if err != nil {
		t.Fatalf("CompareRenders: %v", err)
	}

	if got, want := v.Keys(), []string{keyDeploy, keySecret}; !reflect.DeepEqual(got, want) {
		t.Errorf("volatile keys = %v, want %v (the ConfigMap renders identically)", got, want)
	}

	if len(v.Unstable()) != 0 {
		t.Errorf("unstable keys = %v, want none", v.Unstable())
	}

	same, err := CompareRenders(a, a)
	if err != nil || len(same.Keys()) != 0 {
		t.Errorf("identical renders: keys %v, err %v, want none", same.Keys(), err)
	}

	extra := map[string]string{keyDeploy: volatileDeployB, keySecret: volatileSecretB, keyCM: volatileCM, "v1/Service/apps/x": "{}"}

	unstable, err := CompareRenders(a, extra)
	if err != nil {
		t.Fatalf("CompareRenders: %v", err)
	}

	if got := unstable.Unstable(); !reflect.DeepEqual(got, []string{"v1/Service/apps/x"}) {
		t.Errorf("unstable keys = %v, want the Service only one render has", got)
	}
}

// TestVolatilityEqualOutside: a prior value that differs from a render only
// where every render differs is the same chart output; any other difference,
// including a field the chart stopped rendering, is a change.
func TestVolatilityEqualOutside(t *testing.T) {
	v, err := CompareRenders(
		map[string]string{keyDeploy: volatileDeployA, keySecret: volatileSecretA, keyCM: volatileCM},
		map[string]string{keyDeploy: volatileDeployB, keySecret: volatileSecretB, keyCM: volatileCM},
	)
	if err != nil {
		t.Fatalf("CompareRenders: %v", err)
	}

	// The live object after an earlier install: its own random values.
	live := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"app-volatile"},"spec":{"replicas":1,"template":{"metadata":{"annotations":{"deploy-date":"2026-09-30 08:00:00.1 +0000 UTC","rollme":"Qq7Zr"}},"spec":{"containers":[{"image":"nginx:1.27-alpine","name":"volatile"}]}}}}`

	tests := []struct {
		name       string
		key, prior string
		rendered   string
		want       bool
	}{
		{"only the volatile annotations differ", keyDeploy, live, volatileDeployA, true},
		{"only the generated password differs", keySecret, `{"apiVersion":"v1","data":{"password":"<hidden 24 sensitive bytes, hash 000000000000>"},"kind":"Secret","metadata":{"name":"app-volatile"},"type":"Opaque"}`, volatileSecretA, true},
		{
			"drift outside the volatile annotations (replicas)",
			keyDeploy,
			`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"app-volatile"},"spec":{"replicas":3,"template":{"metadata":{"annotations":{"deploy-date":"x","rollme":"Qq7Zr"}},"spec":{"containers":[{"image":"nginx:1.27-alpine","name":"volatile"}]}}}}`,
			volatileDeployA,
			false,
		},
		{
			"a field the chart no longer renders",
			keyDeploy,
			`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"labels":{"old":"x"},"name":"app-volatile"},"spec":{"replicas":1,"template":{"metadata":{"annotations":{"deploy-date":"x","rollme":"Qq7Zr"}},"spec":{"containers":[{"image":"nginx:1.27-alpine","name":"volatile"}]}}}}`,
			volatileDeployA,
			false,
		},
		{
			"a volatile annotation missing from the prior value",
			keyDeploy,
			`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"app-volatile"},"spec":{"replicas":1,"template":{"metadata":{"annotations":{"rollme":"Qq7Zr"}},"spec":{"containers":[{"image":"nginx:1.27-alpine","name":"volatile"}]}}}}`,
			volatileDeployA,
			true,
		},
		{"an image change", keyDeploy, live, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"app-volatile"},"spec":{"replicas":1,"template":{"metadata":{"annotations":{"deploy-date":"y","rollme":"zzzzz"}},"spec":{"containers":[{"image":"nginx:1.28-alpine","name":"volatile"}]}}}}`, false},
		{"no prior value", keyDeploy, "", volatileDeployA, false},
		{"a placeholder prior value", keyDeploy, "not json", volatileDeployA, false},
		{"a key that is not volatile compares in full", keyCM, volatileCM, volatileCM, true},
		{"a key that is not volatile, changed", keyCM, `{"apiVersion":"v1","data":{"message":"bye"},"kind":"ConfigMap","metadata":{"name":"app-volatile"}}`, volatileCM, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := v.EqualOutside(tt.key, tt.prior, tt.rendered)
			if err != nil {
				t.Fatalf("EqualOutside: %v", err)
			}

			if got != tt.want {
				t.Errorf("EqualOutside = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestVolatilityEqualOutside_StructuralDifferences: a list whose length
// differs between the renders, or a field only one render has, is masked as
// a whole.
func TestVolatilityEqualOutside_StructuralDifferences(t *testing.T) {
	v, err := CompareRenders(
		map[string]string{"k": `{"a":[1,2],"b":"x"}`, "f": `{"a":1,"b":"x"}`},
		map[string]string{"k": `{"a":[1,2,3],"b":"x"}`, "f": `{"b":"x","c":{"d":2}}`},
	)
	if err != nil {
		t.Fatalf("CompareRenders: %v", err)
	}

	if got, _ := v.EqualOutside("k", `{"a":[9],"b":"x"}`, `{"a":[1,2],"b":"x"}`); !got {
		t.Error("a list whose length changes between renders must be masked as a whole")
	}

	if got, _ := v.EqualOutside("k", `{"a":[9],"b":"y"}`, `{"a":[1,2],"b":"x"}`); got {
		t.Error("a difference next to the masked list must still count")
	}

	if got, _ := v.EqualOutside("f", `{"b":"x","c":"anything"}`, `{"a":1,"b":"x"}`); !got {
		t.Error("fields only one render has must be masked whether or not the prior value has them")
	}

	if got, _ := v.EqualOutside("f", `{"b":"y"}`, `{"a":1,"b":"x"}`); got {
		t.Error("a difference next to the masked fields must still count")
	}
}
