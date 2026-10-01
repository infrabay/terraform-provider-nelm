//go:build smoke

// Command lifecycle captures the lifecycle-plan fixtures against the local
// "orbstack" cluster:
//
//	(a) first-install plan into a fresh namespace: every ResourceChange.Type
//	    must be "create" with Before == nil.
//	(b) a no-change plan captured immediately after actually installing:
//	    Changes must be EMPTY (validates the provider's prior-map-merge
//	    assumption). Flagged loudly if not.
//	(c) a drift plan captured after an out-of-band `kubectl patch` of the
//	    Deployment's replica count: expects an "update" change with
//	    Before/After populated.
//	(d) a delete-from-chart plan captured with configMap.enabled=false
//	    (see testdata/charts/basic's fixture-capture hook): expects a
//	    "delete" change for the ConfigMap.
//
// Every plan artifact is saved BOTH as the raw gzip bytes nelm wrote
// (*.artifact.json.gz) and as decoded/indented JSON (*.decoded.json) under
// internal/planconv/testdata/lifecycle/.
//
// PROVENANCE NOTE (see scripts/smoke/README.md for the full explanation): the
// "release plan install" / "release install" steps below shell out to a nelm
// CLI binary (set via $SMOKE_NELM_BIN) instead of calling
// github.com/werf/nelm/pkg/action in-process, because at capture time this
// module's go.sum lacked transitive-dependency entries for that package (see
// smokelib/exec.go's doc comment for the full story). Only the mutating CLI
// calls are worked around this way; every other line of this program
// (ReadArtifact, decode, assertions) uses the real Go library.
//
// This program deliberately leaves the installed release + namespace ALIVE
// on exit (prints "LIFECYCLE_NAMESPACE=..." / "LIFECYCLE_RELEASE=..." on its
// last lines) so scripts/smoke/normalize can capture live-vs-plan-After
// evidence against the same resources immediately afterward. The caller is
// responsible for cleanup (nelm/helm uninstall + kubectl delete ns
// --context orbstack) once normalize has run.
//
// Usage:
//
//	SMOKE_NELM_BIN=/path/to/nelm go run -tags smoke ./scripts/smoke/lifecycle
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/werf/nelm/pkg/plan"

	"github.com/infrabay/terraform-provider-nelm/scripts/smoke/smokelib"
)

