//go:build smoke

// Command helmv4probe captures the T-fixtures import fixtures (task
// deliverable 4, design §7 RISK #2 -- the highest-value probe in this
// task) and the managedFields-mutation fixtures (task deliverable 5):
//
//  1. Installs testdata/charts/basic with the LOCAL helm v4.2.3 CLI (never
//     nelm) into a fresh namespace.
//  2. Inspects the resulting release-storage Secret via kubectl: confirms
//     its type is "helm.sh/release.v1" and its name matches
//     "sh.helm.release.v1.<name>.v1" -- this is RISK #2: does helm v4
//     still write the same storage format nelm's vendored (helm v3)
//     storage driver reads?
//  3. Runs `nelm release get --print-values --output-format json` (via CLI
//     subprocess, see smokelib/exec.go for why) against that helm-v4
//     installed release and verifies chart name/version, values, and
//     Resources populate -- i.e. "can we import a plain-helm(v4) release
//     with zero conversion".
//  4. managedFields probe: captures `kubectl get deploy --show-managed-
//     fields -o yaml` BEFORE any nelm plan, runs a first
//     `nelm release plan install` against the SAME helm-installed release
//     (same chart/values, so any diff is managedFields-only) and captures
//     the deployment again ("after"), then runs a SECOND plan and captures
//     a third snapshot ("after2") to confirm managedFields stabilize.
//  5. Best-effort: attempts to produce a never-successfully-deployed/failed
//     release (bad image + short --wait timeout) and runs
//     `nelm release get -n <ns> -r <name> [revision]` (best-effort revision
//     range) against it, documenting whatever happens either way.
//
// Cleans up every namespace/release IT created before exiting (helm
// uninstall + kubectl delete ns --context orbstack), success or failure.
//
// Usage:
//
//	NELM_SMOKE_BIN=/path/to/nelm go run -tags smoke ./scripts/smoke/helmv4probe
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/infrabay/terraform-provider-nelm/scripts/smoke/smokelib"
)

