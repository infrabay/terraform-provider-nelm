package nelmclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/samber/lo"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/werf/nelm/pkg/kube"
)

// defaultFactoryBuildTimeout caps a single kube-factory construction attempt (which
// includes a live connectivity round-trip that client-go performs WITHOUT any
// caller context). Configurable via Config.RequestTimeout; the default keeps
// a half-open API server from wedging every Read/plan in the process forever
// while kubeMu is held.
const defaultFactoryBuildTimeout = 30 * time.Second

// ensureKubeFactory lazily constructs and caches (once per Client, guarded by
// kubeMu) the nelm kube.ClientFactory backing both LiveObjects and
// IsNamespaced. Only a SUCCESSFUL construction is cached; an error is returned
// but never memoized (a transient blip must not poison later reads), and a
// single attempt is time-bounded (see below) so a half-open API server cannot
// wedge every resource in the process behind this mutex.
func (c *Client) ensureKubeFactory(ctx context.Context) (*kube.ClientFactory, error) {
	if c.clusterErr != nil {
		return nil, c.clusterErr
	}

	c.kubeMu.Lock()
	defer c.kubeMu.Unlock()

	if c.kubeFactory != nil {
		return c.kubeFactory, nil
	}

	// The construction includes a ServerVersion() connectivity check that
	// ignores ctx entirely (client-go discovery has no context parameter), so
	// a blackholed endpoint would block indefinitely — while this mutex is
	// held, wedging every resource's Read/plan in the process. Run the build
	// in a goroutine and give up after a bounded wait; the abandoned worker
	// finishes (or times out at the OS level) on its own and its result is
	// discarded — nothing is cached from a stale attempt.
	timeout := defaultFactoryBuildTimeout
	if c.cfg.RequestTimeout > 0 {
		timeout = c.cfg.RequestTimeout
	}

	type buildResult struct {
		factory *kube.ClientFactory
		err     error
	}

	done := make(chan buildResult, 1)

	go func() {
		factory, err := c.buildKubeFactory(ctx)
		done <- buildResult{factory: factory, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case res := <-done:
		if res.err != nil {
			return nil, res.err
		}

		c.kubeFactory = res.factory

		return c.kubeFactory, nil

	case <-ctx.Done():
		return nil, fmt.Errorf("construct kube client factory: %w", ctx.Err())

	case <-timer.C:
		return nil, fmt.Errorf("construct kube client factory: connectivity check did not complete within %s "+
			"(the API server may be accepting connections but not responding)", timeout)
	}
}

// buildKubeFactory performs the actual (potentially slow) factory
// construction. Called only from ensureKubeFactory's bounded worker.
func (c *Client) buildKubeFactory(ctx context.Context) (*kube.ClientFactory, error) {
	// Nelm's own action entry points (e.g. releasePlanInstall) call
	// KubeConnectionOptions.ApplyDefaults(homeDir) — which fills in
	// KubeConfigPaths with ~/.kube/config when both KubeConfigPaths and
	// KubeConfigBase64 are empty — before ever constructing a KubeConfig;
	// kube.NewKubeConfig itself does NOT apply that default. Mirror that
	// step here so an unconfigured Config (no explicit kube_config_paths)
	// resolves the same way Plan/Install/Get do.
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home directory: %w", err)
	}

	kubeConnOpts := c.toKubeConnectionOptions()
	kubeConnOpts.ApplyDefaults(homeDir)

	var kubeConfigPaths []string
	for _, path := range kubeConnOpts.KubeConfigPaths {
		kubeConfigPaths = append(kubeConfigPaths, filepath.SplitList(path)...)
	}
	kubeConfigPaths = lo.Compact(kubeConfigPaths)

	kubeConfig, err := kube.NewKubeConfig(ctx, kubeConfigPaths, kube.KubeConfigOptions{
		KubeConnectionOptions: kubeConnOpts,
	})
	if err != nil {
		return nil, fmt.Errorf("construct kube config: %w", err)
	}

	factory, err := kube.NewClientFactory(ctx, kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("construct kube client factory: %w", err)
	}

	return factory, nil
}

// isNoKindMatch reports whether err is (or wraps) a RESTMapper "no matches
// for kind" error — the shape produced when a GVK is not served by the
// cluster (e.g. its CRD was deleted, or the API version was retired).
func isNoKindMatch(err error) bool {
	var noMatch *meta.NoKindMatchError
	return errors.As(err, &noMatch)
}

// IsNamespaced reports whether gvk is a namespaced kind, resolved via the
// cached RESTMapper. This is nelmclient's structural implementation of
// internal/planconv.KeyScoper (see key.go's KeyScoper interface) — it is
// implemented here rather than by importing planconv, so planconv can stay a
// pure, cluster-agnostic leaf package (CONTRACTS.md seam 2: both the planned
// side and the live side of the diff MUST resolve namespace-scoping through
// the SAME KeyScoper implementation, i.e. this one, or phantom diffs
// result). The KeyScoper interface signature takes no context, so a
// background context is used for the (cached, one-time) factory
// construction.
func (c *Client) IsNamespaced(gvk schema.GroupVersionKind) (bool, error) {
	factory, err := c.ensureKubeFactory(context.Background())
	if err != nil {
		return false, err
	}

	mapping, err := factory.Mapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return false, fmt.Errorf("rest mapping for %s: %w", gvk.String(), err)
	}

	return mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

