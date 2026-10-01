package nelmclient

import (
	"context"
	"fmt"
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
// desired) can reliably separate them (adversarial review demonstrated
// counterexamples for every rule: a prior poisoned by import's full-live
// first Read keeps tracking live-mutable fields; a field the chart NEWLY
// manages that already exists live gets misclassified as live-carried and
// silently dropped). The rendered manifest resolves the ambiguity at the
// source: a field is chart-managed iff the chart renders it.
func (c *Client) Render(ctx context.Context, spec ReleaseSpec, timeout time.Duration) ([]*unstructured.Unstructured, error) {
	if c.configUnknown {
		return nil, ErrConfigUnknown
	}

	opDir, cleanup, err := newOpDir("nelm-render-")
	if err != nil {
		return nil, err
	}
	defer cleanup()

	chartRef, err := NormalizeChartRef(spec.Chart, spec.Repository)
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

	ctx, buf := captureCtx(ctx)

	opts := action.ChartRenderOptions{
		ChartRepoConnectionOptions: common.ChartRepoConnectionOptions{
			ChartRepoURL: spec.Repository,
		},
		KubeConnectionOptions: c.toKubeConnectionOptions(),
		ValuesOptions:         valuesOpts,

		Chart:                   chartRef,
		ChartVersion:            spec.Version,
		OutputNoPrint:           true,
		RegistryCredentialsPath: registryConfig,
		ReleaseName:             spec.Name,
		ReleaseNamespace:        spec.Namespace,
		ReleaseStorageDriver:    renderStorageDriver(spec),
		Remote:                  true,
		TempDirPath:             opDir,
	}

	result, err := action.ChartRender(ctx, opts)
	if err != nil {
		return nil, tailErr(fmt.Errorf("chart render: %w", err), buf)
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
