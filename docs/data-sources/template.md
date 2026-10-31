---
page_title: "nelm_template Data Source - nelm"
subcategory: ""
description: |-
  Renders a Helm chart with the Nelm Go library, offline or against the provider's cluster, like helm_template. The outputs hold rendered Secrets and set_sensitive values in cleartext.
---

# nelm_template (Data Source)

Renders a Helm chart with the Nelm Go library (`action.ChartRender`, what
`nelm chart render` runs) and returns the rendered manifests: the objects a
`nelm_release` of the same chart and values would install. It is the
counterpart of `hashicorp/helm`'s `helm_template` data source; see
[Migrating from `helm_template`](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/migrating-from-helm_release.md#migrating-from-helm_template)
for the differences. Nothing is installed and no release record is written.

By default the render is **offline**: it never contacts a cluster, and it
works with a provider configuration that names no cluster at all
(`provider "nelm" {}`). With `validate = true` the chart is rendered against
the provider's cluster instead, for its capabilities and `lookup` (see
[Offline rendering and `validate`](#offline-rendering-and-validate)).

Every namespace-scoped object comes back with a `metadata.namespace`: the
release `namespace` where the template wrote none, the namespace a
`nelm_release` would create the object in. This differs from
`helm_template`, which returns the template's text; see
[Namespaces](#namespaces).

> **Warning: `manifest`, `manifests` and `crds` are not sensitive.** They
> hold every rendered `Secret` (its `data`, base64 is not encryption) and
> every value the chart renders — `set_sensitive` values included — in
> cleartext, in the state and wherever a value derived from them is printed.
> Unlike `nelm_release`'s `resources`, nothing is redacted. See
> [Secret data in state](#secret-data-in-state).

## Example Usage

```hcl
# No cluster: an offline render needs none.
provider "nelm" {}

data "nelm_template" "app" {
  name      = "app"
  namespace = "app-ns"

  # ${path.module} makes the reference independent of the directory the
  # terraform CLI is invoked from.
  chart = "${path.module}/charts/app"

  values = [
    file("${path.module}/values.yaml")
  ]

  set = [
    { name = "image.tag", value = "v1.2.3" },
  ]

  # What the templates see as .Capabilities: set them to match the cluster
  # the manifests are meant for.
  kube_version = "1.31.0"
  api_versions = ["monitoring.coreos.com/v1"]
}

output "deployment" {
  value = data.nelm_template.app.manifests["templates/deployment.yaml"]
}
```

`manifests` is not sensitive, so it can drive `for_each`. Every value is
written as rendered, `Secret` data included:

```hcl
resource "local_file" "manifests" {
  for_each = data.nelm_template.app.manifests

  filename = "${path.module}/rendered/${each.key}"
  content  = each.value
}
```

Rendering against the cluster, with its real capabilities and `lookup`:

```hcl
provider "nelm" {
  kube_context = "my-cluster-context"
}

data "nelm_template" "app_live" {
  name      = "app"
  namespace = "app-ns"
  chart     = "oci://registry.example.com/charts/app"
  version   = "1.4.2"

  validate = true
}
```

A runnable version of the offline configuration is
[`examples/template/main.tf`](https://github.com/infrabay/terraform-provider-nelm/blob/main/examples/template/main.tf)
in the provider's repository; its chart is
[`testdata/charts/template`](https://github.com/infrabay/terraform-provider-nelm/tree/main/testdata/charts/template).

## Schema

### Required

- `name` (String) Release name (`.Release.Name`). Must be between 1 and 53
  characters.
- `chart` (String) Chart reference: local directory (absolute or
  relative), `.tgz` archive, `oci://` URL, or `repo/name`, exactly as in
  `nelm_release`. Local relative paths are normalized to absolute. Must be
  at least 1 character. A local directory is rendered with the
  dependencies vendored in its `charts/` directory only (see
  [Chart dependencies](#chart-dependencies)).

### Optional

- `namespace` (String) Release namespace (`.Release.Namespace`). Defaults
  to `"default"`. It is also written into every rendered namespace-scoped
  object that has no `metadata.namespace`, hooks included and the objects
  of `crds/` directories excepted (see [Namespaces](#namespaces)).
- `repository` (String) Chart repository URL used to resolve a bare chart
  name. An `oci://` URL is joined with `chart` into one `oci://` reference
  and authenticated through the provider's `registries` block, as in
  `nelm_release`. Credentials for classic HTTP repositories are out of
  scope.
- `version` (String) Chart version constraint. If omitted, the latest
  version is used, resolved again every time the data source is read (see
  [When the chart is rendered](#when-the-chart-is-rendered)). Pin it for
  charts from a repository or registry.
- `values` (List of String) List of raw YAML documents, merged in order
  with later entries overriding earlier ones, as in `nelm_release`.
- `set` (List of Object) Individual value overrides, applied after `values`
  and before `set_sensitive`; a list-nested attribute, assigned with `=`:
  `set = [{ name = "image.tag", value = "v2" }]`. Entries of one `type` are
  applied in list order, the types in Nelm's fixed order, as in
  `nelm_release` (see its
  [Values precedence](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#values-precedence)).
  See nested schema below.
- `set_sensitive` (List of Object) Same shape as `set`, but `value` is
  marked sensitive; on a name conflict the `set_sensitive` entry wins. Only
  the input is sensitive: wherever the chart renders the value, it appears
  in cleartext in `manifest`, `manifests` and `crds`. Error messages are
  scrubbed of it as in `nelm_release`. See nested schema below.
- `kube_version` (String) Kubernetes version an offline render reports as
  `.Capabilities.KubeVersion` (e.g. `"1.31.0"`). Defaults to Nelm's offline
  default, `1.20.0` in Nelm 1.27 — the same default `helm_template` uses: a
  chart whose `Chart.yaml` `kubeVersion` requires a newer cluster then fails
  to render (`chart requires kubeVersion: ... which is incompatible with
  Kubernetes v1.20.0`), and a chart that picks API versions by Kubernetes
  version renders the old ones. Set it to the version of the cluster the
  manifests are meant for. Cannot be set together with `validate = true`,
  which uses the cluster's version.
- `api_versions` (List of String) Extra API versions added to
  `.Capabilities.APIVersions` (e.g. `"monitoring.coreos.com/v1"`), offline
  and with `validate = true` alike.
- `include_crds` (Boolean) Also put the objects of the `crds/` directories
  (the chart's and its subcharts': their CustomResourceDefinitions, and
  anything else a chart puts there) into `manifest` and `manifests`.
  Defaults to `false`. They are always in `crds`.
- `skip_tests` (Boolean) Leave test hooks (objects whose `helm.sh/hook`
  annotation lists `test` or `test-success`, matched like Helm:
  case-insensitively, surrounding spaces ignored) out of `manifest` and
  `manifests`. Defaults to `false`: test hooks are rendered like any other
  hook.
- `show_only` (List of String) Only put the objects rendered from these
  templates into `manifest` and `manifests`. Each entry is a glob pattern
  (Go's `path.Match`: `*`, `?`, `[...]`, never across a `/`, on every OS)
  matched against the template's path relative to the chart root, e.g.
  `"templates/deployment.yaml"`, `"templates/*.yaml"` or
  `"charts/sub/templates/*"`. Empty entries are ignored, as with
  `helm_template`. A pattern that matches no template that
  rendered an object — after `include_crds` and `skip_tests` — fails the
  read with `Template Not Found`, as `helm_template` does.
- `validate` (Boolean) Render against the provider's cluster instead of
  offline, for two things only: the cluster's version and served API
  versions as `.Capabilities`, and `lookup` against its live objects. The
  release's history is not read: `.Release.IsUpgrade` is false and
  `.Release.Revision` 1, as offline and as with `helm_template`. Defaults
  to `false`, which never contacts a cluster. It does not validate the
  objects against the cluster's OpenAPI schemas; see
  [Offline rendering and `validate`](#offline-rendering-and-validate).
- `timeouts` (Block, Optional) See nested schema below.

### `set` / `set_sensitive` nested schema

Identical to `nelm_release`'s (`set_sensitive.value` is additionally marked
sensitive), and translated to Nelm's `--set*` options the same way:

- `name` (String, Required) Dotted value path (e.g. `"image.tag"`).
- `value` (String, Required) Value to set.
- `type` (String, Optional) How to parse `value`: `""` or `"auto"` infers
  the type (Nelm `ValuesSet`), `"string"` forces a string (`ValuesSetString`),
  `"literal"` forces a literal string even if it looks like a number/bool
  (`ValuesSetLiteral`), `"json"` parses `value` as JSON (`ValuesSetJSON`).
  Must be one of `""`, `"auto"`, `"string"`, `"literal"`, `"json"`.

### `timeouts` nested schema

Optional block, a Go duration string:

- `read` (String) Timeout for the read. Defaults to 5m. It bounds the
  whole render, the remote-chart download included: a render still running
  when it expires — a slow template, a large chart, or, with
  `validate = true`, a cluster that does not answer — fails the read with a
  timeout error. Nelm cannot stop a render once started, so the abandoned
  one keeps running in the background until it finishes. With
  `validate = true`, the API discovery that tells namespace-scoped kinds
  apart after the render (see [Namespaces](#namespaces)) is bounded only
  by the provider's `kube_request_timeout`.

### Not offered (not attributes of this data source)

`helm_template` attributes without a counterpart here, and why, are listed
in the migration guide's
[`helm_template` section](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/migrating-from-helm_release.md#unsupported-helm_template-attributes).
The ones most often missed:

- `notes`, `render_subchart_notes` — Nelm's render does not return the
  chart's `NOTES.txt`.
- `is_upgrade` — every render is a first install (`.Release.IsUpgrade`
  false, `.Release.Revision` 1), `validate = true` included, which is also
  what `helm_template` renders.
- `disable_webhooks` (`helm_template`'s switch to leave hooks out) — hooks
  are always rendered; `skip_tests` leaves out test hooks, and `show_only`
  selects templates.
- `dependency_update` — see [Chart dependencies](#chart-dependencies).

## Attributes Reference

In addition to the arguments above, the following attributes are exported:

- `id` (String) `"<namespace>/<name>"`.
- `manifest` (String) The selected objects (see `include_crds`,
  `skip_tests` and `show_only`), concatenated in Nelm's order (see
  [Output format](#output-format)), each as
  `nelm chart render` prints it: a `---` line, a
  `# Source: <chart name>/<template path>` line, then the object's YAML.
  It equals what `nelm chart render` prints for the same objects, except
  for the release namespace the data source puts back (see
  [Namespaces](#namespaces)). Cleartext; see
  [Secret data in state](#secret-data-in-state).
- `manifests` (Map of String) The selected objects grouped by template:
  keyed by the template's path relative to the chart root
  (`templates/deployment.yaml`, `charts/sub/templates/cm.yaml`,
  `crds/crd.yaml` with `include_crds`), each value the objects rendered
  from that template in `manifest`'s format and order. A template that
  rendered no object has no key. Cleartext.
- `crds` (List of String) The YAML of every object of the `crds/`
  directories (the chart's and its subcharts'), one element per object,
  without the `---` and `# Source:` lines, whatever `include_crds` and
  `show_only` say. These are normally CustomResourceDefinitions; anything
  else a chart puts into `crds/` is in `crds` too, as with `helm_template`.
  Cleartext.

## Caveats and Notes

### Secret data in state

`manifest`, `manifests` and `crds` are not marked sensitive, as with
`helm_template`, so that `for_each` over `manifests` works and
configurations that use `helm_template`'s outputs keep working. The price:

- Every `Secret` the chart renders is in them as rendered. Nelm folds a
  `Secret`'s `stringData` into `data`, so the values are base64-encoded,
  which anyone can decode.
- Every value the chart renders into any object is in them in cleartext,
  `set_sensitive` values included: `set_sensitive` marks only the input
  attribute. `nelm_release`'s redaction (`Secret` data placeholders,
  `set_sensitive` scrubbing) does not apply to this data source.
- Terraform stores data source results in the state, so all of it is in
  `terraform.tfstate` (and in a saved plan file), and Terraform prints it
  wherever it prints a value derived from these attributes: a resource
  argument in a plan, an output, `terraform show`, `terraform console`.
  Terraform accepts an output of `manifest` without `sensitive = true` and
  prints it in full.

Treat the state as sensitive (an encrypted backend with restricted access,
as HashiCorp recommends for every provider), mark outputs that carry these
attributes `sensitive = true`, wrap a value in `sensitive(...)` before
passing it on where you do not need to iterate over it (Terraform cannot
`for_each` over a sensitive map), and leave templates that render secrets
out with `show_only` where the consumer does not need them. Error and
warning messages are scrubbed of `set_sensitive` values the same way as
`nelm_release`'s (see
[Sensitive values in non-`Secret` resources](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#sensitive-values-in-non-secret-resources)).

### Offline rendering and `validate`

**Offline** (`validate = false`, the default), the render never contacts a
cluster, and the templates see:

- `.Capabilities.KubeVersion`: `kube_version`, or Nelm's default (`1.20.0`
  in Nelm 1.27, the same as `helm_template`'s).
- `.Capabilities.APIVersions`: Nelm's built-in set plus `api_versions`.
- `lookup`: always empty, as in `helm template`.
- `.Release.IsInstall` true, `.Release.IsUpgrade` false and
  `.Release.Revision` 1.

**With `validate = true`**, the render runs against the provider's cluster,
and the templates see:

- `.Capabilities.KubeVersion` and `.Capabilities.APIVersions`: the cluster's
  server version and the API versions it serves (from discovery), plus
  `api_versions`.
- `lookup`: the cluster's live objects.
- `.Release.IsInstall` true, `.Release.IsUpgrade` false and
  `.Release.Revision` 1, exactly as offline. The release's history is not
  read, even when the release is installed: the render uses Nelm's empty
  in-memory release storage, like `helm_template`, whose render is always a
  first install too. The output therefore does not change after every
  apply of a `nelm_release` of the same name, and the storage driver the
  release uses does not matter.

The two modes also tell namespace-scoped kinds apart differently (see
[Namespaces](#namespaces)): offline from a built-in table of Kubernetes'
own kinds, with `validate = true` from the cluster's API discovery.

`validate = true` only reads: discovery, the server version, and whatever
objects the chart's `lookup` calls read; the release's storage Secrets are
not read, so their RBAC does not matter. Each read runs Nelm's own API
discovery. It does **not** validate the rendered objects against the
cluster's OpenAPI schemas, dry-run apply them, run admission webhooks, or
check that the namespace exists, unlike `helm_template`'s `validate`: an
object the API server would reject (a misspelled field included) renders
without an error.

`validate = true` needs a provider configuration that names a cluster. With
`provider "nelm" {}` the read fails with `No Kubernetes connection
configured`; see also
[Provider configuration known only at apply](#provider-configuration-known-only-at-apply).

### Namespaces

Nelm removes `metadata.namespace` from every rendered object whose
namespace equals the release `namespace` (`nelm chart render` prints them
that way). A template that writes `namespace: {{ .Release.Namespace }}`
and one that writes no namespace would therefore come back alike, without
one, and a consumer that applies the documents (`kubernetes_manifest`,
`kubectl_manifest`, `kubectl apply -f`) would create those objects in its
own default namespace instead of the release's.

`nelm_template` puts the release namespace back: every object of
`manifest` and `manifests` that has no `metadata.namespace` and whose kind
is namespace-scoped gets `metadata.namespace: <namespace>`, hooks included.
That is the namespace a `nelm_release` (or `helm install`) of the chart
creates it in. Objects that name another namespace keep it, cluster-scoped
kinds (`ClusterRole`, `Namespace`, `CustomResourceDefinition`) get none,
and the objects of `crds/` directories (in `crds`, and in `manifest` with
`include_crds`) are left as rendered.

Whether a kind is namespace-scoped is decided:

- **offline**, from a built-in table of the kinds built into the
  Kubernetes API server (`ConfigMap`, `Deployment`, `ClusterRole`, ...,
  removed kinds and kinds behind alpha feature gates included) and, for a
  custom resource, from its CustomResourceDefinition when the same render
  contains it (in `crds/` or in a template);
- **with `validate = true`**, from the cluster's API discovery, run again
  at every read (so a CRD that an earlier resource of the same apply
  installed counts), and, for a kind the cluster does not serve yet, from a
  CustomResourceDefinition in the render.

A kind neither source knows — typically a custom resource whose CRD comes
from another chart, such as a `ServiceMonitor` rendered offline — keeps no
namespace, and the read returns one warning listing those kinds. Render
with `validate = true` against a cluster that serves them, or set
`metadata.namespace` where the documents are applied.

This differs from `helm_template` on purpose. `helm_template` returns the
template's text, so its objects carry the namespace the template wrote and
no other: an object whose template writes none comes back without one.
`nelm_template` returns such an object with the release namespace.

### Output format

The output is Nelm's, not the chart's text: every object is parsed and
written back out the way `nelm chart render` prints it. Compared with the
template's own text (and with `helm_template`'s output):

- keys are sorted alphabetically, comments and empty documents are dropped,
  and quoting and indentation follow the YAML encoder, not the template;
- every object has `metadata.labels` and `metadata.annotations`, empty
  (`{}`) when the chart renders none; labels and annotations whose value
  is not a string are dropped;
- every namespace-scoped object carries `metadata.namespace`, the release
  `namespace` where the template wrote none (see [Namespaces](#namespaces));
- a `Secret`'s `stringData` is folded into `data`, base64-encoded;
- a `List` (`kind: List` or a typed `*List`) is replaced by its items;
- the objects come in Nelm's order: the objects of `crds/` directories
  (with `include_crds`), then hooks, then everything else, each group
  sorted alphabetically by kind, then API group, version, namespace and
  name — neither Helm's install order nor the order Nelm deploys in. A
  consumer that applies `manifest` as is (`kubectl apply -f`) cannot rely
  on it for dependencies: a `Namespace` comes after the `Deployment` that
  lives in it. `helm_template` puts the hooks last;
- `crds` has one element per object, where `helm_template` returns each
  `crds/` file's text.

`# Source:` lines keep the chart name (`# Source: app/templates/cm.yaml`),
while `manifests` keys and `show_only` patterns leave it out
(`templates/cm.yaml`), as with `helm_template`.

### When the chart is rendered

Terraform reads a data source during every plan and refresh, unless one of
its arguments is unknown or it depends (`depends_on`) on a resource with
pending changes; then it is read during apply and its attributes are known
after apply. Each read renders the chart again, and downloads a remote
chart again (with the provider's `registries` credentials for `oci://`):
the repository or registry must be reachable at every plan, and an unpinned
`version` follows the newest chart.

A template whose output changes on every render — `randAlphaNum`,
`uuidv4`, `now`, `genCA`, `genSelfSignedCert` — therefore changes
`manifest` and `manifests` at every plan, and every resource that consumes
them (a manifest applied with another provider, a `local_file`) plans a
change on every run. `nelm_release` detects such objects by rendering
twice; a data source cannot. With `validate = true`, a template that reads
live objects with `lookup` changes whenever they do. For stable output,
render offline and pass such values in through `values` (e.g. from a
`random_password` resource); otherwise add `lifecycle { ignore_changes }`
on the consumer, or install the chart with `nelm_release`.

### Chart dependencies

A local chart directory is rendered with the dependencies vendored in its
`charts/` directory only, as an archive (`charts/<name>-<version>.tgz`) or
an unpacked chart (`charts/<name>/`), like `helm_template` without
`dependency_update`. The data source never builds or downloads a
dependency from the chart's `Chart.lock` or `requirements.lock` (it loads
them as Helm does: a malformed one fails the read, and a v1 chart's
`requirements.lock` is in its `.Files`): a dependency that
`Chart.yaml` (or `requirements.yaml`) declares and `charts/` lacks fails the
read with `Missing chart dependencies`, which names it, whatever the
provider's `registries`. A `file://` dependency is no exception: like Helm,
copy it into `charts/` first. An archive or directory the chart's
`.helmignore` excludes does not count, as for Helm. Vendor the dependencies
with `helm dependency build` and commit `charts/`, or build it before
Terraform runs. The chart directory itself is only read.

A packaged chart (a `.tgz`, or one from a repository or registry) carries
its dependencies, and is rendered as it is.

`nelm_release` behaves differently: Nelm's chart loader builds a locked
dependency that `charts/` lacks before the install (an unpacked directory
does not count there, only the archive), refreshing every repository in
the Helm configuration of the user running Terraform and downloading over
the network, `oci://` ones with the provider's `registries` credentials and
Docker's `~/.docker/config.json` as a fallback; see
[Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md).

### werf secret values

A chart that carries werf secret values (`secret-values.yaml`, or a
`secret/` directory) is out of scope. Nelm then needs a werf secret key,
which it looks for in `WERF_SECRET_KEY`, in `.werf_secret_key` in
Terraform's working directory and in `~/.werf/global_secret_key`: without
one the read fails (`required encryption key not found`), and with one the
render depends on the environment of the machine that runs Terraform. The
same applies to `nelm_release`.

### Provider configuration known only at apply

When the provider configuration depends on values known only after apply
(e.g. `host` from a cluster created in the same run, see the provider
docs'
[Provider configuration known only at apply](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/index.md#provider-configuration-known-only-at-apply)):

- an offline render works. Only an `oci://` chart (`chart = "oci://..."`,
  or a bare `chart` with an `oci://` `repository`) also needs the
  provider's `registries` attribute to be known (absent counts as known);
  otherwise its read fails with `Provider configuration not known at plan
  time` rather than pull the chart without the configured credentials. A
  local chart and one from a classic `https://` repository render either
  way;
- a `validate = true` render fails with
  `Provider configuration not known at plan time`. Apply the cluster change
  first (`terraform apply -target=...`), keep the cluster in a separate
  root module, render offline, or add a `depends_on` on the cluster
  resource so that Terraform reads the data source during apply.

With Terraform's deferred actions enabled, Terraform defers every
`nelm_template` read while the provider configuration is unknown, offline
renders included.

The provider never contacts a cluster while its configuration is unknown.
