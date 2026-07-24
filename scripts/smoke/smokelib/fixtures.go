//go:build smoke

package smokelib

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WriteFile writes data to destPath (0644), creating parent directories
// (0755) as needed. All fixture files are non-sensitive (fake secret values
// only, per task instructions) so world-readable perms are fine.
func WriteFile(destPath string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		panic(fmt.Errorf("mkdir for %s: %w", destPath, err))
	}

	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		panic(fmt.Errorf("write %s: %w", destPath, err))
	}

	fmt.Printf("wrote fixture: %s\n", destPath)
}

// CopyFile copies srcPath's exact bytes to destPath (0644), creating parent
// directories as needed. Used to save the raw gzip plan-artifact bytes
// unmodified alongside the decoded JSON sibling.
func CopyFile(srcPath, destPath string) {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		panic(fmt.Errorf("mkdir for %s: %w", destPath, err))
	}

	src, err := os.Open(srcPath)
	if err != nil {
		panic(fmt.Errorf("open %s: %w", srcPath, err))
	}
	defer src.Close()

	dst, err := os.Create(destPath)
	if err != nil {
		panic(fmt.Errorf("create %s: %w", destPath, err))
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		panic(fmt.Errorf("copy %s -> %s: %w", srcPath, destPath, err))
	}

	fmt.Printf("wrote fixture: %s\n", destPath)
}
