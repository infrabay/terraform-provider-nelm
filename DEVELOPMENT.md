# Developing terraform-provider-nelm

This page is for contributors. To use the provider, install it from the
Terraform Registry as the [README](README.md#installation) shows.

Local iteration uses Terraform's `dev_overrides` mechanism with a locally
built binary (no `terraform init`), the real Nelm Go library, and — for
acceptance tests and manual end-to-end runs — a real but disposable local
Kubernetes cluster: **kind or OrbStack (`kube_context = "orbstack"`), never a
cloud context**. CI runs the same unit and acceptance tests on every pull
request (see [Continuous integration](#continuous-integration)), and pushing a
version tag publishes a Registry release (see [Releasing](#releasing)).

## Prerequisites

- Go 1.26 or newer (the `go` directive in `go.mod` is `1.26.0`; its
  `toolchain go1.27.1` line is what CI and release builds use, and what a
  local `go` auto-selects).
- Terraform 1.15.8 (the version CI tests with; `terraform init` tolerates
  dev-overridden providers, but we skip `init` anyway — see below).
- helm CLI (used to author/lint/render `testdata/charts/basic`, and by the
  acceptance tests to install a plain-helm release for the import test).
- A local Kubernetes cluster whose kubeconfig context lives in
  `~/.kube/config` (a kind cluster such as the `kind-nelm-acc` context CI
  uses, or OrbStack's `orbstack`), for anything beyond `go build` /
  `go test` (unit tests never touch a cluster).

## Building and installing locally

```sh
make build     # -> bin/terraform-provider-nelm
make install   # go build + copy into $(go env GOBIN), default $(go env GOPATH)/bin
```

## Dev overrides: skip `terraform init` entirely

Terraform's [`dev_overrides`](https://developer.hashicorp.com/terraform/cli/config/config-file#development-overrides-for-provider-developers)
config block redirects a specific provider address straight at a local
binary, bypassing the registry and the provider lock file. Because
`dev_overrides` providers are never resolved from a registry or written to
`.terraform.lock.hcl`, **`terraform init` is not just optional but actively
counter-productive here — skip it.** This has been verified against
Terraform 1.15.8: `plan`/`apply`/`destroy`/`import` all work against
`examples/basic` with no `.terraform` directory and no lock file present at
all.

1. Create a CLI config file (anywhere; not checked in) pointing at your
   `GOBIN`:

   ```hcl
   # ~/.terraformrc.nelm-dev (example path)
   provider_installation {
     dev_overrides {
       "registry.terraform.io/infrabay/nelm" = "/path/to/go/bin"
     }
     direct {}
   }
   ```

   Replace the path with `$(go env GOBIN)` (or `$(go env GOPATH)/bin` if
   `GOBIN` is unset) — the same directory `make install` copies the binary
   into.

2. Export it before running Terraform:

   ```sh
   export TF_CLI_CONFIG_FILE=~/.terraformrc.nelm-dev
   ```

3. From `examples/basic`, run Terraform directly — **no `terraform init`**:

   ```sh
   cd examples/basic
   terraform plan
   terraform apply
   terraform destroy
   ```

   Terraform will print a warning that one or more providers are overridden
   for development; that is expected and confirms `dev_overrides` is active.

`make e2e` prints this exact sequence as a reminder; it does not run
Terraform unattended, since it targets a real (if disposable) cluster.

## Running the gates

```sh
make test        # == ./gates.sh repo: gofmt, go vet, (golangci-lint if present),
                  #    go build ./..., go test -race ./..., required-file check
./gates.sh pkg ./internal/planconv   # package-scoped variant
```

`go test ./...` alone never touches a cluster: acceptance tests self-skip
without `TF_ACC=1`, and live-fixture-capture programs are guarded by the
`smoke` build tag (`go build -tags smoke ...`), which the default `go test`
invocation never activates.

## Acceptance tests (`TF_ACC=1`, strictly local clusters)

```sh
kind create cluster --name nelm-acc               # once: context "kind-nelm-acc"
make testacc                                      # default: kube context "kind-nelm-acc" (as in CI)
make testacc NELM_TEST_KUBE_CONTEXT=orbstack      # any other LOCAL context, e.g. OrbStack
```

This exports `TF_ACC=1` and `NELM_TEST_KUBE_CONTEXT=<context>`. Test code
additionally hard-fails (`t.Fatal`) unless that context exists in
`~/.kube/config` and its cluster's server host is exactly `127.0.0.1`, `::1`
or `localhost` — a deliberate triple guard (explicit env pin with no default +
context existence + explicit `kube_config_paths` and `kube_context` in every
test's provider config) so acceptance tests can never accidentally run
against a real cloud cluster. The guard, the
provider and the `kubectl`/`helm` fixture helpers all read that one file
(`--kubeconfig` is passed explicitly), so an ambient `$KUBECONFIG` cannot make
them disagree about which cluster a context names; the context must therefore
live in `~/.kube/config` (where OrbStack and kind write it by default).

## Continuous integration

`.github/workflows/test.yml` runs on every pull request, every push to
`main`, and for every release tag (`release.yml` calls it):

- `build`: gofmt, `go vet`, `go build`, the unit tests with `-race`, and a
  check that no tracked file contains a workstation path (a home directory
  under `/Users/...` or `/home/...`, or a session scratch path under
  `/private/tmp/...`);
- `lint`: golangci-lint, pinned to the version `./gates.sh` is run with;
- `govulncheck`: lists the vulnerabilities the code can reach and fails on
  those with a fixed version (the ones reachable through Nelm's dependencies
  today have none);
- `docs`: `tfplugindocs validate --provider-name nelm`, the Registry's
  frontmatter and layout rules for the hand-written `docs/`;
- `acceptance`: the acceptance suite against a
  [kind](https://kind.sigs.k8s.io/) cluster (`kind-nelm-acc` context), with
  Terraform 1.15.8 and the real helm CLI for the out-of-band import fixture.

The tool versions are `*_VERSION` variables at the top of `test.yml`, which
Renovate keeps current. To run the docs and vulnerability checks locally:

```sh
go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@latest validate --provider-name nelm
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

## Releasing

Pushing a `vX.Y.Z` tag runs `.github/workflows/release.yml`. It first runs
the whole test workflow against the tagged commit, then
[GoReleaser](https://goreleaser.com/) v2 (`.goreleaser.yml`) builds the
binaries for every platform the Terraform Registry serves, one zip per
platform, the Registry manifest and a GPG-signed `SHA256SUMS`, and publishes
them as a GitHub release. Its before hook, `go mod tidy -diff`, fails the
release when `go.mod`/`go.sum` at the tag are not tidy. Locally,
`goreleaser check` validates the configuration and
`goreleaser build --snapshot --clean` builds every platform into `dist/`
(ignored by git) without publishing anything.

One-time setup for the Terraform Registry:

1. The repository must be public: the public Registry only publishes from
   public GitHub repositories named `terraform-provider-<name>`.
2. Create an RSA or DSA GPG key (the Registry rejects the default ECC
   type), add its ASCII-armored public key in the Registry's publisher
   settings (Signing Keys), and store the private key and its passphrase as
   the `GPG_PRIVATE_KEY` and `PASSPHRASE` secrets.
3. Keep those secrets in the `release` environment (deployment tags `v*`,
   required reviewers) that `release.yml`'s `goreleaser` job runs in, so
   that only a protected, approved tag push can read them. Protect `v*`
   tags with a ruleset as well.
4. Publish the provider from the HCP Terraform organization that owns the
   `infrabay` namespace (Registry -> Public namespaces -> Publish ->
   Provider); the Terraform Cloud GitHub App must have access to this
   repository. Later tags are ingested automatically; if a version does not
   appear, use Resync there.

## Nelm source reference

Keep a read-only checkout of the pinned Nelm version (`github.com/werf/nelm`
in `go.mod`) outside this repository for looking up exact `pkg/action` option
struct fields when working on `internal/nelmclient`. **Never** add a
`replace github.com/werf/nelm => ../nelm` directive to a *committed*
`go.mod` — it's fine as a temporary, uncommitted local edit while iterating,
but the module must always resolve the pinned stable-channel
`github.com/werf/nelm` version from the public proxy in version control.
