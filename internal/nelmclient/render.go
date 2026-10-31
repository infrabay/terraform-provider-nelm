package nelmclient

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/werf/nelm/pkg/action"
	"github.com/werf/nelm/pkg/common"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Render runs nelm's chart-render action (the same client-side template
// evaluation Install/Plan perform, action.ChartRender with Remote:true so
// cluster capabilities match) and returns the rendered resource manifests.
//
// This is the authoritative CHART-DESIRED shape of every resource. It exists
// because an "update" plan change's After is the API server's dry-run merge —
// live and chart fields blended — and no heuristic over (After, Before, prior
// desired) can reliably separate them (every such rule has a counterexample: a
// prior poisoned by import's full-live first Read keeps tracking live-mutable
// fields; a field the chart NEWLY manages that already exists live gets
// misclassified as live-carried and silently dropped). The rendered manifest
// resolves the ambiguity at the source: a field is chart-managed iff the chart
// renders it.
func (c *Client) Render(ctx context.Context, spec ReleaseSpec, timeout time.Duration) ([]*unstructured.Unstructured, error) {
	if c.clusterErr != nil {
		return nil, c.clusterErr
	}

	result, err := c.chartRender(ctx, "nelm-render-", spec, timeout, nil, func(opts *action.ChartRenderOptions) {
		opts.KubeConnectionOptions = c.toKubeConnectionOptions()
		opts.ReleaseStorageDriver = renderStorageDriver(spec)
		opts.Remote = true
	})
	if err != nil {
		return nil, err
	}

	objs := make([]*unstructured.Unstructured, 0, len(result.Resources))
	for _, res := range result.Resources {
		if res == nil || res.Unstruct == nil {
			continue
		}

		objs = append(objs, res.Unstruct)
	}

	return objs, nil
}

// chartRender runs action.ChartRender for spec in a fresh per-op temp dir
// (opDirPrefix) and returns its result: the shared plumbing of Render and
// Template. It resolves the chart reference, writes the values files and the
// registry config into the op dir, fetches a remote chart, and fills in the
// options every render needs; configure then sets the mode-specific ones
// (Remote, the kube connection, the release storage, capabilities). Every
// option configure leaves alone keeps the value set here. prepare, when not
// nil, may then change the options (Template has nelm load a local chart
// without building its dependencies, see noDependencyBuild); it runs inside
// the bounded render, so the timeout covers the chart files it reads.
//
// ctx and, when positive, timeout bound the whole operation: the chart
// download (fetchChart) and the render itself (runChartRender), so a
// runaway template or a stalled cluster read fails with the timeout rather
// than holding the Terraform operation. The timeout's cancel runs before the
// op dir's cleanup (deferred later, so run first).
//
// The caller MUST have checked whether the render may reach a cluster:
// chartRender itself never inspects clusterErr.
func (c *Client) chartRender(
	ctx context.Context,
	opDirPrefix string,
	spec ReleaseSpec,
	timeout time.Duration,
	prepare func(opts *action.ChartRenderOptions) error,
	configure func(opts *action.ChartRenderOptions),
) (*action.ChartRenderResultV2, error) {
	opDir, cleanup, err := newOpDir(opDirPrefix)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	chartRef, repoURL, err := NormalizeChartRef(spec.Chart, spec.Repository)
	if err != nil {
		return nil, fmt.Errorf("normalize chart reference: %w", err)
	}

	valuesOpts, err := buildValuesOptions(opDir, spec)
	if err != nil {
		return nil, err
	}

	registryConfig, err := writeRegistryConfig(opDir, c.cfg.Registries)
	if err != nil {
		return nil, err
	}

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	repoOpts := chartRepoOptions(repoURL, timeout)

	chartPath, err := fetchChart(ctx, opDir, chartRef, spec.Version, repoOpts, registryConfig, timeout)
	if err != nil {
		return nil, err
	}

	ctx, buf := captureCtx(ctx)

	// OutputFilePath is load-bearing: ChartRender ignores OutputNoPrint (nelm
	// v1.26.2 through v1.29.0 only declare it) and, with no OutputFilePath,
	// prints every rendered manifest — Secret data included — to os.Stdout,
	// which inside a plugin process is a pipe Terraform core logs at WARN.
	// The printout goes to a file in the 0700 opDir instead, removed with it
	// by the deferred cleanup. OutputNoPrint stays set for when nelm starts
	// honoring it.
	opts := action.ChartRenderOptions{
		ChartRepoConnectionOptions: repoOpts,
		ValuesOptions:              valuesOpts,

		Chart:                   chartPath,
		ChartVersion:            spec.Version,
		OutputFilePath:          filepath.Join(opDir, "render.yaml"),
		OutputNoPrint:           true,
		RegistryCredentialsPath: registryConfig,
		ReleaseName:             spec.Name,
		ReleaseNamespace:        spec.Namespace,
		TempDirPath:             opDir,
	}

	configure(&opts)

	result, err := runChartRender(ctx, opts, prepare)
	if err != nil {
		return nil, tailErr(fmt.Errorf("chart render: %w", redactManifestErr(err)), buf)
	}

	return result, nil
}