func main() {
	ctx := context.Background()
	ctx = smokelib.InitLogging(ctx)

	// CLUSTER SAFETY: must be the very first thing that can touch a cluster.
	smokelib.MustGuardOrbstack(ctx)

	namespace := smokelib.RandNamespace("lifecycle")
	releaseName := "lifecycle"
	chart := smokelib.BasicChartPath()

	fmt.Printf("=== lifecycle capture: namespace=%s release=%s chart=%s ===\n", namespace, releaseName, chart)

	tempRoot := smokelib.MustTempDir("lifecycle")
	defer os.RemoveAll(tempRoot)

	failures := 0
	assert := func(cond bool, format string, args ...interface{}) {
		if cond {
			fmt.Printf("  ASSERT OK: %s\n", fmt.Sprintf(format, args...))
			return
		}
		failures++
		fmt.Printf("  !!!! ASSERT FAILED (FLAGGED): %s !!!!\n", fmt.Sprintf(format, args...))
	}

	// ---- (a) first-install plan: fresh namespace, release does not exist ----
	fmt.Println("\n--- (a) first-install plan ---")
	artifactA, rawA := runPlan(ctx, "01_first_install", chart, releaseName, namespace, tempRoot, nil)
	allCreate := true
	for _, c := range artifactA.Data.Changes {
		if c.Type != "create" || c.Before != nil {
			allCreate = false
		}
	}
	assert(len(artifactA.Data.Changes) > 0, "first-install plan has at least one change (got %d)", len(artifactA.Data.Changes))
	assert(allCreate, "every first-install change is Type=create with Before=nil")
	saveArtifact(artifactA, rawA, "lifecycle", "01_first_install")

	// ---- actually install (fresh release) ----
	fmt.Println("\n--- installing release for real (needed for b/c/d) ---")
	smokelib.RunNelm(
		"release", "install",
		"--kube-context", smokelib.OrbstackContext,
		"-n", namespace,
		"-r", releaseName,
		"--no-show-progress",
		"--temp-dir", filepath.Join(tempRoot, "install"),
		chart,
	)
	fmt.Println("install OK")

	// ---- (b) no-change plan right after install ----
	fmt.Println("\n--- (b) no-change plan ---")
	artifactB, rawB := runPlan(ctx, "02_no_change", chart, releaseName, namespace, tempRoot, nil)
	assert(len(artifactB.Data.Changes) == 0, "no-change plan has EMPTY Changes (got %d) -- prior-map merge assumption", len(artifactB.Data.Changes))
	if len(artifactB.Data.Changes) != 0 {
		fmt.Println("  !!!! FLAG: no-change plan is NOT empty. planconv's BuildPlannedResources merge design must handle Changes containing unchanged resources. Dumping change types:")
		for _, c := range artifactB.Data.Changes {
			fmt.Printf("      - %s %s/%s type=%s reason=%q\n", c.ResourceMeta.GroupVersionKind, c.ResourceMeta.Namespace, c.ResourceMeta.Name, c.Type, c.Reason)
		}
	}
	saveArtifact(artifactB, rawB, "lifecycle", "02_no_change")

	// ---- (c) drift: kubectl patch Deployment replicas out-of-band, then plan ----
	fmt.Println("\n--- (c) drift plan (kubectl patch replicas) ---")
	deployName := releaseName + "-basic"
	smokelib.RunKubectl("-n", namespace, "patch", "deployment", deployName, "--type", "merge", "-p", `{"spec":{"replicas":3}}`)

	artifactC, rawC := runPlan(ctx, "03_drift", chart, releaseName, namespace, tempRoot, nil)
	foundDeployUpdate := false
	for _, c := range artifactC.Data.Changes {
		if c.ResourceMeta.GroupVersionKind.Kind == "Deployment" && c.Type == "update" && c.Before != nil && c.After != nil {
			foundDeployUpdate = true
		}
	}
	assert(foundDeployUpdate, "drift plan contains an update change for the Deployment with Before/After populated")
	saveArtifact(artifactC, rawC, "lifecycle", "03_drift")

	// ---- (d) delete-from-chart plan: disable the ConfigMap via values ----
	fmt.Println("\n--- (d) delete-from-chart plan (configMap.enabled=false) ---")
	artifactD, rawD := runPlan(ctx, "04_delete", chart, releaseName, namespace, tempRoot, []string{"configMap.enabled=false"})
	foundConfigMapDelete := false
	for _, c := range artifactD.Data.Changes {
		if c.ResourceMeta.GroupVersionKind.Kind == "ConfigMap" && c.Type == "delete" {
			foundConfigMapDelete = true
		}
	}
	assert(foundConfigMapDelete, "delete-from-chart plan contains a delete change for the ConfigMap")
	saveArtifact(artifactD, rawD, "lifecycle", "04_delete")

	fmt.Printf("\n=== lifecycle capture done: %d assertion failure(s) ===\n", failures)
	fmt.Printf("LIFECYCLE_NAMESPACE=%s\n", namespace)
	fmt.Printf("LIFECYCLE_RELEASE=%s\n", releaseName)
	fmt.Printf("LIFECYCLE_DEPLOYMENT=%s\n", deployName)

	if failures > 0 {
		os.Exit(2)
	}
}

// runPlan runs one `nelm release plan install` + ReadArtifact round-trip and
// returns both the decoded artifact and the still-on-disk raw artifact path
// (the temp dir is only cleaned up by main()'s deferred RemoveAll, so the
// raw file is available for saveArtifact to copy from).
func runPlan(ctx context.Context, label, chart, releaseName, namespace, tempRoot string, valuesSet []string) (*plan.PlanArtifact, string) {
	opDir := filepath.Join(tempRoot, label)
	if err := os.MkdirAll(opDir, 0o700); err != nil {
		panic(err)
	}

	artifactPath := filepath.Join(opDir, "plan.artifact")

	args := []string{
		"release", "plan", "install",
		"--kube-context", smokelib.OrbstackContext,
		"-n", namespace,
		"-r", releaseName,
		"--save-plan", artifactPath,
		"--no-final-tracking",
		"--temp-dir", opDir,
	}
	for _, v := range valuesSet {
		args = append(args, "--set", v)
	}
	args = append(args, chart)

	smokelib.RunNelm(args...)

	artifact, err := smokelib.ReadArtifact(ctx, artifactPath)
	if err != nil {
		fmt.Printf("FATAL: ReadArtifact(%s) failed: %v\n", label, err)
		os.Exit(1)
	}

	fmt.Printf("  plan %s: %d change(s)\n", label, len(artifact.Data.Changes))
	for _, c := range artifact.Data.Changes {
		fmt.Printf("      - %s %s/%s type=%s\n", c.ResourceMeta.GroupVersionKind, c.ResourceMeta.Namespace, c.ResourceMeta.Name, c.Type)
	}

	return artifact, artifactPath
}

func saveArtifact(artifact *plan.PlanArtifact, rawPath, sub, label string) {
	dir := smokelib.FixtureDir(sub)
	smokelib.CopyFile(rawPath, filepath.Join(dir, label+".artifact.json.gz"))
	smokelib.WriteFile(filepath.Join(dir, label+".decoded.json"), smokelib.MarshalIndent(smokelib.Decode(artifact)))
}
