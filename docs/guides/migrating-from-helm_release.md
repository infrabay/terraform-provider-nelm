---
page_title: "Migrating from helm_release"
subcategory: ""
description: |-
  Hand releases managed by hashicorp/helm's helm_release over to nelm_release without uninstalling or reinstalling them.
---

# Migrating from `helm_release`

`nelm_release` reads and writes the same release storage Helm does
(`sh.helm.release.v1.<name>.v<revision>` Secrets), so moving a release from
`hashicorp/helm`'s `helm_release` to `nelm_release` is a **state handover**:
Terraform stops managing the `helm_release` and starts managing the very same
Helm release as a `nelm_release`. Nothing is uninstalled and nothing is
reinstalled.

> **Warning: do not just rename the resource type — it uninstalls the
> release.** Changing `resource "helm_release" "x"` to
> `resource "nelm_release" "x"` makes Terraform plan two unrelated
> operations: **destroy** `helm_release.x` (a `helm uninstall` of your
> production release) and **create** `nelm_release.x`. They run concurrently,
> in no defined order. Whichever runs first, the release is uninstalled —
> every Deployment, Service (and its LoadBalancer IP), and PVC without a
> `keep` resource policy. `nelm_release` refuses to create over an existing
> release (see
> [`adopt_existing`](../resources/release.md#creating-a-resource-for-an-existing-release)),
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
   `hashicorp/helm` from `required_providers` once no `helm_release` is left.

**Resources in modules.** Declare the blocks in the root module and use full
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
[Sensitive values in non-`Secret` resources](../resources/release.md#sensitive-values-in-non-secret-resources).

## Attribute mapping

| `helm_release` | `nelm_release` | Notes |
|---|---|---|
| `name`, `namespace` | `name`, `namespace` | Both force replacement when changed. |
| `chart`, `repository`, `version` | `chart`, `repository`, `version` | Both OCI forms work: `repository = "oci://host/path"` plus a bare chart name, or the full reference in `chart` (`oci://host/path/name`). Keep the form the `helm_release` used: after a `moved` block a different spelling of the same chart is still a change, and its apply installs. Set `version` even where the `helm_release` left it unset — see [Values](#values-carry-them-over-verbatim). |
| `values` | `values` | Same list of YAML documents. |
| `set`, `set_sensitive` | `set`, `set_sensitive` | Same `{ name, value, type }` objects; `type` also accepts `"json"`. |
| `set_list`, `set_wo` | — | Express them in `values` (or `set` with `type = "json"`). |
| `max_history` | `release_history_limit` | Not the same default — see [History limit](#history-limit). |
| `timeout` (seconds) | `timeouts { create, update, delete }` | Go durations, e.g. `"600s"` or `"10m"`. |
| `atomic` | `auto_rollback` | Covers only part of `atomic`: no rollback when `timeouts` expires, and a failed first install is not uninstalled — see [`auto_rollback` vs `helm_release`'s `atomic`](../resources/release.md#auto_rollback-vs-helm_releases-atomic). |
| `skip_crds` | `no_install_crds` | |
| `take_ownership` | `force_adoption` | |
| `upgrade_install` | `adopt_existing` | Create-only opt-in; prefer `import`. |
| `create_namespace` | — | Nelm always creates a missing namespace. |
| `wait` | `wait` | Same default (`true`), but Nelm's readiness tracking is stricter, and `wait = false` still waits for what later deploy steps depend on — see [Readiness tracking and `wait`](../resources/release.md#readiness-tracking-and-wait). Size `timeouts` accordingly. |
| `wait_for_jobs` | — | With `wait = true` Nelm always waits for non-hook Jobs to complete. |
| `force_update`, `recreate_pods`, `reset_values`, `reuse_values`, `cleanup_on_fail`, `replace`, `disable_webhooks`, `disable_crd_hooks`, `disable_openapi_validation`, `render_subchart_notes`, `dependency_update`, `devel`, `verify`, `keyring`, `lint`, `description`, `pass_credentials`, `postrender`, `repository_username`/`repository_password`/`repository_*_file` | — | Not supported. |
| `id` (`<name>`) | `id` (`<namespace>/<name>`) | Rewire references that use the id. |
| `metadata` | `metadata` (`app_version`, `chart_name`, `chart_version`, `values_json`), `revision`, `status` | `helm_release`'s `metadata.revision`/`notes`/`values` have no 1:1 counterpart. |
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
[Choosing the cluster](../index.md#choosing-the-cluster), but `KUBE_CTX`
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
accordingly.

## History limit

`helm_release`'s `max_history` defaults to `0`, which Helm treats as
**unlimited** history. In `nelm_release`, an unset `release_history_limit`
means Nelm's default of **10** (see the attribute's documentation for what
`0` means): the first revision Nelm writes prunes all older revisions beyond
the limit, and pruned revisions cannot be recovered. Set
`release_history_limit` explicitly before the first apply — to the old
`max_history`, or to a large number if you relied on unlimited history.

## Release storage driver

`terraform import` and the `moved` block assume the default `secret`
storage driver. Releases that were installed with `HELM_DRIVER=configmap`
(or the `helm` provider's `helm_driver = "configmap"`) are not found that
way; see the resource docs and [Known limitations](../KNOWN_LIMITATIONS.md)
for their status.

## Field managers

Objects that `helm_release` created carry a field manager named after the
`helm` provider binary (`terraform-provider-helm_v<version>_x5`), which Nelm
itself does not recognise as Helm's own. Right before each install (never
during a plan), the provider renames that manager to `helm`, the Helm 3
CLI's, so Nelm takes the fields over and prunes what the chart no longer
renders; see
[Migrating from `helm_release`](../resources/release.md#migrating-from-helm_release)
in the resource docs. An object whose hand-over hits an unavailable admission
webhook is skipped with a warning and handed over by a later apply. Keep the
first `nelm_release` apply rendering exactly what `helm_release` last applied
(see [Values](#values-carry-them-over-verbatim)) and make removals
afterwards anyway: the handover apply then changes nothing but ownership.

One default differs: with `no_remove_manual_changes = false`, fields added
with `kubectl edit` are removed by the next update, which `helm_release`
kept. Decide before the first plan; see
[Fields added out of band](../resources/release.md#fields-added-out-of-band).

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
  [Non-deterministic charts](../resources/release.md#non-deterministic-charts).
  Set `diff_mode = "none"` on that release, or make the chart deterministic.
- **The apply fails with `... is locked by another operation`.** The release
  has a `pending-*` revision: a `helm` command (or another apply) is still
  running against it, or one was interrupted. See
  [Pending releases](../resources/release.md#pending-releases-helms-release-lock).
