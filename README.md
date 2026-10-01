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
  (Helm 3 **or** Helm 4) or by Nelm, with zero storage conversion.
- **Chart sources** — local directories and `.tgz`, plus `oci://` and
  `repo/name` remote charts.
- **Secret redaction** — Secret data (and `werf.io/sensitive`-annotated fields)
  are redacted to deterministic placeholders before entering state.
- **Flexible connection** — a kubeconfig (`kube_config_paths` / `kube_context`)
  or an inline `host` / `token` / `cluster_ca_certificate` (mirrors the
  `kubernetes`/`helm` providers), plus a `registries` block for private OCI
  charts.

## Using the provider

> Not yet published to the Terraform Registry. Until it is, use the local
> `dev_overrides` flow below. Once published, usage will be:

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

Full documentation: [`docs/index.md`](docs/index.md) (provider) and
[`docs/resources/release.md`](docs/resources/release.md) (`nelm_release`),
including a GKE + Google Artifact Registry example.

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.0
- [Go](https://go.dev/dl/) (matching the `go` directive in `go.mod`) — to build
  the provider.

## Local development (`dev_overrides`)

Until the provider is published, it is used locally via Terraform's
`dev_overrides`, which also means **`terraform init` is skipped entirely** —
see [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) for the full explanation.

```sh
# 1. Build and install the binary.
make install                       # -> $(go env GOBIN) (or $(go env GOPATH)/bin)

# 2. Point a CLI config file at it. (GOBIN is empty on a default Go install,
#    so fall back to GOPATH/bin — the same fallback `make install` uses.)
BIN_DIR="$(go env GOBIN)"; BIN_DIR="${BIN_DIR:-$(go env GOPATH)/bin}"
cat > /tmp/terraformrc.nelm-dev <<EOF
provider_installation {
  dev_overrides {
    "registry.terraform.io/infrabay/nelm" = "${BIN_DIR}"
  }
  direct {}
}
EOF
export TF_CLI_CONFIG_FILE=/tmp/terraformrc.nelm-dev

# 3. Run Terraform directly against the example — no `terraform init`.
cd examples/basic
terraform plan
terraform apply
terraform destroy
```

Terraform prints a warning that a provider is dev-overridden; that's expected
and confirms the override is active.

## Testing

```sh
make test        # gofmt, go vet, go build ./..., go test -race ./... (no cluster)
make testacc     # acceptance tests: TF_ACC=1 NELM_TEST_KUBE_CONTEXT=orbstack, real cluster
```

`make test` never touches a cluster (acceptance tests self-skip without
`TF_ACC=1`). `make testacc` runs the acceptance suite against a **local**
Kubernetes cluster named by `NELM_TEST_KUBE_CONTEXT` (default `orbstack`;
e.g. `make testacc NELM_TEST_KUBE_CONTEXT=kind-mycluster`); test code
hard-fails unless that kubeconfig context exists and its `cluster.server`
resolves to `127.0.0.1`/`localhost`, so acceptance tests can never
accidentally run against a real (e.g. cloud) cluster. CI runs the same suite
on every pull request against a [kind](https://kind.sigs.k8s.io/) cluster.

## Releasing

Pushing a `vX.Y.Z` tag triggers `.github/workflows/release.yml`, which runs
[GoReleaser](https://goreleaser.com/) to build cross-platform binaries, a
GPG-signed `SHA256SUMS`, and the Terraform Registry manifest. The signing key's
public half must be registered with the Registry publisher, and the repo must
have `GPG_PRIVATE_KEY` and `PASSPHRASE` secrets set.

## Known limitations

See [`docs/KNOWN_LIMITATIONS.md`](docs/KNOWN_LIMITATIONS.md) for the current
list of known edge-case limitations and the roadmap toward v1.0.

## License

[Mozilla Public License 2.0](./LICENSE).
