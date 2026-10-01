//go:build smoke

package smokelib

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// NelmBin returns the path to the nelm CLI binary every smoke program
// shells out to for mutating/planning/get operations (release plan install,
// release install, release get, release uninstall).
//
// WHY A SUBPROCESS AND NOT github.com/werf/nelm/pkg/action DIRECTLY: at
// capture time, this module's go.sum was missing transitive-dependency entries
// for pkg/action's CLI-output-formatting deps (alecthomas/chroma/v2,
// dustin/go-humanize, jedib0t/go-pretty/v6), and the capture was kept
// independent of go.mod/go.sum changes. The nelm CLI binary is built straight
// from the exact pinned commit (github.com/werf/nelm v1.26.2, tag v1.26.2-2)
// via `go build ./cmd/nelm` in ITS OWN separately-go.sum'd module -- it is a
// thin wrapper around the exact same action.ReleasePlanInstall/ReleaseInstall/
// ReleaseGet/ReleaseUninstall functions the harness would otherwise call
// in-process, so the resulting PlanArtifact/ReleaseGetResultV1 JSON is
// byte-for-byte what those functions would have produced. Every fixture's
// provenance note records this explicitly. Reading/decoding/analyzing the
// artifacts (pkg/plan.ReadPlanArtifact, pkg/resource/spec.CleanUnstruct,
// pkg/resource sensitive-info helpers, pkg/kube live GETs) all compile fine
// in-process and are NOT worked around -- only the mutating CLI actions are.
//
// Set SMOKE_NELM_BIN to override (defaults to "nelm" on PATH, i.e. a
// separately-installed nelm CLI, if the exact-pinned-version binary isn't
// provided).
//
// Deliberately NOT named with an "NELM_"-prefix: the nelm CLI itself parses
// every "NELM_*" environment variable as its own config (see any `--foo`
// flag's "Vars: $NELM_FOO" help text) and prints a warning to stdout for
// ones it doesn't recognize -- which would otherwise land in the middle of
// JSON output callers like helmv4probe parse from RunNelm's return value.
func NelmBin() string {
	if v := os.Getenv("SMOKE_NELM_BIN"); v != "" {
		return v
	}

	return "nelm"
}

// RunNelm runs the nelm CLI binary with args, always printing the exact
// command line first (provenance: every fixture's exact capture command
// must be reconstructable from program output). Panics on non-zero exit
// (a throwaway capture program should stop loudly, not silently continue
// with a partial fixture). Returns ONLY stdout -- callers that need to
// parse structured output (e.g. `release get --output-format json`) get a
// clean value with no stderr noise (progress logs, warnings) mixed in;
// stderr is still printed to the console for visibility, just not returned.
func RunNelm(args ...string) string {
	return mustRun(NelmBin(), args...)
}

// RunKubectl runs kubectl with args, ALWAYS prepending "--context orbstack"
// -- no caller may omit it. Panics on non-zero exit. Returns stdout only.
func RunKubectl(args ...string) string {
	full := append([]string{"--context", OrbstackContext}, args...)
	return mustRun("kubectl", full...)
}

// RunHelm runs the local helm CLI with args, ALWAYS prepending
// "--kube-context orbstack" -- no caller may omit it. Panics on non-zero
// exit. Returns stdout only.
func RunHelm(args ...string) string {
	full := append([]string{"--kube-context", OrbstackContext}, args...)
	return mustRun("helm", full...)
}

func mustRun(bin string, args ...string) string {
	fmt.Printf("+ %s %s\n", bin, strings.Join(args, " "))

	var stdout, stderr bytes.Buffer

	cmd := exec.Command(bin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	fmt.Print(stdout.String())
	if stderr.Len() > 0 {
		fmt.Fprint(os.Stderr, stderr.String())
	}

	if err != nil {
		panic(fmt.Errorf("command failed: %s %s: %w\nstdout: %s\nstderr: %s", bin, strings.Join(args, " "), err, stdout.String(), stderr.String()))
	}

	return stdout.String()
}
