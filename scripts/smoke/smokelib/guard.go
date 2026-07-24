//go:build smoke

// Package smokelib is the shared connection/guard helper imported by every
// live-capture program under scripts/smoke/ (T-fixtures, design §4 Phase B
// wave 1). Every file in this package carries the "smoke" build tag, so the
// package contributes zero buildable files (and therefore never breaks
// `go build ./...` / `go vet ./...`) unless invoked with `-tags smoke`.
//
// This package is intentionally standalone: it does NOT import
// internal/nelmclient (that package is owned by the parallel T-nelmclient
// task and may still be stubbed while T-fixtures runs) or any other
// internal/* package. It talks to nelm's pkg/action, pkg/plan, pkg/kube,
// pkg/resource, pkg/resource/spec directly, exactly like a standalone
// operator script would.
package smokelib

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/werf/nelm/pkg/common"
	"github.com/werf/nelm/pkg/kube"
)

// OrbstackContext is the ONLY kubeconfig context any smoke program is ever
// allowed to touch. current-context is NEVER consulted anywhere in this
// package or its callers — every KubeConnectionOptions built here pins
// KubeContextCurrent explicitly.
const OrbstackContext = "orbstack"

// MustGuardOrbstack loads the "orbstack" kubeconfig context through nelm's
// own config loader (kube.NewKubeConfig) — the same loader production code
// will use — and hard-asserts the resolved API server host is local
// (127.0.0.1 or localhost). It never reads or trusts kubeconfig
// current-context. On any failure (context missing/unresolvable, or a
// non-local server — e.g. a GKE endpoint) it prints a loud diagnostic to
// stderr and calls os.Exit(1): no further cluster contact happens.
//
// Every //go:build smoke program under scripts/smoke/ MUST call this before
// doing anything else that could reach a cluster.
func MustGuardOrbstack(ctx context.Context) *kube.KubeConfig {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ORBSTACK GUARD FAILED: could not determine home directory: %v\n", err)
		os.Exit(1)
	}

	connOpts := ConnectionOptions()
	// Mirrors what every action.Release* function does internally
	// (applyRelease*OptionsDefaults -> KubeConnectionOptions.ApplyDefaults):
	// defaults KubeConfigPaths to ~/.kube/config when unset. We call
	// kube.NewKubeConfig directly (not through an action func), so we must
	// replicate this default ourselves or context resolution fails.
	connOpts.ApplyDefaults(homeDir)

	kubeConfig, err := kube.NewKubeConfig(ctx, connOpts.KubeConfigPaths, kube.KubeConfigOptions{
		KubeConnectionOptions: connOpts,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ORBSTACK GUARD FAILED: could not load kubeconfig context %q: %v\n", OrbstackContext, err)
		os.Exit(1)
	}

	host := kubeConfig.RestConfig.Host
	if !isLocalHost(host) {
		fmt.Fprintf(os.Stderr,
			"ORBSTACK GUARD FAILED: context %q resolves to server %q -- expected an https://127.0.0.1*"+
				" or https://localhost* endpoint. This looks like it could be a remote (possibly"+
				" production) cluster. REFUSING to proceed.\n", OrbstackContext, host)
		os.Exit(1)
	}

	fmt.Printf("orbstack guard OK: context=%q server=%q\n", OrbstackContext, host)

	return kubeConfig
}

func isLocalHost(host string) bool {
	for _, prefix := range []string{"https://127.0.0.1", "http://127.0.0.1", "https://localhost", "http://localhost"} {
		if strings.HasPrefix(host, prefix) {
			return true
		}
	}

	return false
}

// ConnectionOptions returns the common.KubeConnectionOptions every nelm
// action call in scripts/smoke must embed: pinned to the "orbstack" context,
// explicit, never inherited from environment or kubeconfig current-context.
func ConnectionOptions() common.KubeConnectionOptions {
	return common.KubeConnectionOptions{
		KubeContextCurrent: OrbstackContext,
	}
}
