---
page_title: "Migrating from helm_release"
subcategory: ""
description: |-
  Hand releases managed by hashicorp/helm's helm_release over to nelm_release without uninstalling or reinstalling them, and replace helm_template data sources with nelm_template.
---

# Migrating from `helm_release`

`nelm_release` reads and writes the same release storage Helm does
(`sh.helm.release.v1.<name>.v<revision>` Secrets), so moving a release from
`hashicorp/helm`'s `helm_release` to `nelm_release` is a **state handover**:
Terraform stops managing the `helm_release` and starts managing the very same
Helm release as a `nelm_release`. Nothing is uninstalled and nothing is
reinstalled. `helm_template` data sources are replaced separately; see
[Migrating from `helm_template`](#migrating-from-helm_template).

> **Warning: do not just rename the resource type — it uninstalls the
> release.** Changing `resource "helm_release" "x"` to
> `resource "nelm_release" "x"` makes Terraform plan two unrelated
> operations: **destroy** `helm_release.x` (a `helm uninstall` of your
> production release) and **create** `nelm_release.x`. They run concurrently,
> in no defined order. Whichever runs first, the release is uninstalled —
> every Deployment, Service (and its LoadBalancer IP), and PVC without a
> `keep` resource policy. `nelm_release` refuses to create over an existing
> release (see
> [`adopt_existing`](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#creating-a-resource-for-an-existing-release)),
> but that does not stop the `helm_release` destroy. Always use the recipe
> below, and check that the plan destroys nothing.

## Requirements

- Terraform **1.7 or later** (`removed` blocks with
  `lifecycle { destroy = false }`, and `import` blocks); 1.8 or later for the
  [`moved` alternative](#alternative-a-moved-block-terraform-18).
- Keep `hashicorp/helm` in `required_providers`, and its `provider "helm"`
  block, until the migration has been applied: the `helm_release` objects are
  still in the state while Terraform plans the handover.
- Configure the `nelm` provider for the same cluster (see
  [Provider configuration](#provider-configuration)).
- Declare `nelm = { source = "infrabay/nelm" }` in `required_providers` in
  **every** module that contains a `nelm_release`, not only in the root
  module (see [Resources in modules](#resources-in-modules)).
- Plan with credentials that may write to the cluster. Unlike
  `helm_release`'s, a `nelm_release` plan dry-runs a server-side apply of
  every object and can patch their `managedFields`, so a pipeline that plans
  with a read-only identity needs write access for its plans too (see
  [Cluster permissions](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/index.md#cluster-permissions)).

## Check the charts first

`nelm_release` diffs every rendered object against the cluster and applies
charts with Nelm, so a few chart patterns behave differently from
`helm_release`. Before moving a release, check its chart for:

- **Templates that render a new value every time** (`randAlphaNum`, `genCA`,
  `now`, `.Release.Revision`): see
  [Non-deterministic charts](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#non-deterministic-charts).
- **Fields a controller owns** — `replicas` rendered unconditionally next to
  an autoscaler, a webhook `caBundle` that an injector fills in: every plan
  shows them as drift and every apply resets them (see
  [Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md#drift-and-out-of-band-changes)).
- **A `crds/` directory**: Nelm updates those CRDs on every install, where
  Helm only created them; set `no_install_crds = true` where the CRDs are
  managed elsewhere (see
  [Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md#chart-versions-and-crds)).
- **Size**: a large chart's handover plan prints every object, megabytes for
  charts with many CRDs. Move large releases one per change, and review the
  full plan rather than a size-capped pull-request comment (see
  [Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md#plan-size)).
- **Dependencies that are not vendored** (a local chart directory): when its
  `Chart.lock` (or `requirements.lock`) lists a dependency from an
  `https://` or `oci://` repository and `charts/` has no
  `<name>-<version>.tgz` archive for it (or only one the chart's
  `.helmignore` excludes), Nelm's chart loader downloads it on each
  machine (each CI run without a warm `~/.werf` cache), updating every
  repository in your Helm configuration and writing progress output to the
  Terraform log, where `helm_release` fails without
  `dependency_update = true`. Vendor the archives
  (`helm dependency build`) first (see
  [Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md#chart-versions-and-crds)).

## The one-apply recipe

For each `helm_release`, in one change (one plan, one apply):

```hcl
# 1. Stop managing the helm_release WITHOUT uninstalling it.
removed {
  from = helm_release.app

  lifecycle {
    destroy = false
  }
}

# 2. Adopt the same Helm release into nelm_release.
import {
  to = nelm_release.app
  id = "app/app" # "<namespace>/<release name>"
}

# 3. The replacement resource, with the SAME chart, version and values the
#    helm_release last applied.
resource "nelm_release" "app" {
  name      = "app"
  namespace = "app"
  chart     = "oci://registry-1.docker.io/bitnamicharts/nginx"
  version   = "1.2.3" # the chart version the helm_release pinned

  release_history_limit = 10 # was max_history

  set = [
    {
      name  = "replicaCount"
      value = "2"
    },
  ]
}
```

Then:

1. Run `terraform plan` and check it before anything else:
   - `helm_release.app will no longer be managed by Terraform`
     — and **no** `destroy` of anything.
   - `nelm_release.app will be imported` and `updated in-place`
     (never `must be replaced`). The update is expected: `chart` cannot be
     recovered from Helm's release storage, so it is null right after the
     import, and the first apply runs `nelm install` (see
     [What the first apply does](#what-the-first-apply-does)).
   - The `resources` diff should show no changes to your objects. A diff
     there means the new configuration does not render what the
     `helm_release` last applied; fix the configuration first.
2. `terraform apply`.
3. Delete the `removed` and `import` blocks (they are one-shot), and drop
   `hashicorp/helm` from `required_providers` once no `helm_release` and no
   `helm_template` data source is left (see
   [Migrating from `helm_template`](#migrating-from-helm_template)).

### Resources in modules

Every module that declares a `nelm_release` must name the provider's source
in its own `required_providers`. A module without one gets away with
`helm_release`, because Terraform infers `hashicorp/helm` from the type
name; for `nelm_release` it infers `hashicorp/nelm`, which does not exist,
and `terraform init` fails to find that provider:

```hcl
# modules/app/versions.tf
terraform {
  required_providers {
    nelm = {
      source = "infrabay/nelm"
    }
  }
}
```

The root module's `provider "nelm"` block reaches the module as its default
`nelm` provider, or explicitly through `providers = { nelm = nelm.<alias> }`
on the module call.

Declare the `removed` and `import` blocks in the root module and use full
addresses: `from = module.app.helm_release.this`,
`to = module.app.nelm_release.this`. A `removed` block takes no instance keys
and covers every instance of the resource (all `count`/`for_each` instances,
in all instances of the module), so every instance needs its own `import`
block (or one `import` block with `for_each`) in the same change. When a
shared module switches from `helm_release` to `nelm_release`, every root
module that uses it needs these blocks in the apply that picks up the new
module version (the `moved` alternative below avoids that).

## Alternative: a `moved` block (Terraform 1.8+)

`nelm_release` also accepts a cross-provider move from `helm_release`:

```hcl
moved {
  from = helm_release.app
  to   = nelm_release.app
}

resource "nelm_release" "app" {
  # ... the same inputs as in the recipe above
}
```

The move carries over the release identity and the `helm_release` inputs
that have a `nelm_release` counterpart: `name`, `namespace`, `chart` and
`repository` as written (a split `repository = "oci://..."` plus chart name
included), `version` (except for a local chart, whose `helm_release` version
is just its `Chart.yaml` version), `values`, `set`, `set_sensitive`,
`max_history`, `atomic`, `wait`, `skip_crds` and `take_ownership`, mapped as
in the [table](#attribute-mapping) below; `release_storage_driver` becomes
`secret`. The refresh before the plan reads `status`, `revision`,
`metadata` and `resources` from the cluster. Unlike after an import,
`chart` is known, so when the `nelm_release` configuration reproduces the
`helm_release` inputs the plan can be empty: no install, no new revision, no
hooks.

The move leaves `timeouts` null (`helm_release`'s `timeout` is not carried
over) and `release_history_limit` null when `max_history` was `0`, and it
sets every attribute without a `helm_release` counterpart to its default.
A configuration with a `timeouts` block, with an explicit
`release_history_limit` where `max_history` was `0` (as
[History limit](#history-limit) recommends), or with a non-default value
for such an attribute (e.g. `no_remove_manual_changes = true` or
`diff_mode = "none"`) therefore plans an in-place update, and its apply
runs `nelm install` as after an import (see
[What the first apply does](#what-the-first-apply-does)). So does anything
else that differs.

Check the plan the same way: `has moved to`, no destroy, never
`must be replaced`. In a shared module, put the `moved` block inside the
module, next to the new resource: it applies to every instance of the
module. Keep `hashicorp/helm` installed for this apply too — the moved object
still names it in the state.

## Values: carry them over verbatim

Copy the `helm_release`'s `values`, `set` and `set_sensitive` into the
`nelm_release` **unchanged**: same chart, same `version`, same values. Do
**not** seed them from `helm get values --all` (`-a`): that returns the
chart's defaults merged in, which pins every default in your configuration
and makes the first apply write a new revision for nothing. If the original
inputs are lost, `helm get values <name> -n <namespace>` (without `-a`)
returns only the user-supplied values.

**Pin `version`, even if the `helm_release` did not.** An unset `version`
means the newest chart version, and the plan does not say which one that is
(only the `resources` changes it brings). When the `helm_release` has no
`version`, the handover apply installs the newest chart, which can be an
upgrade (even across a major version) on top of the migration: after an
import (`chart` is null, so the first apply always installs), and after a
`moved` block too (the plan changes `version` from the moved value to null,
which installs again). Set `version` to the chart version the release runs
now (`terraform state show helm_release.x` before the handover, or the chart
column of `helm list -n <namespace>`), and drop the pin in a later apply if
tracking the newest version is intended.

Make removals, chart bumps and other changes in a **later** apply, after the
handover has been applied once. This also matters for field ownership: see
[Field managers](#field-managers).

**Check where secrets go before the first plan.** `helm_release` prints no
rendered manifest unless `experiments { manifest = true }` is set;
`nelm_release` prints every object the chart renders in each plan. `Secret`
data and `set_sensitive` values are replaced by placeholders, but a secret
passed through `values` or `set` — even from a `sensitive = true` variable —
is printed wherever the chart renders it outside a `Secret` (a container
`env` value, a `ConfigMap`). Pass such a value through `set_sensitive`
instead; the chart renders the same objects either way. See
[Sensitive values in non-`Secret` resources](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#sensitive-values-in-non-secret-resources).

## Attribute mapping

| `helm_release` | `nelm_release` | Notes |
|---|---|---|
| `name`, `namespace` | `name`, `namespace` | Both force replacement when changed. |
| `chart`, `repository`, `version` | `chart`, `repository`, `version` | Both OCI forms work: `repository = "oci://host/path"` plus a bare chart name, or the full reference in `chart` (`oci://host/path/name`). Keep the form the `helm_release` used: after a `moved` block a different spelling of the same chart is still a change, and its apply installs. Set `version` even where the `helm_release` left it unset — see [Values](#values-carry-them-over-verbatim). |
| `values` | `values` | Same list of YAML documents. |
| `set`, `set_sensitive` | `set`, `set_sensitive` | Same `{ name, value, type }` objects; `type` also accepts `"json"`. |
| `set_list`, `set_wo` | — | Express them in `values` (or `set` with `type = "json"`). |
| `max_history` | `release_history_limit` | Not the same default — see [History limit](#history-limit). |
| `timeout` (seconds), `timeouts = { ... }` | `timeouts { create, update, delete }` | A block, written without `=`; Go durations, e.g. `"600s"` or `"10m"`. |
| `atomic` | `auto_rollback` | Covers only part of `atomic`: no rollback when `timeouts` expires, and a failed first install is not uninstalled — see [`auto_rollback` vs `helm_release`'s `atomic`](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#auto_rollback-vs-helm_releases-atomic). |
| `skip_crds` | `no_install_crds` | |
| `take_ownership` | `force_adoption` | |
| `upgrade_install` | `adopt_existing` | Create-only opt-in; prefer `import`. |
| `create_namespace` | — | The provider always lets Nelm create a missing namespace, like `create_namespace = true`; there is no attribute to turn that off yet. Where the `helm_release` left it `false` (the default), keep managing the namespace separately; a missing namespace is then created instead of failing the apply. See [Namespace lifecycle](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#namespace-lifecycle). |
| `wait` | `wait` | Same default (`true`), but Nelm's readiness tracking is stricter, and `wait = false` still waits for what later deploy steps depend on — see [Readiness tracking and `wait`](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#readiness-tracking-and-wait). Size `timeouts` accordingly. |
| `wait_for_jobs` | — | With `wait = true` Nelm always waits for non-hook Jobs to complete. |
| `force_update`, `recreate_pods`, `reset_values`, `reuse_values`, `cleanup_on_fail`, `replace`, `disable_webhooks`, `disable_crd_hooks`, `disable_openapi_validation`, `render_subchart_notes`, `dependency_update`, `devel`, `verify`, `keyring`, `lint`, `description`, `pass_credentials`, `postrender`, `repository_username`/`repository_password`/`repository_*_file` | — | Not supported. |
| `id` (`<name>`) | `id` (`<namespace>/<name>`) | Rewire references that use the id. A resource that uses it as a replacement trigger (`terraform_data`'s `triggers_replace`, `null_resource`'s `triggers`) is replaced once. |
| `metadata` | `metadata` (`app_version`, `chart_name`, `chart_version`, `values_json`), `name`, `namespace`, `revision`, `status` | `metadata.name`, `namespace` and `revision` are top-level attributes; `metadata.chart` and `version` are `metadata.chart_name` and `chart_version`. `metadata.values_json` holds the coalesced values (chart defaults included, not just the user-supplied `metadata.values`) and is sensitive, so an output of it needs `sensitive = true`. `metadata.notes`, `first_deployed` and `last_deployed` have no counterpart. |
| `manifest` | `resources` | One canonical, redacted JSON per object. |

## Provider configuration

| `provider "helm"` | `provider "nelm"` |
|---|---|
| `kubernetes.host` | `host` (a missing scheme defaults to `https://`) |
| `kubernetes.token` | `token` |
| `kubernetes.cluster_ca_certificate` | `cluster_ca_certificate` |
| `kubernetes.insecure`, `kubernetes.tls_server_name` | `insecure`, `tls_server_name` |
| `kubernetes.config_path` / `config_paths` | `kube_config_paths` |
| `kubernetes.config_context` | `kube_context` |
| `kubernetes.exec`, `client_certificate`/`client_key`, `proxy_url`, `username`/`password` | not inline: use a kubeconfig (`kube_config_paths`) that has them |
| `registries` | `registries` (same `url`/`username`/`password` objects) |
| `burst_limit`, `qps` | `kube_burst`, `kube_qps` |
| `helm_driver` | `release_storage_driver` on each `nelm_release` |
| `repository_config_path`, `repository_cache`, `registry_config_path`, `plugins_path`, `debug`, `experiments` | — |

A typical GKE setup carries over one to one:

```hcl
provider "nelm" {
  host                   = data.google_container_cluster.main.private_cluster_config[0].public_endpoint
  token                  = data.google_client_config.default.access_token
  cluster_ca_certificate = base64decode(data.google_container_cluster.main.master_auth[0].cluster_ca_certificate)

  registries = [
    {
      url      = "oci://us-central1-docker.pkg.dev"
      username = "oauth2accesstoken"
      password = data.google_client_config.default.access_token
    },
  ]
}
```

Configure the provider explicitly like this rather than relying on
environment variables the `helm` provider reads. `KUBE_CONFIG_PATH(S)` and
`KUBE_CTX` are honoured as described in the provider docs'
[Choosing the cluster](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/index.md#choosing-the-cluster), but `KUBE_CTX`
alone names no cluster, and `HELM_DRIVER` is not read at all (set
`release_storage_driver` on each `nelm_release`).

## What the first apply does

The first apply after the import is an in-place update (after a `moved`
block, only if the configuration differs from what the move carried over —
a `timeouts` block or a new `release_history_limit` is enough), and it runs
`nelm install` for real. Expect a new release revision, and expect **hooks
to run**: a hook without a `helm.sh/hook-delete-policy` (or with
`before-hook-creation`) is re-created, so `pre-upgrade`/`post-upgrade` Jobs
— admission-webhook certificate patch Jobs, database migrations — run
again, exactly as they would on a `helm upgrade`. Plan the migration window
accordingly. If the chart has a `crds/` directory, the apply also updates
those CRDs to the chart's copies for the first time since `helm_release`
created them (unless `no_install_crds = true`).

## History limit

`helm_release`'s `max_history` defaults to `0`, which Helm treats as
**unlimited** history. In `nelm_release`, an unset `release_history_limit`
means Nelm's default of **10** (see the attribute's documentation for what
`0` means): the first revision Nelm writes prunes all older revisions beyond
the limit, and pruned revisions cannot be recovered. Set
`release_history_limit` explicitly before the first apply — to the old
`max_history`, or to a large number if you relied on unlimited history.

## Release storage driver

`terraform import` and the `moved` block record the default `secret`
storage driver, so a release installed with `HELM_DRIVER=configmap` (or the
`helm` provider's `helm_driver = "configmap"`) is not found that way: the
import fails with `Cannot import non-existent remote object`, and a moved
release drops out of the state at the refresh and is planned as a new
resource. Take such a release over without an import: keep the `removed`
block, and give the new `nelm_release`
`release_storage_driver = "configmap"` and `adopt_existing = true` for that
one apply. Its plan is a create of a live release, so it shows no object
diff (`resources` is known after apply, with a warning); the apply upgrades
the existing release in place and records its objects. Remove
`adopt_existing` afterwards. ConfigMap release records, values included,
can be read with the built-in `view` role; see
[Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md#storage-driver-and-import).

## Field managers

Objects that `helm_release` created carry a field manager named after the
`helm` provider binary (`terraform-provider-helm_v<version>_x5`), which Nelm
itself does not recognise as Helm's own. Right before each install (never
during a plan), the provider renames that manager to `helm`, the Helm 3
CLI's, so Nelm takes the fields over and prunes what the chart no longer
renders; see
[Migrating from `helm_release`](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#migrating-from-helm_release)
in the resource docs. An object whose hand-over hits an unavailable admission
webhook is skipped with a warning and handed over by a later apply. Keep the
first `nelm_release` apply rendering exactly what `helm_release` last applied
(see [Values](#values-carry-them-over-verbatim)) and make removals
afterwards anyway: the handover apply then changes nothing but ownership.

One default differs: with `no_remove_manual_changes = false`, fields added
with `kubectl edit` are removed by the next update, which `helm_release`
kept. Decide before the first plan; see
[Fields added out of band](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#fields-added-out-of-band).

## After the migration

- **Make hotfixes through Terraform.** An out-of-band
  `helm upgrade --reuse-values`, `helm rollback` or `kubectl set image` on a
  migrated release is drift: the next apply of the root module reverts it,
  whatever that apply is for. Under `helm_release`, a successful hotfix that
  kept the chart version survived until the release's own configuration
  changed. Put on-call fixes into the configuration, or hold applies of that
  root module until they are (see
  [Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md#drift-and-out-of-band-changes)).
- **`helm history` reads differently.** Revisions Nelm writes have an empty
  DESCRIPTION, and a superseded revision's UPDATED time is when the next
  revision finished; pick rollback targets by REVISION, CHART and APP
  VERSION (see [Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md#release-history)).
- **Nelm leaves a lock ConfigMap**, `werf-synchronization`, in each release
  namespace. If you move a release back to `helm_release`, delete it once no
  `nelm_release` is left in that namespace.

## If something goes wrong

- **The plan shows a destroy of `helm_release.x`.** The `removed` block is
  missing, misspelled, or lacks `destroy = false`. Do not apply.
- **The apply fails with `nelm release <ns>/<name> already exists`.** The
  `import` block is missing or names a different address, so Terraform tried
  to create the release. Nothing was changed; add the import.
- **Every plan shows the same object of a migrated release changing, or the
  apply fails with `Provider produced inconsistent final plan`.** The chart
  renders something different on every plan in a way the provider cannot
  absorb (a `.Release.Revision` annotation, a coarse timestamp); see
  [Non-deterministic charts](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#non-deterministic-charts).
  Set `diff_mode = "none"` on that release, or make the chart deterministic.
- **The apply fails with `... is locked by another operation`.** The release
  has a `pending-*` revision: a `helm` command (or another apply) is still
  running against it, or one was interrupted. See
  [Pending releases](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#pending-releases-helms-release-lock).

## Migrating from `helm_template`

`nelm_template` renders a chart without installing it, like `helm_template`.
A data source has no state to hand over: change `data "helm_template"` to
`data "nelm_template"`, adjust the attributes as in the table below, and
rewrite the references (`data.helm_template.app.manifests` becomes
`data.nelm_template.app.manifests`). No `moved`, `import` or `removed`
block is involved, and nothing changes in the cluster by itself.

```hcl
# Was: data "helm_template" "app" { ... } with the same arguments.
data "nelm_template" "app" {
  name       = "app"
  namespace  = "app"
  repository = "https://charts.example.com"
  chart      = "app"
  version    = "1.2.3"

  values = [file("${path.module}/values.yaml")]

  kube_version = "1.31.0"
}
```

An offline render (the default) needs no cluster, so a configuration that
only renders charts can use an empty `provider "nelm" {}` block; with
`validate = true`, configure the provider for the cluster as in
[Provider configuration](#provider-configuration). Every module that
declares a `nelm_template` needs `nelm = { source = "infrabay/nelm" }` in
its `required_providers`, as for `nelm_release` (see
[Resources in modules](#resources-in-modules)).

The outputs keep their names and `manifests` its keys, but Nelm writes the
YAML differently from Helm, and namespace-scoped objects gain the release
namespace where the template wrote none (see
[Behavioral differences](#behavioral-differences)): a resource that
consumes `manifest`, `manifests` or `crds` — a manifest applied with
another provider, a file written with `local_file`, a commit to a GitOps
repository — plans a one-time update after the switch. Review those changes
in the first plan.

### `helm_template` attribute mapping

| `helm_template` | `nelm_template` | Notes |
|---|---|---|
| `name`, `namespace` | `name`, `namespace` | `namespace` defaults to `"default"`; `HELM_NAMESPACE` is not read. |
| `chart`, `repository`, `version` | `chart`, `repository`, `version` | As in `nelm_release`: both OCI forms, `oci://` credentials from the provider's `registries`. Pin `version`: the chart is resolved and downloaded again at every read. |
| `values` | `values` | Same list of YAML documents. |
| `set`, `set_sensitive` | `set`, `set_sensitive` | Same `{ name, value, type }` objects; `type` also accepts `"json"`. Lists: entries of one `type` are applied in list order, but Nelm applies the types in a fixed order (`json`, `auto`, `string`, `literal`), so an earlier `type = "string"` entry overrides a later same-name `auto` one. `set_sensitive` wins over `set` on a name conflict. See [Values precedence](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#values-precedence). |
| `set_list`, `set_wo` | — | Express them in `values` (or `set` with `type = "json"`). |
| `kube_version` | `kube_version` | Offline only: an error together with `validate = true`, where `helm_template` ignores it. Unset, it is Nelm's default (`1.20.0` in Nelm 1.27), the same as `helm_template`'s; set it to the target cluster's version. |
| `api_versions` | `api_versions` | Also added with `validate = true`, where `helm_template` ignores them. |
| `include_crds` | `include_crds` | |
| `skip_tests` | `skip_tests` | Same test hooks (`helm.sh/hook` `test` or `test-success`, case-insensitive like Helm). |
| `show_only` | `show_only` | Same patterns against the path relative to the chart root (matched with Go's `path.Match`, so `*` never crosses a `/` on Windows either), empty entries ignored, and the same `Template Not Found` error for a pattern that matches nothing. |
| `validate` | `validate` | Only the cluster's capabilities and `lookup`; no OpenAPI validation — see [Behavioral differences](#behavioral-differences). |
| `timeout` (seconds), `timeouts = { read = ... }` | `timeouts { read = ... }` | A block, written without `=`; defaults to 5m (`helm_template`'s `timeouts.read` defaults to 20m). It bounds the whole render, the chart download included; a render that runs past it fails the read and finishes in the background. |
| `id` (`<name>`) | `id` (`<namespace>/<name>`) | Rewire references that use the id. A resource that uses it as a replacement trigger (`terraform_data`'s `triggers_replace`, `null_resource`'s `triggers`) is replaced once. |
| `manifest`, `manifests` | `manifest`, `manifests` | Same structure (`---`, `# Source: <chart>/<path>`, YAML; `manifests` keyed by the path relative to the chart root), Nelm's YAML, and the release namespace on every namespace-scoped object. |
| `crds` | `crds` | One element per object of the `crds/` directories, re-serialized, where `helm_template` returns each `crds/` file's text. |
| `notes`, `is_upgrade`, and the attributes below | — | See [Unsupported `helm_template` attributes](#unsupported-helm_template-attributes). |

### Unsupported `helm_template` attributes

- `notes`, `render_subchart_notes` — Nelm's chart render does not return
  the chart's `NOTES.txt`, so there is nothing to export.
- `is_upgrade` — every render is a first install (`.Release.IsUpgrade`
  false, `.Release.Revision` 1), with `validate = true` too, which is also
  what `helm_template` renders.
- `disable_webhooks` — in `helm_template` it leaves every hook out of
  `manifest`. Hooks are always rendered here; `skip_tests` leaves out test
  hooks, and `show_only` selects templates.
- `disable_openapi_validation` — `nelm_template` never validates against
  OpenAPI schemas, not even with `validate = true`.
- `skip_crds` — it only skips installing CRDs, which a render never does,
  so it does not change `helm_template`'s output either; `include_crds`
  decides whether CRDs are in `manifest`.
- `atomic`, `wait`, `replace`, `create_namespace`, `description`,
  `reset_values`, `reuse_values` — settings of an install or upgrade, which
  a render does not run.
- `dependency_update` — not offered: `nelm_template` always behaves like
  `helm_template` with its default `dependency_update = false`. A local
  chart is rendered only with the dependencies vendored in its `charts/`
  directory (archive or unpacked chart): nothing is ever built or
  downloaded from its lock files; a dependency `charts/` lacks fails the read
  with `Missing chart dependencies`, as with `helm_template`. Vendor them
  first (`helm dependency build`); see the data source's
  [Chart dependencies](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/data-sources/template.md#chart-dependencies).
- `devel` — set `version` to a constraint that admits pre-releases, e.g.
  `">0.0.0-0"`.
- `verify`, `keyring`, `pass_credentials`, `postrender`,
  `repository_username`/`repository_password`/
  `repository_ca_file`/`repository_cert_file`/`repository_key_file` — not
  supported, as for `nelm_release`. Credentials for `oci://` charts come
  from the provider's `registries` block.
- `set_list`, `set_wo` — see the table above.

### Behavioral differences

- **Namespace-scoped objects carry a namespace.** `helm_template`
  returns the template's text, so an object has `metadata.namespace` only
  where its template writes one. Nelm, for its part, removes
  `metadata.namespace` wherever it equals the release namespace, which
  would leave objects written with `namespace: {{ .Release.Namespace }}`
  without one and send them to the consumer's default namespace.
  `nelm_template` therefore sets the release `namespace` on every
  namespace-scoped object that has none, hooks included and `crds/` objects
  excepted: an object whose template writes no namespace now carries the
  release namespace too, exactly where a `nelm_release` would create it.
  This is deliberate. Offline, a custom resource whose CRD is not in the
  chart (a `ServiceMonitor`, say) cannot be scoped and keeps no namespace,
  with a warning naming its kind; render with `validate = true`, or set its
  namespace where you apply it. See the data source's
  [Namespaces](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/data-sources/template.md#namespaces).
- **The YAML is Nelm's, not the template's.** Nelm parses every object and
  writes it back out: keys sorted, comments dropped, empty
  `metadata.labels: {}` and `metadata.annotations: {}` added where the
  chart renders none, labels and annotations whose value is not a string
  dropped, a `Secret`'s `stringData` folded into base64 `data` (a
  `yamldecode(...).stringData` reference breaks), `List` objects replaced by
  their items. The objects come as the `crds/` objects (with
  `include_crds`), then hooks, then the rest, where `helm_template` puts the
  hooks last, and each group is sorted alphabetically by kind and name, not
  in Helm's install order (a `Namespace` comes after the `Deployment` in
  it), so a consumer that applies `manifest` as is cannot rely on the order
  for dependencies. See the data source's
  [Output format](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/data-sources/template.md#output-format).
- **No notes.** There is no `notes` attribute.
- **Every render is a first install, as with `helm_template`.**
  `.Release.IsInstall` is true, `.Release.IsUpgrade` false and
  `.Release.Revision` 1, whatever is installed, with `validate = true` too;
  there is no `is_upgrade` to override that.
- **`validate` means a render against the cluster, not validation.**
  `helm_template`'s `validate = true` runs Helm's install dry run against
  the cluster: the templates see its capabilities, and the rendered objects
  are checked against its OpenAPI schemas. `nelm_template`'s renders with
  the cluster's real capabilities and `lookup` against its live objects,
  and checks nothing: an object the API server would reject (a misspelled
  field included) renders without an error, and fails only where it is
  applied. It reads no release history and only needs read access to
  discovery and to what `lookup` reads (see the data source's
  [Offline rendering and `validate`](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/data-sources/template.md#offline-rendering-and-validate)).
- **Cleartext outputs, as before.** `manifest`, `manifests` and `crds` are
  not sensitive, as with `helm_template`: rendered `Secret`s and
  `set_sensitive` values are in the state and in anything derived from
  them. `nelm_release`'s redaction does not apply to `nelm_template`; see
  [Secret data in state](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/data-sources/template.md#secret-data-in-state).
