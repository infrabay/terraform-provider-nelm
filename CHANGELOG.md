# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.2] - 2026-10-15

### Fixed

- A refresh of a `nelm_release` no longer fails with
  `Failed to read live resources`, nor an apply of an existing release with
  `nelm_release: helm_release field-manager hand-over failed`, when the API
  server fails a single read of one of the release's objects transiently,
  for example GKE's
  `an error on the server ("Internal Server Error: ...: the server is currently unable to handle the request")`
  when its IAM authorizer is briefly unavailable. A read that the API server
  answers with an internal error (a `500`, or any other 5xx without a
  Kubernetes `Status` body), `503`, its own timeout (`504`) or
  `429` is now retried up to three more times, 0.5, 1 and 2 seconds apart,
  on top of the client's own retries of a response that carries
  `Retry-After`, within `timeouts.read` on refresh and the apply's
  `create`/`update` timeout for the hand-over. A read cut off by
  `kube_request_timeout` or by those timeouts is not retried
  ([#17](https://github.com/infrabay/terraform-provider-nelm/issues/17)).

## [0.1.1] - 2026-10-09

### Fixed

- `terraform plan` of an existing `nelm_release` no longer fails with
  `read plan artifact: decode artifact data json: json: cannot unmarshal object into Go struct field PlanArtifactData.installableResourceInfos.<n>.dryApplyErr of type error`
  when Nelm's dry-run server-side apply of one of the release's objects
  returns an error, for example an RBAC `forbidden` for a plan identity that
  may read but not patch `clusterroles`. The plan artifact is now read
  regardless, and each object that Nelm plans as a blind apply (a non-hook
  resource whose dry run was refused) is reported with a
  `nelm_release: blind apply for <key>` warning that carries the error.
  Dry-run errors that Nelm itself treats as fatal, such as an immutable-field
  change that would need a recreate, still fail the plan with Nelm's error. See
  [Known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md#provider-configuration-and-cluster-access)
  ([#8](https://github.com/infrabay/terraform-provider-nelm/issues/8)).

### Security

- Built with Go 1.27.2, which fixes vulnerabilities in `crypto/tls`,
  `net/http`, `net/textproto`, `os`, `html/template` and the `go` command
  (GO-2026-6603, GO-2026-6605, GO-2026-6607, GO-2026-6608, GO-2026-6610,
  GO-2026-6611, GO-2026-6612, GO-2026-6613, GO-2026-6617).
- `golang.org/x/net` v0.60.0, with the HTTP/2 fixes for the same issues in
  `x/net/http2`, which the Kubernetes client and gRPC use (GO-2026-6603,
  GO-2026-6610, GO-2026-6611, GO-2026-6612, GO-2026-6617).

## [0.1.0] - 2026-10-02

### Added

- Initial release.

[Unreleased]: https://github.com/infrabay/terraform-provider-nelm/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/infrabay/terraform-provider-nelm/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/infrabay/terraform-provider-nelm/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/infrabay/terraform-provider-nelm/releases/tag/v0.1.0
