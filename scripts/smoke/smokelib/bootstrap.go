//go:build smoke

package smokelib

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/werf/nelm/pkg/log"
)

// InitLogging sets up nelm's logging for a smoke program. Each `go run`
// invocation is its own OS process, so — unlike production
// internal/nelmclient.Init (CONTRACTS.md: sync.Once, called once per
// process) — there is no cross-call reuse concern here; every smoke
// program's main() calls this exactly once anyway, at the top.
func InitLogging(ctx context.Context) context.Context {
	return log.SetupLogging(ctx, log.InfoLevel, log.SetupLoggingOptions{
		ColorMode:      "off",
		LogIsParseable: true,
	})
}

// MustTempDir creates a fresh 0700 temp directory prefixed
// tf-nelm-smoke-<tag>-, mirroring the per-op TempDirPath discipline
// CONTRACTS.md requires of production nelmclient action calls. Callers
// should `defer os.RemoveAll(dir)`.
func MustTempDir(tag string) string {
	dir, err := os.MkdirTemp("", "tf-nelm-smoke-"+tag+"-")
	if err != nil {
		panic(fmt.Errorf("create temp dir: %w", err))
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		panic(fmt.Errorf("chmod temp dir: %w", err))
	}

	return dir
}

// RandNamespace returns a throwaway namespace name "tfnelm-fix-<tag>-<hex>"
// per the task's cluster-safety rules (unique per run, always cleaned up by
// the caller).
func RandNamespace(tag string) string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}

	return fmt.Sprintf("tfnelm-fix-%s-%s", tag, hex.EncodeToString(b))
}
