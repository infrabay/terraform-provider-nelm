package nelmclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/werf/nelm/pkg/common"
	"github.com/werf/nelm/pkg/kube"
	"github.com/werf/nelm/pkg/util"
)

// helmProviderFieldManagerPrefix prefixes the field manager hashicorp/helm's
// helm_release writes objects with. The Helm 3 SDK it embeds creates and
// patches client-side under the calling binary's basename (helm v3 pkg/kube
// getManagedFieldsManager), i.e. "terraform-provider-helm_v<version>_x5" —
// one entry per provider version that ever wrote the object.
const helmProviderFieldManagerPrefix = "terraform-provider-helm"

// HandOverHelmProviderFieldManagers hands the field ownership hashicorp/helm's
// helm_release left on a release's live objects over to nelm, so that the
// install that follows prunes fields the chart no longer renders.
//
// nelm already migrates objects written by the Helm 3 CLI: before its forced
// server-side apply it folds every "helm"/Update managedFields entry into its
// own "helm"/Apply entry (pkg/plan fixManagedFields). helm_release writes
// through the same client-side Helm 3 code, but under a
// "terraform-provider-helm_*" manager nelm does not recognize, so nelm's apply
// only ever co-owns those fields: a field the chart stops rendering stays
// live, owned by the stale manager, and the projected resources diff never
// shows it. Renaming those entries to "helm"/Update routes helm_release
// objects through nelm's own Helm 3 hand-over unchanged.
//
// The release's objects are the refs in release storage (Get). A missing
// release or object, a kind the cluster no longer serves, and an object
// without such an entry are all no-ops, so this is idempotent and, once the
// hand-over is done, costs a release read plus one GET per object. An object
// whose patch hits an unavailable admission webhook, or whose GET still hits
// an unavailable conversion webhook once getLive's retries run out, is
// skipped, as nelm's own managedFields fix does, reported in the returned
// list (one line per object) and handed over by a later apply. timeout
// bounds the whole call.
//
// An object whose stored manifest is identity only (newIdentityOnlyObjects) is
// handled differently: its Helm entries ("terraform-provider-helm*" and
// "helm", main-resource Update) are dropped instead of renamed. nelm's own
// fix-up dry-runs that manifest under server-side apply to rebuild its
// "helm"/Apply entry, and an apply that owns no field leaves none, so nelm
// (through at least v1.30.4) fails the whole plan or install with "find apply
// managed fields entry in dry-run result". Without a "helm"/Update entry nelm
// skips that fix-up and simply takes the object over; there is nothing to
// prune on an object whose manifest sets no field.
//
// This is a workaround for https://github.com/werf/nelm/issues/831. Once the
// nelm version the provider builds on treats a dry-run result without an
// Apply entry as an empty field set, remove it (newIdentityOnlyObjects,
// identityOnlyObjects, isIdentityOnly, dropHelmUpdateEntries and the
// dropHelmEntries branch of handOverObject), together with its tests and the
// known-limitations entry.
//
// Callers run this at apply only, never while planning: it is a real write,
// and a plan must not add one to the managedFields fix-up nelm's own plan
// already performs.
func (c *Client) HandOverHelmProviderFieldManagers(ctx context.Context, name, namespace, storageDriver string, timeout time.Duration) ([]string, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	info, err := c.Get(ctx, name, namespace, storageDriver, timeout)
	if err != nil {
		if IsReleaseNotFound(err) {
			return nil, nil
		}

		return nil, err
	}

	factory, err := c.ensureKubeFactory(ctx)
	if err != nil {
		return nil, err
	}

	return handOverHelmProviderFieldManagers(ctx, factory.Mapper(), factory.Dynamic(), info.Resources,
		newIdentityOnlyObjects(info.Manifests))
}

