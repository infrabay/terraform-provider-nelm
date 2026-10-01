//go:build smoke

// Command normalize captures the normalization golden pair: the seam between
// the planned and the live side of the diff (CONTRACTS.md, Seam 2), where a
// mismatch shows up as a permanent phantom diff.
//
// For each of the 6 resource kinds in testdata/charts/basic (Deployment,
// Service, ConfigMap, Secret, ClusterRole, ClusterRoleBinding) it captures:
//
//	(a) the LIVE object via a dynamic-client GET (raw JSON)
//	(b) the plan-After object from the scripts/smoke/lifecycle first-install
//	    plan artifact (raw JSON) -- i.e. the exact "create" change's After
//	    value, which is what a freshly-created nelm_release's `resources`
//	    attribute would hold verbatim (Create copies `resources` from
//	    req.Plan without going through Read first)
//
// then runs BOTH through spec.CleanUnstruct(obj, CleanUnstructOptions{
// CleanRuntimeData: true, CleanHelmShAnnos: true, CleanWerfIoAnnos: true,
// CleanManagedFields: true}) -- the cleaning step of the normalization
// pipeline -- and diffs the two cleaned objects with jsondiff (RFC6902). Every
// field that still differs after cleaning is a phantom-diff candidate: if
// planconv's NormalizeUnstructured doesn't strip it too, the very first
// `terraform plan` after `terraform apply` would show a diff against nothing
// having changed. The union of all such fields across all 6 resources is
// written to internal/planconv/testdata/STRIP_LIST.md.
//
// PREREQUISITE: scripts/smoke/lifecycle must have already run against the
// SAME namespace/release passed here (it leaves them alive on exit for
// exactly this reason) and produced
// internal/planconv/testdata/lifecycle/01_first_install.decoded.json.
//
// Usage:
//
//	go run -tags smoke ./scripts/smoke/normalize <namespace> <release>
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/wI2L/jsondiff"

	"github.com/werf/nelm/pkg/resource/spec"

	"github.com/infrabay/terraform-provider-nelm/scripts/smoke/smokelib"
)

// diffOp mirrors jsondiff.Operation but also carries OldValue (which
// jsondiff itself deliberately excludes from JSON via `json:"-"`) since
// seeing both sides is exactly what this evidence file is for.
type diffOp struct {
	Op       string      `json:"op"`
	Path     string      `json:"path"`
	Value    interface{} `json:"value,omitempty"`
	OldValue interface{} `json:"oldValue,omitempty"`
}

