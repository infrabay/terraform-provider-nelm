package provider

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
	"github.com/infrabay/terraform-provider-nelm/internal/planconv"
)

// releaseClient is the subset of *nelmclient.Client that releaseResource
// calls (CONTRACTS.md seam 1). Configure always stores a *nelmclient.Client;
// the interface exists so the plan and CRUD paths can be unit-tested offline
// against a fake — the branches that matter most (a refused adoption, a held
// release lock, an install that succeeded but could not be read back, a
// failed update) cannot be reached against a real cluster in a unit test.
//
// It embeds planconv.KeyScoper because the same client is handed to the
// planconv map builders as their scoper (seam 2's bold invariant).
type releaseClient interface {
	planconv.KeyScoper

	// ConfigUnknown reports the nelmclient.NewUnknownConfigClient
	// placeholder: the provider configuration is not known at plan time.
	ConfigUnknown() bool
	Plan(ctx context.Context, spec nelmclient.ReleaseSpec, timeout time.Duration) (*nelmclient.PlanResult, error)
	Render(ctx context.Context, spec nelmclient.ReleaseSpec, timeout time.Duration) ([]*unstructured.Unstructured, error)
	Install(ctx context.Context, spec nelmclient.ReleaseSpec, timeout time.Duration) error
	Uninstall(ctx context.Context, name, namespace, storageDriver string, timeout time.Duration) error
	Get(ctx context.Context, name, namespace, storageDriver string, timeout time.Duration) (*nelmclient.ReleaseInfo, error)
	History(ctx context.Context, name, namespace, storageDriver string, timeout time.Duration) (*nelmclient.ReleaseHistory, error)
	HandOverHelmProviderFieldManagers(ctx context.Context, name, namespace, storageDriver string, timeout time.Duration) ([]string, error)
	LiveObjects(ctx context.Context, refs []nelmclient.ResourceRef) (map[nelmclient.ResourceRef]*unstructured.Unstructured, error)
}

var _ releaseClient = (*nelmclient.Client)(nil)
