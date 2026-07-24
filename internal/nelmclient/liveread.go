package nelmclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/samber/lo"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/werf/nelm/pkg/kube"
)

// ensureKubeFactory lazily constructs and caches (once per Client, guarded by
// kubeMu) the nelm kube.ClientFactory backing both LiveObjects and
// IsNamespaced. Building it performs a live connectivity check
// (kube.NewClientFactory dials the API server's /version endpoint, see
// factory.go and testdata/errors/unreachable_cluster.txt), so the first
// caller pays that cost; every later call from either method reuses the
// cached factory and its RESTMapper/dynamic client.
//
// Only a SUCCESSFUL construction is cached. A construction error is returned
// but NOT memoized: the connectivity check can fail transiently (a momentary
// API-server 503 during a control-plane rollout, or KubeRequestTimeout
// expiring under load), and nelm's own Plan/Install/Get each build a fresh
// factory per call and shrug such a blip off. Memoizing the error here would
// instead poison LiveObjects/IsNamespaced for every resource for the rest of
// the process even after the cluster recovered, so the next call always
// retries construction until it succeeds.
func (c *Client) ensureKubeFactory(ctx context.Context) (*kube.ClientFactory, error) {
	c.kubeMu.Lock()
	defer c.kubeMu.Unlock()

	if c.kubeFactory != nil {
		return c.kubeFactory, nil
	}

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

	c.kubeFactory = factory

	return c.kubeFactory, nil
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

// LiveObjects fetches the current live cluster state for each ref via the
// cached RESTMapper + dynamic client (design §2.4: Read builds the live side
// of the diff from these, normalized through the same
// planconv.NormalizeUnstructured pipeline as the planned side). A ref whose
// object no longer exists (NotFound) is simply omitted from the result — an
// out-of-band deletion is signaled by absence, not an error, and diffs as a
// re-create at the next plan.
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

		obj, err := resClient.Get(ctx, ref.Name, metav1.GetOptions{})
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