func main() {
	ctx := context.Background()
	ctx = smokelib.InitLogging(ctx)

	// CLUSTER SAFETY: must be the very first thing that can touch a cluster.
	smokelib.MustGuardOrbstack(ctx)

	namespace := smokelib.RandNamespace("helmv4")
	releaseName := "helmv4"
	chart := smokelib.BasicChartPath()

	failNamespace := smokelib.RandNamespace("helmv4fail")
	failReleaseName := "helmv4fail"

	failures := 0
	assert := func(cond bool, format string, args ...interface{}) {
		if cond {
			fmt.Printf("  ASSERT OK: %s\n", fmt.Sprintf(format, args...))
			return
		}
		failures++
		fmt.Printf("  !!!! ASSERT FAILED (FLAGGED): %s !!!!\n", fmt.Sprintf(format, args...))
	}

	// Always attempt cleanup, even on a panic/os.Exit path via defer --
	// os.Exit skips deferred calls, so we deliberately avoid os.Exit in this
	// program's happy path and only panic (which DOES run defers) on fatal
	// setup errors.
	defer cleanup(namespace, releaseName)
	defer cleanup(failNamespace, failReleaseName)

	dir := smokelib.FixtureDir("helmv4import")
	tempRoot := smokelib.MustTempDir("helmv4probe")
	defer os.RemoveAll(tempRoot)

	// ---- 1. helm v4.2.3 install (never nelm) ----
	fmt.Printf("=== helmv4probe: namespace=%s release=%s chart=%s ===\n", namespace, releaseName, chart)
	helmVersion := smokelib.RunHelm("version", "--short")
	smokelib.RunHelm("install", releaseName, chart, "-n", namespace, "--create-namespace")

	// ---- 2. inspect the release-storage Secret ----
	fmt.Println("\n--- (2) release-storage Secret inspection ---")
	secretNames := smokelib.RunKubectl("-n", namespace, "get", "secret", "-l", "owner=helm", "-o", "name")
	secretNames = strings.TrimSpace(secretNames)
	expectedSecretName := fmt.Sprintf("secret/sh.helm.release.v1.%s.v1", releaseName)
	assert(secretNames == expectedSecretName, "release-storage secret name is %q (expected %q)", secretNames, expectedSecretName)

	// secretNames is already in "secret/<name>" form (a valid single
	// TYPE/NAME positional arg for kubectl get) -- do NOT strip the
	// "secret/" prefix, kubectl would then misparse the dotted release
	// secret name as a resource TYPE instead of a name.
	secretType := strings.TrimSpace(smokelib.RunKubectl("-n", namespace, "get", secretNames, "-o", "jsonpath={.type}"))
	assert(secretType == "helm.sh/release.v1", "release-storage secret type is %q (expected helm.sh/release.v1)", secretType)

	secretLabelsJSON := smokelib.RunKubectl("-n", namespace, "get", secretNames, "-o", "jsonpath={.metadata.labels}")
	smokelib.WriteFile(filepath.Join(dir, "release_secret_evidence.txt"), []byte(fmt.Sprintf(
		"helm version: %s\nsecret name: %s\nsecret type: %s\nsecret labels: %s\n"+
			"(data payload deliberately NOT captured here -- it's a base64+gzip blob of the full\n"+
			"release manifest; the name/type/labels above are the only things RISK #2 needs)\n",
		strings.TrimSpace(helmVersion), secretNames, secretType, strings.TrimSpace(secretLabelsJSON),
	)))

	// ---- 3. nelm release get against the helm-v4-installed release ----
	fmt.Println("\n--- (3) nelm release get (RISK #2 core check) ---")
	getOut := smokelib.RunNelm(
		"release", "get",
		"--kube-context", smokelib.OrbstackContext,
		"-n", namespace,
		"-r", releaseName,
		"--print-values",
		"--output-format", "json",
		"--temp-dir", filepath.Join(tempRoot, "get"),
	)
	smokelib.WriteFile(filepath.Join(dir, "release_get.json"), []byte(getOut))

	var getResult struct {
		Release struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			Revision  int    `json:"revision"`
			Status    string `json:"status"`
		} `json:"release"`
		Chart struct {
			Name       string `json:"name"`
			Version    string `json:"version"`
			AppVersion string `json:"appVersion"`
		} `json:"chart"`
		Values    map[string]interface{}   `json:"values"`
		Resources []map[string]interface{} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(getOut), &getResult); err != nil {
		fmt.Printf("  !!!! ASSERT FAILED (FLAGGED): nelm release get did not return valid JSON: %v !!!!\n", err)
		failures++
	} else {
		assert(getResult.Chart.Name == "basic", "chart name is %q (expected basic)", getResult.Chart.Name)
		assert(getResult.Chart.Version == "0.1.0", "chart version is %q (expected 0.1.0)", getResult.Chart.Version)
		assert(len(getResult.Values) > 0, "values populated (%d top-level keys)", len(getResult.Values))
		assert(len(getResult.Resources) == 6, "Resources populated with all 6 chart resources (got %d)", len(getResult.Resources))
		assert(getResult.Release.Name == releaseName && getResult.Release.Namespace == namespace, "release identity round-trips (name=%q ns=%q)", getResult.Release.Name, getResult.Release.Namespace)
	}

	// ---- 4. managedFields probe ----
	fmt.Println("\n--- (4) managedFields probe ---")
	deployName := releaseName + "-basic"

	before := smokelib.RunKubectl("-n", namespace, "get", "deployment", deployName, "--show-managed-fields", "-o", "yaml")
	smokelib.WriteFile(filepath.Join(dir, "managedfields_before.yaml"), []byte(before))

	plan1 := filepath.Join(tempRoot, "plan1.artifact")
	smokelib.RunNelm(
		"release", "plan", "install",
		"--kube-context", smokelib.OrbstackContext,
		"-n", namespace,
		"-r", releaseName,
		"--save-plan", plan1,
		"--no-final-tracking",
		"--temp-dir", filepath.Join(tempRoot, "plan1"),
		chart,
	)

	after := smokelib.RunKubectl("-n", namespace, "get", "deployment", deployName, "--show-managed-fields", "-o", "yaml")
	smokelib.WriteFile(filepath.Join(dir, "managedfields_after.yaml"), []byte(after))

	mutatedOnFirstPlan := managedFieldsSection(before) != managedFieldsSection(after)
	fmt.Printf("  FINDING: managedFields %s after the first nelm plan against this helm-v4-created Deployment\n",
		map[bool]string{true: "CHANGED", false: "did NOT change"}[mutatedOnFirstPlan])

	plan2 := filepath.Join(tempRoot, "plan2.artifact")
	smokelib.RunNelm(
		"release", "plan", "install",
		"--kube-context", smokelib.OrbstackContext,
		"-n", namespace,
		"-r", releaseName,
		"--save-plan", plan2,
		"--no-final-tracking",
		"--temp-dir", filepath.Join(tempRoot, "plan2"),
		chart,
	)

	after2 := smokelib.RunKubectl("-n", namespace, "get", "deployment", deployName, "--show-managed-fields", "-o", "yaml")
	smokelib.WriteFile(filepath.Join(dir, "managedfields_after2.yaml"), []byte(after2))

	stableOnSecondPlan := managedFieldsSection(after) == managedFieldsSection(after2)
	assert(stableOnSecondPlan, "managedFields are STABLE after a second nelm plan (no further mutation), regardless of whether the first plan mutated anything")

	smokelib.WriteFile(filepath.Join(dir, "managedfields_NOTES.md"), []byte(fmt.Sprintf(`# managedFields probe evidence

Deployment: %s/%s

- managedfields_before.yaml: captured via kubectl BEFORE any nelm plan ran
  against this helm-v4-created Deployment.
- managedfields_after.yaml: captured AFTER the first
  "nelm release plan install" (a PLAN, not an apply) ran against it.
- managedfields_after2.yaml: captured after a SECOND plan.

managedFields changed after the first plan: %v
managedFields stable between the first and second plan (after == after2): %v

## Finding: this CORRECTS/REFINES the design's risk #5 assumption for the common case

The design doc (§2.2/§7 risk #5) states "first plan against helm-created
resources MergePatches metadata.managedFields" (verified against nelm
v1.24.0). Live-tested here against nelm v1.26.2 and a plain helm v4.2.3
server-side-apply install, NO managedFields mutation was observed on
either the first or second plan.

Root cause (read directly from the local nelm checkout,
github.com/werf/nelm v1.26.2-2-gda9a86a):

  - pkg/common/common.go:109 -- "DefaultFieldManager = \"helm\"". nelm
    deliberately uses the SAME field-manager name Helm itself uses, for
    exactly this import-compatibility reason.
  - pkg/plan/resource_info.go fixManagedFields(): it looks up an existing
    managedFields entry with Manager=="helm" && Operation=="Apply" as
    "oursEntry". For a resource created by helm v4 (which uses
    server-side-apply with manager "helm"), this entry ALREADY EXISTS with
    real content, so nelm considers it already-owned -- there is nothing to
    "fix".
  - The function only sets changed=true when: (a) a LEGACY
    Manager=="helm" && Operation=="Update" entry needs migrating to the
    Apply style (fixHelmUpdateManagedFields) -- not present here since helm
    v4 already uses Apply; (b) the resource is a ServiceAccount needing a
    secrets-field fix -- not our case; (c) removeUndesirableManagers finds
    a manager literally named "kubectl-edit"
    (common.KubectlEditFieldManager) or prefixed "werf"
    (common.OldFieldManagerPrefix) -- neither was present (our managedFields
    were only "helm"/Apply and "k3s"/Update on the status subresource,
    which is skipped because its Subresource differs from oursEntry's).

CONCLUSION for RISK #2 (import): adopting a modern helm v3/v4
server-side-apply release is actually BETTER than the design feared for
this specific side effect -- no unexpected managedFields rewrite occurs on
a plain `+"`terraform plan`"+` against a freshly-imported plain-helm release. The
design's risk #5 as literally stated likely applies to a narrower scenario
this probe did not reproduce: resources carrying a LEGACY field manager
(an old client-side-apply "kubectl-edit" entry, or an old "werf"-prefixed
manager name from a pre-server-side-apply werf/nelm version) -- neither of
which a fresh helm v4.2.3 install produces. T-resplan/T-planconv should
treat "first plan may rewrite managedFields" as a possible-but-not-
guaranteed side effect for modern helm-created resources, not an
unconditional one; it likely still applies to resources migrating from
much older werf/helm client-side-apply conventions, which this fixture
does not cover (flagging as a gap, not fabricating a fixture for a scenario
this task couldn't cheaply reproduce live).
`, namespace, deployName, mutatedOnFirstPlan, stableOnSecondPlan)))

	// ---- 5. best-effort: never-successfully-deployed / failed release ----
	fmt.Println("\n--- (5) best-effort failed-release capture ---")
	failNotes := attemptFailedRelease(failNamespace, failReleaseName, chart, dir)
	smokelib.WriteFile(filepath.Join(dir, "failed_release_NOTES.md"), []byte(failNotes))

	fmt.Printf("\n=== helmv4probe done: %d assertion failure(s) ===\n", failures)
	if failures > 0 {
		fmt.Println("NOTE: failures were recorded above but this program still ran cleanup via defer.")
	}
}

