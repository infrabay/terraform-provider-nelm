# Nelm Provider

The `nelm` provider manages Helm-chart-based releases on a Kubernetes cluster
through the [Nelm](https://github.com/werf/nelm) Go library — not the `helm`
CLI, and not the werf CLI. It calls Nelm's `action` package directly
in-process (`action.ReleasePlanInstall` for diffs, `action.ReleaseInstall`
for apply, `action.ReleaseUninstall` for destroy, `action.ReleaseGet` for
refresh), so every plan and apply speaks Nelm's own rendering, ordering, and
resource-tracking logic rather than shelling out.

Nelm is a drop-in-compatible successor to Helm 3: release storage uses the
same Secret/ConfigMap format Helm writes (`sh.helm.release.v1.<name>.v<rev>`
by default), so this provider can adopt releases that were created with
plain `helm install` — see the resource docs' Import section. Releases
managed by `hashicorp/helm`'s `helm_release` are handed over without a
reinstall by following
[Migrating from `helm_release`](guides/migrating-from-helm_release.md);
**never** just rename `helm_release` to `nelm_release`, which uninstalls the
release.

Unlike `helm_release`, every plan renders the chart and shows each object's
changes and out-of-band drift. `Secret` data and `set_sensitive` values are
redacted from that diff, but a secret passed through `values` or `set` is
shown wherever the chart renders it outside a `Secret` — see
[Sensitive values in non-`Secret` resources](resources/release.md#sensitive-values-in-non-secret-resources).
Charts whose templates generate random or time-based values are handled, but
a few template patterns can never converge or abort the apply; check
[Non-deterministic charts](resources/release.md#non-deterministic-charts)
before migrating, and set `diff_mode = "none"` on such a release for
`helm_release`-style plans.

v1 of this provider ships exactly one resource, `nelm_release`. There is no
`nelm_release` data source and no chart-repository data source in v1.

## Example Usage

```hcl
terraform {
  required_providers {
    nelm = {
      source = "infrabay/nelm"
    }
  }
}

provider "nelm" {
  kube_context = "my-cluster-context"
}

resource "nelm_release" "example" {
  name      = "example"
  namespace = "example-ns"
  # ${path.module} makes the reference independent of the directory the
  # terraform CLI is invoked from (a bare relative path would resolve
  # against the CLI's working directory, not this module's).
  chart     = "${path.module}/charts/example"

  values = [
    file("${path.module}/values.yaml")
  ]
}
```

### GKE + a private OCI chart (Google Artifact Registry)

This mirrors a `hashicorp/helm` provider setup: connect to the cluster with an
inline `host` / `token` / `cluster_ca_certificate` from the `google` provider
(no kubeconfig context needed), and authenticate to a private OCI registry with
a `registries` block.

```hcl
data "google_client_config" "default" {}

data "google_container_cluster" "main" {
  name     = "my-cluster"
  location = "us-central1"
}

provider "nelm" {
  host                   = "https://${data.google_container_cluster.main.private_cluster_config[0].public_endpoint}"
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

resource "nelm_release" "app" {
  name      = "app"
  namespace = "app-ns"
  chart     = "oci://us-central1-docker.pkg.dev/my-project/helm/app"
  version   = "0.2.0"
}
```

Why the `registries` block is needed for OCI: Nelm's underlying Helm v4 OCI
client does not reliably use an external Docker credential helper (e.g.
`docker-credential-gcloud`) for some registries such as Artifact Registry — it
obtains a valid token yet the pull still returns `401`. Supplying a static
username/password here (written to a private, per-operation Docker
`config.json`) is what the `helm` provider's `registries` block does too, and it
authenticates reliably. The access token is short-lived (~1h); because it comes
from a data source it is refreshed on every `plan`/`apply`.

For the `helm` provider's settings and their `nelm` equivalents, see the
provider-block mapping in the
[migration guide](guides/migrating-from-helm_release.md#provider-configuration).

## Schema

### Optional

- `kube_config_paths` (List of String) Paths to kubeconfig files; contents
  are merged if more than one is given. Defaults to `~/.kube/config` when
  this and `kube_config_base64` are both empty.
- `kube_config_base64` (String, Sensitive) Base64-encoded kubeconfig
  content. Takes precedence over `kube_config_paths`.
- `kube_context` (String) Kubeconfig context to use.
- `kube_qps` (Number) Queries-per-second limit for the Kubernetes client.
  Must be at least 1 if set. Nelm defaults to 30 if unset.
- `kube_burst` (Number) Burst limit for the Kubernetes client. Must be at
  least 1 if set. Nelm defaults to 100 if unset.
- `kube_request_timeout` (String) Timeout for individual Kubernetes API
  requests, as a Go duration string (e.g. `"30s"`). Unset means no timeout.
- `host` (String) Kubernetes API server URL (e.g. `"https://10.0.0.1"`).
  Mirrors the `kubernetes`/`helm` providers' `host`. When set, `host` + `token`
  + `cluster_ca_certificate` form a **standalone** connection (a complete
  kubeconfig is synthesized internally) that fully replaces
  `kube_config_paths`/`kube_config_base64`/`kube_context` — the ambient
  `~/.kube/config` is never read, so it can't collide with the current context.
  A missing scheme defaults to `https://`.
- `token` (String, Sensitive) Bearer token for the Kubernetes API (e.g.
  `data.google_client_config.default.access_token`).
- `cluster_ca_certificate` (String) PEM-encoded root certificate bundle for
  the API server (the decoded value, as with the other providers'
  `base64decode(...)`).
- `insecure` (Boolean) Skip TLS verification of the API server certificate.
  Testing only.
- `tls_server_name` (String) Server name for API TLS validation when it
  differs from the host.
- `registries` (List of Object, assigned with `=` — HCL block syntax is
  rejected) Static OCI registry credentials for pulling
  `oci://` charts (mirrors the `helm` provider's `registries`). Each element
  takes `url` (only the host is used; one entry per host), `username`, and
  `password` (Sensitive).
  Required for private registries whose credentials come from a Docker
  credential helper that Nelm's OCI client cannot use (see the GKE example
  above).

None of these attributes may depend on values that are only known after
apply (e.g. an attribute of another resource created in the same run): the
provider hard-errors on `Configure` if any of them is Unknown. Data sources
such as `google_client_config` / `google_container_cluster` are read during
plan, so using their attributes here is fine. This is a documented v1
limitation — deferred provider configuration is experimental in the
underlying plugin framework version this provider uses, and this provider
does not build on it.

Kubernetes authentication can come either from a kubeconfig
(`kube_config_paths` / `kube_config_base64` / `kube_context` — client
certificates and exec-based auth plugins such as cloud-provider token helpers
all work as long as the kubeconfig is valid) or from the inline
`host`/`token`/`cluster_ca_certificate` fields above. Setting `host` selects
the inline path: a complete kubeconfig is built from those fields and used on
its own, so the two mechanisms never mix and the ambient `~/.kube/config` (and
whatever its current context is) is ignored entirely.

## Local development: `dev_overrides`

Local iteration on the provider itself uses Terraform's
[`dev_overrides`](https://developer.hashicorp.com/terraform/cli/config/config-file#development-overrides-for-provider-developers)
mechanism to point a `infrabay/nelm` provider address directly at a
`go build` binary, bypassing the registry, the provider lock file, and
**`terraform init` entirely** — `dev_overrides` providers are never
resolved from a registry or written to `.terraform.lock.hcl`, so running
`init` against them is unnecessary (and actively skipped in this
project's workflow).

See [`DEVELOPMENT.md`](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/DEVELOPMENT.md)
for the full setup: building the binary (`make install`), writing a
`TF_CLI_CONFIG_FILE` pointing `dev_overrides` at `$(go env GOBIN)`, and
running `terraform plan` / `apply` / `import` / `destroy` directly against
`examples/basic` with no `.terraform` directory and no lock file present at
all.