// chartRenderAction is the nelm call runChartRender bounds. Tests replace it
// to wait for an abandoned render to finish (it reads process globals other
// tests swap, such as os.Stdout) or to make one panic; nothing else may
// assign it.
var chartRenderAction = action.ChartRender

// runChartRender runs action.ChartRender bounded by ctx. Nelm uses ctx for
// little more than logging: helm's chart loader (loader.LoadDir even makes
// its own context.Background()), the values merge and the template engine
// take none, and neither does a remote render's client-go ServerVersion
// check or lookup, which kube_request_timeout bounds only when it is set. A
// slow or non-terminating template (a large range, a lookup the API server
// never answers) would hold the read past its timeout, or forever. The
// render therefore runs in a goroutine, like fetchChart's download, and on
// <-ctx.Done() runChartRender returns context.Cause(ctx) and abandons it.
// prepare, when not nil, runs first on the same goroutine, on the render's
// copy of opts, and its error is the render's.
//
// The abandoned render runs on until it finishes by itself (its kube calls
// that do take ctx fail at once: ctx is cancelled), and all it can still
// touch is safe to touch late:
//
//   - the op dir (its TempDirPath files and the OutputFilePath printout),
//     which chartRender's cleanup removes: a later os.Create or read there
//     fails with an error, and a file it creates while cleanup is removing
//     the dir can only leave the dir behind until Shutdown removes the temp
//     root;
//   - the captured logger, a syncBuffer, which nothing closes and which is
//     safe to write while tailErr reads it;
//   - done, buffered so that its one send never blocks: the goroutine always
//     exits, and its result is dropped.
//
// It shares no memory anyone writes afterwards: opts is a copy, and the
// slices it shares (the ReleaseSpec's, TemplateOptions.APIVersions, the
// Client's kubeconfig paths) are never written once the call is made. A
// panic on the goroutine is recovered into an error, since nothing else
// would recover it and it would crash the provider plugin, aborting every
// concurrent operation.
func runChartRender(
	ctx context.Context,
	opts action.ChartRenderOptions,
	prepare func(opts *action.ChartRenderOptions) error,
) (*action.ChartRenderResultV2, error) {
	type renderResult struct {
		result *action.ChartRenderResultV2
		err    error
	}

	done := make(chan renderResult, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- renderResult{err: fmt.Errorf("recovered from a panic: %v\n%s", r, debug.Stack())}
			}
		}()

		if prepare != nil {
			if err := prepare(&opts); err != nil {
				done <- renderResult{err: err}
				return
			}
		}

		result, err := chartRenderAction(ctx, opts)
		done <- renderResult{result: result, err: err}
	}()

	select {
	case res := <-done:
		return res.result, res.err
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// renderStorageDriver is the release storage Render reads the release history
// from (the history decides the deploy type and revision the templates see).
// A RenderAsFirstInstall render uses nelm's in-memory driver, which is always
// empty: no history means deploy type "Initial" and revision 1, exactly what
// Install renders when the release does not exist (yet, or any more).
func renderStorageDriver(spec ReleaseSpec) string {
	if spec.RenderAsFirstInstall {
		return common.ReleaseStorageDriverMemory
	}

	return spec.StorageDriver
}
