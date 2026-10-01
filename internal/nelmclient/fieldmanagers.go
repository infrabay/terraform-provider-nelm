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
// whose patch hits an unavailable admission webhook is skipped, as nelm's own
// managedFields fix does, reported in the returned list (one line per object)
// and handed over by a later apply. timeout bounds the whole call.
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

	return handOverHelmProviderFieldManagers(ctx, factory.Mapper(), factory.Dynamic(), info.Resources)
}

// handOverHelmProviderFieldManagers is HandOverHelmProviderFieldManagers'
// cluster-facing body. It takes the RESTMapper and dynamic client explicitly
// so it can be unit tested against a fake API server.
func handOverHelmProviderFieldManagers(ctx context.Context, mapper meta.RESTMapper, dyn dynamic.Interface, refs []ResourceRef) ([]string, error) {
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

		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			return handOverObject(ctx, resClient, ref.Name)
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
// fresh GET, instead of having that writer's own entry overwritten.
func handOverObject(ctx context.Context, client dynamic.ResourceInterface, name string) error {
	obj, err := client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("get: %w", err)
	}

	entries, changed, err := handOverHelmProviderEntries(obj.GetManagedFields())
	if err != nil || !changed {
		return err
	}

	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"managedFields":   entries,
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
