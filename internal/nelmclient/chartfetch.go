package nelmclient

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/werf/nelm/pkg/common"
	"github.com/werf/nelm/pkg/helm/pkg/cli"
	helmdownloader "github.com/werf/nelm/pkg/helm/pkg/downloader"
	helmgetter "github.com/werf/nelm/pkg/helm/pkg/getter"
	"github.com/werf/nelm/pkg/helm/pkg/helmpath"
	helmregistry "github.com/werf/nelm/pkg/helm/pkg/registry"
	helmrepo "github.com/werf/nelm/pkg/helm/pkg/repo"
)

// maxChartRepoRequestTimeout caps one chart-repository HTTP request (helm's
// own getter default). Left at zero, nelm's downloader applies no timeout at
// all — http.Client{Timeout: 0} on a transport with no response-header
// timeout — so a server that accepts the connection and then stops sending
// hangs the fetch forever.
const maxChartRepoRequestTimeout = 2 * time.Minute

// chartRepoOptions maps spec onto the chart-repository connection options
// shared by Plan, Install and Render (both fetchChart and the nelm action
// get the same ones). timeout is the operation's timeout: one request may
// take at most that long, and never more than maxChartRepoRequestTimeout.
func chartRepoOptions(spec ReleaseSpec, timeout time.Duration) common.ChartRepoConnectionOptions {
	requestTimeout := maxChartRepoRequestTimeout
	if timeout > 0 && timeout < requestTimeout {
		requestTimeout = timeout
	}

	return common.ChartRepoConnectionOptions{
		ChartRepoRequestTimeout: requestTimeout,
		ChartRepoURL:            spec.Repository,
	}
}

// fetchChart downloads a remote chart reference into its own directory under
// opDir and returns the archive's absolute path, which the caller hands to
// nelm as the chart: nelm treats an absolute path as a local chart and skips
// its own download. A local reference is returned unchanged.
//
// nelm would download into the shared helm repository cache
// (HELM_REPOSITORY_CACHE, default ~/.cache/helm/repository) under a file
// named only <chart>-<version>.tgz — no repository, no digest — and then
// re-open that path to load it. Two operations running at once (Terraform
// runs up to 10 per provider process; parallel CI jobs share a HOME) that
// fetch different charts with the same name and version, e.g. the same leaf
// name and tag in two Artifact Registry repositories, could each load the
// other's archive and silently plan or install the wrong chart. A per-op
// download directory shares nothing.
//
// The downloader is set up exactly as nelm sets up its own
// (pkg/chart/chart_download.go newChartDownloader, plus the registry-client
// options of pkg/action), so every reference resolves the same way —
// oci://, a repository URL plus chart name, and repo/name against the
// `helm repo add` config and index cache, which stay where nelm reads them.
// Re-check it against nelm on every nelm upgrade.
//
// The fetch is bounded by ctx and, when positive, by timeout. Helm's download
// machinery takes no context, and an OCI pull goes through a registry client
// with no HTTP timeout at all (ChartRepoRequestTimeout bounds only HTTP
// repositories), so the fetch runs in a goroutine that is abandoned when the
// bound fires. An abandoned fetch writes only under opDir, which the
// caller's cleanup removes.
func fetchChart(ctx context.Context, opDir, chartRef, version string, repo common.ChartRepoConnectionOptions, registryConfig string, timeout time.Duration) (string, error) {
	if isLocalChartRef(chartRef) {
		return chartRef, nil
	}

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	type fetchResult struct {
		path string
		err  error
	}

	done := make(chan fetchResult, 1)

	go func() {
		// A panic in helm's download code on this goroutine would otherwise
		// crash the provider plugin, aborting every concurrent resource.
		defer func() {
			if r := recover(); r != nil {
				done <- fetchResult{err: fmt.Errorf("recovered from a panic: %v", r)}
			}
		}()

		path, err := downloadChart(opDir, chartRef, version, repo, registryConfig)
		done <- fetchResult{path: path, err: err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			return "", fmt.Errorf("download chart %q: %w", chartRef, res.err)
		}

		return res.path, nil
	case <-ctx.Done():
		return "", fmt.Errorf("download chart %q: %w", chartRef, context.Cause(ctx))
	}
}

