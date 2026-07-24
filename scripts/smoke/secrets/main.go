//go:build smoke

// Command secrets captures the T-fixtures secret/redaction fixtures (task
// deliverable 3):
//
//  1. A plan artifact for testdata/charts/basic (which has a Secret) is
//     captured with an OBVIOUSLY-FAKE secret value (never a real secret,
//     per task instructions) and evidence is saved proving that value
//     appears in CLEARTEXT inside the artifact's `dataRaw` JSON string --
//     this is what justifies the provider's temp-file/delete-in-same-frame
//     policy for plan artifacts (CONTRACTS.md, design §2.1/§7 risk #7).
//  2. The SAME plan also enables testdata/charts/basic's
//     `configMap.sensitivePathsAnnotation` fixture-capture hook, which adds
//     a `werf.io/sensitive-paths: "data.message"` annotation to the
//     ConfigMap (a non-Secret kind) -- so planconv can golden-test
//     path-redaction against a resource that isn't sensitive-by-default.
//  3. Both resources' raw (cleartext) plan-After objects are saved, along
//     with what `resource.GetSensitiveInfo` + `resource.RedactSensitiveData`
//     (the exact functions design §2.1 steps 1-3 specify) produce for each,
//     run directly in this program (no chart/fixture change needed to
//     prove this -- these are pure functions of the captured object).
//
// This program does NOT install anything -- a ReleasePlanInstall against a
// nonexistent namespace/release is sufficient (same as
// scripts/smoke/lifecycle's first-install plan) and requires no cleanup.
//
// PROVENANCE NOTE: like scripts/smoke/lifecycle, the plan itself is
// produced via a nelm CLI subprocess ($NELM_SMOKE_BIN) rather than an
// in-process pkg/action call -- see smokelib/exec.go's doc comment for why.
// Reading/decoding the artifact and the sensitive-info/redaction calls all
// use the real Go library in-process.
//
// Usage:
//
//	NELM_SMOKE_BIN=/path/to/nelm go run -tags smoke ./scripts/smoke/secrets
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/werf/nelm/pkg/resource"

	"github.com/infrabay/terraform-provider-nelm/scripts/smoke/smokelib"
)

const fakePassword = "s3cr3t-fake-9f2c" // obviously-fake, never a real secret