// handOverHelmProviderFieldManagers is HandOverHelmProviderFieldManagers'
// cluster-facing body. It takes the RESTMapper and dynamic client explicitly
// so it can be unit tested against a fake API server.
func handOverHelmProviderFieldManagers(ctx context.Context, mapper meta.RESTMapper, dyn dynamic.Interface, refs []ResourceRef, identityOnly identityOnlyObjects) ([]string, error) {
	var skipped []string

	for _, ref := range refs {
		// client-go honours ctx per request; checking it between objects as
		// well stops promptly once the operation's budget is spent.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("hand over helm_release field managers: %w", err)
		}

		gvk := schema.GroupVersionKind{Group: ref.Group, Version: ref.Version, Kind: ref.Kind}

		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			// A kind the cluster no longer serves has no live object to hand
			// over (LiveObjects treats it as absent too).
			if isNoKindMatch(err) {
				continue
			}

			return nil, fmt.Errorf("rest mapping for %s: %w", gvk.String(), err)
		}

		var resClient dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			resClient = dyn.Resource(mapping.Resource).Namespace(ref.Namespace)
		} else {
			resClient = dyn.Resource(mapping.Resource)
		}

		// Workaround for werf/nelm#831; see HandOverHelmProviderFieldManagers.
		dropHelmEntries := identityOnly.has(ref)

		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			return handOverObject(ctx, resClient, ref.Name, dropHelmEntries)
		})
		if err != nil {
			if kube.IsWebhookErr(err) {
				skipped = append(skipped, fmt.Sprintf("%s %s/%s: %s", gvk.String(), ref.Namespace, ref.Name, err))
				continue
			}

			return nil, fmt.Errorf("hand over helm_release field managers of %s %s/%s: %w", gvk.String(), ref.Namespace, ref.Name, err)
		}
	}

	return skipped, nil
}

// handOverObject renames one live object's helm_release entries (see
// handOverHelmProviderEntries) with a single merge patch of
// metadata.managedFields, sent as nelm's own managedFields fix sends it
// (field manager "helm"; it changes no field, so it adds no entry of its
// own). The patch carries the resourceVersion it was computed from: a
// concurrent write fails it with a Conflict, which the caller retries from a
// fresh GET, instead of having that writer's own entry overwritten. A GET
// the API server fails transiently is retried as LiveObjects retries it
// (getLive); that includes a conversion webhook's 500, so such an object is
// skipped only after those retries. The PATCH is not retried.
//
// With dropHelmEntries (an identity-only object, see
// HandOverHelmProviderFieldManagers) the Helm entries are dropped instead
// (dropHelmUpdateEntries); when none are left, the patch sends
// managedFields [{}], the API server's way to clear the list (an empty list is
// ignored).
func handOverObject(ctx context.Context, client dynamic.ResourceInterface, name string, dropHelmEntries bool) error {
	obj, err := getLive(ctx, client, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("get: %w", err)
	}

	var (
		entries []metav1.ManagedFieldsEntry
		changed bool
	)

	if dropHelmEntries {
		entries, changed = dropHelmUpdateEntries(obj.GetManagedFields())
	} else {
		entries, changed, err = handOverHelmProviderEntries(obj.GetManagedFields())
		if err != nil {
			return err
		}
	}

	if !changed {
		return nil
	}

	var managedFields any = entries
	if len(entries) == 0 {
		managedFields = []map[string]any{{}}
	}

	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"managedFields":   managedFields,
			"resourceVersion": obj.GetResourceVersion(),
		},
	})
	if err != nil {
		return fmt.Errorf("marshal managed fields patch: %w", err)
	}

	if _, err := client.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{
		FieldManager: common.DefaultFieldManager,
	}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("patch managed fields: %w", err)
	}

	return nil
}

// handOverHelmProviderEntries returns entries with every main-resource
// "terraform-provider-helm*"/Update entry renamed to nelm's "helm"/Update,
// its fields unioned into an existing "helm"/Update entry of the same
// apiVersion (with util.MergeJSON, as nelm folds entries itself). An Update
// manager's identity includes its apiVersion, so entries of different
// apiVersions stay separate; nelm folds every one of them. Subresource and
// Apply entries are left alone: nelm only folds main-resource entries, and
// helm_release never applies. changed is false, with entries returned as-is,
// when there is nothing to hand over. entries itself is never modified.
func handOverHelmProviderEntries(entries []metav1.ManagedFieldsEntry) (out []metav1.ManagedFieldsEntry, changed bool, err error) {
	var legacy []metav1.ManagedFieldsEntry

	out = make([]metav1.ManagedFieldsEntry, 0, len(entries))

	// helmUpdate indexes out's "helm"/Update entries by apiVersion.
	helmUpdate := map[string]int{}

	for _, e := range entries {
		if e.Subresource == "" && e.Operation == metav1.ManagedFieldsOperationUpdate {
			switch {
			case strings.HasPrefix(e.Manager, helmProviderFieldManagerPrefix):
				legacy = append(legacy, e)
				continue

			case e.Manager == common.DefaultFieldManager:
				helmUpdate[e.APIVersion] = len(out)
			}
		}

		out = append(out, e)
	}

	if len(legacy) == 0 {
		return entries, false, nil
	}

	for _, e := range legacy {
		i, ok := helmUpdate[e.APIVersion]
		if !ok {
			e.Manager = common.DefaultFieldManager
			helmUpdate[e.APIVersion] = len(out)
			out = append(out, e)

			continue
		}

		merged, _, err := util.MergeJSON(fieldsJSON(e.FieldsV1), fieldsJSON(out[i].FieldsV1))
		if err != nil {
			return nil, false, fmt.Errorf("merge %q fields into %q: %w", e.Manager, common.DefaultFieldManager, err)
		}

		// Fresh pointers: out[i] still shares FieldsV1/Time with entries.
		out[i].FieldsV1 = &metav1.FieldsV1{Raw: merged}
		if e.Time != nil && (out[i].Time == nil || out[i].Time.Before(e.Time)) {
			out[i].Time = e.Time.DeepCopy()
		}
	}

	return out, true, nil
}

