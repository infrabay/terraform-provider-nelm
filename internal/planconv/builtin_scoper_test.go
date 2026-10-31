package planconv

import (
	"errors"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestBuiltinScoper(t *testing.T) {
	tests := []struct {
		name           string
		gvk            schema.GroupVersionKind
		wantNamespaced bool
		wantNoMatch    bool
	}{
		{name: "core Namespace", gvk: schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}},
		{name: "core ConfigMap", gvk: gvkConfigMap, wantNamespaced: true},
		{name: "apps Deployment", gvk: gvkDeployment, wantNamespaced: true},
		{name: "rbac ClusterRole", gvk: gvkClusterRole},
		{name: "rbac Role", gvk: schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"}, wantNamespaced: true},
		{
			name:           "the version is not part of the key",
			gvk:            schema.GroupVersionKind{Group: "apps", Version: "v1beta2", Kind: "Deployment"},
			wantNamespaced: true,
		},
		{
			name:           "a removed group still resolves",
			gvk:            schema.GroupVersionKind{Group: "extensions", Version: "v1beta1", Kind: "Ingress"},
			wantNamespaced: true,
		},

		// Kinds a default 1.36 server does not serve, scoped as k8s.io/api
		// declares them: behind alpha feature gates, then removed.
		{name: "alpha PodCertificateRequest", gvk: schema.GroupVersionKind{Group: "certificates.k8s.io", Version: "v1beta1", Kind: "PodCertificateRequest"}, wantNamespaced: true},
		{name: "alpha LeaseCandidate", gvk: schema.GroupVersionKind{Group: "coordination.k8s.io", Version: "v1beta1", Kind: "LeaseCandidate"}, wantNamespaced: true},
		{name: "alpha DeviceTaintRule", gvk: schema.GroupVersionKind{Group: "resource.k8s.io", Version: "v1alpha3", Kind: "DeviceTaintRule"}},
		{name: "alpha ResourcePoolStatusRequest", gvk: schema.GroupVersionKind{Group: "resource.k8s.io", Version: "v1alpha3", Kind: "ResourcePoolStatusRequest"}},
		{name: "alpha PodGroup", gvk: schema.GroupVersionKind{Group: "scheduling.k8s.io", Version: "v1alpha2", Kind: "PodGroup"}, wantNamespaced: true},
		{name: "alpha Workload", gvk: schema.GroupVersionKind{Group: "scheduling.k8s.io", Version: "v1alpha1", Kind: "Workload"}, wantNamespaced: true},
		{name: "removed AuditSink", gvk: schema.GroupVersionKind{Group: "auditregistration.k8s.io", Version: "v1alpha1", Kind: "AuditSink"}},
		{name: "removed PodScheduling", gvk: schema.GroupVersionKind{Group: "resource.k8s.io", Version: "v1alpha1", Kind: "PodScheduling"}, wantNamespaced: true},
		{name: "removed PodSchedulingContext", gvk: schema.GroupVersionKind{Group: "resource.k8s.io", Version: "v1alpha2", Kind: "PodSchedulingContext"}, wantNamespaced: true},
		{name: "removed ResourceClaimParameters", gvk: schema.GroupVersionKind{Group: "resource.k8s.io", Version: "v1alpha2", Kind: "ResourceClaimParameters"}, wantNamespaced: true},
		{name: "removed ResourceClass", gvk: schema.GroupVersionKind{Group: "resource.k8s.io", Version: "v1alpha2", Kind: "ResourceClass"}},
		{name: "removed ResourceClassParameters", gvk: schema.GroupVersionKind{Group: "resource.k8s.io", Version: "v1alpha2", Kind: "ResourceClassParameters"}, wantNamespaced: true},
		{name: "removed PodPreset", gvk: schema.GroupVersionKind{Group: "settings.k8s.io", Version: "v1alpha1", Kind: "PodPreset"}, wantNamespaced: true},
		{name: "a webhook payload is not a resource", gvk: schema.GroupVersionKind{Group: "imagepolicy.k8s.io", Version: "v1alpha1", Kind: "ImageReview"}, wantNoMatch: true},

		{name: "a custom resource", gvk: schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"}, wantNoMatch: true},
		{name: "a built-in kind name in another group", gvk: schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "ConfigMap"}, wantNoMatch: true},
		{name: "an unknown core kind", gvk: schema.GroupVersionKind{Version: "v1", Kind: "Gadget"}, wantNoMatch: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			namespaced, err := BuiltinScoper{}.IsNamespaced(tt.gvk)

			if !tt.wantNoMatch {
				if err != nil || namespaced != tt.wantNamespaced {
					t.Errorf("IsNamespaced(%s) = %v, %v; want %v, nil", tt.gvk, namespaced, err, tt.wantNamespaced)
				}

				return
			}

			var noMatch *meta.NoKindMatchError
			if !errors.As(err, &noMatch) {
				t.Fatalf("IsNamespaced(%s) = %v, %v; want a *meta.NoKindMatchError", tt.gvk, namespaced, err)
			}

			if noMatch.GroupKind != tt.gvk.GroupKind() || !reflect.DeepEqual(noMatch.SearchedVersions, []string{tt.gvk.Version}) {
				t.Errorf("NoKindMatchError = %+v, want GroupKind %s, SearchedVersions [%s]", noMatch, tt.gvk.GroupKind(), tt.gvk.Version)
			}
		})
	}
}

// TestBuiltinScoper_InRenderScoper: offline, a custom resource takes its scope
// from a CRD in the render, and one without a CRD is reported as unresolved.
func TestBuiltinScoper_InRenderScoper(t *testing.T) {
	gvkGadget := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Gadget"}
	gvkMonitor := schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"}

	scoper := NewRenderScoper([]*unstructured.Unstructured{
		crd("example.com", "Gadget", "Namespaced", "v1"),
		crd("cert-manager.io", "ClusterIssuer", "Cluster", "v1"),
		cr(gvkGadget, "g"),
	}, BuiltinScoper{})

	for _, tt := range []struct {
		gvk  schema.GroupVersionKind
		want bool
	}{
		{gvk: gvkDeployment, want: true},
		{gvk: gvkClusterRole, want: false},
		{gvk: gvkGadget, want: true},
		{gvk: gvkClusterIssuer, want: false},
	} {
		if namespaced, err := scoper.IsNamespaced(tt.gvk); err != nil || namespaced != tt.want {
			t.Errorf("IsNamespaced(%s) = %v, %v; want %v, nil", tt.gvk, namespaced, err, tt.want)
		}
	}

	if got := scoper.Unresolved(); len(got) != 0 {
		t.Errorf("unresolved = %v, want none (built-in kinds and kinds with a CRD in the render)", got)
	}

	var noMatch *meta.NoKindMatchError
	if _, err := scoper.IsNamespaced(gvkMonitor); !errors.As(err, &noMatch) {
		t.Errorf("IsNamespaced(%s) error = %v, want a *meta.NoKindMatchError", gvkMonitor, err)
	}

	if got := scoper.Unresolved(); !reflect.DeepEqual(got, []schema.GroupVersionKind{gvkMonitor}) {
		t.Errorf("unresolved = %v, want [%v]", got, gvkMonitor)
	}
}