func main() {
	ctx := context.Background()
	ctx = smokelib.InitLogging(ctx)

	// CLUSTER SAFETY: must be the very first thing that can touch a cluster
	// (this program only PLANS, never installs, but the guard still runs
	// first on principle -- every smoke program does).
	smokelib.MustGuardOrbstack(ctx)

	namespace := smokelib.RandNamespace("secrets")
	releaseName := "secrets"
	chart := smokelib.BasicChartPath()

	tempRoot := smokelib.MustTempDir("secrets")
	defer os.RemoveAll(tempRoot)

	artifactPath := filepath.Join(tempRoot, "plan.artifact")

	fmt.Printf("=== secrets capture: namespace=%s release=%s chart=%s (plan-only, no install, no cleanup needed) ===\n", namespace, releaseName, chart)

	smokelib.RunNelm(
		"release", "plan", "install",
		"--kube-context", smokelib.OrbstackContext,
		"-n", namespace,
		"-r", releaseName,
		"--save-plan", artifactPath,
		"--no-final-tracking",
		"--temp-dir", tempRoot,
		"--set", "secret.password="+fakePassword,
		"--set", "configMap.sensitivePathsAnnotation=true",
		chart,
	)

	artifact, err := smokelib.ReadArtifact(ctx, artifactPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: ReadArtifact: %v\n", err)
		os.Exit(1)
	}

	dir := smokelib.FixtureDir("secrets")
	smokelib.CopyFile(artifactPath, filepath.Join(dir, "secret_plan.artifact.json.gz"))
	smokelib.WriteFile(filepath.Join(dir, "secret_plan.decoded.json"), smokelib.MarshalIndent(smokelib.Decode(artifact)))

	// ---- 1. cleartext-in-dataRaw evidence ----
	containsCleartext := strings.Contains(artifact.DataRaw, fakePassword)
	fmt.Printf("\n--- (1) cleartext-in-dataRaw evidence ---\n")
	if containsCleartext {
		fmt.Printf("  ASSERT OK: artifact.DataRaw (the exact JSON string nelm gzips onto disk) contains the fake secret value %q in CLEARTEXT\n", fakePassword)
	} else {
		fmt.Printf("  !!!! ASSERT FAILED (FLAGGED): fake secret value %q NOT found in artifact.DataRaw !!!!\n", fakePassword)
	}

	idx := strings.Index(artifact.DataRaw, fakePassword)
	snippetStart := max(0, idx-80)
	snippetEnd := min(len(artifact.DataRaw), idx+len(fakePassword)+40)
	snippet := ""
	if idx >= 0 {
		snippet = artifact.DataRaw[snippetStart:snippetEnd]
	}

	// ---- 2. find the Secret and annotated-ConfigMap changes ----
	var secretAfter, configMapAfter *unstructured.Unstructured
	var secretMeta, configMapMeta string

	for _, c := range artifact.Data.Changes {
		switch c.ResourceMeta.GroupVersionKind.Kind {
		case "Secret":
			secretAfter = c.After
			secretMeta = fmt.Sprintf("%s %s/%s", c.ResourceMeta.GroupVersionKind, c.ResourceMeta.Namespace, c.ResourceMeta.Name)
		case "ConfigMap":
			configMapAfter = c.After
			configMapMeta = fmt.Sprintf("%s %s/%s", c.ResourceMeta.GroupVersionKind, c.ResourceMeta.Namespace, c.ResourceMeta.Name)
		}
	}

	if secretAfter == nil || configMapAfter == nil {
		fmt.Fprintln(os.Stderr, "FATAL: expected both Secret and ConfigMap changes in the plan, got neither/one")
		os.Exit(1)
	}

	smokelib.WriteFile(filepath.Join(dir, "secret_after.raw.json"), smokelib.MarshalIndent(secretAfter.Object))
	smokelib.WriteFile(filepath.Join(dir, "configmap_after.raw.json"), smokelib.MarshalIndent(configMapAfter.Object))

	// ---- 3. resource.GetSensitiveInfo + resource.RedactSensitiveData (design §2.1 steps 1-3) ----
	secretInfo := resource.GetSensitiveInfo(secretAfter.GroupVersionKind().GroupKind(), secretAfter.GetAnnotations())
	configMapInfo := resource.GetSensitiveInfo(configMapAfter.GroupVersionKind().GroupKind(), configMapAfter.GetAnnotations())

	fmt.Printf("\n--- (2) resource.GetSensitiveInfo ---\n")
	fmt.Printf("  Secret %s: IsSensitive=%v SensitivePaths=%v FullySensitive=%v\n", secretMeta, secretInfo.IsSensitive, secretInfo.SensitivePaths, secretInfo.FullySensitive())
	fmt.Printf("  ConfigMap (werf.io/sensitive-paths annotated) %s: IsSensitive=%v SensitivePaths=%v FullySensitive=%v\n", configMapMeta, configMapInfo.IsSensitive, configMapInfo.SensitivePaths, configMapInfo.FullySensitive())

	secretRedacted := resource.RedactSensitiveData(secretAfter, secretInfo.SensitivePaths)
	configMapRedacted := resource.RedactSensitiveData(configMapAfter, configMapInfo.SensitivePaths)

	smokelib.WriteFile(filepath.Join(dir, "secret_redacted.json"), smokelib.MarshalIndent(secretRedacted.Object))
	smokelib.WriteFile(filepath.Join(dir, "configmap_redacted.json"), smokelib.MarshalIndent(configMapRedacted.Object))

	fmt.Printf("\n--- (3) resource.RedactSensitiveData output ---\n")
	fmt.Printf("  Secret redacted (default HideAll -- V1 behavior, global featgate off): %s\n", string(smokelib.MarshalIndent(secretRedacted.Object)))
	fmt.Printf("  ConfigMap redacted (path-specific, only data.message hidden): %s\n", string(smokelib.MarshalIndent(configMapRedacted.Object)))

	notes := fmt.Sprintf(`# secrets/redaction fixture evidence notes

Captured by scripts/smoke/secrets against nelm CLI (%s), release %s/%s
(plan-only, no install, no cleanup needed -- namespace was never created).
Chart: %s, with --set secret.password=%s --set configMap.sensitivePathsAnnotation=true.

## (1) Cleartext-in-dataRaw evidence

fake secret value %q found in artifact.DataRaw (the JSON string nelm gzips
onto disk as the plan artifact's dataRaw field): %v

Snippet from artifact.DataRaw around the fake password (proves the Secret's
stringData is stored in PLAIN CLEARTEXT in the plan artifact, not hashed or
redacted by nelm itself -- redaction is the CONSUMER's responsibility,
exactly as design §2.1/§7 risk #7 states):

    ...%s...

This is why CONTRACTS.md mandates: plan artifacts are read and deleted in
the SAME function call frame, under a 0700 per-op temp dir, and never
persist past that call.

## (2) resource.GetSensitiveInfo results

- Secret %s: IsSensitive=%v SensitivePaths=%v FullySensitive=%v
  (default Secret behavior, V1/HideAll -- global FeatGateFieldSensitive is
  OFF in this codebase per CONTRACTS.md, so nelm's own default for Secret
  kind is the full-skeleton HideAll, not path-specific. The PROVIDER
  overrides this locally per design §2.1 step 2 -- see
  configmap_redacted.json below for what path-specific redaction produces,
  which is what the provider will replicate for Secrets too.)
- ConfigMap (werf.io/sensitive-paths: "data.message" annotated) %s:
  IsSensitive=%v SensitivePaths=%v FullySensitive=%v
  (annotation-driven path redaction works on ANY kind, not just Secret --
  resource.GetSensitiveInfo checks the werf.io/sensitive-paths annotation
  before falling back to any Secret-specific default.)

## (3) resource.RedactSensitiveData output

See secret_redacted.json (nelm's own default HideAll skeleton -- the
provider will NOT use this shape for Secrets, see design §2.1 step 2) and
configmap_redacted.json (path-specific: data.message replaced with a
deterministic "<hidden N sensitive bytes, hash sha256[:12]>" placeholder,
everything else -- name, labels, other data keys if any -- untouched).

## Files in this directory

- secret_plan.artifact.json.gz -- raw gzip plan artifact (as nelm wrote it)
- secret_plan.decoded.json -- decoded/indented JSON of the same artifact
- secret_after.raw.json -- the Secret's plan-After object, RAW/cleartext
- configmap_after.raw.json -- the annotated ConfigMap's plan-After object, RAW/cleartext
- secret_redacted.json -- resource.RedactSensitiveData(secret, HideAll) output
- configmap_redacted.json -- resource.RedactSensitiveData(configmap, ["data.message"]) output
`,
		smokelib.NelmBin(), namespace, releaseName, chart, fakePassword,
		fakePassword, containsCleartext, snippet,
		secretMeta, secretInfo.IsSensitive, secretInfo.SensitivePaths, secretInfo.FullySensitive(),
		configMapMeta, configMapInfo.IsSensitive, configMapInfo.SensitivePaths, configMapInfo.FullySensitive(),
	)
	smokelib.WriteFile(filepath.Join(dir, "NOTES.md"), []byte(notes))

	fmt.Println("\ndone.")
}
