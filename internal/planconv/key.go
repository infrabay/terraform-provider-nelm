// Package planconv holds pure functions (no cluster access) that turn Nelm's
// plan output and live-cluster reads into the canonical Terraform diff
// surface (the nelm_release "resources" attribute). See CONTRACTS.md seam 2:
// both the ModifyPlan (planned) side and the Read (live) side of the diff
// MUST use the same NormalizeUnstructured + Key(..., scoper) with the same
// KeyScoper implementation, or phantom diffs result.
package planconv

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Ref identifies a single Kubernetes resource for the purposes of building a
// "resources" map key: its GroupVersionKind plus namespace/name. It mirrors
// internal/nelmclient.ResourceRef but is redeclared here so that planconv
// stays a leaf package with no dependency on nelmclient (or any cluster
// client) — see CONTRACTS.md, Seam 2.
type Ref struct {
	GroupVersionKind schema.GroupVersionKind
	Namespace        string
	Name             string
}

// KeyScoper resolves whether a given GroupVersionKind is namespaced. It is
// backed in production by internal/nelmclient's cached RESTMapper
// (liveread.go), letting planconv remain cluster-agnostic and pure for unit
// testing against fixtures.
type KeyScoper interface {
	IsNamespaced(gvk schema.GroupVersionKind) (bool, error)
}

// Key returns the canonical resources-map key for ref:
//
//	"<apiVersion>/<Kind>/<namespace>/<name>"
//
// with an empty namespace segment for cluster-scoped kinds (e.g.
// "rbac.authorization.k8s.io/v1/ClusterRole//my-role"). releaseNS resolves an
// implicit/missing namespace on namespaced kinds: a create-planned local
// manifest may omit namespace (defaulting to the release namespace), while a
// later live read always carries an explicit one — both must converge on the
// same key.
//
// Namespace resolution: ref.Namespace wins if non-empty (callers are expected
// to have already preferred the object's own metadata.namespace, falling back
// to nelm's ResourceMeta.Namespace, before constructing ref — see
// BuildPlannedResources/BuildLiveResources). If ref.Namespace is empty,
// scoper.IsNamespaced resolves whether the GroupVersionKind is
// namespace-scoped at all: namespaced kinds fall back to releaseNS (the
// implicit-namespace case above); cluster-scoped kinds keep the empty segment.
func Key(ref Ref, releaseNS string, scoper KeyScoper) (string, error) {
	if scoper == nil {
		return "", fmt.Errorf("planconv: Key: nil KeyScoper for %s", ref.GroupVersionKind)
	}

	// The scoper is consulted UNCONDITIONALLY (not only when ref.Namespace is
	// empty): a manifest may pin metadata.namespace on a cluster-scoped kind,
	// in which case the planned side would otherwise key with that namespace
	// while the live side (whose GET returns no namespace for cluster-scoped
	// objects) keys with an empty segment — a permanent key split. Scope wins
	// over whatever namespace the manifest claims.
	namespaced, err := scoper.IsNamespaced(ref.GroupVersionKind)
	if err != nil {
		return "", fmt.Errorf("planconv: Key: IsNamespaced(%s): %w", ref.GroupVersionKind, err)
	}

	ns := ""
	if namespaced {
		ns = ref.Namespace
		if ns == "" {
			ns = releaseNS
		}
	}

	return fmt.Sprintf("%s/%s/%s/%s", ref.GroupVersionKind.GroupVersion().String(), ref.GroupVersionKind.Kind, ns, ref.Name), nil
}

// keyWithScopeFallback is Key, except that a scoper "no matches for kind"
// failure (the kind is not currently served — its CRD is being installed in
// this very release, or was removed out-of-band) degrades to a best-guess
// namespace resolution instead of a hard error: the manifest's own namespace
// if set, else releaseNS. The PLANNED side must key such resources (nelm
// tolerates NoSuchKind while planning a chart that ships a CRD plus its CR,
// and still emits a create change for the CR); the live side never sees them
// (LiveObjects skips unmapped kinds as absent). If the guess is wrong for a
// cluster-scoped CR, the next Read rebuilds the map from live refs and the
// key self-heals in one refresh cycle.
func keyWithScopeFallback(ref Ref, releaseNS string, scoper KeyScoper) (string, error) {
	key, err := Key(ref, releaseNS, scoper)
	if err == nil {
		return key, nil
	}

	var noMatch *meta.NoKindMatchError
	if !errors.As(err, &noMatch) {
		return "", err
	}

	ns := ref.Namespace
	if ns == "" {
		ns = releaseNS
	}

	return fmt.Sprintf("%s/%s/%s/%s", ref.GroupVersionKind.GroupVersion().String(), ref.GroupVersionKind.Kind, ns, ref.Name), nil
}
