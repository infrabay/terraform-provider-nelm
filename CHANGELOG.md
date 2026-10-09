# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/infrabay/terraform-provider-nelm/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/infrabay/terraform-provider-nelm/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/infrabay/terraform-provider-nelm/releases/tag/v0.1.0
