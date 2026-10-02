---
page_title: "Known limitations"
subcategory: ""
description: |-
  Known limitations of nelm_release, and the differences from helm_release that matter when migrating, with their workarounds.
---

# Known limitations

`nelm_release` is a young (v0.x) resource. This page lists its known
limitations and the differences from `hashicorp/helm`'s `helm_release` that
matter in production, each with its impact and a workaround where there is
one. Read it together with
[Migrating from `helm_release`](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/migrating-from-helm_release.md)
before moving production releases.

## Provider configuration and cluster access

- **A provider configuration that is unknown at plan time only works for new
  releases.** When `host` (or any provider attribute) depends on something
  applied in the same run — typically a GKE cluster created or replaced
  alongside its releases — new `nelm_release`s plan with an Unknown diff and
  are installed at apply, but releases already in state cannot be refreshed
  or planned and the plan fails. Apply the cluster change first with
  `-target`, or keep the cluster and its releases in separate root modules.
  (`helm_release` gets through this case only because it silently drops an
  unreadable release from state.) Terraform's experimental deferred actions
  are honoured when the CLI enables them.

- **`$KUBECONFIG` and an in-cluster service account are never used
  implicitly.** The kubeconfig comes from `kube_config_paths` /
  `KUBE_CONFIG_PATH(S)` (or `~/.kube/config` for an explicit
  `kube_context` attribute — `KUBE_CTX` alone is not enough, as with the
  `helm` provider); a configuration that names no cluster is an error by
  design. Inside a pod, pass `host` / `token` /
  `cluster_ca_certificate` explicitly.

