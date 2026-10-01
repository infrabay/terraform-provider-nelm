# terraform-provider-nelm

[![test](https://github.com/infrabay/terraform-provider-nelm/actions/workflows/test.yml/badge.svg)](https://github.com/infrabay/terraform-provider-nelm/actions/workflows/test.yml)
[![License: MPL 2.0](https://img.shields.io/badge/License-MPL_2.0-brightgreen.svg)](./LICENSE)

A Terraform provider for [Nelm](https://github.com/werf/nelm), a
drop-in-compatible successor to Helm 3. The provider calls Nelm's Go library
directly (no `helm` or `werf` CLI shell-out) to install, diff, and uninstall
chart-based releases on a Kubernetes cluster.

Its headline feature: **`terraform plan` shows a real, per-resource,
per-field diff** — both configuration changes and out-of-band cluster drift —
computed from Nelm's own plan engine, not just "this release will change".

## Features

- **`nelm_release`** — manage a chart release like `helm_release`, but with a
  field-level plan diff surfaced through a computed `resources` map.
- **Drift detection** — out-of-band changes to fields the chart sets
  (`kubectl edit`/`scale`, a controller mutating a chart-set field) show up on
  the next plan. Fields added out of band that the chart does not render are
  not drift; note that ones added with `kubectl edit` are removed on the next
  update unless `no_remove_manual_changes = true` (see the resource docs).
- **`terraform import`** — adopt releases created by plain `helm install`
  (Helm 3 **or** Helm 4) or by Nelm, with zero storage conversion. Moving
  off `hashicorp/helm`? Follow
  [Migrating from `helm_release`](docs/guides/migrating-from-helm_release.md).
- **Chart sources** — local directories and `.tgz`, plus `oci://` and
  `repo/name` remote charts.
- **Secret redaction** — Secret data (and `werf.io/sensitive`-annotated fields)
  are redacted to deterministic placeholders before entering state, and so
  are `set_sensitive` values wherever a chart renders them.
- **Flexible connection** — a kubeconfig (`kube_config_paths` / `kube_context`,
  or the `helm` provider's `KUBE_CONFIG_PATH(S)` / `KUBE_CTX` variables) or an
  inline `host` / `token` / `cluster_ca_certificate` (mirrors the
  `kubernetes`/`helm` providers), plus a `registries` block for private OCI
  charts. A configuration that names no cluster is an error — never an
  implicit `~/.kube/config` current-context.

## Installation

Releases are published to the
[Terraform Registry](https://registry.terraform.io/providers/infrabay/nelm/latest)
as `infrabay/nelm`. Declare the provider and run `terraform init`:

```hcl
terraform {
  required_providers {
    nelm = {
      source  = "infrabay/nelm"
      version = "~> 0.1"
    }
  }
}

provider "nelm" {
  kube_context = "my-cluster"
}

resource "nelm_release" "example" {
  name      = "example"
  namespace = "example-ns"
  chart     = "${path.module}/charts/example"
  values    = [file("${path.module}/values.yaml")]
}
```

Pin the provider version and commit `.terraform.lock.hcl`: the provider is
pre-1.0, so read the release notes before moving to a new minor version.

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) 1.0 or
  later (CI tests 1.15.8). Migrating from `helm_release` needs 1.7 or later
  (`removed` and `import` blocks), or 1.8 or later for a `moved` block.
- [Go](https://go.dev/dl/) (matching the `go` directive in `go.mod`) — only
  to build the provider from source.

## Documentation

- [Provider configuration](docs/index.md), including a GKE + Google Artifact
  Registry example
- [`nelm_release`](docs/resources/release.md)
- [Migrating from `helm_release`](docs/guides/migrating-from-helm_release.md)

The same pages are rendered on the Terraform Registry.

## Development

```sh
make install     # build the binary and install it for dev_overrides
make test        # gofmt, go vet, golangci-lint (if installed), go build, go test -race
make testacc     # acceptance tests against a LOCAL cluster (NELM_TEST_KUBE_CONTEXT)
```

A locally built provider is run through Terraform's `dev_overrides`, which
skips `terraform init`; [`DEVELOPMENT.md`](DEVELOPMENT.md) covers that setup,
the acceptance tests' local-cluster guard, CI and the release process, and
[`CONTRIBUTING.md`](CONTRIBUTING.md) covers pull requests.

## Known limitations

See [`docs/KNOWN_LIMITATIONS.md`](docs/KNOWN_LIMITATIONS.md) for the current
list of known edge-case limitations and the roadmap toward v1.0.

## License

[Mozilla Public License 2.0](./LICENSE).
