//go:build smoke

// Command errorshapes captures error-shape fixtures for
// internal/nelmclient/testdata/, used by the error-classification unit tests
// of internal/nelmclient (errors.go).
//
// Captures:
//  1. The exact error text nelm CLI produces when the target API server is
//     unreachable (a synthetic kubeconfig pointing at 127.0.0.1:1, a port
//     nothing listens on -- NOT orbstack, NOT any real context). This
//     capture alone never touches orbstack or any real cluster.
//  2. The exact error text for a bare relative chart directory name that
//     doesn't exist on disk and isn't a valid remote ref either.
//  3. The exact error text for an oci:// / repo/name chart reference when
//     FeatGateRemoteCharts is NOT enabled (this harness never enables
//     it, matching CONTRACTS.md's global-state rules).
//
// IMPORTANT: nelm's connectivity check (kube.NewClientFactory) runs BEFORE
// any chart loading, so captures (2) and (3) need a REACHABLE cluster to
// get past connectivity and actually reach chart-reference validation --
// otherwise they'd just re-capture the same unreachable-cluster error as
// (1). So (2) and (3) run against the guarded `orbstack` context (read-only
// from nelm's point of view: chart loading fails before anything would be
// created), while (1) deliberately uses the synthetic unreachable
// kubeconfig and never touches orbstack.
//
// Usage:
//
//	SMOKE_NELM_BIN=/path/to/nelm go run -tags smoke ./scripts/smoke/errorshapes
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/infrabay/terraform-provider-nelm/scripts/smoke/smokelib"
)

const syntheticKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: unreachable-synthetic
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: unreachable-synthetic
  context:
    cluster: unreachable-synthetic
    user: unreachable-synthetic
current-context: unreachable-synthetic
users:
- name: unreachable-synthetic
  user: {}
`

func main() {
	tempRoot := smokelib.MustTempDir("errorshapes")
	defer os.RemoveAll(tempRoot)

	dir := smokelib.NelmclientFixtureDir("errors")

	kubeconfigPath := filepath.Join(tempRoot, "unreachable-kubeconfig.yaml")
	if err := os.WriteFile(kubeconfigPath, []byte(syntheticKubeconfig), 0o600); err != nil {
		panic(err)
	}

	// ---- 1. unreachable cluster ----
	fmt.Println("=== (1) unreachable cluster error shape ===")
	out, runErr := runNelmCapture(
		"release", "plan", "install",
		"--kube-config", kubeconfigPath,
		"--kube-context", "unreachable-synthetic",
		"--kube-request-timeout", "3s",
		"-n", "default",
		"-r", "unreachable-probe",
		"--save-plan", filepath.Join(tempRoot, "unreachable.plan"),
		"--temp-dir", filepath.Join(tempRoot, "unreachable-op"),
		smokelib.BasicChartPath(),
	)
	smokelib.WriteFile(filepath.Join(dir, "unreachable_cluster.txt"), []byte(fmt.Sprintf(
		"command: nelm release plan install --kube-config <synthetic, server=https://127.0.0.1:1> --kube-context unreachable-synthetic --kube-request-timeout 3s ...\nexit error: %v\n\n--- combined output ---\n%s\n",
		runErr, out)))
	fmt.Printf("captured (exit err: %v)\n", runErr)

	// Captures (2) and (3) need a REACHABLE cluster to get past nelm's
	// connectivity check and actually reach chart-reference validation --
	// guard + use orbstack explicitly (chart loading fails before anything
	// would be created, but the guard runs on principle regardless).
	ctx := smokelib.InitLogging(context.Background())
	smokelib.MustGuardOrbstack(ctx)

	// ---- 2. bare relative / nonexistent chart directory ----
	fmt.Println("\n=== (2) bare relative nonexistent chart path error shape ===")
	out2, runErr2 := runNelmCapture(
		"release", "plan", "install",
		"--kube-context", smokelib.OrbstackContext,
		"-n", smokelib.RandNamespace("badchart"),
		"-r", "badchart-probe",
		"--save-plan", filepath.Join(tempRoot, "badchart.plan"),
		"--temp-dir", filepath.Join(tempRoot, "badchart-op"),
		"./this-relative-chart-path-does-not-exist-anywhere",
	)
	smokelib.WriteFile(filepath.Join(dir, "bad_chart_ref.txt"), []byte(fmt.Sprintf(
		"command: nelm release plan install --kube-context orbstack ... ./this-relative-chart-path-does-not-exist-anywhere\nexit error: %v\n\n--- combined output ---\n%s\n",
		runErr2, out2)))
	fmt.Printf("captured (exit err: %v)\n", runErr2)

	// ---- 3. remote chart ref without FeatGateRemoteCharts ----
	fmt.Println("\n=== (3) remote chart ref without FeatGateRemoteCharts error shape ===")
	out3, runErr3 := runNelmCapture(
		"release", "plan", "install",
		"--kube-context", smokelib.OrbstackContext,
		"-n", smokelib.RandNamespace("remotechart"),
		"-r", "remotechart-probe",
		"--save-plan", filepath.Join(tempRoot, "remotechart.plan"),
		"--temp-dir", filepath.Join(tempRoot, "remotechart-op"),
		"oci://example.com/charts/does-not-matter",
	)
	smokelib.WriteFile(filepath.Join(dir, "remote_chart_no_featgate.txt"), []byte(fmt.Sprintf(
		"command: nelm release plan install --kube-context orbstack ... oci://example.com/charts/does-not-matter (NELM_FEAT_REMOTE_CHARTS unset/false)\nexit error: %v\n\n--- combined output ---\n%s\n",
		runErr3, out3)))
	fmt.Printf("captured (exit err: %v)\n", runErr3)

	fmt.Println("\ndone. See internal/nelmclient/testdata/errors/*.txt")
}

// runNelmCapture is like smokelib.RunNelm but does NOT panic on non-zero
// exit -- for this program, a non-zero exit + its exact stderr/stdout IS
// the fixture being captured, not a failure of the capture itself.
func runNelmCapture(args ...string) (string, error) {
	bin := smokelib.NelmBin()
	fmt.Printf("+ %s %s\n", bin, strings.Join(args, " "))

	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	fmt.Print(string(out))

	return string(out), err
}