// ResetKindCache drops the discovery the cached RESTMapper answers from (in
// memory, and the on-disk discovery cache behind it), so that the next
// IsNamespaced or LiveObjects lookup discovers the cluster's kinds again.
// The mapper lives as long as the Client, and client-go re-discovers on a
// miss only while its discovery came from another process's disk cache, so
// without a reset a kind whose CRD was installed after the first lookup can
// stay unserved for the rest of the process. nelm_template's Read calls it
// once per validate = true render, before scoping that render's objects.
// It sends no request (the next lookup pays one discovery round-trip) and is
// a no-op while no factory has been built, so on the placeholder clients,
// which never build one.
//
// kubeMu guards only the read of the cached factory, never the Reset: the
// mapper's Reset waits for its own init lock, which a concurrent lookup
// holds for a whole discovery round-trip, and holding kubeMu meanwhile would
// stall every other caller of ensureKubeFactory (History, LiveObjects,
// IsNamespaced) behind that discovery. client-go synchronizes the Reset
// with concurrent lookups itself.
func (c *Client) ResetKindCache() {
	c.kubeMu.Lock()
	factory := c.kubeFactory
	c.kubeMu.Unlock()

	if factory != nil {
		factory.Mapper().Reset()
	}
}

// liveGetAttempts and liveGetRetryDelay bound getLive's retries of a GET the
// API server failed transiently: up to four attempts, 0.5s, 1s and 2s apart.
// liveGetRetryDelay is a variable so that tests can shorten it.
const liveGetAttempts = 4

var liveGetRetryDelay = 500 * time.Millisecond

// isTransientServerErr reports whether err is an API server failure that
// the same GET may not hit again: an internal error (a 500, or a 5xx
// without a Status body), a 503, a server-side timeout (504, ServerTimeout)
// or a 429. GKE, for one, answers a request its IAM authorizer failed to
// check with a 500 "the server is currently unable to handle the request".
// client-go retries such a response itself only when it carries
// Retry-After, which a failed authorizer's 500 does not. A request cut off
// on the client side (kube_request_timeout, ctx) is not an API status and
// is not retried.
func isTransientServerErr(err error) bool {
	return apierrors.IsInternalError(err) ||
		apierrors.IsServiceUnavailable(err) ||
		apierrors.IsServerTimeout(err) ||
		apierrors.IsTimeout(err) ||
		apierrors.IsTooManyRequests(err)
}

// getLive is resClient.Get(name), retried while it fails with a transient
// server error (isTransientServerErr), at most liveGetAttempts times and
// never past ctx. These retries come on top of client-go's own retries of a
// response that carries Retry-After. When retries run out, or ctx ends a
// wait between them, it returns the last error prefixed with the attempts
// made (and what stopped them), still wrapping the API server's error (and
// ctx's cause).
func getLive(ctx context.Context, resClient dynamic.ResourceInterface, name string) (*unstructured.Unstructured, error) {
	delay := liveGetRetryDelay

	for attempt := 1; ; attempt++ {
		obj, err := resClient.Get(ctx, name, metav1.GetOptions{})
		if err == nil || !isTransientServerErr(err) {
			return obj, err
		}

		if attempt == liveGetAttempts {
			return nil, fmt.Errorf("after %d attempts: %w", attempt, err)
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("after %d of %d attempts, stopped by %w: %w", attempt, liveGetAttempts, context.Cause(ctx), err)
		case <-time.After(delay):
		}

		delay *= 2
	}
}

// LiveObjects fetches the current live cluster state for each ref via the
// cached RESTMapper + dynamic client (Read builds the live side of the diff
// from these, normalized through the same planconv.NormalizeUnstructured
// pipeline as the planned side). A ref whose object no longer exists
// (NotFound) is simply omitted from the result — an out-of-band deletion is
// signaled by absence, not an error, and diffs as a re-create at the next
// plan. A GET the API server fails transiently is retried (getLive), so a
// single failed request does not fail the whole Read.
func (c *Client) LiveObjects(ctx context.Context, refs []ResourceRef) (map[ResourceRef]*unstructured.Unstructured, error) {
	factory, err := c.ensureKubeFactory(ctx)
	if err != nil {
		return nil, err
	}

	result := make(map[ResourceRef]*unstructured.Unstructured, len(refs))

	for _, ref := range refs {
		gvk := schema.GroupVersionKind{Group: ref.Group, Version: ref.Version, Kind: ref.Kind}

		mapping, err := factory.Mapper().RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			// A kind the cluster no longer serves (its CRD was removed
			// out-of-band, or an API version was retired) means the object
			// cannot exist live: treat it as absent — like a NotFound below —
			// rather than a hard error that would permanently wedge every
			// Read/refresh of a release still holding such a resource in its
			// stored manifest. Absence surfaces as a re-create diff at the
			// next plan, which is the correct drift signal.
			if isNoKindMatch(err) {
				continue
			}

			return nil, fmt.Errorf("rest mapping for %s: %w", gvk.String(), err)
		}

		var resClient dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			resClient = factory.Dynamic().Resource(mapping.Resource).Namespace(ref.Namespace)
		} else {
			resClient = factory.Dynamic().Resource(mapping.Resource)
		}

		obj, err := getLive(ctx, resClient, ref.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return nil, fmt.Errorf("get %s %s/%s: %w", gvk.String(), ref.Namespace, ref.Name, err)
		}

		result[ref] = obj
	}

	return result, nil
}
