package nelmclient

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeChartRef(t *testing.T) {
	tmpDir := t.TempDir()

	localChartDir := filepath.Join(tmpDir, "mychart")
	if err := os.Mkdir(localChartDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Run relative-path cases with the process cwd pinned to tmpDir so
	// "mychart" (bare) and "./mychart" resolve deterministically regardless
	// of where `go test` happens to be invoked from.
	t.Chdir(tmpDir)

	tests := []struct {
		name       string
		chart      string
		repository string
		want       string
		wantAbs    bool // want is relative to tmpDir; resolve via filepath.Join
		wantErr    bool
	}{
		{
			// The repository-bypass regression (Opus 5 review): a bare name
			// that HAPPENS to exist as a local directory must stay remote
			// when a repository is set, or the repo would be silently
			// defeated by an unrelated same-named directory on disk.
			name:       "bare name that exists on disk stays remote when repository is set",
			chart:      "mychart",
			repository: "https://charts.example.com",
			want:       "mychart",
			wantAbs:    false,
		},
		{
			name:       "repo/chart with repository set passes through",
			chart:      "myrepo/mychart",
			repository: "https://charts.example.com",
			want:       "myrepo/mychart",
			wantAbs:    false,
		},
		{
			name:       "local absolute path with repository set is a contradiction error",
			chart:      localChartDir,
			repository: "https://charts.example.com",
			wantErr:    true,
		},
		{
			name:       "explicit relative path with repository set is a contradiction error",
			chart:      "./mychart",
			repository: "https://charts.example.com",
			wantErr:    true,
		},
		{
			name:    "already absolute path is kept as-is",
			chart:   localChartDir,
			want:    localChartDir,
			wantAbs: false,
		},
		{
			name:    "dot-slash relative path is absolutized even if it does not exist",
			chart:   "./does-not-exist-anywhere",
			want:    "does-not-exist-anywhere",
			wantAbs: true,
		},
		{
			name:    "dot-dot-slash relative path is absolutized even if it does not exist",
			chart:   "../does-not-exist-either",
			want:    "../does-not-exist-either",
			wantAbs: true,
		},
		{
			name:    "bare relative name that exists on disk is absolutized",
			chart:   "mychart",
			want:    "mychart",
			wantAbs: true,
		},
		{
			name:    "bare relative name that does NOT exist on disk passes through untouched (remote)",
			chart:   "this-relative-chart-path-does-not-exist-anywhere",
			want:    "this-relative-chart-path-does-not-exist-anywhere",
			wantAbs: false,
		},
		{
			name:    "oci:// reference passes through untouched",
			chart:   "oci://example.com/charts/does-not-matter",
			want:    "oci://example.com/charts/does-not-matter",
			wantAbs: false,
		},
		{
			name:    "repo/chart reference passes through untouched",
			chart:   "myrepo/mychart",
			want:    "myrepo/mychart",
			wantAbs: false,
		},
		{
			name:    "empty chart reference is an error",
			chart:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeChartRef(tt.chart, tt.repository)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeChartRef(%q) = %q, nil; want error", tt.chart, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeChartRef(%q) unexpected error: %v", tt.chart, err)
			}

			want := tt.want
			if tt.wantAbs {
				abs, err := filepath.Abs(tt.want)
				if err != nil {
					t.Fatalf("filepath.Abs(%q): %v", tt.want, err)
				}
				want = abs
			}

			if got != want {
				t.Fatalf("NormalizeChartRef(%q) = %q, want %q", tt.chart, got, want)
			}

			if tt.wantAbs && !filepath.IsAbs(got) {
				t.Fatalf("NormalizeChartRef(%q) = %q, want an absolute path", tt.chart, got)
			}
		})
	}
}