- **A token from a data source is not refreshed during a run.** A `token`
  or `registries` password taken from `google_client_config` (or any data
  source) is read once, when the run starts — a saved plan keeps the one it
  was made with — so an apply that outlives the token (about an hour, often
  less), or a saved plan applied after it expired, fails mid-apply with
  `401 Unauthorized`. The `helm` and `kubernetes` providers behave the same.
  For long applies or saved plans, connect with a kubeconfig whose user runs
  an exec plugin (`gke-gcloud-auth-plugin`); see the
  [GKE example](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/index.md#gke--a-private-oci-chart-google-artifact-registry).

- **`terraform plan` writes to the cluster and needs write access.**
  Planning an existing release is not read-only. Nelm's plan runs a dry-run
  server-side apply of every existing object, which the API server
  authorizes like a real `patch`, and before that it rewrites some objects'
  `metadata.managedFields` with a real (not dry-run) patch: on the first
  plan of a release last written by the Helm 3 CLI, and whenever another
  field manager co-owns fields Nelm's own manager owns (see
  [`managedFields` and `terraform plan`](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#managedfields-and-terraform-plan)).
  That patch changes only metadata (no rollout) and does not show in the
  plan. The credentials `terraform plan` uses therefore need `get`, `list`
  and `patch` on every kind the chart renders, and read access to the
  release's storage Secrets (or ConfigMaps), like the apply's. With a
  read-only plan identity the plan fails on such a patch (`cannot patch`).
  An object that needs no such patch but whose dry run is refused is
  planned as a "blind apply" instead: the plan succeeds with a
  `nelm_release: blind apply for <key>` warning carrying the API server's
  error, and the object's planned value is still the chart's render (an
  unchanged object plans no change), only without the API server's
  validation. Provider versions before 0.1.1 failed such a plan with
  `read plan artifact: decode artifact data json: ... dryApplyErr of type error`
  ([#8](https://github.com/infrabay/terraform-provider-nelm/issues/8)).
  `helm_release` (without its `manifest` experiment) plans with read access
  only. Plan and apply with the same writer identity.

- **Nelm keeps a lock ConfigMap in every release namespace.** Every apply
  and destroy gets an unlabelled ConfigMap named `werf-synchronization` in
  the release namespace, creating it with a server-side apply if it is
  missing, and keeps per-release lease locks in its annotations
  (`lockgate.io/<hash>`); every Nelm or werf release in that namespace
  shares it. Since Nelm 1.27, every install (each create and update of a
  release) also starts with a dry-run server-side apply of that ConfigMap,
  which Nelm uses to tell whether the release namespace exists (see
  [Namespace lifecycle](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#namespace-lifecycle)).
  The provider therefore needs `get`, `create`, `update` and `patch` on
  ConfigMaps in each release namespace, even with
  `release_storage_driver = "secret"`. Without `patch` there, the dry run
  is refused and Nelm server-side-applies the release's `Namespace` object
  instead, which needs `patch` on Namespaces, a permission a namespaced
  `Role` cannot grant. With neither, every create and update fails with
  `create release namespace: can't apply ConfigMap for locking, and can't apply release namespace`.
  An admission policy that requires labels on ConfigMaps must exempt this
  one, or applies fail with
  `create release namespace: dry-run apply release synchronization configmap`,
  `can't apply ConfigMap for locking` or
  `unable to prepare kubernetes cm/werf-synchronization`, depending on how
  the policy rejects it.
  `terraform destroy` leaves it in place (and creates it if it is missing);
  Helm and `helm_release` ignore it. Delete it by hand only when no `nelm_release`
  (and no Nelm or werf CLI user) is left in the namespace. Upstream in Nelm.

## Drift and out-of-band changes

- **A field a controller owns that the chart also renders makes every plan
  an update.** When a chart renders a field that a controller or operator
  then changes — `replicas` rendered unconditionally next to a
  HorizontalPodAutoscaler or a KEDA `ScaledObject`, a webhook `caBundle`
  (often rendered empty) that an injector fills in, an image tag an image
  updater rewrites — the live value differs from the chart's on every
  refresh. Every plan then shows that object changing (e.g. `replicas`
  `7` -> `2`), and every apply of the root module, including one made for an
  unrelated change, runs Nelm's install: it resets the field (scaling the
  Deployment down until the autoscaler scales it up again, or blanking the
  CA until it is injected again), creates a new release revision (pushing
  older ones out of the history limit) and runs the chart's upgrade hooks,
  which `resources` does not show. The plan does not converge while the
  controller keeps changing the field. `helm_release` (without its
  `manifest` experiment) does not compare live objects and plans nothing.
  `lifecycle { ignore_changes = [resources] }` has no effect: `resources`
  is computed, and Terraform reports the entry as redundant. Workarounds:
  stop the chart from rendering the field while a controller owns it (most
  charts guard `replicas` with their autoscaling value), or set
  `diff_mode = "none"` on the release, which plans like `helm_release`: no
  object diff and no drift detection, and an install only when the
  release's inputs change.

- **Out-of-band hotfixes are reverted by the next apply.** Any out-of-band
  change to a field the chart renders —
  `helm upgrade --reuse-values --set image.tag=...`, `helm rollback`,
  `kubectl set image`, `kubectl scale`, `kubectl edit` — is drift: the next
  plan shows the release changing, and the next apply of the root module,
  whatever change it is made for, re-installs the configured chart and
  values and so reverts the hotfix.
  `helm_release` (without its `manifest` experiment) keeps a successful
  hotfix or rollback that did not change the chart version until its own
  configuration changes, and then reverts it without showing it. Make
  on-call fixes through Terraform, or mirror them into the configuration
  before the next apply of that root module, and read the `resources` diff
  of releases nobody meant to change. `ignore_changes` has no effect here
  either; `diff_mode = "none"` restores `helm_release`'s behavior for a
  release.

- **Changes that render the same objects can plan nothing.** A plan updates
  a release when one of its arguments changes or an object the chart renders
  differs from the live one. A change confined to what only the release
  record holds — hooks, `NOTES.txt`, values that only hooks or notes use —
  passes neither check:
  - editing a local chart's hooks or `NOTES.txt` without changing any
    argument (same chart path, same `version`) gives an empty plan, and the
    change reaches the cluster only with the next update made for another
    reason;
  - an out-of-band `helm upgrade` or `helm rollback` confined to those parts,
    or to the chart version of a chart that does not stamp its version into
    labels, gives an empty plan too: the refresh records the new revision's
    `metadata.chart_version` and `values_json` without planning a change, so
    the configured chart is not applied again until something else changes
    (`helm_release` catches the chart-version case as a `version` change).
    Undo such a change with `helm rollback` to the revision Terraform
    applied, or apply the configuration again through any in-place change
    of the release (its `timeouts`, for example).

  A failed or pending release is not affected: it always re-plans as an
  update until it is deployed (though the apply refuses to run over a
  pending revision until it is stale, see
  [Release lifecycle](#release-lifecycle)). Nelm's own plan detects most
  such changes ("no resource changes planned, but still must install
  release", and since Nelm 1.27 it also says why), but the provider does
  not act on that signal yet. Acting on it would cover the hooks,
  `NOTES.txt` and values cases above, but not an out-of-band change of
  only the chart version: Nelm's check does not compare chart versions.

## Diff surface (`resources`)

- **List projection is positional, not merge-key aware.** The live→desired
  projection pairs list elements by index (`containers[i]`, `env[i]`, …).
  Kubernetes strategic-merge pairs them by an identity key (usually `name`).
  If something injects a list element *before* a chart-managed one (e.g. an
  admission webhook prepends a sidecar container), positional projection
  misaligns and can misreport that element or hide drift in the real one.
  Appended injections are handled correctly. A merge-key-aware projection is
  planned for v1.0.

- **Objects whose render changes on every render** (random or time-based
  template functions: `rollme` annotations, `genCA` certificates, generated
  passwords, deploy timestamps) are detected by rendering the chart twice per
  plan — a second chart render on every plan. Such an object is
  `(known after apply)` whenever the release is reinstalled, so the plan
  does not show which of its fields change, and out-of-band changes to its
  random fields are not drift. Two kinds of templates escape the check: a
  `.Release.Revision`-dependent one renders the next revision against the
  live one on every plan (a perpetual update and rollout), and one whose
  output changes only once a second or slower (`now | unixEpoch`,
  `now | date ...`) can render identically twice within one plan and
  differently at apply, which aborts the apply with "Provider produced
  inconsistent final plan". Use `diff_mode = "none"` for those (no object
  diff and no drift detection for that release, like `helm_release`), or
  make the chart deterministic. See
  [Non-deterministic charts](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#non-deterministic-charts).

- **Kinds the cluster does not serve at plan time.** When a chart renders
  objects of a kind that is not served yet and whose
  CustomResourceDefinition the chart itself does not contain — typically a
  CRD installed by another release earlier in the same apply (cert-manager
  and a chart of ClusterIssuers) — the whole `resources` map, with
  `status`/`revision`/`metadata`, is known after apply, with a warning.
  Templates gated on `.Capabilities.APIVersions.Has`, or on a `lookup` of
  objects another release creates in the same apply, give no such signal:
  they can still render differently at apply and abort it. Apply the
  providing release first (`-target`) or apply twice.

- **A chart-rendered field the API server refuses to persist (dropped via
  `omitempty`/pruning) shows as permanent drift.** The desired side always
  renders the field; the live side never has it. Rare in practice (the field
  is doing nothing anyway); fix requires schema-aware comparison.

- **A resource whose only change is its `apiVersion`** (e.g. an HPA moving
  `autoscaling/v1` → `v2`) leaves the old versioned map key in state for one
  refresh cycle; the next `Read` rebuilds the map from live refs and it
  clears. Cosmetic, self-healing.

- **`werf.io/resource-policy` skip policies are invisible in the diff.**
  Since Nelm 1.26, a resource annotated with a `skip-create`, `skip-update`
  or `skip-recreate` policy gets no planned change, so `resources` keeps its
  prior/live entry and a chart change to it is not shown; the apply leaves
  the object untouched. A `keep` / `skip-delete` resource removed from the
  chart, or left behind by a destroy, stays in the cluster (as with
  `helm.sh/resource-policy: keep`); its key stays in state until the next
  refresh drops it. An invalid policy value fails the plan.

- **Secret redaction placeholders embed a truncated unsalted SHA-256 and the
  value's byte length.** This holds for `Secret` data and for scrubbed
  `set_sensitive` values alike. Deterministic placeholders are what make
  Secret drift visible without cleartext, but they also let someone with
  plan output/state verify a GUESS of a low-entropy secret offline. Use
  high-entropy secrets (which are immune); a salted scheme is being
  considered for v1.0.

- **Only `set_sensitive` values are scrubbed from non-`Secret` objects.** A
  secret that reaches a chart through `values` or `set` — even from a
  `sensitive = true` variable — appears in cleartext in `resources` (plan
  output and state) wherever the chart renders it outside a `Secret`:
  Terraform never tells a provider which inputs are sensitive.
  `hashicorp/helm`'s `helm_release` prints no rendered manifest by default,
  so this is new exposure for such configurations; pass these values through
  `set_sensitive`. `set_sensitive` values themselves are scrubbed only where
  they are rendered verbatim (or quoted, JSON-escaped or base64-encoded
  whole), only from strings and keys, and only when at least 4 bytes long; a
  hashed, partial or otherwise transformed rendering is not recognized. See
  [Sensitive values in non-`Secret` resources](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#sensitive-values-in-non-secret-resources).

- **State can hold a `set_sensitive` value in cleartext right after an
  import.** Read only knows the `set_sensitive` names and values stored in
  state (and, under those names, the values of the release's last
  revision): none after `terraform import` until the first apply.
  Self-healing; a `moved` block from `helm_release` carries the values over
  and is not affected.

## Plan size

- **`crds/` CRDs show up as additions in every plan that changes them.**
  Nelm does not store a chart's `crds/` CustomResourceDefinitions in the
  release, so a refresh never reads them back, but its plan reports the ones
  it creates or updates. A create, and every upgrade that changes such a CRD
  (a chart version bump that moves the CRDs' version labels, an out-of-band
  edit of a CRD), therefore lists each one in `resources` as a full-body
  addition (`+`), and the refresh after the apply drops it from state again.
  For operator charts this is large: a kube-prometheus-stack version bump
  adds its ten Prometheus Operator CRDs, about 2.5 MB of JSON, to the plan
  and, until the next refresh, to state, and since their
  `apiextensions.k8s.io/...` keys sort first, a size-capped pull-request
  comment shows nothing else. With `no_install_crds = true` (see
  [Chart versions and CRDs](#chart-versions-and-crds)) upgrades no longer
  plan them; a create plan still lists them (below).

- **Plans for large charts are large.** Every object a change touches is
  printed as its full canonical JSON, and a chart version bump touches almost
  every object, because charts stamp their version into the `helm.sh/chart`
  and `app.kubernetes.io/version` labels: a routine kube-prometheus-stack or
  kyverno bump prints tens of kilobytes where `helm_release` prints
  `~ version`. An `import` block prints the whole map, templated CRDs
  included: several megabytes for a chart such as kyverno. And since
  `resources` sorts before `values` and `version`, a size-capped
  pull-request comment can be cut off before the change that triggered the
  plan. Review such plans from the full plan output (the job log or a saved
  plan file), and move large releases from `helm_release` one per change.

## Chart versions and CRDs

- **An unset `version` follows the newest chart on every plan.** With
  `version` unset, every plan, chart render and install resolves the newest
  chart version in the repository on its own. A new upstream chart release
  therefore becomes an upgrade at the next apply of the root module without
  any configuration change, and the plan shows only the objects' changes
  (`metadata.chart_version` is known after apply), not a `version` line. A
  saved plan aborts with "Provider produced inconsistent final plan" when a
  chart is released between the plan and the apply, and the install
  resolves the version once more after the plan. `helm_release` keeps the
  installed version on a plan with no other change. Pin `version` to an
  exact version for every chart from a repository or registry.

- **`crds/` CRDs are applied on every install and upgrade.** Helm (and
  `helm_release`) only creates a chart's `crds/` CustomResourceDefinitions
  when they are missing and never updates them. Nelm server-side-applies
  them with force on every install, upgrade and rollback, from the chart
  and every enabled subchart, with no ownership check: an existing CRD —
  including one upgraded out of band (`kubectl apply --server-side` with a
  newer version) or owned by another release or tool — is rewritten to the
  chart's copy, and a chart downgrade that drops a version still listed in
  a CRD's `status.storedVersions` fails the apply. The first `nelm_release`
  apply after a migration is the first update of those CRDs since
  `helm_release` created them. Before migrating, check every chart with a
  `crds/` directory (subcharts included) and set `no_install_crds = true`
  where the CRDs are managed elsewhere: a separate CRD chart or release, a
  CRD upgrade Job (kube-prometheus-stack's `crds.upgradeJob`), CRDs shared
  by several releases. With it, the CRDs must already exist when the release
  is first installed; turning it on for an installed release deletes
  nothing.

- **A create plan lists the chart's `crds/` CRDs even with
  `no_install_crds = true`.** The planned `resources` of a create is the
  chart's render, which includes `crds/`; the first refresh after the apply
  drops them again. Cosmetic.

## Release lifecycle

- **`create_before_destroy` cannot replace a release under the same name.**
  The new and the old object are the same Helm release — across a
  `release_storage_driver` change too — so the create half of such a
  replacement is refused (`already exists`, or `already exists in the
  <driver> storage backend` for a driver change): the apply fails and the
  release stays installed. Remove `create_before_destroy` — Terraform also
  enables it implicitly when a dependent resource has it (`+/-` in the plan
  instead of `-/+`). Only the apply refuses: the plan does not warn for a
  driver change or a `-replace`. With `adopt_existing = true` the
  same-backend refusal is off and such a replacement would uninstall the
  release, so set that flag only for the apply that adopts a release. The
  driver-change check reads the other backend's Secrets or ConfigMaps; if
  the provider's credentials may not list them, Create only warns, and a
  `create_before_destroy` driver change would again install into the new
  backend and then uninstall the release from the old one.

- **The pending-release lock is a fixed-age heuristic.** The apply refuses
  to install over a `pending-*` revision younger than the operation timeout
  (at least 15 minutes, not configurable) and takes an older one over. The
  age comes from the timestamp the writer (helm, nelm) stored, so large
  clock skew between machines shifts it; an operation that starts between
  the provider's history read and nelm's install is not detected; destroy
  does not check the lock (like `helm uninstall`). See
  [Pending releases](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#pending-releases-helms-release-lock)
  for manual recovery.

- **A replacement's plan does not show the new release's objects.** The
  create half of a taint, `-replace` or `release_storage_driver` change —
  and of a `name`/`namespace` change whose objects collide with the old
  release's — is planned while the old release is still live, where a
  chart's `lookup`s (a `lookup`-guarded generated password) find the old
  objects; after the destroy they find nothing. Its `resources`, `status`,
  `revision` and `metadata` are therefore known after apply, with a
  warning, and computed by the apply after the destroy. An `adopt_existing`
  create is planned the same way. A `release_storage_driver` change is
  recognized by reading the old backend's release records. A read that
  fails for a reason other than RBAC fails the plan (`Failed to read nelm
  release history`), and fails the apply when it is the apply's re-plan
  after the uninstall that cannot read (the next apply installs the
  release). If the provider's credentials may not read that backend at
  all, the create is planned as a fresh install, and a chart with
  `lookup`-dependent templates can abort that apply after the uninstall
  ("Provider produced inconsistent final plan"; the next apply installs
  the release).

- **A replacement's conflicts with live objects are only warnings at plan
  time.** The create half of a replacement (a `name`/`namespace`/
  `release_storage_driver` change, a tainted resource) is planned while the
  old release still owns its objects, so Nelm's ownership and
  immutable-field checks against live objects only warn there and run for
  real after the destroy. A conflict with an object the old release does
  NOT own (another release's, or one created outside Helm) therefore fails
  the apply after the old release was uninstalled. Read the objects the
  `re-checked at apply` warning names before applying. Telling the two
  apart needs owner information Nelm's error does not carry in structured
  form.

- **`-replace` cannot get past an immutable-field change.** Terraform plans
  `-replace` as an update of the same release first, and Nelm's
  immutable-field check fails that plan. Use `terraform taint` (planned as
  a plain create) or `terraform destroy -target=...` then `terraform apply`.

- **First plan after import is an in-place update.** `chart` is not
  recoverable from release storage, so it is null right after import; any
  real configuration supplies a chart, making the first post-import plan an
  update whose apply runs `nelm install` for real: a new revision, and the
  chart's upgrade hooks (migration Jobs, webhook certificate patch Jobs) run
  as on any `helm upgrade`. Inherent to Helm/nelm storage.

- **`werf.io/deploy-dependency-*: state=ready` targets are always
  readiness-tracked** (Nelm 1.26.2+), even when unchanged, so a release whose
  dependency target is unhealthy fails its apply even if nothing about that
  target changed. Charts without werf.io annotations are unaffected.

## Storage driver and import

- **ConfigMap-backed releases cannot be imported.** An import never sees the
  configuration, so it always records `release_storage_driver = "secret"`;
  the refresh after it looks for the release in Secrets, finds none, and the
  import fails with "Cannot import non-existent remote object". A `moved`
  block from `helm_release` records `secret` too, so the moved release drops
  out of the state and is planned as a new resource. To take such a release
  over, stop managing the `helm_release` (a `removed` block with
  `destroy = false`) and create the `nelm_release` with
  `release_storage_driver = "configmap"` and `adopt_existing = true` for
  that one apply: Create upgrades the existing release in place. The plan is
  a create, so it shows the chart's full render rather than a diff against
  the live objects. Remove `adopt_existing` afterwards. Driver detection on
  import is planned.

- **A different spelling of the storage driver replaces the release.**
  `"secret"` and `"secrets"` name the same backend, and so do `"configmap"`
  and `"configmaps"`, but the replacement check compares the strings:
  changing `"secret"` to `"secrets"` (or back) plans a replacement, an
  uninstall and a fresh install. An import and a `moved` block record
  `"secret"`, so write `"secret"` and `"configmap"` in configurations.

- **With the `configmap` driver, release records are readable with the
  `view` role.** With `release_storage_driver = "configmap"` (or
  `"configmaps"`), each revision's record — the user-supplied values,
  `set_sensitive` ones included, and the rendered manifest with every
  `Secret` the chart renders — is stored in a ConfigMap
  `sh.helm.release.v1.<name>.v<revision>`, which anyone who may read
  ConfigMaps in the namespace can decode, including Kubernetes' built-in
  `view` role (which deliberately excludes Secrets). Helm and
  `helm_release` with `helm_driver = "configmap"` store it the same way.
  Keep the default `secret` for any release that carries credentials.

## Release history

- **Nelm writes no revision description and rewrites timestamps.**
  Revisions Nelm writes have an empty DESCRIPTION in `helm history`, where
  Helm writes `Install complete`, `Upgrade complete`, `Rollback to N` or the
  failure message, so an `auto_rollback` revision looks like any other
  upgrade, and alerts built on the description (helm-exporter's
  `description` label, for example) carry no error text: take the cause from
  the Terraform apply log. Nelm also rewrites a revision's timestamps
  whenever it updates its status, so a superseded revision's UPDATED column
  shows when the next revision finished, not when it was deployed — the
  last revision `helm_release` wrote included, once Nelm supersedes it. Pick
  rollback targets by REVISION, CHART and APP VERSION, not by UPDATED or
  DESCRIPTION. Upstream in Nelm.

## References from `helm_release` configurations

- **`id` and `metadata` differ from `helm_release`'s.** `nelm_release.id`
  is `<namespace>/<name>`, where `helm_release.id` is the release name, and
  `metadata` holds `app_version`, `chart_name`, `chart_version` and
  `values_json` (the coalesced values, sensitive), with `name`,
  `namespace`, `revision` and `status` as top-level attributes;
  `helm_release`'s `metadata.notes`, `first_deployed` and `last_deployed`
  have no counterpart. Expressions that read these attributes must be
  rewritten when migrating, and a resource that uses the id as a
  replacement trigger (`terraform_data`'s `triggers_replace`,
  `null_resource`'s `triggers`) is replaced once. `timeouts` is a block
  (`timeouts { create = "10m" }`), not `helm_release`'s `timeout` seconds
  or `timeouts = { ... }` attribute. See the
  [attribute mapping](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/migrating-from-helm_release.md#attribute-mapping).

## Readiness tracking and `helm_release` parity

Details and workarounds for all of these are in the resource docs'
[Readiness tracking and `wait`](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#readiness-tracking-and-wait)
and
[`auto_rollback` vs `helm_release`'s `atomic`](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#auto_rollback-vs-helm_releases-atomic).

- **`auto_rollback` does not roll back when `timeouts` expires.** It only
  acts on failures nelm detects inside the `timeouts.create`/`update`
  budget. Pending/unschedulable pods, init containers waiting on a
  dependency, slow-but-progressing rollouts and long Jobs end only when the
  budget runs out; nelm then aborts the operation and its rollback runs on
  the expired context, so nothing is rolled back (the release stays
  `failed`/`pending-upgrade` with the new revision live). An early-detected
  failure's rollback also only gets the rest of the same budget. Helm's
  `atomic` rolls back after a timeout with a fresh timeout. Planned: after a
  timed-out install with `auto_rollback = true`, roll back to the last
  deployed revision from the provider under its own bounded context.

- **`auto_rollback` does not uninstall a failed first install.** With no
  prior deployed revision the failed release and its resources stay in the
  cluster and the resource is tainted; the next apply replaces it
  (uninstall + fresh install). `atomic` uninstalls instead. An optional
  provider-side uninstall of a failed first install (with nothing
  persisted to state) is being considered.

- **Readiness tracking is stricter than `helm_release`'s `wait = true`.**
  nelm awaits non-hook Jobs (no failure allowed) and custom resources with
  a kubedog status rule (`ExternalSecret`, cert-manager `Certificate`, Argo
  CD `Application`, Flux, Prometheus Operator `Prometheus`/`Alertmanager`,
  Kyverno, …), plus any resource whose `status.phase`/`state`/`status`/
  `health` holds a recognized pending or failed word; Helm treats all of
  those as ready at once. `ExternalSecret` `Ready=False` and a `Degraded`
  Argo `Application` fail the apply immediately, and the generic tracker
  fails a resource after 4 minutes without activity regardless of
  `timeouts`. Conversely, a custom resource that only reports a `Ready`
  condition is treated as ready at once. Per-resource `werf.io/*`
  annotations (`track-termination-mode: NonBlocking`, `fail-mode`,
  `no-activity-timeout`, `failures-allowed-per-replica`) tune it; there is
  no per-release override besides `wait = false` yet.

- **`wait = false` is not exactly `helm_release`'s `wait = false`.** It maps
  to nelm's `NoFinalTracking`, which only drops tracking that no later
  deploy step depends on: pre-install/pre-upgrade hooks, earlier weight
  groups, `state=ready` dependency targets and — unlike Helm — every main
  resource of a chart with a post-install/post-upgrade hook are still
  awaited, while a post-install hook without a `hook-succeeded` delete
  policy is not. Untracked resources cannot fail the apply, so
  `auto_rollback` does not trigger for them.

- **`OnDelete` StatefulSets block the apply until their pods are deleted
  by hand.** Helm skips readiness checks for `updateStrategy: OnDelete`;
  nelm waits until every replica runs the new revision, so a pod-template
  change fails after `timeouts` (and every retry waits again) unless the
  StatefulSet carries `werf.io/track-termination-mode: NonBlocking`.
  `werf.io/fail-mode` does not help.

## Field ownership

- **Fields added with `kubectl edit` are removed by the next update, and the
  diff does not show it.** With the default `no_remove_manual_changes =
  false`, nelm's plan already moves the `kubectl-edit` field manager's fields
  to its own `helm` manager (a real `managedFields` patch), and the next apply
  that updates the release for any reason removes the ones the chart does not
  render. The `resources` diff never shows them, because the live side is
  projected onto the chart's shape. `helm_release` (Helm 3 three-way merge)
  keeps such fields. Turning the flag on afterwards does not bring them back,
  and the flag change is itself an update. Set `no_remove_manual_changes =
  true` before the first plan if you rely on `kubectl edit` hotfixes; other
  managers (`kubectl patch`/`label`/`annotate`/`apply`) are not affected. A
  plan-time warning for removals the diff cannot show is being considered.

- **The `helm_release` field-manager hand-over runs at apply, not plan.**
  Objects written by hashicorp/helm carry a `terraform-provider-helm_*`
  field manager that nelm does not recognize; the provider renames it to
  `helm` right before each install so nelm's own Helm 3 hand-over prunes what
  the chart no longer renders, and `terraform plan` adds no write of its own.
  The plan already shows those removals (its planned side is the chart
  render). If an admission webhook is unavailable during the hand-over, the
  object is skipped with a warning and a later apply that updates the release
  finishes it, removing fields the chart stopped rendering in between; that
  later diff does not show them. nelm's Helm 3 hand-over also dry-runs the
  previous revision's manifest, so a `helm_release` revision that recorded a
  resource under an API version the cluster no longer serves fails that first
  apply, exactly as it fails a `helm_release` upgrade; clean up the stored
  manifest first (e.g. `helm mapkubeapis`). A previous manifest that the API
  server now rejects for another reason (as invalid, e.g. a changed
  immutable field, or with a field the object's schema does not accept) no
  longer fails the apply since nelm 1.27.1: nelm skips that object's
  managed-fields reconstruction with a warning the provider does not
  surface, so fields the chart stopped rendering since that revision may
  stay live, and no diff shows them.

## Values precedence

- **Ordering across `set` types is not preserved.** nelm merges the underlying
  `--set` / `--set-string` / `--set-literal` / `--set-json` categories in a
  fixed order, not in the order entries appear in a single `set` list. So two
  entries with the *same name* but different `type` in one list do not honor
  list order (the later category wins, not the later list position). The
  provider does guarantee that `set_sensitive` wins over `set` on a name
  conflict. Avoid same-name-different-type entries in a single list.

- **Diagnostics scrub `set_sensitive` values as Nelm parses them, not every
  fragment.** Errors and warnings are scrubbed of each value as written, the
  strings Nelm parses out of it and their quoted and base64 forms. A value
  Nelm *fails* to parse can still leak the fragment its error names (e.g.
  `key "word" has no value` for an unescaped comma in `pass,word`). Escape
  commas in sensitive values (`value = "pass\\,word"` in HCL), or use
  `type = "literal"`.

## Timeouts / nelm internals

- **`timeouts.read` cannot bound everything inside `ReleaseGet`.** Several
  nelm-internal calls use their own background contexts, so a half-open API
  server can still stall the release-storage read beyond the configured read
  timeout. The live-object phase of `Read` *is* bounded, and the shared
  kube-client construction is capped (at `kube_request_timeout`, default 30s)
  so a black-holed endpoint cannot wedge the whole process. Upstream in nelm.

- **A timed-out operation's nelm worker is not joined before cleanup.** nelm
  runs an action in a goroutine and returns when its timeout fires without
  joining the worker; the provider then removes the per-operation temp dir. A
  genuinely stuck worker could briefly outlive cleanup (the provider's log
  capture buffer is mutex-guarded, so this is not a data race on our side).
  Set generous `timeouts` for very large releases. Upstream in nelm. The
  same goes for a remote chart download cut off by its timeout: Helm's
  download code takes no context, so the abandoned download keeps running
  in the background — for an OCI pull, whose registry client has no HTTP
  timeout, until the registry answers or drops the connection.

- **A killed or crashed provider leaves its 0700 temp root behind.** Each
  provider process removes its `tf-nelm-*` temp root when Terraform shuts
  it down normally, and ordinary error paths clean per-operation
  directories in the same call frame, but a SIGKILL or crash mid-operation
  can leave values/plan-artifact files (0600, inside the 0700 root) until
  the OS temp cleaner runs.

- **A nelm global (`loader.NoChartLockWarning`) is written by ReleaseGet
  without synchronization**, so highly-parallel `Read` + chart-loading is a
  data race whose only visible effect today is nondeterministic suppression
  of a chart-dependency warning. Upstream in nelm.
