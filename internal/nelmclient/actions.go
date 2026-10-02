package nelmclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/werf/logboek"
	"github.com/werf/nelm/pkg/action"
	"github.com/werf/nelm/pkg/common"
	"github.com/werf/nelm/pkg/plan"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// newOpDir creates a fresh per-operation temp directory (0700, per
// os.MkdirTemp's own default) under TempRoot() and returns it plus a cleanup
// func the caller MUST defer immediately. Every Nelm action call gets its own
// opDir passed as TempDirPath (CONTRACTS.md global-state rule): this is also
// where Plan's plan-artifact file and Install/Plan's values files live, so
// deleting opDir deletes them too, in the same function call frame that
// created them.
func newOpDir(prefix string) (dir string, cleanup func(), err error) {
	dir, err = os.MkdirTemp(TempRoot(), prefix)
	if err != nil {
		return "", func() {}, fmt.Errorf("create per-op temp dir: %w", err)
	}

	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// syncBuffer is a mutex-guarded in-memory writer for per-call nelm log
// capture. The synchronization is REQUIRED, not defensive: on a timeout nelm's
// action functions return without joining their worker goroutine, which can
// still be logging into this buffer while tailErr reads it — an unguarded
// bytes.Buffer would be a data race on every cancelled/timed-out
// Plan/Install/Uninstall.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

// captureCtx returns a ctx carrying a per-call logboek logger that writes
// into an in-memory buffer instead of the process's real stdout/stderr (nelm
// actions log via log.Default, which resolves its writer from
// logboek.Context(ctx) on every call — see pkg/log/logger_logboek.go). The
// returned buffer's tail can be folded into an error for diagnostics.
// bootstrap.go's Init is still the only place log.SetupLogging or any
// featgate.*.Enable() may be called; this only redirects per-call output.
func captureCtx(ctx context.Context) (context.Context, *syncBuffer) {
	buf := &syncBuffer{}
	return logboek.NewContext(ctx, logboek.NewLogger(buf, buf)), buf
}

// captureWarningsCtx is captureCtx without nelm's info output: only warnings
// and errors (logboek's err stream) reach the buffer. Plan uses it because
// ReleasePlanInstall logs every planned change's unified diff at info level,
// and nelm's diff hides only fully-sensitive objects — a Secret annotated
// werf.io/sensitive: "false" (or whose werf.io/sensitive-paths miss data) is
// diffed with its data, bypassing planconv's unconditional Secret redaction
// on the way into an error diagnostic. Plan's info output carries nothing
// else a failure needs (progress lines and the change summary).
func captureWarningsCtx(ctx context.Context) (context.Context, *syncBuffer) {
	buf := &syncBuffer{}
	return logboek.NewContext(ctx, logboek.NewLogger(io.Discard, buf)), buf
}

// maxErrTail bounds the captured nelm output tailErr folds into an error.
// nelm reports what failed last (failed operations, the final error), so the
// end of the log is what diagnoses a failure; the whole log of a large
// release would bury the actual error in plan/apply output.
const maxErrTail = 8 << 10

// tailErr folds the last maxErrTail bytes of buf's captured nelm output into
// err's message, if any was captured, so operators get actionable
// diagnostics without nelm's log output ever reaching the real process
// stdout/stderr. The capture covers nelm's logger only: anything nelm writes
// to os.Stdout directly (ChartRender's manifest printout, see render.go)
// bypasses it and must be redirected at the call site.
func tailErr(err error, buf *syncBuffer) error {
	if err == nil {
		return nil
	}

	tail := buf.String()
	if tail == "" {
		return err
	}

	if len(tail) > maxErrTail {
		cut := len(tail) - maxErrTail

		// Start the tail on a line boundary rather than mid-line.
		if i := strings.IndexByte(tail[cut:], '\n'); i >= 0 && cut+i+1 < len(tail) {
			cut += i + 1
		}

		tail = fmt.Sprintf("[... %d earlier bytes omitted ...]\n%s", cut, tail[cut:])
	}

	return fmt.Errorf("%w\n--- nelm output ---\n%s", err, tail)
}

// writeValuesFiles writes each ValuesYAML entry to its own file under dir, in
// order, returning the file paths for ValuesOptions.ValuesFiles (later files
// override earlier ones, matching config order per types.go).
func writeValuesFiles(dir string, valuesYAML []string) ([]string, error) {
	if len(valuesYAML) == 0 {
		return nil, nil
	}

	files := make([]string, 0, len(valuesYAML))

	for i, y := range valuesYAML {
		p := filepath.Join(dir, fmt.Sprintf("values-%d.yaml", i))
		if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
			return nil, fmt.Errorf("write values file %d: %w", i, err)
		}

		files = append(files, p)
	}

	return files, nil
}

