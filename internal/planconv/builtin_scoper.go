package planconv

import (
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// BuiltinScoper is a KeyScoper that needs no cluster: it answers from a
// static table of the built-in Kubernetes kinds (builtinKindScopes), whatever
// the version asked for (a built-in kind keeps its scope across versions).
// Any other kind — a custom resource — is a *meta.NoKindMatchError, exactly
// what a cluster that does not serve the kind answers, so
// NewRenderScoper(objs, BuiltinScoper{}) scopes it from the CRDs in the
// render and records it as Unresolved when the render has none.
//
// It serves the offline nelm_template render, which restores the release
// namespace nelm strips from namespace-scoped objects and has no cluster to
// ask. It must never key a nelm_release "resources" map: both sides of that
// diff ask the cluster's RESTMapper (CONTRACTS.md seam 2), which knows the
// cluster's own kinds and versions.
type BuiltinScoper struct{}

var _ KeyScoper = BuiltinScoper{}

// IsNamespaced implements KeyScoper.
func (BuiltinScoper) IsNamespaced(gvk schema.GroupVersionKind) (bool, error) {
	namespaced, ok := builtinKindScopes[gvk.GroupKind()]
	if !ok {
		return false, &meta.NoKindMatchError{GroupKind: gvk.GroupKind(), SearchedVersions: []string{gvk.Version}}
	}

	return namespaced, nil
}

// builtinKindScopes is whether each built-in kind is namespace-scoped, keyed
// by group ("" is the core group) and kind. It was generated from the API
// discovery of a Kubernetes 1.36 API server (every group, kind and scope it
// serves, `kubectl api-resources`), plus the kinds a default 1.36 server
// does not serve but charts still render, on purpose: removed ones (the
// extensions group, PodSecurityPolicy, PodPreset, AuditSink, ClusterCIDR,
// the alpha DRA kinds resource.k8s.io dropped) and ones behind alpha feature
// gates. Those come from every top-level type k8s.io/api v0.18 to v0.36
// declares (+genclient; cluster-scoped when +genclient:nonNamespaced),
// except the ones no API server serves as a resource: imagepolicy.k8s.io
// ImageReview (an admission webhook's payload) and kinds served only as a
// subresource, such as policy Eviction.
//
// Only kinds built into the Kubernetes API server belong here. A custom
// resource's scope is whatever its CRD says, which may differ between
// clusters; it is left to the CRDs in the render (NewRenderScoper) or to the
// cluster (validate = true).
var builtinKindScopes = map[schema.GroupKind]bool{
	{Group: "", Kind: "Binding"}:               true,
	{Group: "", Kind: "ComponentStatus"}:       false,
	{Group: "", Kind: "ConfigMap"}:             true,
	{Group: "", Kind: "Endpoints"}:             true,
	{Group: "", Kind: "Event"}:                 true,
	{Group: "", Kind: "LimitRange"}:            true,
	{Group: "", Kind: "Namespace"}:             false,
	{Group: "", Kind: "Node"}:                  false,
	{Group: "", Kind: "PersistentVolume"}:      false,
	{Group: "", Kind: "PersistentVolumeClaim"}: true,
	{Group: "", Kind: "Pod"}:                   true,
	{Group: "", Kind: "PodTemplate"}:           true,
	{Group: "", Kind: "ReplicationController"}: true,
	{Group: "", Kind: "ResourceQuota"}:         true,
	{Group: "", Kind: "Secret"}:                true,
	{Group: "", Kind: "Service"}:               true,
	{Group: "", Kind: "ServiceAccount"}:        true,

	{Group: "admissionregistration.k8s.io", Kind: "MutatingAdmissionPolicy"}:          false,
	{Group: "admissionregistration.k8s.io", Kind: "MutatingAdmissionPolicyBinding"}:   false,
	{Group: "admissionregistration.k8s.io", Kind: "MutatingWebhookConfiguration"}:     false,
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingAdmissionPolicy"}:        false,
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingAdmissionPolicyBinding"}: false,
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingWebhookConfiguration"}:   false,

	{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}: false,

	{Group: "apiregistration.k8s.io", Kind: "APIService"}: false,

	{Group: "apps", Kind: "ControllerRevision"}: true,
	{Group: "apps", Kind: "DaemonSet"}:          true,
	{Group: "apps", Kind: "Deployment"}:         true,
	{Group: "apps", Kind: "ReplicaSet"}:         true,
	{Group: "apps", Kind: "StatefulSet"}:        true,

	{Group: "auditregistration.k8s.io", Kind: "AuditSink"}: false,

	{Group: "authentication.k8s.io", Kind: "SelfSubjectReview"}: false,
	{Group: "authentication.k8s.io", Kind: "TokenReview"}:       false,

	{Group: "authorization.k8s.io", Kind: "LocalSubjectAccessReview"}: true,
	{Group: "authorization.k8s.io", Kind: "SelfSubjectAccessReview"}:  false,
	{Group: "authorization.k8s.io", Kind: "SelfSubjectRulesReview"}:   false,
	{Group: "authorization.k8s.io", Kind: "SubjectAccessReview"}:      false,

	{Group: "autoscaling", Kind: "HorizontalPodAutoscaler"}: true,

	{Group: "batch", Kind: "CronJob"}: true,
	{Group: "batch", Kind: "Job"}:     true,

	{Group: "certificates.k8s.io", Kind: "CertificateSigningRequest"}: false,
	{Group: "certificates.k8s.io", Kind: "ClusterTrustBundle"}:        false,
	{Group: "certificates.k8s.io", Kind: "PodCertificateRequest"}:     true,

	{Group: "coordination.k8s.io", Kind: "Lease"}:          true,
	{Group: "coordination.k8s.io", Kind: "LeaseCandidate"}: true,

	{Group: "discovery.k8s.io", Kind: "EndpointSlice"}: true,

	{Group: "events.k8s.io", Kind: "Event"}: true,

	{Group: "extensions", Kind: "DaemonSet"}:         true,
	{Group: "extensions", Kind: "Deployment"}:        true,
	{Group: "extensions", Kind: "Ingress"}:           true,
	{Group: "extensions", Kind: "NetworkPolicy"}:     true,
	{Group: "extensions", Kind: "PodSecurityPolicy"}: false,
	{Group: "extensions", Kind: "ReplicaSet"}:        true,

	{Group: "flowcontrol.apiserver.k8s.io", Kind: "FlowSchema"}:                 false,
	{Group: "flowcontrol.apiserver.k8s.io", Kind: "PriorityLevelConfiguration"}: false,

	{Group: "internal.apiserver.k8s.io", Kind: "StorageVersion"}: false,

	{Group: "networking.k8s.io", Kind: "ClusterCIDR"}:   false,
	{Group: "networking.k8s.io", Kind: "Ingress"}:       true,
	{Group: "networking.k8s.io", Kind: "IngressClass"}:  false,
	{Group: "networking.k8s.io", Kind: "IPAddress"}:     false,
	{Group: "networking.k8s.io", Kind: "NetworkPolicy"}: true,
	{Group: "networking.k8s.io", Kind: "ServiceCIDR"}:   false,

	{Group: "node.k8s.io", Kind: "RuntimeClass"}: false,

	{Group: "policy", Kind: "PodDisruptionBudget"}: true,
	{Group: "policy", Kind: "PodSecurityPolicy"}:   false,

	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"}:        false,
	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRoleBinding"}: false,
	{Group: "rbac.authorization.k8s.io", Kind: "Role"}:               true,
	{Group: "rbac.authorization.k8s.io", Kind: "RoleBinding"}:        true,

	{Group: "resource.k8s.io", Kind: "DeviceClass"}:               false,
	{Group: "resource.k8s.io", Kind: "DeviceTaintRule"}:           false,
	{Group: "resource.k8s.io", Kind: "PodScheduling"}:             true,
	{Group: "resource.k8s.io", Kind: "PodSchedulingContext"}:      true,
	{Group: "resource.k8s.io", Kind: "ResourceClaim"}:             true,
	{Group: "resource.k8s.io", Kind: "ResourceClaimParameters"}:   true,
	{Group: "resource.k8s.io", Kind: "ResourceClaimTemplate"}:     true,
	{Group: "resource.k8s.io", Kind: "ResourceClass"}:             false,
	{Group: "resource.k8s.io", Kind: "ResourceClassParameters"}:   true,
	{Group: "resource.k8s.io", Kind: "ResourcePoolStatusRequest"}: false,
	{Group: "resource.k8s.io", Kind: "ResourceSlice"}:             false,

	{Group: "scheduling.k8s.io", Kind: "PodGroup"}:      true,
	{Group: "scheduling.k8s.io", Kind: "PriorityClass"}: false,
	{Group: "scheduling.k8s.io", Kind: "Workload"}:      true,

	{Group: "settings.k8s.io", Kind: "PodPreset"}: true,

	{Group: "storage.k8s.io", Kind: "CSIDriver"}:             false,
	{Group: "storage.k8s.io", Kind: "CSINode"}:               false,
	{Group: "storage.k8s.io", Kind: "CSIStorageCapacity"}:    true,
	{Group: "storage.k8s.io", Kind: "StorageClass"}:          false,
	{Group: "storage.k8s.io", Kind: "VolumeAttachment"}:      false,
	{Group: "storage.k8s.io", Kind: "VolumeAttributesClass"}: false,

	{Group: "storagemigration.k8s.io", Kind: "StorageVersionMigration"}: false,
}