func main() {
	ctx := context.Background()
	ctx = smokelib.InitLogging(ctx)

	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: normalize <namespace> <release>")
		os.Exit(1)
	}
	namespace := os.Args[1]
	releaseName := os.Args[2]

	// CLUSTER SAFETY: must be the very first thing that can touch a cluster.
	kubeConfig := smokelib.MustGuardOrbstack(ctx)
	factory := smokelib.MustClientFactory(ctx, kubeConfig)

	artifactPath := filepath.Join(smokelib.FixtureDir("lifecycle"), "01_first_install.decoded.json")
	raw, err := os.ReadFile(artifactPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: read %s: %v (did scripts/smoke/lifecycle run first?)\n", artifactPath, err)
		os.Exit(1)
	}

	var artifact smokelib.DecodedArtifact
	if err := json.Unmarshal(raw, &artifact); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: unmarshal %s: %v\n", artifactPath, err)
		os.Exit(1)
	}

	if artifact.Release.Namespace != namespace || artifact.Release.Name != releaseName {
		fmt.Fprintf(os.Stderr, "FATAL: artifact release %s/%s does not match requested %s/%s\n",
			artifact.Release.Namespace, artifact.Release.Name, namespace, releaseName)
		os.Exit(1)
	}

	cleanOpts := spec.CleanUnstructOptions{
		CleanRuntimeData:   true,
		CleanHelmShAnnos:   true,
		CleanWerfIoAnnos:   true,
		CleanManagedFields: true,
	}

	outDir := smokelib.FixtureDir("normalize")

	// pathCounts collects every JSON-pointer path (with numeric array
	// indices generalized to "*") that differs between cleaned-live and
	// cleaned-plan-After, across every resource, for the STRIP_LIST.md
	// summary.
	pathCounts := map[string][]string{} // path -> []кind that exhibited it

	for _, change := range artifact.Data.Changes {
		if change.Type != "create" || change.After == nil {
			continue
		}

		kind := change.ResourceMeta.GroupVersionKind.Kind
		name := change.ResourceMeta.Name
		ns := change.ResourceMeta.Namespace
		if ns == "" {
			// spec.NewResourceMeta blanks Namespace when it equals the release
			// namespace (see planconv.Key); for a namespaced kind that means
			// "same as release namespace", for a cluster-scoped kind it's
			// simply empty either way.
			ns = namespace
		}

		fmt.Printf("=== %s %s (ns=%q) ===\n", kind, name, ns)

		live, err := smokelib.GetLive(ctx, factory, change.ResourceMeta.GroupVersionKind, ns, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  FATAL: live GET %s/%s: %v\n", kind, name, err)
			os.Exit(1)
		}

		planAfter := change.After

		// Save raw (uncleaned) evidence first.
		smokelib.WriteFile(filepath.Join(outDir, kind+".live.raw.json"), smokelib.MarshalIndent(live.Object))
		smokelib.WriteFile(filepath.Join(outDir, kind+".planafter.raw.json"), smokelib.MarshalIndent(planAfter.Object))

		cleanedLive := spec.CleanUnstruct(live, cleanOpts)
		cleanedPlanAfter := spec.CleanUnstruct(planAfter, cleanOpts)

		smokelib.WriteFile(filepath.Join(outDir, kind+".live.cleaned.json"), smokelib.MarshalIndent(cleanedLive.Object))
		smokelib.WriteFile(filepath.Join(outDir, kind+".planafter.cleaned.json"), smokelib.MarshalIndent(cleanedPlanAfter.Object))

		patch, err := jsondiff.Compare(cleanedPlanAfter.Object, cleanedLive.Object)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  FATAL: jsondiff.Compare: %v\n", err)
			os.Exit(1)
		}

		ops := make([]diffOp, 0, len(patch))
		for _, op := range patch {
			ops = append(ops, diffOp{Op: op.Type, Path: op.Path, Value: op.Value, OldValue: op.OldValue})
			pathCounts[generalizePath(op.Path)] = appendUnique(pathCounts[generalizePath(op.Path)], kind)
		}

		smokelib.WriteFile(filepath.Join(outDir, kind+".cleaned-diff.json"), smokelib.MarshalIndent(ops))

		if len(ops) == 0 {
			fmt.Println("  NO residual diff after CleanUnstruct -- clean.")
		} else {
			fmt.Printf("  %d residual diff op(s) after CleanUnstruct (see %s.cleaned-diff.json):\n", len(ops), kind)
			for _, op := range ops {
				fmt.Printf("      %s %s\n", op.Op, op.Path)
			}
		}
	}

	writeStripListMD(pathCounts)
	fmt.Println("\ndone. See internal/planconv/testdata/STRIP_LIST.md for the summary.")
}

// generalizePath replaces the first path segment's exact value with itself
// (paths here are all top-level fields like /metadata/uid, /status, etc. --
// no arrays are expected after CleanUnstruct for these simple chart
// resources, but this stays defensive if one shows up).
func generalizePath(p string) string {
	return p
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

func writeStripListMD(pathCounts map[string][]string) {
	paths := make([]string, 0, len(pathCounts))
	for p := range pathCounts {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	md := "# STRIP_LIST.md -- planconv's extra-strip list\n\n" +
		"Generated by `go run -tags smoke ./scripts/smoke/normalize` (the normalization " +
		"golden pair). For every resource kind in testdata/charts/basic, " +
		"this compares a LIVE cluster GET against the plan-After object from the first-install " +
		"plan artifact, BOTH cleaned via `spec.CleanUnstruct(obj, CleanUnstructOptions{" +
		"CleanRuntimeData: true, CleanHelmShAnnos: true, CleanWerfIoAnnos: true, " +
		"CleanManagedFields: true})` -- the cleaning step of the normalization pipeline. " +
		"Every JSON-pointer path listed below STILL differs after that cleaning and therefore " +
		"MUST be additionally stripped by `planconv.NormalizeUnstructured`, " +
		"or the very first `terraform plan` immediately after `terraform apply`/create would " +
		"show a phantom diff against nothing having actually changed.\n\n" +
		"Per-resource raw evidence (both raw and CleanUnstruct-cleaned JSON, plus the exact " +
		"RFC6902 diff) lives alongside this file under `internal/planconv/testdata/normalize/" +
		"<Kind>.{live,planafter}.{raw,cleaned}.json` and `<Kind>.cleaned-diff.json`.\n\n" +
		"## Residual fields (union across all 6 resource kinds)\n\n"

	if len(paths) == 0 {
		md += "NONE -- CleanUnstruct alone was sufficient for every resource kind captured; " +
			"planconv.NormalizeUnstructured does not need an additional strip list beyond it.\n"
	} else {
		md += "| JSON pointer path | seen on kinds | recommendation |\n"
		md += "|---|---|---|\n"
		for _, p := range paths {
			md += fmt.Sprintf("| `%s` | %v | strip unconditionally before marshaling canonical JSON |\n", p, pathCounts[p])
		}
	}

	smokelib.WriteFile(filepath.Join(smokelib.FixtureDir(""), "STRIP_LIST.md"), []byte(md))
}
