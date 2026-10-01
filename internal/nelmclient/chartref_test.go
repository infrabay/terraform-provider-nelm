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
		wantAbs    bool   // want is relative to tmpDir; resolve via filepath.Join
		wantRepo   string // the ChartRepoURL handed to nelm
		wantErr    bool
	}{
		{
			// The repository-bypass regression: a bare name that HAPPENS to
			// exist as a local directory must stay remote when a repository is
			// set, or the repo would be silently defeated by an unrelated
			// same-named directory on disk.
			name:       "bare name that exists on disk stays remote when repository is set",
			chart:      "mychart",
			repository: "https://charts.example.com",
			want:       "mychart",
			wantAbs:    false,
			wantRepo:   "https://charts.example.com",
		},
		{
			name:       "repo/chart with repository set passes through",
			chart:      "myrepo/mychart",
			repository: "https://charts.example.com",
			want:       "myrepo/mychart",
			wantAbs:    false,
			wantRepo:   "https://charts.example.com",
		},
		{
			// helm_release's OCI form. Passed to nelm as-is, the repository is
			// fetched as a classic index.yaml repo and every plan fails ("not
			// a valid chart repository ... object required"); it must be
			// folded into one oci:// chart ref.
			name:       "oci:// repository is folded into the chart reference",
			chart:      "app",
			repository: "oci://us-central1-docker.pkg.dev/my-project/charts",
			want:       "oci://us-central1-docker.pkg.dev/my-project/charts/app",
		},
		{
			name:       "oci:// repository with a trailing slash",
			chart:      "app",
			repository: "oci://registry.example.com/charts/",
			want:       "oci://registry.example.com/charts/app",
		},
		{
			name:       "oci:// repository that is a bare registry host",
			chart:      "app",
			repository: "oci://registry.example.com",
			want:       "oci://registry.example.com/app",
		},
		{
			name:       "oci:// repository with a port and a nested chart path",
			chart:      "team/app",
			repository: "oci://localhost:5000/helm-charts",
			want:       "oci://localhost:5000/helm-charts/team/app",
		},
		{
			name:       "mixed-case OCI:// scheme is folded and canonicalized",
			chart:      "app",
			repository: "OCI://registry.example.com/charts",
			want:       "oci://registry.example.com/charts/app",
		},
		{
			// A same-named local directory must not hijack the oci:// form
			// either: the repository still makes the chart remote.
			name:       "bare name that exists on disk is folded, not localized, with an oci:// repository",
			chart:      "mychart",
			repository: "oci://registry.example.com/charts",
			want:       "oci://registry.example.com/charts/mychart",
		},
		{
			name:       "oci:// repository without a registry host is an error",
			chart:      "app",
			repository: "oci:///charts",
			wantErr:    true,
		},
		{
			name:       "full oci:// chart with an oci:// repository is a contradiction error",
			chart:      "oci://registry.example.com/charts/app",
			repository: "oci://registry.example.com/charts",
			wantErr:    true,
		},
		{
			name:       "full oci:// chart with a classic repository is a contradiction error",
			chart:      "oci://registry.example.com/charts/app",
			repository: "https://charts.example.com",
			wantErr:    true,
		},
		{
			name:       "local path with an oci:// repository is a contradiction error",
			chart:      "./mychart",
			repository: "oci://registry.example.com/charts",
			wantErr:    true,
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
			got, gotRepo, err := NormalizeChartRef(tt.chart, tt.repository)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeChartRef(%q, %q) = %q, %q, nil; want error", tt.chart, tt.repository, got, gotRepo)
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

			if gotRepo != tt.wantRepo {
				t.Fatalf("NormalizeChartRef(%q, %q) repoURL = %q, want %q", tt.chart, tt.repository, gotRepo, tt.wantRepo)
			}

			// toReleaseSpec normalizes once and every Client method
			// normalizes the resulting spec again, so the output must be a
			// fixed point (an oci:// repository folded twice would yield
			// ".../app/app" or reject its own output).
			again, againRepo, err := NormalizeChartRef(got, gotRepo)
			if err != nil {
				t.Fatalf("NormalizeChartRef(%q, %q) (second pass) unexpected error: %v", got, gotRepo, err)
			}
			if again != got || againRepo != gotRepo {
				t.Fatalf("NormalizeChartRef is not idempotent: (%q, %q) -> (%q, %q)", got, gotRepo, again, againRepo)
			}
		})
	}
}
