# managedFields probe evidence

Deployment: tfnelm-fix-helmv4-b38442/helmv4-basic

- managedfields_before.yaml: captured via kubectl BEFORE any nelm plan ran
  against this helm-v4-created Deployment.
- managedfields_after.yaml: captured AFTER the first
  "nelm release plan install" (a PLAN, not an apply) ran against it.
- managedfields_after2.yaml: captured after a SECOND plan.

managedFields changed after the first plan: false
managedFields stable between the first and second plan (after == after2): true

## Finding: the managedFields assumption does not hold for the common case

The working assumption was "first plan against helm-created resources
MergePatches metadata.managedFields" (verified against nelm v1.24.0).
Live-tested here against nelm v1.26.2 and a plain helm v4.2.3
server-side-apply install, NO managedFields mutation was observed on
either the first or second plan.

Root cause (read directly from the local nelm checkout,
github.com/werf/nelm v1.26.2-2-gda9a86a):

  - pkg/common/common.go:109 -- "DefaultFieldManager = \"helm\"". nelm
    deliberately uses the SAME field-manager name Helm itself uses, for
    exactly this import-compatibility reason.
  - pkg/plan/resource_info.go fixManagedFields(): it looks up an existing
    managedFields entry with Manager=="helm" && Operation=="Apply" as
    "oursEntry". For a resource created by helm v4 (which uses
    server-side-apply with manager "helm"), this entry ALREADY EXISTS with
    real content, so nelm considers it already-owned -- there is nothing to
    "fix".
  - The function only sets changed=true when: (a) a LEGACY
    Manager=="helm" && Operation=="Update" entry needs migrating to the
    Apply style (fixHelmUpdateManagedFields) -- not present here since helm
    v4 already uses Apply; (b) the resource is a ServiceAccount needing a
    secrets-field fix -- not our case; (c) removeUndesirableManagers finds
    a manager literally named "kubectl-edit"
    (common.KubectlEditFieldManager) or prefixed "werf"
    (common.OldFieldManagerPrefix) -- neither was present (our managedFields
    were only "helm"/Apply and "k3s"/Update on the status subresource,
    which is skipped because its Subresource differs from oursEntry's).

CONCLUSION for importing plain-helm releases: adopting a modern helm v3/v4
server-side-apply release is actually BETTER than assumed for
this specific side effect -- no unexpected managedFields rewrite occurs on
a plain `terraform plan` against a freshly-imported plain-helm release. The
assumption as literally stated likely applies to a narrower scenario
this probe did not reproduce: resources carrying a LEGACY field manager
(an old client-side-apply "kubectl-edit" entry, or an old "werf"-prefixed
manager name from a pre-server-side-apply werf/nelm version) -- neither of
which a fresh helm v4.2.3 install produces. The provider should
treat "first plan may rewrite managedFields" as a possible-but-not-
guaranteed side effect for modern helm-created resources, not an
unconditional one; it likely still applies to resources migrating from
much older werf/helm client-side-apply conventions, which this fixture
does not cover (flagging as a gap, not fabricating a fixture for a scenario
that could not be cheaply reproduced live).
