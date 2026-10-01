# Contributing

Thanks for your interest in improving `terraform-provider-nelm`.

## Development

See [`DEVELOPMENT.md`](DEVELOPMENT.md) for the full setup. In short:

```sh
make install     # build + install the binary for local dev_overrides use
make test        # gofmt, go vet, go build ./..., go test -race ./...  (no cluster)
make testacc     # acceptance tests against a local cluster (kube context "orbstack" by default)
```

`./gates.sh repo` runs the same checks CI does (plus `golangci-lint` if
installed). Please make sure it passes before opening a pull request.

## Pull requests

- Keep the change focused; add or update tests for behavior changes.
- Run `gofmt`, `go vet`, and `golangci-lint run ./...` — CI enforces all three.
- Unit tests must not require a cluster. Cluster-dependent behavior belongs in
  the acceptance suite (`TF_ACC=1`), which is strictly guarded to a local
  cluster (`NELM_TEST_KUBE_CONTEXT`, `orbstack` by default).
- Update `docs/` when you change the schema or user-visible behavior; the
  provider and resource docs are what the Terraform Registry renders.

## Reporting issues

Please include the provider version, Terraform version, a minimal
configuration, and the relevant `terraform plan`/`apply` output (with secrets
redacted). Known limitations are tracked in
[`docs/KNOWN_LIMITATIONS.md`](docs/KNOWN_LIMITATIONS.md).

## License

By contributing, you agree that your contributions are licensed under the
[Mozilla Public License 2.0](./LICENSE).
