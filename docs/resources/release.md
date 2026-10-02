---
page_title: "nelm_release Resource - nelm"
subcategory: ""
description: |-
  Manages a Helm chart release with the Nelm Go library; the plan shows each rendered object's changes and out-of-band drift.
---

# nelm_release (Resource)

Manages a Helm/Nelm release via the Nelm Go library
(`action.ReleaseInstall` for apply, `action.ReleasePlanInstall` and
`action.ChartRender` for diff).

## Example Usage

```hcl
resource "nelm_release" "basic" {
  name      = "basic-example"
  namespace = "tf-nelm-basic-example"

  # A local chart directory. ${path.module} makes it absolute regardless of
  # the working directory Terraform is invoked from.
  chart = "${path.module}/charts/basic"

  values = [
    <<-YAML
    replicaCount: 1
    configMap:
      message: "hello from examples/basic"
    YAML
  ]
}

output "release_id" {
  value = nelm_release.basic.id
}

output "release_status" {
  value = nelm_release.basic.status
}

output "release_resources" {
  value = nelm_release.basic.resources
}
```

A runnable version of this configuration is
[`examples/basic/main.tf`](https://github.com/infrabay/terraform-provider-nelm/blob/main/examples/basic/main.tf)
in the provider's repository; its chart is
[`testdata/charts/basic`](https://github.com/infrabay/terraform-provider-nelm/tree/main/testdata/charts/basic).

## Schema

### Required

- `name` (String) Release name. Changing this attribute forces resource
  replacement. Must be between 1 and 53 characters.
- `chart` (String) Chart reference: local directory (absolute or
  relative), `.tgz` archive, `oci://` URL, or `repo/name`. Local relative
  paths are normalized to absolute before every Nelm call. Must be at
  least 1 character.

### Optional

- `namespace` (String) Kubernetes namespace for the release. Defaults to
  `"default"`. Changing this attribute forces resource replacement. The
  provider always lets Nelm create the namespace if it is missing, like
  `helm_release` with `create_namespace = true`; there is no
  `create_namespace` attribute to turn that off yet (see "Namespace
  lifecycle" below).
- `repository` (String) Chart repository URL used to resolve a bare chart
  name. An `oci://` URL is `helm_release`'s OCI form: it is joined with
  `chart` into one `oci://` reference (`repository = "oci://host/path"` +
  `chart = "app"` → `oci://host/path/app`) and authenticated through the
  provider's `registries` block. Credentials for classic HTTP repositories
  are out of scope for v1. See "Chart references" below.
- `version` (String) Chart version constraint. If omitted, the latest
  version is used, resolved again by every plan and apply; the resolved
  version surfaces in `metadata.chart_version`. Pin it for charts from a
  repository or registry: with `version` unset, every new upstream chart
  release becomes an upgrade at the next apply, and the plan shows only the
  objects' changes, not the version (see
  [Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md#chart-versions-and-crds)).
- `values` (List of String) List of raw YAML documents, merged in order
  with later entries overriding earlier ones (mirrors Nelm's `ValuesFiles`
  precedence). There is no `values_files` attribute by design: a
  file-path indirection would hide value-content changes from the
  Terraform diff. Use `values = [file("values.yaml")]` to get file-based
  values with the content still visible to `terraform plan`.
- `set` (List of Object) Individual value overrides, applied after `values`
  and before `set_sensitive`. This is a list-nested **attribute** (assigned
  with `=`, not repeated blocks): `set = [{ name = "image.tag", value = "v2" }]`.
  See nested schema below.
- `set_sensitive` (List of Object) Same shape as `set`, but `value` is marked
  sensitive; on a name conflict the `set_sensitive` entry wins. Also an
  attribute: `set_sensitive = [{ name = "db.password", value = var.pw }]`.
  See nested schema below.
- `auto_rollback` (Boolean) Automatically roll back to the previous
  deployed release on install failure. Only works if a previous release
  successfully deployed. Defaults to `false`. Narrower than `helm_release`'s
  `atomic`: there is no rollback when the `timeouts` budget expires, and a
  failed first install is not uninstalled — see "`auto_rollback` vs
  `helm_release`'s `atomic`" below.
- `wait` (Boolean) Wait for the release's resources to become ready
  (Nelm's readiness tracking) before the apply succeeds. Defaults to
  `true`. `false` is the closest equivalent of `helm_release`'s
  `wait = false` but not identical: tracking that a later deploy step
  depends on still runs. Resources that are not tracked cannot fail the
  apply, so `auto_rollback` never triggers for them. See "Readiness
  tracking and `wait`" below.
- `force_adoption` (Boolean) Allow adopting live resources that belong to
  a different Helm release or were created out-of-band. Not required to
  import plain-helm-installed releases (see Caveats below). Defaults to
  `false`.
- `no_remove_manual_changes` (Boolean) Preserve fields added to live
  resources with `kubectl edit` (field manager `kubectl-edit`) that the chart
  does not render. Defaults to `false` (Nelm's own default), which differs
  from `helm_release`: Nelm takes such fields over to its own `helm` field
  manager already during `terraform plan`, and the next apply that updates
  the release removes them — **without the removal appearing in the
  `resources` diff**. Setting it to `true` afterwards does not bring them
  back (the flag change is itself an update). Set it before the first plan
  if you rely on `kubectl edit` hotfixes surviving upgrades; see "Fields
  added out of band" below.
- `no_install_crds` (Boolean) Skip installing CustomResourceDefinitions
  from the chart's `crds/` directory. Defaults to `false`. Unlike
  `helm_release`, which only creates missing ones, Nelm server-side-applies
  these CRDs with force on every install and upgrade, overwriting existing
  ones, including CRDs that another release or tool manages; set this where
  the CRDs are managed elsewhere. They must then already exist when the
  release is first installed; turning it on for an installed release deletes
  nothing. See [Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md#chart-versions-and-crds).
- `adopt_existing` (Boolean) Allow Create to take over a release of the
  same name that already exists in the namespace (one with a deployed
  revision). Defaults to `false`: Create then fails instead, like
  `helm_release` without `upgrade_install`, so a forgotten import, a
  duplicate resource, or a `create_before_destroy` replacement can never
  silently adopt — and then uninstall — a live release. Prefer
  `terraform import`. It does not override the `pending-*` lock, and it
  cannot take over a release stored in the other storage backend. Only
  Create reads it; see "Creating a resource for an existing release"
  below.
- `diff_mode` (String) How the plan computes `resources`: `"full"` (the
  default) or `"none"`. `"full"` renders the chart and runs Nelm's plan
  against the cluster, so the plan shows each object's changes and
  out-of-band drift; an object whose render changes on every render is
  known after apply whenever the release is reinstalled. `"none"` renders
  nothing at plan time, like `helm_release`: `resources` (with `status`,
  `revision` and `metadata`) is known after apply whenever the release is
  (re)installed and otherwise keeps its refreshed value — no object diff, no
  drift detection, and chart or values errors only surface at apply. Use it
  for charts that can never converge under `"full"`; see
  [Non-deterministic charts](#non-deterministic-charts). Changing it is an
  in-place update that runs Nelm's install. Must be `"full"` or `"none"`.
- `release_history_limit` (Number) Maximum number of release revisions
  kept in storage. Null or `0` uses Nelm's own default (10). Only release
  metadata is pruned; cluster resources are unaffected. Must be `0` or
  greater.
- `release_storage_driver` (String, **Forces replacement**) Where release
  metadata is stored: `"secret"`, `"secrets"`, `"configmap"`, or
  `"configmaps"`. Defaults to `"secret"`. Changing it destroys and recreates
  the whole release (Nelm does not migrate history between backends; the
  destroy uses the old backend, the create the new one), so apply such a
  change destroy-first, never `create_before_destroy` (see "Replacement
  and `create_before_destroy`" below). Enum-validated at plan time — an
  unrecognized driver string panics inside Nelm, so `"memory"` and `"sql"`
  are rejected in v1. `"secret"` and `"secrets"` (like the two ConfigMap
  spellings) name the same backend, but changing one spelling to the other
  is still a replacement; import records `"secret"`. With a ConfigMap
  driver, the release records — the user-supplied values, `set_sensitive`
  ones included, and every Secret the chart renders — can be read by anyone
  who may read ConfigMaps in the namespace, including Kubernetes' built-in
  `view` role; keep `"secret"` for releases that carry credentials.
- `timeouts` (Block, Optional) See nested schema below.

### `set` / `set_sensitive` nested schema

Both attributes share the same element shape (`set_sensitive.value` is
additionally marked sensitive):

- `name` (String, Required) Dotted value path (e.g. `"image.tag"`).
- `value` (String, Required) Value to set.
- `type` (String, Optional) How to parse `value`: `""` or `"auto"` infers
  the type (Nelm `ValuesSet`), `"string"` forces a string (`ValuesSetString`),
  `"literal"` forces a literal string even if it looks like a number/bool
  (`ValuesSetLiteral`), `"json"` parses `value` as JSON (`ValuesSetJSON`).
  Must be one of `""`, `"auto"`, `"string"`, `"literal"`, `"json"`.

### `timeouts` nested schema

Optional block; all four are Go duration strings (e.g. `"20m"`):

- `create` (String) Timeout for the install action backing Create.
  Defaults to 10m.
- `read` (String) Timeout for each read-side step, bounded separately: a
  refresh's release read (`ReleaseGet`) and live-object reads; the
  plan-time diff (`ReleasePlanInstall` and each of the two chart renders,
  `ChartRender`); and, around an install, the release-history read before
  it and the read-back after it. Defaults to 5m.
- `update` (String) Timeout for the install action backing Update.
  Defaults to 10m.
- `delete` (String) Timeout for the uninstall action backing Delete.
  Defaults to 5m.

`create`, `update` and `read` also bound the remote-chart download that
precedes each install and each plan-time diffing step, separately from the
Nelm action itself; a stalled chart repository or registry fails the
operation once the timeout expires instead of hanging it. Each HTTP request
to a classic chart repository is additionally capped at 2 minutes (Helm's
default).

### Not offered (not attributes of this resource)

These exist on comparable Helm-based providers but are not exposed here,
mostly because the underlying Nelm behavior makes them either meaningless
or actively misleading:

- `create_namespace` — the provider always lets Nelm create the release
  namespace if it is missing, which is what `helm_release` does with
  `create_namespace = true` (its default is `false`). Nelm 1.27 and later
  have an option to skip that step; this provider does not expose it yet.
  To manage namespaces yourself in the meantime, see "Namespace lifecycle"
  below.
- `upgrade_install` — Nelm install is natively idempotent
  install-or-upgrade, so there is no install/upgrade split to select
  between. Its safety role is covered by `adopt_existing`: Create refuses
  an existing release unless that is set.
- `wait_for_jobs` / other readiness-tracking knobs — with `wait = true`
  Nelm always tracks non-hook Jobs to completion; per-resource tuning is
  done with `werf.io/*` annotations in the chart (see "Readiness tracking
  and `wait`" below). `timeout` is the `timeouts` block: `timeout = 300`
  becomes a `timeouts` block with `create = "5m"` and `update = "5m"`.
- `atomic` — use `auto_rollback`, which only covers part of `atomic` (see
  "`auto_rollback` vs `helm_release`'s `atomic`" below).
- `devel`, `verify`, `keyring`, `postrender`, `description`,
  `dependency_update`, write-only `set` variants — all out of scope for
  v1.

## Attributes Reference

In addition to the arguments above, the following attributes are exported
and only ever set by the provider:

- `id` (String) `"<namespace>/<name>"`. Set to a KNOWN value during
  plan-time diffing — it is a pure function of the (known) `name` and
  `namespace` attributes.
- `status` (String) Release status as reported by the cluster
  (`ReleaseGet`). Unknown whenever the release will be re-installed: a
  create, a release that is not `deployed` (failed or `pending-*`),
  out-of-band drift in `resources`, or a change to any argument that does
  not force replacement — `chart`, `repository`, `version`, `values`,
  `set`, `set_sensitive`, the boolean flags, `diff_mode`,
  `release_history_limit` and `timeouts` alike; left untouched on a clean
  no-change plan. Every such in-place update runs Nelm's install. Nelm
  skips writing a revision only when the coalesced values, the release
  notes and every rendered object are unchanged and the chart has no hooks;
  otherwise it creates a new revision and runs the chart's
  `pre-upgrade`/`post-upgrade` hooks again — so even a `timeouts`-only edit
  re-runs them, as any in-place change does with `helm_release`.
- `revision` (Number) Release revision number. Unknown under exactly the
  same condition as `status` above.
- `metadata` (Object) Release metadata resolved from the cluster
  (`ReleaseGet`). Unknown under the same condition as `status`/`revision`.
  Contains:
  - `app_version` (String) `Chart.yaml` `appVersion` as reported by Nelm.
  - `chart_name` (String) Resolved chart name.
  - `chart_version` (String) Resolved chart version.
  - `values_json` (String, Sensitive) Canonical JSON of the coalesced
    values used to render the release.
- `resources` (Map of String) **The plan-time diff surface.** A map of
  `"<apiVersion>/<Kind>/<namespace>/<name>"` (cluster-scoped kinds use an
  empty namespace segment, e.g.
  `rbac.authorization.k8s.io/v1/ClusterRole//my-role`) to canonical,
  redacted JSON of that resource: `Secret` data and `set_sensitive` values
  are replaced by placeholders (see "Sensitive values in non-`Secret`
  resources" below). An object whose render changes on every
  render is known after apply whenever the release is reinstalled. The
  whole map is known after apply whenever the release is reinstalled with
  `diff_mode = "none"`, and on a create whose release is live while the
  plan runs (the create half of a replacement, an `adopt_existing`
  create). See "How `resources` drives `terraform plan`" below.

## How `resources` drives `terraform plan`

`resources` is deliberately the mechanism this provider uses to surface
both configuration changes and out-of-band cluster drift in
`terraform plan`, and it is worth understanding to read a plan
correctly:

- On every plan, the **planned** side of `resources` is computed by
  running Nelm's own plan engine (`action.ReleasePlanInstall`) against
  your current configuration, which says which objects change, and taking
  each changed object's value from the chart's own render, normalized
  (sensitive-path redaction, then stripping runtime metadata such as
  `status`, `managedFields`, `resourceVersion`, and `uid`, and the release
  ownership metadata Nelm adds at install — `meta.helm.sh/release-name`,
  `meta.helm.sh/release-namespace` and `app.kubernetes.io/managed-by` —
  then replacing `set_sensitive` values with placeholders) to a
  deterministic, key-sorted JSON string per resource. The render is free of
  server defaulting and of fields the chart does not render (an HPA-owned
  `replicas` that the chart leaves out, controller-written annotations),
  which Nelm's own plan value — the API server's dry-run merge — carries.
  A field the chart does render is planned at the chart's value even when a
  controller keeps changing it, so it shows as drift on every plan (see
  [Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md#drift-and-out-of-band-changes)).
  When the plan creates the release — a new resource, or the create half of
  a replacement — the planned map is the chart's first-install render, not
  Nelm's live-relative plan. While the release is live as the plan runs —
  the create half of a replacement (the old release still exists, in this
  or the other storage backend), an `adopt_existing` create, objects
  another release still owns — the whole map (with `status`, `revision`
  and `metadata`) is known after apply instead, with a warning: a template
  that reads live objects with `lookup` renders differently once the
  replacement's destroy has removed them, and the apply's re-plan computes
  the map after that destroy. A create plan that cannot read the other
  storage backend for a reason other than RBAC (a transient API error)
  cannot tell whether a release is live there: it fails with `Failed to
  read nelm release history`, as Create does, at plan time and in the
  apply's re-plan alike (a re-plan after a replacement's destroy fails that
  apply, and the next apply installs the release). A read the provider's
  credentials may not make counts as no release there.
- The chart is rendered twice per plan; an object the two renders disagree
  on (random or time-based template functions) is handled as described in
  [Non-deterministic charts](#non-deterministic-charts).
- The **prior** side of `resources` (what's already in state) is
  refreshed on every `terraform plan`/`apply` by reading the objects
  *live from the cluster* — not from the release's stored chart
  manifests — and normalizing them the same way, then **projecting** each
  live object onto the shape of its planned counterpart (or, for an object
  state has no value for yet, such as right after an import, onto the
  release's stored manifest of it): fields present
  only on the live object (the API server's server-side defaulting — empty
  `resources: {}`, `dnsPolicy`, a Service's `clusterIP`, a StatefulSet's
  `updateStrategy`, and so on) are dropped, so they never show up as a
  phantom diff. This projection is generic — it works for every resource
  kind, not a hand-maintained list — while still surfacing genuine drift:
  a field your chart *does* set whose live value changed stays in the
  projection and diffs normally.
- Terraform's built-in map differ then compares the two maps entry by
  entry. Because the prior side is always the live cluster state, an
  out-of-band **change to a field your chart manages** (e.g. `kubectl
  scale` a chart-set `replicas`, `kubectl edit` a chart-set container
  image) shows up as a diff on that resource's `resources["..."]` entry,
  even though your Terraform configuration hasn't changed — this is the
  intended drift-detection behavior, not a bug. Applying it reverts the
  change and runs Nelm's install, and every apply of the root module
  applies it, whatever change the apply is for: an out-of-band hotfix
  (`helm upgrade --reuse-values`, `helm rollback`, `kubectl set image`)
  lasts only until the next apply. `helm_release` (without its `manifest`
  experiment) ignores such a change until its own configuration changes.
  `lifecycle { ignore_changes = [resources] }` has no effect;
  `diff_mode = "none"` turns drift detection off for a release (see
  [Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md#drift-and-out-of-band-changes)).
- Because the live side is projected onto the chart's rendered shape,
  drift detection is scoped to **fields the chart actually sets** — the
  same managed-field semantics Helm itself uses. An out-of-band change
  that only *adds* a field the chart never rendered (a `kubectl`-added
  annotation or label, an admission-webhook-injected sidecar or env var,
  a controller-populated default) is **not** shown as drift; only changes
  to, and removals of, chart-managed fields are. This is deliberate: it is
  exactly what prevents the API server's own server-side defaulting from
  showing as a permanent phantom diff on every plan. Not shown is not the
  same as kept, though: a field added with `kubectl edit` is removed by the
  next apply that updates the release unless `no_remove_manual_changes =
  true` — see "Fields added out of band" below.
- `resources` is set explicitly on **every** plan, including plans with
  no other changes, specifically so that drift stays visible on
  no-change plans too.
- Each value is the resource's rendered manifest, not a hash or summary,
  so `terraform plan -json` / structured plan output shows the actual
  field-level differences within a resource, not just "this resource
  changed."
- If the cluster is unreachable at plan time, `resources` (along with
  `status`/`revision`/`metadata`) becomes Unknown and the plan proceeds
  with a warning; the actual diff is only knowable once `apply` reaches a
  reachable cluster. (A remote chart is downloaded before the cluster is
  contacted, so its repository or registry must still be reachable; a
  failed chart download is a plan error.) If your Terraform configuration
  itself has any unknown inputs (e.g. `chart` computed from another
  resource not yet applied), the same Unknown degradation applies — the
  provider never guesses at a diff it can't actually compute.
- The same Unknown degradation (with a warning) applies to a **new**
  release while the provider configuration itself is not yet known (e.g.
  `host` comes from a cluster created in the same run); a release already
  in state fails to plan in that case instead. See the provider docs'
  [Provider configuration known only at apply](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/index.md#provider-configuration-known-only-at-apply).

## Caveats and Notes

### Import

```sh
terraform import nelm_release.example namespace/name
```

The import ID is exactly `<namespace>/<name>` — no other separator or
format is accepted.

A release managed by `hashicorp/helm`'s `helm_release` can also be handed
over with `moved { from = helm_release.x  to = nelm_release.x }`
(Terraform 1.8+), which carries the `helm_release` inputs over as well; see
the [migration guide](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/migrating-from-helm_release.md).

Import adopts an existing release with **zero conversion**, whether it
was created by `helm install`/`helm upgrade` (Helm 3 **or** Helm 4), by
hashicorp/helm's `helm_release` (see "Migrating from `helm_release`"
below for its field managers) or by Nelm itself: Nelm's default
release-storage format
(`sh.helm.release.v1.<name>.v<rev>` Secrets) is exactly the format Helm
itself writes, so `ReleaseGet` reads a plain-helm release directly. `force_adoption = true` is **not** required
purely to import a plain-helm release — Helm's own release-ownership
annotations already satisfy Nelm's adoption check. `force_adoption` only
matters for resources that are live in the cluster but untracked by any
release record at all (e.g. created by a completely separate tool).

Two things are **not** recoverable from Helm/Nelm's release storage on
import, and both matter for the first plan you run afterward:

- `values` / `set` / `set_sensitive` — the resolved/coalesced values used
  to render the release are stored (and exposed back to you as
  `metadata.values_json`), but the original *inputs* (which values file
  had what content, which flags were passed as `--set`) are not. If your
  Terraform configuration for the imported resource doesn't already
  reproduce those exact inputs, the **first plan after import will show a
  values-driven diff** — because Nelm will (correctly) re-render the
  chart with your configured `values`/`set`/`set_sensitive`, which may
  differ from what was previously deployed.
- `repository` and `version` are not persisted by Helm's release record
  either, so they are simply absent from imported state until you set
  them in configuration. Set `version` to the chart version the release
  runs now (see `version` above).

`chart` is not recoverable either, so it is null right after the import:
the **first plan after an import is always an in-place update**, and its
apply runs `nelm install` for real — a new revision, and the chart's
upgrade hooks run as on any `helm upgrade`.

**Recommendation:** write the configuration for an imported release from
the inputs that installed it — when migrating from `helm_release`, copy its
`chart`, `version`, `values`, `set` and `set_sensitive` verbatim (see the
[migration guide](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/migrating-from-helm_release.md)). If those are
lost, `helm get values <name> -n <namespace>` returns the user-supplied
values. Do not seed them from `helm get values -a` or
`metadata.values_json`: those are the coalesced values, chart defaults
included, which pins every default in your configuration.

An import never goes through Create, so `adopt_existing` plays no part in
it; ImportState seeds it as `false`. It also seeds
`release_storage_driver = "secret"` (an import never sees the
configuration), so a release stored in ConfigMaps cannot be imported; see
[Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md#storage-driver-and-import)
for how to take one over instead.

### Creating a resource for an existing release

When `nelm_release` is **created** — a new resource, or the create half of
a replacement — the provider first reads the release's stored history. If
a release of that name already exists in the namespace and Nelm would
*upgrade* it (it has a deployed or superseded revision since its last
uninstall), the apply fails with `nelm release <namespace>/<name> already
exists` and nothing is changed.
This is `helm install`'s "cannot re-use a name that is still in use", and
it catches:

- a release installed elsewhere (by the `helm` CLI, by `helm_release`, by
  another Terraform configuration) that should have been imported — see the
  [migration guide](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/migrating-from-helm_release.md);
- two `nelm_release` resources, in one or several root modules, for the
  same release;
- a `create_before_destroy` replacement (below).

To manage such a release with Terraform, import it (`terraform import
nelm_release.x <namespace>/<name>`, or an `import` block).
`adopt_existing = true` lets Create take it over instead (Nelm upgrades it
in place); set it only for the apply that adopts the release. The plan of
that create cannot show the object diff: the release is live, so
`resources` is known after apply (with a warning) and read back from the
cluster after the upgrade.

When the configured storage backend holds no deployed revision, Create also
reads the **other** backend (`configmap` when `release_storage_driver` is
`secret`/`secrets`, and the other way round). A release still deployed
there fails the apply with `nelm release <namespace>/<name> already exists
in the <driver> storage backend`, and nothing is changed — with or without
`adopt_existing`, since Nelm cannot take a release over from another
backend, only install a second one over the same objects. This catches a
`create_before_destroy` replacement that changes `release_storage_driver`
(below), and a new resource whose `release_storage_driver` does not match
where an existing release is stored (set it to that backend). If the
provider's credentials may not read the other backend (RBAC that covers
only the configured one), Create only warns (`Could not check the <driver>
storage backend ...`) and installs.

A release whose history holds only failed or uninstalled revisions — a
first install that failed, or a `helm uninstall --keep-history` — does not
count as existing: Create installs over it, so retrying a failed first
install keeps working. The same goes for a first install that was killed
mid-way and left only a `pending-install` revision, once that revision is
stale; a younger one is refused as a lock (see
[Pending releases](#pending-releases-helms-release-lock)), with or without
`adopt_existing`.

The plan warns (`a release with this name already exists`) when it can see
this coming (not with `diff_mode = "none"`, whose plan does not ask Nelm).
It cannot fail at plan time: a destroy-first replacement
legitimately plans the create while the old release is still there. A
release in the other storage backend only gets the more general `this
create's release is live` warning, and a `-replace` no warning at all:
Terraform plans a `-replace` as an update first and keeps only the errors,
not the warnings, of its replacement re-plan. A taint, a
`name`/`namespace` change or a plain create does show the warning. The
apply refuses in every case.

### Replacement and `create_before_destroy`

Changing `name`, `namespace` or `release_storage_driver`, tainting the
resource, or `terraform apply -replace=...` replaces the release: Terraform
destroys (uninstalls) it and then creates (installs) it again in the same
apply, with downtime in between.

The create is planned while the old release is still installed. When the
new release is the same Helm release — a taint, `-replace`, a
`release_storage_driver` change — or its objects collide with the old
one's (below), the create's `resources`, `status`, `revision` and
`metadata` are known after apply, with a warning. A chart that reads live
objects with `lookup` (a `lookup`-guarded generated password, as in
Bitnami charts or `grafana`) finds the old release's objects at plan time
and nothing once the destroy has removed them, so the plan cannot commit
to the new objects; the apply computes them after the destroy. A `name` or
`namespace` change whose objects do not collide plans the new release's
render as usual.

When the new release reuses the names of objects the old one owns — a
namespace move of a chart whose cluster-scoped objects (ClusterRoles,
webhook configurations, CRDs) have fixed names, or a rename with
`fullnameOverride` — Nelm's ownership and immutable-field checks fail
against the old objects. The plan shows that as a warning (`the live-cluster
plan for this create failed (it is re-checked at apply)`) instead of an
error, because the destroy removes those objects before the create runs.
Read the objects that warning names: the apply re-runs the checks only
**after** the destroy, so a conflict with anything else — an object owned by
a third release, or created outside Helm — fails the create once the old
release is already uninstalled. Resolve such a conflict (or set
`force_adoption`) before applying.

`-replace` is the exception: Terraform plans it as an update of the same
release first, so a change Nelm refuses to apply in place (an immutable
field, such as a Deployment's `selector` or a StatefulSet's
`volumeClaimTemplates`, on an object without
`werf.io/delete-policy: before-creation-if-immutable`) still fails the plan
with `immutable fields change in resource ...`. To replace the release over
such a change, taint it instead (`terraform taint <address>`, then
`terraform apply`), which Terraform plans as a plain create, or run
`terraform destroy -target=<address>` and then `terraform apply`.

`create_before_destroy` is **not supported** for a replacement that keeps
the same `name` and `namespace`: the new and the old object are the same
Helm release, so the create would take over the live release and the
destroy of the old object would then uninstall it. That holds for a
`release_storage_driver` change too: the create would install the release
into the new backend over its live objects, and the destroy would then
uninstall it from the old backend, deleting those objects. The create is
refused instead (see above) — in the same backend, and for a release still
deployed in the other one: the apply fails, Terraform keeps the old object,
and nothing is uninstalled. Remove `create_before_destroy` and apply again.
Terraform also turns `create_before_destroy` on implicitly for a resource
when anything that depends on it has it (including through a module's
`depends_on`), so look for `+/-` ("create replacement and then destroy")
rather than `-/+` in the plan. Never combine `adopt_existing = true` with
such a replacement: the opt-in disables the same-backend refusal.

### Pending releases (Helm's release lock)

Helm marks a release's newest revision `pending-install`,
`pending-upgrade` or `pending-rollback` while an operation runs, and refuses
to start another operation on that release until it has finished. Nelm
itself treats a pending revision as failed and installs over it; this
provider restores Helm's behavior. When the release's last revision is
`pending-*` and was written less than the operation's timeout ago (and at
least 15 minutes), the apply fails with `nelm release <namespace>/<name>
is locked by another operation` and changes nothing — so a CI apply cannot
override, say, an on-call `helm rollback --wait` that is still waiting for
its pods. The plan warns when the refreshed release is pending.

A pending revision older than that is assumed to be left behind by an
operation that was killed (a cancelled CI job, a crashed process) and is
taken over with a warning, so a stuck release recovers on the next apply
after that. To recover sooner, once you are sure nothing is still running:

- roll back to the last good revision: `helm rollback <name> <revision> -n
  <namespace>` (find it with `helm history <name> -n <namespace>`); or
- delete the stuck revision's record: `kubectl delete secret
  sh.helm.release.v1.<name>.v<revision> -n <namespace>` (the
  `configmap` driver stores it as a ConfigMap of the same name).

Destroy does not check the lock (neither does `helm uninstall`). The check
reads the history right before installing, so an operation that starts in
between is not detected.

### Failed applies

- **A failed update** (the install failed, or failed and was rolled back by
  `auto_rollback`) saves the refreshed `status`/`revision`/`resources`, but
  keeps the configuration that was in state before the apply, so the next
  plan shows the same change again and retries it.
- **A failed create** leaves the resource tainted if the release was
  (partly) installed, and the next apply replaces it: uninstall, then
  install. To retry in place instead, `terraform untaint` it; a release that
  is not cleanly deployed always re-plans as an update.
- **A create that was killed** mid-install (a cancelled CI job, a crashed
  `terraform`) leaves a `pending-install` revision and no Terraform state.
  The next apply creates the resource again: it fails with `is locked by
  another operation` while that revision is younger than the lock age (see
  [Pending releases](#pending-releases-helms-release-lock)) and installs
  over it, with a warning, once it is older. A kill after Nelm already
  recorded a later revision (for example `deployed`) leaves a release that
  exists: import it.
- **An install that succeeded but could not be read back** right away (for
  example a transient API error) does not fail the apply: it warns, and the
  next refresh fills in `status`, `revision` and `metadata`.

`ImportStateVerify`-style equality checks intentionally ignore `values`,
`set`, `set_sensitive`, `repository`, `version`, and `resources` for the
reasons above; everything else (id, name, namespace, status, revision,
metadata, the boolean flags, `release_storage_driver`) is expected to
match immediately after import.

### `managedFields` and `terraform plan`

Running a plan against a resource is not perfectly read-only at the
Kubernetes API level. Before its dry-run server-side apply of an existing
object, Nelm's plan machinery fixes up the object's `metadata.managedFields`
so that its own field manager (`helm`, operation `Apply`) owns what Helm
owned, and it writes that fix-up to the cluster with a real (not dry-run)
patch. It does so for:

- objects last written by the **Helm 3** CLI, which applies client-side
  under field manager `helm` with operation `Update`: the first plan folds
  that entry into Nelm's `helm`/`Apply` entry;
- fields owned by `kubectl edit` (manager `kubectl-edit`, unless
  `no_remove_manual_changes = true`) or by a legacy `werf*` manager, folded
  the same way (see "Fields added out of band" below);
- fields Nelm's `helm`/`Apply` entry owns (once Nelm has applied the object,
  or folded a Helm 3 entry into it) that another manager co-owns: they are
  removed from that other manager's entry.

Objects written by **Helm 4** (server-side apply as `helm`/`Apply`, the
manager Nelm itself uses) need no fix-up. This was verified live:
`managedFields` was identical before and after the first plan, and stable
across a second plan, against a resource created by `helm install` v4.
Objects written by hashicorp/helm's **`helm_release`** carry a
`terraform-provider-helm_v<version>_x5`/`Update` manager that Nelm does not
recognize at all; this provider hands it over at apply, never during plan
(see "Migrating from `helm_release`" below).

`managedFields` are stripped by the normalization pipeline before entering
`resources`, so none of this shows up as diff noise in `terraform plan`
output, and none of it changes an object's spec (no rollout).

The plan also runs a dry-run server-side apply of every existing object,
which the API server authorizes like a real `patch`. The credentials
`terraform plan` runs with therefore need `patch` on every kind the chart
renders, like the apply's: with a read-only plan identity the plan fails on
the fix-up (`cannot patch`), and an object it may not dry-run is planned as
a blind apply, with a `nelm_release: blind apply for <key>` warning carrying
the API server's error: its planned value is still the chart's render, only
without the API server's validation. See
[Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md#provider-configuration-and-cluster-access).

### Fields added out of band

Because the live side of `resources` is projected onto the chart's rendered
shape, a field added out of band that the chart does not render is never
shown as drift (see "How `resources` drives `terraform plan`"). A field the
chart *does* render is reverted to the chart's value by the next apply, and
that drift is shown. What happens to a field the chart does not render
depends on the field manager that added it:

- **`kubectl edit`** (manager `kubectl-edit`): with the default
  `no_remove_manual_changes = false`, Nelm takes the field over to its own
  `helm` manager during `terraform plan` (see above), and its server-side
  apply then **removes** it on the next apply that updates the release for
  any reason — with nothing in that plan's `resources` diff saying so.
  Setting `no_remove_manual_changes = true` afterwards does not restore it:
  ownership has already moved, and the flag change is itself an update.
  This is a deliberate difference from `helm_release`, whose Helm 3
  three-way merge only removes fields that were in the previous manifest,
  so such edits survive its upgrades. If you rely on `kubectl edit`
  hotfixes surviving until the chart is fixed, set
  `no_remove_manual_changes = true` before the first plan (for an adopted
  release, in the configuration you import with).
- **Any other manager** (`kubectl patch`/`label`/`annotate`/`apply`,
  controllers, admission webhooks): the field stays owned by that manager
  and survives Nelm's applies, as it does Helm's.

### Migrating from `helm_release`

A release created or upgraded by hashicorp/helm's `helm_release` is a plain
Helm 3 release and is adopted like one (see Import above: a
`removed { lifecycle { destroy = false } }` block plus an `import` block),
or handed over with a `moved` block;
[Migrating from `helm_release`](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/migrating-from-helm_release.md)
walks through both. Two field-ownership differences matter:

- **Field managers are handed over at apply.** `helm_release` writes objects
  client-side (Helm 3 SDK) under the field manager
  `terraform-provider-helm_v<version>_x5`, one per provider version that
  wrote the object, which Nelm does not recognize. Left as is, Nelm's
  server-side apply would only co-own those fields, so a field the chart
  stops rendering (a removed value's env var, label, annotation, ConfigMap
  key, …) would stay live indefinitely, invisible in `resources`. So before
  every install (Create/Update — never during `terraform plan`), the
  provider renames those entries on the release's live objects (the
  resources recorded in release storage) to `helm`/`Update`, the Helm 3
  CLI's manager, and Nelm's Helm 3 hand-over above then takes ownership and
  prunes as usual. The plan already shows such removals (an updated
  resource's planned side is the chart's render); the hand-over is what
  makes the apply carry them out. The rename is a `metadata.managedFields`
  patch, conditional on the object's `resourceVersion` (no spec change, no
  rollout), and shares the apply's `create`/`update` timeout. Once done it
  is a no-op that costs one GET per object per apply. An object whose patch
  hits an unavailable admission webhook is skipped with a warning and
  handed over by a later apply.
- **`kubectl edit` hotfixes** survive `helm_release` upgrades but not
  Nelm's by default — see "Fields added out of band" above.

### Secret data in state

Every `data`/`stringData` key of a core `Secret` — and any path an object
is annotated with via `werf.io/sensitive-paths` — is redacted before it
enters `resources`, so it never appears as cleartext. Each redacted value
is replaced with a deterministic placeholder of the form:

```
<hidden N sensitive bytes, hash abcdef123456>
```

where `N` is the byte length and the hash is a truncated SHA-256 of the
original value. This means a Secret's key *changing* is still visible as
a diff (the placeholder text changes), without ever writing the actual
secret content into Terraform state or plan output.

Secret redaction is **unconditional**: even a chart that sets
`werf.io/sensitive: "false"` on a `Secret` (an opt-out that Nelm's own
ephemeral CLI diff honors) is still redacted here, because — unlike the
CLI — this provider persists `resources` durably into `terraform.tfstate`
and prints it in plan output. A `Secret`'s contents are treated as
sensitive by definition; the opt-out is deliberately ignored.

An object of any other kind annotated `werf.io/sensitive: "true"` (and no
`werf.io/sensitive-paths`) enters `resources` reduced to its identity —
`apiVersion`, `kind`, `metadata.name` and `metadata.namespace` — so none of
its fields show up in plan output or state. This is Nelm's v1 behavior, and
it holds even where `NELM_FEAT_FIELD_SENSITIVE` / `NELM_FEAT_PREVIEW_V2`
are exported: the provider ignores Nelm's feature-gate environment
variables (see the provider docs).

### Sensitive values in non-`Secret` resources

Redaction by kind only covers `Secret`s and objects annotated sensitive. A
value passed through `set_sensitive` is also scrubbed from every other
object a chart renders it into — a `ConfigMap` `data` entry, a container
`env` value or argument, an annotation, a key: each occurrence, also inside
a longer string, is replaced by a placeholder of the same form as `Secret`
data, on the planned side of `resources` and on the refreshed one alike:

```
"value": "<hidden 31 sensitive bytes, hash 1a2b3c4d5e6f>"
```

An unchanged value therefore plans no change, and a rotated one shows as a
change of placeholder. Scrubbed are each `set_sensitive` value as written,
the strings Nelm parses out of it (escaped commas resolved, `{a,b}` lists
split, every string and number of a `type = "json"` value), and each of
those as a template embeds it with `quote`, `toJson` or `b64enc`. The same
forms are scrubbed from error and warning messages.

What is **not** scrubbed, and appears in cleartext in plan output and in
state:

- **Values passed through `values` or `set`**, even from a variable marked
  `sensitive = true`. Terraform hides such values in its own output, but it
  never tells a provider which of its inputs are sensitive, so the provider
  cannot know them. Pass a secret that a chart renders into a non-`Secret`
  object through `set_sensitive` instead:
  `set_sensitive = [{ name = "auth.password", value = var.password }]`.
- A value shorter than 4 bytes (it would match all over the manifest), and
  numbers and booleans rendered as such (`port: 5432`): only strings and
  keys are searched.
- Any other transformation of a value: a part of it, `sha256sum`, `upper`,
  `b64enc` of a string the value is only part of, and so on.
- The `resources` map keys themselves (an object *named* after a secret).

`hashicorp/helm`'s `helm_release` shows no rendered manifest at all unless
`experiments { manifest = true }` is set, and with it scrubs none of these
values (it hashes the `set_sensitive` *names*), so a secret it never printed
can show up in this provider's plans after a migration: move such values to
`set_sensitive` first.

A refresh scrubs the `set_sensitive` values stored in state and the values
the release's last revision was installed with under the same names, so a
failed apply that changed a `set_sensitive` value — the state keeps the
previous configuration so that the change is retried, while the revision
Nelm recorded and the objects it updated carry the new value — is covered
too. One window remains in which state can hold a `set_sensitive` value in
cleartext (a plan does not print an unchanged map element, so it reaches
state rather than plan output): **after `terraform import`**. Imported
state has no `set_sensitive` values until the first apply, so the
refreshes up to then cannot scrub them. A `moved` block from `helm_release`
carries them over and is not affected.

The placeholders carry a truncated SHA-256 of the value; see
[Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md) for what that means for
low-entropy secrets. Put sensitive data in Kubernetes `Secret`s where you
control the chart (such data is stored in cleartext in the cluster
otherwise too), and treat `terraform.tfstate` as sensitive with an
encrypted backend, as HashiCorp recommends for all providers: the
`Sensitive` flag masks output, it does not encrypt state.

### Non-deterministic charts

Terraform runs the provider's planning again during every apply — a plain
`terraform apply` too, not only a saved plan — and aborts the apply with
"Provider produced inconsistent final plan" if any value the plan showed as
known comes out different. Chart templates that produce a new value on every
render — `randAlphaNum` and the other `rand*` functions, `uuidv4`, `genCA`,
`genSelfSignedCert`, `genPrivateKey`, `now` — are common: Helm's documented
`rollme: {{ randAlphaNum 5 | quote }}` annotation, webhook certificates
generated with `genCA`, deploy timestamps (a `now` pod annotation that
rolls the pods on every upgrade), and passwords generated behind a
`lookup` guard (Bitnami charts, `grafana` without `adminPassword`,
`airflow`'s `fernetKey`). A `lookup` guard does **not** help on a first install: the
object it looks for does not exist yet at either render.

With `diff_mode = "full"` the provider renders the chart twice for every
plan and compares the two renders:

- An object they disagree on is **volatile**. Whenever the release is
  installed or reinstalled (a create, any change of its inputs, drift in any
  object), the volatile object's entry in `resources` is `(known after
  apply)` and is read back from the cluster after the install. The plan then
  shows that the object is re-rendered, not which of its fields change
  (the input change itself is shown).
- When nothing changes, a volatile object whose live state differs from the
  new render only where every render differs (the random annotation, the
  timestamp, the generated password) keeps its value: the plan is empty,
  like `helm_release`'s, and the pods are not rolled on every apply. Any
  other difference in it (out-of-band drift, a chart change) is a change,
  and reinstalls the release.
- If the two renders do not even contain the same objects, the whole
  `resources` map is known after apply, with a warning.

Two kinds of templates are not handled this way and need
`diff_mode = "none"` or a chart change:

- **Revision-dependent templates** (`.Release.Revision` in a pod annotation,
  for example): every render of one plan agrees, but it renders the next
  revision against the live one, so every plan shows a change and every
  apply reinstalls the release and rolls its pods — it never converges.
  `helm_release` (without its `manifest` experiment) does not compare
  manifests and is not affected.
- **Coarse-grained time** (`now | unixEpoch`, `now | date "2006-01-02"`): the
  two renders of one plan usually agree, while the apply's re-plan, seconds
  or minutes later, renders a different value — and the apply aborts. A
  template that prints `now` itself (with nanoseconds) is handled as
  volatile.

`diff_mode = "none"` renders nothing at plan time, so neither affects it.
The alternative is to make the chart deterministic: set generated values
explicitly (an `existingSecret`, or a password from a `random_password`
resource) or turn the feature off (a chart that stamps a deploy timestamp
usually has a value that disables it).

Workflows that refuse to apply a plan that differs from the reviewed one —
a saved plan, Atlantis, `dflook/terraform-apply` comparing the plan with the
one on the pull request — are blocked by any release whose plan text
changes from one plan to the next, which is the coarse-grained-time case; one
such release blocks the whole root module. Before migrating, check the
charts' templates for `rand`, `uuid`, `gen`, `derivePassword`, `now`, `date`
and `.Release.Revision`.

A **replacement** (taint, `-replace`, a `release_storage_driver` change, a
`name`/`namespace` change whose objects collide with the old release's) is
planned while the old release's objects exist, so a `lookup` guard finds
them at plan time and nothing at apply time, after the destroy. Its
`resources` are therefore known after apply as a whole (see
[Replacement and `create_before_destroy`](#replacement-and-create_before_destroy)),
and the apply installs freshly generated values — a new generated
password, as `helm uninstall` followed by `helm install` would.

### Readiness tracking and `wait`

With `wait = true` (the default) an apply succeeds only once Nelm's
readiness tracking (kubedog) reports every created or updated resource
ready. The wait has no deadline of its own: only the `timeouts`
`create`/`update` value ends it, and then the whole operation is aborted
(see `auto_rollback` below). This is **stricter than `helm_release`'s
`wait = true`**, which only checks Pods, Deployments, StatefulSets,
DaemonSets, ReplicaSets, PVCs, Services and CRDs and treats every other
kind as ready at once:

- **Non-hook Jobs** are awaited to completion, with no tracked failure
  allowed (Helm waits for Jobs only with `wait_for_jobs`).
- **Custom resources with a built-in status rule** are awaited: External
  Secrets `ExternalSecret`, cert-manager `Certificate`, Argo CD
  `Application`/`ApplicationSet`, Flux `HelmRelease`/`Kustomization`,
  Prometheus Operator `Prometheus`/`Alertmanager`/`ThanosRuler`, Kyverno
  `Policy`/`ClusterPolicy`, `SealedSecret`, Longhorn `Volume`/`Backup` and
  Zalando `postgresql`. An `ExternalSecret` reporting `Ready=False` or a
  `Degraded` Argo `Application` fails the apply within seconds. An operator
  chart such as kube-prometheus-stack now waits for its `Prometheus` and
  `Alertmanager` to become available, so raise its `timeouts`.
- **Any other resource** whose `status.phase`, `status.state`,
  `status.status` or `status.health` (or a `current*` variant) holds a
  recognized pending or failed word (`Pending`, `Progressing`, `Failed`,
  `Error`, …) is awaited too. A custom resource without such a field,
  including one that only reports a `Ready` condition, counts as ready at
  once.
- These generic resources fail after **4 minutes without any status
  change or event**, independent of `timeouts`. Deployments, StatefulSets,
  DaemonSets, Jobs and Pods have dedicated trackers without that cap.
- **`OnDelete` StatefulSets** are awaited until every replica runs the new
  revision; see below.

`wait = false` sets Nelm's `NoFinalTracking`, which drops only the
tracking that **no later step of the deploy plan depends on**. Compared
with `helm_release`'s `wait = false`, which waits for hooks and nothing
else:

- Still awaited: `pre-install`/`pre-upgrade` hooks, every resource in an
  earlier `werf.io/weight` group than the last one, targets of a
  `werf.io/deploy-dependency-*` annotation with `state=ready`, and —
  unlike Helm — **all main resources of a chart that has a
  `post-install`/`post-upgrade` hook**, because the hook is applied only
  after them.
- Not awaited, unlike Helm: a `post-install`/`post-upgrade` hook without a
  `hook-succeeded` delete policy. With that policy its deletion is a later
  step, so the hook is awaited.
- A resource that is not tracked cannot fail the apply: a crashlooping
  Deployment is reported `deployed`, and `auto_rollback` has nothing to
  react to. Errors that are not readiness failures (the API server
  rejecting a manifest, a tracked hook failing) still fail the apply.

Per-resource overrides are annotations on the rendered manifest, set
through the chart's templates or values:

- `werf.io/track-termination-mode: NonBlocking` — never wait for this
  resource (whatever `wait` says).
- `werf.io/fail-mode: IgnoreAndContinueDeployProcess` — a failure of this
  resource does not fail the release. It does **not** end a wait for a
  resource that never becomes ready; only `NonBlocking` does.
- `werf.io/no-activity-timeout` (e.g. `"15m"`) — raise the 4-minute
  no-activity cap of the generic tracker.
- `werf.io/failures-allowed-per-replica` (e.g. `"3"`) — tolerate more
  failed probes/restarts per replica of a Deployment, StatefulSet or
  DaemonSet (default 1). Jobs always allow 0.

#### `OnDelete` StatefulSets

Helm treats a StatefulSet with `updateStrategy.type: OnDelete` as ready
without checking it. Nelm waits until every replica runs the new
revision, and the controller never replaces `OnDelete` pods by itself. So
any pod-template change (image, resources, labels, …) blocks the apply
("user should delete old pods manually now!") until `timeouts` expires.
The release is left `failed` or `pending-upgrade`, and every retry waits
again until someone deletes the old pods. Before moving such a release
from `helm_release`, annotate the StatefulSet with
`werf.io/track-termination-mode: NonBlocking` (for example through the
chart's StatefulSet annotation or merge-patch values): Nelm then skips it,
exactly like Helm. `wait = false` also avoids the wait when no later
deploy step depends on the StatefulSet, but it drops tracking for the whole
release. `werf.io/fail-mode` does not help.

### `auto_rollback` vs `helm_release`'s `atomic`

`auto_rollback = true` rolls a failed upgrade back to the last `deployed`
revision, like `atomic`, but only for a failure Nelm detects **while the
operation is still inside its `timeouts` budget**: a resource failing
readiness tracking (`CrashLoopBackOff`, `ImagePullBackOff`, probe failures
beyond the allowed count, an `ExternalSecret` with `Ready=False`, …), a
failing hook, or the API server rejecting a manifest. Two differences
matter when migrating `atomic = true`:

- **No rollback when `timeouts.create`/`update` expires.** Pods stuck
  `Pending` (unschedulable, or waiting for a node pool to scale up), an
  init container waiting on a dependency, a slow but progressing rollout
  and a long-running Job are not failures to the tracker. They end only
  when the budget runs out; Nelm then aborts the whole operation and its
  rollback runs on the already-expired context, so nothing is rolled back.
  The release is left `failed` (or `pending-upgrade`) with the new
  revision's resources live, and the next apply retries the upgrade. Helm
  rolls back after a timeout too, with a fresh timeout for the rollback.
  Even for an early failure the rollback gets only what is left of the
  same budget, so size `timeouts` well above the expected rollout time.
- **A failed first install is not uninstalled.** With no previous
  `deployed` revision there is nothing to roll back to: the failed release
  and its resources stay in the cluster, and the Terraform resource is
  tainted, so the next apply replaces it (uninstall, then a fresh
  install). `atomic` uninstalls a failed first install and leaves nothing
  in state.

### Namespace lifecycle

The provider always lets Nelm create the release namespace if it is
missing, which is what `helm_release` does with `create_namespace = true`
(its default is `false`). Nelm 1.27 and later have an option to skip that
step; this provider does not expose it yet, so there is no
`create_namespace` attribute. Before every install (each create and update
of the release; plans and destroys do not run it), Nelm checks the
namespace like this:

1. It runs a dry-run server-side apply of its lock ConfigMap
   `werf-synchronization` in the release namespace. If that succeeds, the
   namespace exists, and Nelm leaves it alone.
2. If the dry run is refused as `Forbidden` or `NotFound` (the namespace
   is missing, or the credentials may not `patch` ConfigMaps there, or
   `create` them while the ConfigMap does not exist yet), Nelm
   server-side-applies the `Namespace` object itself, with only its name:
   first as a dry run, then for real, as field manager `helm`. This creates
   the namespace if it is missing, and also runs when it already exists. It
   needs `patch` on Namespaces, and `create` for a missing one.
3. If the `Namespace` dry run is refused as `Forbidden` or `NotFound` too,
   the apply fails with
   `create release namespace: can't apply ConfigMap for locking, and can't apply release namespace (in case ConfigMap apply error caused by non-existent namespace)`,
   followed by both errors.
4. Any other error from the ConfigMap dry run, for example from an
   admission policy that rejects the unlabelled ConfigMap as `Invalid`,
   fails the apply with
   `create release namespace: dry-run apply release synchronization configmap`.
5. Any other error from the `Namespace` dry run or from the real apply
   fails the apply with `create release namespace: dry-run apply release namespace`
   or `create release namespace: create release namespace`.

Credentials limited to the release namespace (a `Role`, with no access to
`Namespace` objects) therefore need `get`, `create`, `update` and `patch`
on ConfigMaps there; without `patch`, every create and update fails at
step 3. See
[Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md#provider-configuration-and-cluster-access).

`terraform destroy` does **not** delete the namespace, whether Nelm created
it or not — only the release's managed resources and release-storage
records are removed. Nelm's lock ConfigMap `werf-synchronization`, which
every apply and destroy creates in the release namespace if it is missing,
stays as well.

To manage a namespace yourself (its labels and annotations, or deleting it
on destroy), declare it with a separate resource, such as
`kubernetes_namespace_v1` from the `hashicorp/kubernetes` provider, and
pass its name to the release, e.g.
`namespace = kubernetes_namespace_v1.app.metadata[0].name`. The reference
makes Terraform create the namespace before the release and destroy the
release before the namespace. With the ConfigMap permissions above, Nelm's
check then stops at step 1 and does not touch the `Namespace` object.
Until this provider exposes Nelm's option, an install into a namespace
that does not exist still creates it instead of failing. To remove a
namespace Nelm created, delete it manually once no release is left in it.

### Values precedence

When multiple `values` documents and `set`/`set_sensitive` entries are
given, later ones win over earlier ones, key by key:

1. Each `values` list element is treated as its own values file, applied
   in list order (later elements override earlier ones on overlapping
   keys).
2. `set` entries are applied next, in list order, each one after the
   fully-merged `values` result.
3. `set_sensitive` entries take precedence over both `values` and `set`
   on any overlapping key. If the **same** value name appears in both
   `set` and `set_sensitive`, the `set` entry is dropped so the
   `set_sensitive` value wins — regardless of the two entries' `type`s.
   (This is enforced by the provider: Nelm merges the underlying
   `--set`/`--set-string`/`--set-literal`/`--set-json` categories in a
   fixed order rather than the order they were written, so simply
   "applying `set_sensitive` last" would not otherwise guarantee it wins.)

### Chart references

`chart` accepts:

- A local directory, given as an absolute or relative path (`./charts/x`,
  `../charts/x`, or an existing relative directory name) — the provider
  normalizes any local reference to an absolute path before handing it to
  Nelm, so a long-running provider process is never tripped up by a
  working-directory change.
- A local `.tgz` chart archive.
- A remote reference: an `oci://` registry URL, or a bare `repo/name`
  reference resolved against the `repository` attribute.

Setting `repository` makes the chart reference unambiguously **remote**: it
is then never resolved against the local filesystem (a same-named local
directory cannot hijack it), and combining `repository` with a local path
(`/abs`, `./rel`) or with a full `oci://` chart is rejected as
contradictory.

`helm_release`'s OCI form works unchanged: `repository = "oci://host/path"`
with `chart = "app"` is joined into the single reference
`oci://host/path/app` before it reaches Nelm, exactly as the `helm`
provider does (Nelm on its own would fetch an `oci://` repository as a
classic `index.yaml` repository and fail). Both spellings install the same
chart, and state keeps `chart` and `repository` exactly as written:

```hcl
# Equivalent to chart = "oci://us-central1-docker.pkg.dev/my-project/charts/app"
resource "nelm_release" "app" {
  name       = "app"
  repository = "oci://us-central1-docker.pkg.dev/my-project/charts"
  chart      = "app"
  version    = "0.2.0"
}
```

Each plan and apply step downloads a remote chart afresh into its own
private temporary directory, removed when the step ends. No chart archive
is stored in the shared Helm cache (`~/.cache/helm/repository`), so
releases whose charts share a name and version but come from different
repositories or registries never pick up each other's archive, even when
Terraform runs them in parallel or several Terraform runs share a host.
`repo/name` references are still resolved from the usual `helm repo add`
configuration and index cache.

Private **OCI registries** are supported via the provider-level `registries`
block (see the provider docs' GKE + Artifact Registry example). Out of scope
for v1: credentials for classic HTTP `repository` URLs, and werf-specific
encrypted "secret values" files (`WERF_SECRET_KEY` /
`.helm/secret-values.yaml`-style workflows) — this provider never sets a
werf secret key, by design.