// buildValuesOptions writes spec's ValuesYAML entries under dir and maps
// Set/SetString/SetLiteral/SetJSON onto the matching common.ValuesOptions
// fields (never SecretKey — CONTRACTS.md global-state rule).
func buildValuesOptions(dir string, spec ReleaseSpec) (common.ValuesOptions, error) {
	files, err := writeValuesFiles(dir, spec.ValuesYAML)
	if err != nil {
		return common.ValuesOptions{}, err
	}

	return common.ValuesOptions{
		ValuesFiles:      files,
		ValuesSet:        spec.Set,
		ValuesSetString:  spec.SetString,
		ValuesSetLiteral: spec.SetLiteral,
		ValuesSetJSON:    spec.SetJSON,
	}, nil
}

// runtimeOptions maps the ReleaseSpec fields shared by Plan and Install onto
// common.ReleaseInstallRuntimeOptions.
func runtimeOptions(spec ReleaseSpec) common.ReleaseInstallRuntimeOptions {
	return common.ReleaseInstallRuntimeOptions{
		ForceAdoption:           spec.ForceAdoption,
		NoRemoveManualChanges:   spec.NoRemoveManualChanges,
		NoInstallStandaloneCRDs: spec.NoInstallCRDs,
		ReleaseHistoryLimit:     spec.HistoryLimit,
		ReleaseStorageDriver:    spec.StorageDriver,
	}
}

// installTrackingOptions maps the ReleaseSpec onto Install's
// common.TrackingOptions. NoFinalTracking (wait = false) makes nelm squash
// every readiness-tracking operation that no later resource operation in the
// deploy plan depends on (pkg/plan squashFinalTrackingOperations). It is NOT
// a blanket "track nothing": pre-install/pre-upgrade hooks, earlier weight
// groups, werf.io/deploy-dependency state=ready targets, every resource ahead
// of a post-install/post-upgrade hook, and a hook with a hook-succeeded
// delete policy are still awaited (actions_test.go pins this against nelm's
// plan builder).
func installTrackingOptions(spec ReleaseSpec) common.TrackingOptions {
	return common.TrackingOptions{
		NoFinalTracking:      spec.NoFinalTracking,
		NoProgressTablePrint: true,
	}
}