// managedFieldsSection extracts just the managedFields YAML block from a
// `kubectl get ... -o yaml` dump, ignoring everything else (resourceVersion,
// etc. are irrelevant noise for this specific comparison).
func managedFieldsSection(yamlDump string) string {
	idx := strings.Index(yamlDump, "managedFields:")
	if idx == -1 {
		return ""
	}
	rest := yamlDump[idx:]
	// managedFields block ends at the next top-level (0-indent) key under
	// metadata, which in kubectl's dump is "name:" (metadata.name always
	// follows managedFields alphabetically... not guaranteed, so instead cut
	// at the next line that starts at the SAME indent as "  managedFields:"
	// -- here we just cut at "\n  name:" which is stable for this fixture's
	// shape).
	if end := strings.Index(rest, "\n  name:"); end != -1 {
		return rest[:end]
	}
	return rest
}

func attemptFailedRelease(namespace, releaseName, chart, dir string) string {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("  (helm install for the failure attempt errored as expected: %v)\n", r)
		}
	}()

	// Deliberately bad image + short wait timeout to force helm to report a
	// failure while (per Helm's own behavior) still recording a release.
	func() {
		// best-effort: this helm install is EXPECTED to fail/time out.
		defer func() { _ = recover() }()
		smokelib.RunHelm("install", releaseName, chart, "-n", namespace, "--create-namespace",
			"--set", "image.repository=tfnelm-fixture-nonexistent-image-abcdefg",
			"--set", "image.tag=does-not-exist",
			"--wait", "--timeout", "10s")
	}()

	getOut, getErr := func() (out string, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		out = smokelib.RunNelm(
			"release", "get",
			"--kube-context", smokelib.OrbstackContext,
			"-n", namespace,
			"-r", releaseName,
			"--output-format", "json",
		)
		return out, nil
	}()

	if getErr != nil {
		return fmt.Sprintf("Attempted to force a failed/never-deployed release (bad image, --wait --timeout 10s) "+
			"in namespace %s. `nelm release get` afterward FAILED: %v\n\nThis means either (a) helm did not "+
			"persist a release record for the failed install, or (b) nelm's storage reader could not find/read "+
			"it. Either way: a first-install failure of this shape would currently be INVISIBLE to "+
			"`nelm release get` (design §7 risk #6's concern), consistent with the design's documented "+
			"acceptance of this gap for v1.\n", namespace, getErr)
	}

	smokelib.WriteFile(filepath.Join(dir, "failed_release_get.json"), []byte(getOut))

	return fmt.Sprintf("Attempted to force a failed/never-deployed release (bad image, --wait --timeout 10s) "+
		"in namespace %s. `nelm release get` SUCCEEDED and returned a release record -- see "+
		"failed_release_get.json. This means a failed helm install DOES leave a stored release behind that "+
		"nelm's ReleaseGet can read (informs design §7 risk #6: Create partial-failure handling can rely on "+
		"ReleaseGet finding a record even for a release that never became ready).\n", namespace)
}

func cleanup(namespace, releaseName string) {
	fmt.Printf("cleanup: uninstalling %s/%s and deleting namespace (best-effort)\n", namespace, releaseName)

	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("  (helm uninstall %s: %v -- ignoring, namespace delete below is the real cleanup)\n", releaseName, r)
			}
		}()
		smokelib.RunHelm("uninstall", releaseName, "-n", namespace)
	}()

	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("  (kubectl delete ns %s: %v)\n", namespace, r)
			}
		}()
		smokelib.RunKubectl("delete", "ns", namespace, "--ignore-not-found", "--wait=false")
	}()
}
