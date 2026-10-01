package nelmclient

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/werf/nelm/pkg/featgate"
	"github.com/werf/nelm/pkg/log"
)

var (
	initOnce sync.Once
	initErr  error
	tempRoot string
)

// Init performs the process-wide, one-time Nelm bootstrap:
//
//  1. log.SetupLogging (ColorMode off) — must be called exactly once per
//     process, never per-CRUD-call, or nelm's global logging/color/klog
//     state gets clobbered mid-operation.
//  2. pinGates — every nelm feature gate is forced on or off, so the
//     provider's behaviour never depends on NELM_FEAT_* in the environment
//     Terraform runs in.
//  3. A per-process temp-directory root (cleaned up by each action's own
//     per-op subdirectory; see actions.go), avoiding the temp-dir leak that
//     results from never setting TempDirPath.
//
// Init is idempotent: the underlying setup runs exactly once via sync.Once,
// no matter how many times or from how many goroutines Init is called. It
// MUST be the only caller of log.SetupLogging and any featgate.*.Enable() or
// Disable() in this codebase (CONTRACTS.md global-state rule).
func Init(ctx context.Context) error {
	initOnce.Do(func() {
		log.SetupLogging(ctx, log.InfoLevel, log.SetupLoggingOptions{
			ColorMode:      log.LogColorModeOff,
			LogIsParseable: true,
		})

		pinGates()

		dir, err := os.MkdirTemp("", "tf-nelm-*")
		if err != nil {
			initErr = fmt.Errorf("create nelm temp root: %w", err)
			return
		}

		tempRoot = dir
	})

	return initErr
}

// pinGates enables featgate.FeatGateRemoteCharts (oci:// and repo/name chart
// references in addition to local paths) and disables every other nelm
// feature gate. An unforced gate reads NELM_FEAT_<NAME> from the process
// environment, which a provider inherits from Terraform — and teams that also
// run the nelm CLI often export e.g. NELM_FEAT_PREVIEW_V2=true. Left unpinned,
// that would silently change redaction (werf.io/sensitive: "true" on a
// non-Secret would expose everything but data/stringData in plan output and
// state), rewrite manifests (clean-null-fields), fetch validation schemas
// from the internet at plan time (resource-validation), and make the stored
// resources map depend on which machine ran the plan.
func pinGates() {
	for _, g := range featgate.FeatGates {
		if g == featgate.FeatGateRemoteCharts {
			g.Enable()
		} else {
			g.Disable()
		}
	}
}

// TempRoot returns the process-wide temp directory root created by Init.
// Init must have returned successfully before TempRoot is meaningful.
func TempRoot() string {
	return tempRoot
}