// Plan runs Nelm's release-install planning machinery
// (action.ReleasePlanInstall) against a per-op temp dir and plan-artifact
// path, reads the artifact back (plan.ReadPlanArtifact), deletes the
// artifact and its directory before returning, and surfaces the resulting
// []*plan.ResourceChange as a PlanResult.
func (c *Client) Plan(ctx context.Context, spec ReleaseSpec, timeout time.Duration) (*PlanResult, error) {
	if c.configUnknown {
		return nil, ErrConfigUnknown
	}

	opDir, cleanup, err := newOpDir("nelm-plan-")
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

	repoOpts := chartRepoOptions(repoURL, timeout)

	chartPath, err := fetchChart(ctx, opDir, chartRef, spec.Version, repoOpts, registryConfig, timeout)
	if err != nil {
		return nil, err
	}

	artifactPath := filepath.Join(opDir, "plan.artifact")

	ctx, buf := captureWarningsCtx(ctx)

	opts := action.ReleasePlanInstallOptions{
		ChartRepoConnectionOptions:   repoOpts,
		KubeConnectionOptions:        c.toKubeConnectionOptions(),
		ReleaseInstallRuntimeOptions: runtimeOptions(spec),
		ValuesOptions:                valuesOpts,

		Chart:                   chartPath,
		ChartVersion:            spec.Version,
		NoFinalTracking:         true,
		PlanArtifactPath:        artifactPath,
		RegistryCredentialsPath: registryConfig,
		TempDirPath:             opDir,
		Timeout:                 timeout,
	}

	if err := action.ReleasePlanInstall(ctx, spec.Name, spec.Namespace, opts); err != nil {
		return nil, tailErr(fmt.Errorf("release plan install: %w", err), buf)
	}

	// No nelm output here: planning already succeeded, so the log cannot
	// explain a failure to read the artifact back.
	artifact, err := plan.ReadPlanArtifact(ctx, artifactPath, "", "")
	if err != nil {
		return nil, fmt.Errorf("read plan artifact: %w", err)
	}

	return &PlanResult{
		Changes:    artifact.Data.Changes,
		DeployType: string(artifact.DeployType),
	}, nil
}

// Install runs action.ReleaseInstall for the given spec WITHOUT a
// PlanArtifactPath (Create/Update always use a fresh install, never artifact
// replay).
func (c *Client) Install(ctx context.Context, spec ReleaseSpec, timeout time.Duration) error {
	if c.configUnknown {
		return ErrConfigUnknown
	}

	opDir, cleanup, err := newOpDir("nelm-install-")
	if err != nil {
		return err
	}
	defer cleanup()

	chartRef, repoURL, err := NormalizeChartRef(spec.Chart, spec.Repository)
	if err != nil {
		return fmt.Errorf("normalize chart reference: %w", err)
	}

	valuesOpts, err := buildValuesOptions(opDir, spec)
	if err != nil {
		return err
	}

	registryConfig, err := writeRegistryConfig(opDir, c.cfg.Registries)
	if err != nil {
		return err
	}

	repoOpts := chartRepoOptions(repoURL, timeout)

	chartPath, err := fetchChart(ctx, opDir, chartRef, spec.Version, repoOpts, registryConfig, timeout)
	if err != nil {
		return err
	}

	ctx, buf := captureCtx(ctx)

	opts := action.ReleaseInstallOptions{
		ChartRepoConnectionOptions:   repoOpts,
		KubeConnectionOptions:        c.toKubeConnectionOptions(),
		ReleaseInstallRuntimeOptions: runtimeOptions(spec),
		TrackingOptions:              installTrackingOptions(spec),
		ValuesOptions:                valuesOpts,

		AutoRollback:            spec.AutoRollback,
		Chart:                   chartPath,
		ChartVersion:            spec.Version,
		RegistryCredentialsPath: registryConfig,
		TempDirPath:             opDir,
		Timeout:                 timeout,
	}

	if err := action.ReleaseInstall(ctx, spec.Name, spec.Namespace, opts); err != nil {
		return tailErr(fmt.Errorf("release install: %w", err), buf)
	}

	return nil
}

// Uninstall runs action.ReleaseUninstall for the given release. Idempotent:
// a missing release/namespace is not an error (nelm behavior).
func (c *Client) Uninstall(ctx context.Context, name, namespace, storageDriver string, timeout time.Duration) error {
	if c.configUnknown {
		return ErrConfigUnknown
	}

	opDir, cleanup, err := newOpDir("nelm-uninstall-")
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, buf := captureCtx(ctx)

	opts := action.ReleaseUninstallOptions{
		KubeConnectionOptions: c.toKubeConnectionOptions(),
		// Mirror Install: silence nelm's ProgressTablesPrinter. Besides keeping
		// nelm off the provider's stdout, this avoids a data race inside the
		// printer (Start goroutine vs Stop) that the race detector flags on the
		// fast uninstall of a small release (observed at nelm v1.26.2 pkg/track,
		// unchanged through v1.27.2).
		TrackingOptions: common.TrackingOptions{
			NoProgressTablePrint: true,
		},

		DeleteReleaseNamespace: false,
		ReleaseStorageDriver:   storageDriver,
		TempDirPath:            opDir,
		Timeout:                timeout,
	}

	if err := action.ReleaseUninstall(ctx, name, namespace, opts); err != nil {
		return tailErr(fmt.Errorf("release uninstall: %w", err), buf)
	}

	return nil
}

