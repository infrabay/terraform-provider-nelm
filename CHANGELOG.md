# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `nelm_template` data source: renders a chart with Nelm's
  `action.ChartRender` (what `nelm chart render` runs) without installing
  it, the counterpart of `hashicorp/helm`'s `helm_template`. It takes
  `nelm_release`'s chart and values inputs (`name`, `namespace`, `chart`,
  `repository`, `version`, `values`, `set`, `set_sensitive`) plus
  `kube_version`, `api_versions`, `include_crds`, `skip_tests`, `show_only`
  and `validate`, and exports `manifest`, `manifests` and `crds` (the
  objects of the charts' `crds/` directories) in the format
  `nelm chart render` prints. The render is offline by default and never
  contacts a cluster; `validate = true` renders against the provider's
  cluster for its capabilities and `lookup` only. Like `helm_template`,
  every render is a first install (`.Release.IsUpgrade` false,
  `.Release.Revision` 1), `validate = true` included, and `validate` does
  not check the objects against the cluster's OpenAPI schemas. Unlike
  `helm_template`, every namespace-scoped object carries a
  `metadata.namespace`, the release namespace where the template wrote
  none (Nelm itself drops a namespace equal to the release namespace), with
  a warning for custom resource kinds whose scope cannot be told offline.
  The outputs are not sensitive, as with `helm_template`: rendered
  `Secret`s and `set_sensitive` values are in cleartext in the state. There
  is no `notes` or `is_upgrade` attribute. Like `helm_template` without
  `dependency_update`, a local chart is rendered only with the dependencies
  vendored in its `charts/` directory: the data source never builds or
  downloads a dependency from its lock files, and fails with `Missing chart
  dependencies` for one `charts/` lacks. See the
  [data source docs](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/data-sources/template.md)
  and
  [Migrating from `helm_template`](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/migrating-from-helm_release.md#migrating-from-helm_template).

### Changed

- A provider configuration that names no cluster is no longer an error by
  itself, so an offline `nelm_template` works with `provider "nelm" {}`.
  Every `nelm_release` of such a configuration still fails, with the same
  `No Kubernetes connection configured` error (now reported for each
  `nelm_release` instance — at refresh for one in state, at validation or
  plan for a new one — instead of once by the provider), and so does a
  `nelm_template` with `validate = true`. The provider still never falls
  back to `~/.kube/config`'s current-context.
- Built on Nelm v1.29.0 (was v1.27.2), Nelm's current stable channel. Its
  new feature gates (`adopt-deckhouse-controller-fields`,
  `case-insensitive-condition-tracking`) stay off, like every Nelm gate
  but remote charts: their `NELM_FEAT_*` variables have no effect. See
  [Nelm feature gates](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/index.md#nelm-feature-gates).

### Fixed

- `timeouts.read` now bounds the chart renders of a `nelm_release` plan, as
  documented: a render that runs past it (a slow template, a dependency
  build, a cluster that does not answer) fails the plan with a timeout
  error instead of holding it until the render finishes, which it then does
  in the background.
- The error of a `nelm_release` plan or apply that fails on a chart's
  rendered output no longer quotes that output, which can hold `Secret`
  data that `resources` keeps redacted: for a template that renders an
  object without a `kind`, it quoted the whole object
  (`Object 'Kind' is missing in '...'`), and for rendered YAML that does
  not parse, up to three lines on either side of the bad line. The error
  still names the template and what is wrong.

### Security

- Built with Go 1.27.3, which distrusts compromised intermediate
  certificates in `crypto/x509` (GO-2026-6809, CVE-2026-111943).

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
