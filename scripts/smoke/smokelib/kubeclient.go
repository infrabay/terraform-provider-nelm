//go:build smoke

package smokelib

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/werf/nelm/pkg/kube"
)

// MustClientFactory builds nelm's ClientFactory (static/dynamic/discovery
// clients + cached RESTMapper) from a guarded KubeConfig. Panics on error —
// this is only ever called right after MustGuardOrbstack succeeded, so a
// failure here means the (already-verified-local) cluster is unreachable,
// which is a hard stop for a capture program.
func MustClientFactory(ctx context.Context, kubeConfig *kube.KubeConfig) *kube.ClientFactory {
	factory, err := kube.NewClientFactory(ctx, kubeConfig)
	if err != nil {
		panic(fmt.Errorf("construct kube client factory: %w", err))
	}

	return factory
}

// GetLive fetches the live object for gvk/namespace/name via the dynamic
// client + cached RESTMapper — the same resolution path the provider's
// internal/nelmclient/liveread.go uses. namespace is ignored for
// cluster-scoped kinds.
func GetLive(ctx context.Context, factory *kube.ClientFactory, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, error) {
	mapping, err := factory.Mapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("rest mapping for %s: %w", gvk, err)
	}

	nri := factory.Dynamic().Resource(mapping.Resource)

	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		return nri.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	}

	return nri.Get(ctx, name, metav1.GetOptions{})
}