// fieldsJSON returns f's raw FieldsV1 JSON, "{}" (the empty set) when absent.
func fieldsJSON(f *metav1.FieldsV1) []byte {
	if f == nil || len(f.Raw) == 0 {
		return []byte("{}")
	}

	return f.Raw
}

// dropHelmUpdateEntries returns entries without the main-resource Update
// entries of the Helm managers: "terraform-provider-helm*" (helm_release) and
// "helm" (the Helm 3 CLI, or a helm_release entry an earlier hand-over
// renamed). Apply and subresource entries, and every other manager, are kept.
// changed is false, with entries returned as-is, when nothing is dropped.
// entries itself is never modified.
func dropHelmUpdateEntries(entries []metav1.ManagedFieldsEntry) (out []metav1.ManagedFieldsEntry, changed bool) {
	out = make([]metav1.ManagedFieldsEntry, 0, len(entries))

	for _, e := range entries {
		if e.Subresource == "" && e.Operation == metav1.ManagedFieldsOperationUpdate &&
			(strings.HasPrefix(e.Manager, helmProviderFieldManagerPrefix) || e.Manager == common.DefaultFieldManager) {
			changed = true

			continue
		}

		out = append(out, e)
	}

	if !changed {
		return entries, false
	}

	return out, true
}

// identityOnlyObjects indexes the stored release's identity-only manifests
// (isIdentityOnly) by group, kind and name, each with the namespaces it was
// rendered for ("" when the manifest names none).
type identityOnlyObjects map[schema.GroupKind]map[string][]string

// newIdentityOnlyObjects collects the identity-only objects of a stored
// release's manifests.
func newIdentityOnlyObjects(manifests []*unstructured.Unstructured) identityOnlyObjects {
	out := identityOnlyObjects{}

	for _, m := range manifests {
		if m == nil || !isIdentityOnly(m.Object) {
			continue
		}

		gk := m.GroupVersionKind().GroupKind()
		if out[gk] == nil {
			out[gk] = map[string][]string{}
		}

		out[gk][m.GetName()] = append(out[gk][m.GetName()], m.GetNamespace())
	}

	return out
}

// has reports whether ref is one of the identity-only objects. A manifest
// without a namespace matches any: Helm renders namespaced objects into the
// release namespace, and a cluster-scoped ref has none.
func (o identityOnlyObjects) has(ref ResourceRef) bool {
	for _, ns := range o[schema.GroupKind{Group: ref.Group, Kind: ref.Kind}][ref.Name] {
		if ns == "" || ns == ref.Namespace {
			return true
		}
	}

	return false
}

// isIdentityOnly reports whether a manifest sets nothing but its identity:
// apiVersion, kind, metadata.name and metadata.namespace, with empty values
// (null, "", {} and []) counting as unset. Server-side apply records none of
// those as owned, so applying such a manifest leaves its manager no
// managedFields entry.
func isIdentityOnly(obj map[string]any) bool {
	for k, v := range obj {
		switch k {
		case "apiVersion", "kind":
		case "metadata":
			md, ok := v.(map[string]any)
			if !ok {
				return isEmptyManifestValue(v)
			}

			for mk, mv := range md {
				if mk != "name" && mk != "namespace" && !isEmptyManifestValue(mv) {
					return false
				}
			}
		default:
			if !isEmptyManifestValue(v) {
				return false
			}
		}
	}

	return true
}

func isEmptyManifestValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	default:
		return false
	}
}
