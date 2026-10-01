# nelm_release (Resource)

Manages a Helm/Nelm release via the Nelm Go library
(`action.ReleaseInstall` for apply, `action.ReleasePlanInstall` for diff).

## Example Usage

```hcl
resource "nelm_release" "basic" {
  name      = "basic-example"
  namespace = "tf-nelm-basic-example"

  # ${path.module} makes this absolute regardless of the working directory
  # Terraform is invoked from.
  chart = "${path.module}/../../testdata/charts/basic"

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

See `examples/basic/main.tf` in this repository for the exact, verified
version of this configuration (run via `dev_overrides`, `terraform init`
intentionally skipped — see `docs/DEVELOPMENT.md`).

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
  `"default"`. Changing this attribute forces resource replacement. Nelm's
  `ReleaseInstall` always creates the namespace if missing; there is no
  `create_namespace` toggle (see Caveats below).
- `repository` (String) Chart repository URL used to resolve a bare chart
  name. Private-repo authentication is out of scope for v1.
- `version` (String) Chart version constraint. If omitted, the latest
  version is used; the resolved version surfaces in
  `metadata.chart_version`. Unlike some Helm-based providers, this
  attribute is Optional only — not Computed — so there is no plan-time
  "unknown until apply" dance around it.
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
  successfully deployed. Defaults to `false`.
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
  from the chart's `crds/` directory. Defaults to `false`.
- `release_history_limit` (Number) Maximum number of release revisions
  kept in storage. Null or `0` uses Nelm's own default (10). Only release
  metadata is pruned; cluster resources are unaffected. Must be `0` or
  greater.
- `release_storage_driver` (String, **Forces replacement**) Where release
  metadata is stored: `"secret"`, `"secrets"`, `"configmap"`, or
  `"configmaps"`. Defaults to `"secret"`. Changing it destroys and recreates
  the whole release (Nelm does not migrate history between backends; the
  destroy uses the old backend, the create the new one). Enum-validated at
  plan time — an unrecognized driver string panics inside Nelm, so
  `"memory"` and `"sql"` are rejected in v1.
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
- `read` (String) Timeout bounding `ReleaseGet` during Read AND the
  `ReleasePlanInstall` call inside plan-time diffing. Defaults to 5m.
- `update` (String) Timeout for the install action backing Update.
  Defaults to 10m.
- `delete` (String) Timeout for the uninstall action backing Delete.
  Defaults to 5m.

### Dropped by design (not attributes of this resource)

These exist on comparable Helm-based providers but are intentionally not
exposed here, because the underlying Nelm behavior makes them either
meaningless or actively misleading:

- `create_namespace` — Nelm's `ReleaseInstall` always auto-creates the
  target namespace if it is missing; a toggle to disable that would not
  do anything, so it is not offered.
- `upgrade_install` / a separate "adopt existing release" toggle — Nelm
  install is natively idempotent install-or-upgrade; there is no
  install/upgrade split to select between.
- `wait` / readiness-tracking knobs — Nelm tracks resource readiness
  natively; only the overall operation timeout (`timeouts` block) is
  exposed.
- `atomic` — renamed `auto_rollback` (same semantics, matching Nelm's own
  naming).
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
  (`ReleaseGet`). Unknown whenever the release will be re-installed —
  i.e. a create, out-of-band drift, or any change to
  `chart`/`version`/`values`/`set`/`set_sensitive`/`repository`; left
  untouched on a clean no-change plan. It goes Unknown even when the config
  change renders no manifest change, because Nelm still creates a new
  revision in that case (its up-to-date check compares the coalesced values
  and release notes, not just the rendered resources).
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
  redacted JSON of that resource. See "How `resources` drives
  `terraform plan`" below.

## How `resources` drives `terraform plan`

`resources` is deliberately the mechanism this provider uses to surface
both configuration changes and out-of-band cluster drift in
`terraform plan`, and it is worth understanding to read a plan
correctly:

- On every plan, the **planned** side of `resources` is computed by
  running Nelm's own plan engine (`action.ReleasePlanInstall`) against
  your current configuration and normalizing each resulting resource
  (sensitive-path redaction, then stripping runtime metadata such as
  `status`, `managedFields`, `resourceVersion`, and `uid`) to a
  deterministic, key-sorted JSON string per resource. For *create*d
  resources this is Nelm's client-side render, free of server defaulting.
  For *update*d resources Nelm's plan value is the API server's dry-run
  merge, which carries live fields (server defaults, an HPA-owned
  `replicas`, controller-written annotations) — the provider projects
  those away three-ways against the change's live object and the stored
  desired shape, so the planned value stays a pure function of your
  configuration and never depends on live-mutable cluster state.
- The **prior** side of `resources` (what's already in state) is
  refreshed on every `terraform plan`/`apply` by reading the objects
  *live from the cluster* — not from the release's stored chart
  manifests — and normalizing them the same way, then **projecting** each
  live object onto the shape of its planned counterpart: fields present
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
  intended drift-detection behavior, not a bug.
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
  reachable cluster. If your Terraform configuration itself has any
  unknown inputs (e.g. `chart` computed from another resource not yet
  applied), the same Unknown degradation applies — the provider never
  guesses at a diff it can't actually compute.

## Caveats and Notes

### Import

```sh
terraform import nelm_release.example namespace/name
```

The import ID is exactly `<namespace>/<name>` — no other separator or
format is accepted.

Import adopts an existing release with **zero conversion**, whether it
was created by `helm install`/`helm upgrade` (Helm 3 **or** Helm 4), by
hashicorp/helm's `helm_release` (see "Migrating from `helm_release`"
below for its field managers) or by Nelm itself: Nelm's default
release-storage format
(`sh.helm.release.v1.<name>.v<rev>` Secrets, or the analogous ConfigMap
form) is exactly the format Helm itself writes, so `ReleaseGet` reads a
plain-helm release directly. `force_adoption = true` is **not** required
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
  them in configuration (or leave `version` unset to always track
  latest).

**Recommendation:** before writing the Terraform configuration for a
release you're about to import, capture its current effective values —
either from `metadata.values_json` after the import completes, or via
`helm get values -a <name> -n <namespace>` beforehand — and seed your
`values`/`set` blocks from that, so the first post-import plan is clean
rather than surprising.

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
Helm 3 release and is adopted like one (see Import above, or use a
`removed { lifecycle { destroy = false } }` block plus an `import` block);
[Migrating from `helm_release`](../guides/migrating-from-helm_release.md)
walks through the procedure. Two field-ownership differences matter:

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

### Sensitive values in non-`Secret` resources

Redaction is driven by the *resource kind* (a `Secret`, or an object
explicitly annotated sensitive), **not** by which values you supplied via
`set_sensitive`. If a chart renders a value you passed through
`set_sensitive` into a **non-`Secret`** resource — e.g. a `ConfigMap`
`data` entry, or a container `env` value — that value appears **in
cleartext** in the `resources` diff surface, in both `terraform plan`
output and `terraform.tfstate`. Marking `set_sensitive.value` sensitive
protects the input attribute, but it cannot follow the value through Helm
templating into an arbitrary rendered manifest.

This is inherent to a provider whose primary feature is showing a readable
resource-level diff: the alternative — marking the whole `resources` map
sensitive — would collapse the entire diff to `(sensitive value)` and
defeat that feature, while *not* protecting state at rest (Terraform's
sensitive flag masks CLI output but does not encrypt state). Practical
guidance:

- Put sensitive data in Kubernetes `Secret`s (which are always redacted),
  not in `ConfigMap`s or inline `env` values. This is good Kubernetes
  hygiene regardless of Terraform — such data is stored in cleartext in
  the cluster too.
- Treat `terraform.tfstate` as sensitive and use an encrypted backend, as
  HashiCorp recommends for all providers.

### Non-deterministic charts

The `resources` diff is computed by rendering the chart at plan time and
again at apply time. A chart whose templates are **non-deterministic** —
e.g. generating a password or certificate with `randAlphaNum`, `genCA`, or
`uuidv4` on every render — produces a different rendered manifest each
time, so a saved plan (`terraform plan -out=…` then `terraform apply
<file>`) can fail its consistency check with "Provider produced an
inconsistent final plan". This is inherent to any plan-based Helm tool.
Prefer charts that read such secrets from existing Kubernetes `Secret`s or
`lookup`-guarded templates so a value is generated once and reused, rather
than regenerated on every render.

### Namespace lifecycle

The provider does not offer a `create_namespace` flag because there is
nothing to toggle: Nelm's install path always creates the target
namespace if it doesn't already exist. Symmetrically, `terraform destroy`
does **not** delete the namespace — only the release's managed resources
and release-storage records are removed. If a namespace should be
removed too, manage it with a separate `kubernetes_namespace`-style
resource (from another provider) or delete it manually.

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
(`/abs`, `./rel`) is rejected as contradictory.

Private **OCI registries** are supported via the provider-level `registries`
block (see the provider docs' GKE + Artifact Registry example). Out of scope
for v1: credentials for classic HTTP `repository` URLs, and werf-specific
encrypted "secret values" files (`WERF_SECRET_KEY` /
`.helm/secret-values.yaml`-style workflows) — this provider never sets a
werf secret key, by design.
