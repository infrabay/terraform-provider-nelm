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
//  2. featgate.FeatGateRemoteCharts.Enable() — allows oci:// and repo/name
//     chart references in addition to local paths.
//  3. A per-process temp-directory root (cleaned up by each action's own
//     per-op subdirectory; see actions.go), avoiding the temp-dir leak that
//     results from never setting TempDirPath.
//
// Init is idempotent: the underlying setup runs exactly once via sync.Once,
// no matter how many times or from how many goroutines Init is called. It
// MUST be the only caller of log.SetupLogging and any featgate.*.Enable() in
// this codebase (CONTRACTS.md global-state rule).
func Init(ctx context.Context) error {
	initOnce.Do(func() {
		log.SetupLogging(ctx, log.InfoLevel, log.SetupLoggingOptions{
			ColorMode:      log.LogColorModeOff,
			LogIsParseable: true,
		})

		featgate.FeatGateRemoteCharts.Enable()

		dir, err := os.MkdirTemp("", "tf-nelm-*")
		if err != nil {
			initErr = fmt.Errorf("create nelm temp root: %w", err)
			return
		}

		tempRoot = dir
	})

	return initErr
}

// TempRoot returns the process-wide temp directory root created by Init.
// Init must have returned successfully before TempRoot is meaningful.
func TempRoot() string {
	return tempRoot
}
