# Developing terraform-provider-nelm

This provider is developed and tested entirely locally: no Terraform Registry
publishing, no CI pipeline, no `terraform init`. Local iteration uses
Terraform's `dev_overrides` mechanism together with a real Nelm Go library
dependency and (for acceptance tests / manual e2e) a real but disposable
Kubernetes cluster — **OrbStack, `kube_context = "orbstack"`, never a remote
context**.

## Prerequisites

- Go (matching the `go` directive in `go.mod`, currently `1.25.8`; toolchain
  1.26.x works fine).
- Terraform 1.15.8 (verified; `terraform init` tolerates dev-overridden
  providers, but we skip `init` anyway — see below).
- helm CLI (used only to author/lint/render `testdata/charts/basic` and, in
  Phase B fixtures, to install a plain-helm release for the import test).
- An OrbStack Kubernetes cluster reachable as kubeconfig context `orbstack`,
  for anything beyond `go build` / `go test` (unit tests never touch a
  cluster).

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
counter-productive here — skip it.** This has been verified against the
locally installed Terraform 1.15.8: `plan`/`apply`/`destroy`/`import` all
work against `examples/basic` with no `.terraform` directory and no lock file
present at all.

1. Create a CLI config file (anywhere; not checked in) pointing at your
   `GOBIN`:

   ```hcl
   # ~/.terraformrc.nelm-dev (example path)
   provider_installation {
     dev_overrides {
       "registry.terraform.io/infrabay/nelm" = "/Users/you/go/bin"
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
make testacc                                      # default: kube context "orbstack"
make testacc NELM_TEST_KUBE_CONTEXT=kind-nelm-acc # e.g. a kind cluster
```

This exports `TF_ACC=1` and `NELM_TEST_KUBE_CONTEXT=<context>`. Test code
additionally hard-fails (`t.Fatal`) unless that kubeconfig context exists and
its `cluster.server` looks like `127.0.0.1`/`localhost` — a deliberate triple
guard (explicit env pin with no default + context existence + explicit
`kube_context = <context>` in every test's provider config) so acceptance
tests can never accidentally run against a real cloud cluster.

CI (`.github/workflows/test.yml`, job `acceptance`) runs the same suite on
every pull request against a [kind](https://kind.sigs.k8s.io/) cluster
(`kind-nelm-acc` context) with terraform 1.15.8 and the real helm CLI for the
out-of-band import fixture.

## Nelm source reference

A read-only checkout of the pinned Nelm version lives outside this repo (see
the orchestrator's plan notes) for looking up exact `pkg/action` option
struct fields when implementing `internal/nelmclient`. **Never** add a
`replace github.com/werf/nelm => ../nelm` directive to a *committed*
`go.mod` — it's fine as a temporary, uncommitted local edit while iterating,
but the module must always resolve the pinned stable-channel `github.com/werf/nelm` version from the
public proxy in version control.