// downloadChart is fetchChart's unbounded download of a remote chartRef into
// <opDir>/chart, returning the archive's path.
func downloadChart(opDir, chartRef, version string, repo common.ChartRepoConnectionOptions, registryConfig string) (string, error) {
	credentials := registryConfig
	if credentials == "" {
		credentials = common.DefaultRegistryCredentialsPath
	}

	registryOpts := []helmregistry.ClientOption{
		helmregistry.ClientOptWriter(io.Discard),
		helmregistry.ClientOptCredentialsFile(credentials),
	}

	if repo.ChartRepoInsecure {
		registryOpts = append(registryOpts, helmregistry.ClientOptPlainHTTP())
	}

	registryClient, err := helmregistry.NewClient(registryOpts...)
	if err != nil {
		return "", fmt.Errorf("construct registry client: %w", err)
	}

	providers := helmgetter.Providers{helmgetter.HttpProvider, helmgetter.OCIProvider}

	downloader := &helmdownloader.ChartDownloader{
		Out:     io.Discard,
		Verify:  helmdownloader.VerificationStrategyString(common.DefaultChartProvenanceStrategy).ToVerificationStrategy(),
		Getters: providers,
		Options: []helmgetter.Option{
			helmgetter.WithPassCredentialsAll(repo.ChartRepoPassCreds),
			helmgetter.WithTLSClientConfig(repo.ChartRepoCertPath, repo.ChartRepoKeyPath, repo.ChartRepoCAPath),
			helmgetter.WithInsecureSkipVerifyTLS(repo.ChartRepoSkipTLSVerify),
			helmgetter.WithPlainHTTP(repo.ChartRepoInsecure),
			helmgetter.WithRegistryClient(registryClient),
			helmgetter.WithTimeout(repo.ChartRepoRequestTimeout),
		},
		RegistryClient:   registryClient,
		RepositoryConfig: cli.EnvOr("HELM_REPOSITORY_CONFIG", helmpath.ConfigPath("repositories.yaml")),
		RepositoryCache:  cli.EnvOr("HELM_REPOSITORY_CACHE", helmpath.CachePath("repository")),
	}

	ref := chartRef

	if repo.ChartRepoURL != "" {
		chartURL, err := helmrepo.FindChartInAuthAndTLSAndPassRepoURL(repo.ChartRepoURL,
			repo.ChartRepoBasicAuthUsername, repo.ChartRepoBasicAuthPassword, chartRef, version,
			repo.ChartRepoCertPath, repo.ChartRepoKeyPath, repo.ChartRepoCAPath,
			repo.ChartRepoSkipTLSVerify, repo.ChartRepoPassCreds, providers)
		if err != nil {
			return "", fmt.Errorf("get chart URL: %w", err)
		}

		repoURL, err := url.Parse(repo.ChartRepoURL)
		if err != nil {
			return "", fmt.Errorf("parse repo URL: %w", err)
		}

		parsedChartURL, err := url.Parse(chartURL)
		if err != nil {
			return "", fmt.Errorf("parse chart URL: %w", err)
		}

		// Repository credentials go only to the repository's own scheme+host
		// unless ChartRepoPassCreds says otherwise (nelm's rule).
		if repo.ChartRepoPassCreds || (repoURL.Scheme == parsedChartURL.Scheme && repoURL.Host == parsedChartURL.Host) {
			downloader.Options = append(downloader.Options, helmgetter.WithBasicAuth(repo.ChartRepoBasicAuthUsername, repo.ChartRepoBasicAuthPassword))
		} else {
			downloader.Options = append(downloader.Options, helmgetter.WithBasicAuth("", ""))
		}

		ref = chartURL
	} else {
		downloader.Options = append(downloader.Options, helmgetter.WithBasicAuth(repo.ChartRepoBasicAuthUsername, repo.ChartRepoBasicAuthPassword))
	}

	dir := filepath.Join(opDir, "chart")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("create chart download dir: %w", err)
	}

	path, _, err := downloader.DownloadTo(ref, version, dir)
	if err != nil {
		return "", err
	}

	return path, nil
}

// isLocalChartRef is nelm's local-vs-remote chart classification
// (pkg/chart/chart_render.go isLocalChart): an absolute path, or anything
// starting with "." (which covers "./" and "../").
func isLocalChartRef(chartRef string) bool {
	return filepath.IsAbs(chartRef) || strings.HasPrefix(chartRef, ".")
}