// Get runs action.ReleaseGet(OutputNoPrint:true, PrintValues:true) for the
// given release and maps the result onto ReleaseInfo. A not-found release
// surfaces as an error satisfying IsReleaseNotFound (errors.go).
func (c *Client) Get(ctx context.Context, name, namespace, storageDriver string, timeout time.Duration) (*ReleaseInfo, error) {
	if c.configUnknown {
		return nil, ErrConfigUnknown
	}

	opDir, cleanup, err := newOpDir("nelm-get-")
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Bound the read: without this, a configured timeouts.read has no effect on
	// Read and a half-open API connection (endpoint accepts but stops
	// responding) could block ReleaseGet indefinitely. nelm's ReleaseGet
	// honours ctx cancellation for the parts under its control.
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	ctx, buf := captureCtx(ctx)

	opts := action.ReleaseGetOptions{
		KubeConnectionOptions: c.toKubeConnectionOptions(),

		OutputNoPrint:        true,
		PrintValues:          true,
		ReleaseStorageDriver: storageDriver,
		TempDirPath:          opDir,
	}

	result, err := action.ReleaseGet(ctx, name, namespace, opts)
	if err != nil {
		var notFound *action.ReleaseNotFoundError
		if errors.As(err, &notFound) {
			// Sentinel preserved unwrapped so IsReleaseNotFound (errors.go)
			// recognizes it via errors.As.
			return nil, err
		}

		return nil, tailErr(fmt.Errorf("release get: %w", err), buf)
	}

	info := &ReleaseInfo{
		Name:      result.Release.Name,
		Namespace: result.Release.Namespace,
		Revision:  result.Release.Revision,
		Status:    string(result.Release.Status),

		ChartName:    result.Chart.Name,
		ChartVersion: result.Chart.Version,
		AppVersion:   result.Chart.AppVersion,

		Values: result.Values,
	}

	for _, res := range result.Resources {
		info.Manifests = append(info.Manifests, &unstructured.Unstructured{Object: res})

		ref := resourceRefFromObject(res)

		if ref.Namespace == "" {
			// The stored release manifest may omit metadata.namespace
			// (helm/nelm install resources into the release namespace
			// regardless); resolve via the cached RESTMapper so
			// LiveObjects can actually target the right namespace.
			// Cluster-scoped kinds are left with an empty Namespace.
			gvk := schema.GroupVersionKind{Group: ref.Group, Version: ref.Version, Kind: ref.Kind}

			namespaced, err := c.IsNamespaced(gvk)
			if err != nil {
				// A kind the cluster no longer serves (CRD removed
				// out-of-band) cannot be live-read at all: omit the ref —
				// absence, mirroring LiveObjects' own NoKindMatch handling —
				// instead of wedging every Read of this release forever.
				if isNoKindMatch(err) {
					continue
				}

				return nil, fmt.Errorf("resolve scope for %s: %w", gvk.String(), err)
			}

			if namespaced {
				ref.Namespace = namespace
			}
		}

		info.Resources = append(info.Resources, ref)
	}

	return info, nil
}

// resourceRefFromObject extracts a ResourceRef from a raw resource manifest
// map (as returned by action.ReleaseGetResultV1.Resources).
func resourceRefFromObject(obj map[string]interface{}) ResourceRef {
	u := unstructured.Unstructured{Object: obj}
	gvk := u.GroupVersionKind()

	return ResourceRef{
		Group:     gvk.Group,
		Version:   gvk.Version,
		Kind:      gvk.Kind,
		Namespace: u.GetNamespace(),
		Name:      u.GetName(),
	}
}
