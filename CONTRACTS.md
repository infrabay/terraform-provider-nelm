# CONTRACTS.md

This file records the seams between the provider's packages: the contracts
a change must not break without updating this document (and the code on both
sides of the seam) in the same change. It is the load-bearing summary of the
provider's architecture; DEVELOPMENT.md covers building and testing.

## Seam 1 — `internal/nelmclient.Client` consumed by `internal/provider`

`internal/nelmclient` is the **single point of contact** with the Nelm
(`github.com/werf/nelm`) Go library. Nothing outside this package may import
`github.com/werf/nelm/pkg/action` or read a Nelm plan artifact (the
`scripts/smoke` fixture capture aside).

`internal/provider` consumes `*nelmclient.Client` only through its exported
methods, declared as two interfaces so unit tests can substitute an offline
fake (both `Configure`s always store a `*nelmclient.Client`): the
`releaseClient` interface in `internal/provider/release_client.go`, held by
`nelm_release`, and the `templateClient` interface in
`internal/provider/template_datasource.go` (`Template`, `NoConnection`,
`IsNamespaced` and `ResetKindCache`), held by the `nelm_template` data
source. `nelm_release` uses:

- `Plan(ctx, ReleaseSpec, timeout) (*PlanResult, error)`
- `Render(ctx, ReleaseSpec, timeout) ([]*unstructured.Unstructured, error)` —
  with `ReleaseSpec.RenderAsFirstInstall` it ignores the release history
  (create plans render exactly what a first install renders)
- `Install(ctx, ReleaseSpec, timeout) error`
- `Uninstall(ctx, name, namespace, storageDriver string, timeout) error`
- `Get(ctx, name, namespace, storageDriver string, timeout) (*ReleaseInfo, error)`
  — `ReleaseInfo.Manifests` carries the stored release's manifests (cleartext,
  like Render's output): the projection template for objects the plan or
  state has no value for
- `History(ctx, name, namespace, storageDriver string, timeout) (*ReleaseHistory, error)`
  — the stored-revision summary behind Create's adoption guards (the
  configured storage backend and, on Create, the other one) and the
  pending-* lock check; ModifyPlan also reads the other backend's on a
  create plan nelm plans as `DeployTypeInitial` (a release still live there
  makes the create's `resources` Unknown, step 6c'; a read that fails other
  than forbidden is an error at the plan phase and the apply-time re-plan
  alike, as on Create, and a forbidden one finds no release)
- `HandOverHelmProviderFieldManagers(ctx, name, namespace, storageDriver string, timeout) ([]string, error)`
  — Create/Update call it right before `Install`, after the History guards
  (a refused Create writes nothing), sharing the operation's timeout budget,
  and NEVER from ModifyPlan: it renames hashicorp/helm's
  `terraform-provider-helm*`/Update managedFields entries on the release's
  live objects to `helm`/Update so nelm's own Helm 3 field-ownership
  hand-over takes over (a real `managedFields` patch, which a plan must not
  add to nelm's own). The returned strings name objects skipped because an
  admission webhook was unavailable (surfaced as a warning).
- `LiveObjects(ctx, refs)` and `IsNamespaced(gvk)` (the `planconv.KeyScoper`)

and `nelm_template` uses:

- `Template(ctx, ReleaseSpec, TemplateOptions, timeout) ([]TemplateObject, error)`
  — `action.ChartRender` like `nelm chart render`: every object of the
  render (hooks and every object of the `crds/` directories included, in
  Nelm's order: `spec.ResourceSpecSortHandler` puts the `crds/` objects
  first, then hooks, then the rest, each group sorted lexicographically by
  kind, group, version, namespace and name — neither Helm's install order
  nor Nelm's deploy order), each with its template path as Nelm reports it
  (`<chart name>/templates/...`), its hook / standalone-CRD flags, the
  object as Nelm leaves it (without a `metadata.namespace` equal to the
  release namespace, see seam 2), and its YAML exactly as
  `nelm chart render` prints it (`TemplateObject.YAML`, `ManifestYAML` of
  the object, cleartext). `StandaloneCRD` means "from a `crds/` directory"
  (`StoreAs == none`) whatever the kind, as Helm treats every file there;
  Nelm's own printout hides only the CustomResourceDefinitions among them.
  With `TemplateOptions.Remote` false (the data source's default) it passes
  Nelm no `KubeConnectionOptions` and `Remote: false`: Nelm builds no kube
  client, uses its empty in-memory release storage, and the render never
  contacts a cluster (`KubeVersion`, `APIVersions` set the capabilities).
  With `Remote` true (`validate = true`) it renders against the cluster
  with Render's connection, for the capabilities and `lookup` only: the
  release storage stays `common.ReleaseStorageDriverMemory` (never
  `ReleaseSpec.StorageDriver`), so the templates always see a first install
  at revision 1 — `helm_template`'s semantics, deterministic across applies,
  and no read access to the release Secrets needed. Neither mode validates
  the objects against the cluster's OpenAPI schemas. The data source
  filters the result itself (`include_crds`, `skip_tests`, `show_only`) and
  never passes Nelm's `ShowOnlyFiles` / `ShowStandaloneCRDs`, which only
  filter Nelm's printout. A local chart directory is loaded with Nelm's
  chart stub type (`noDependencyBuild` sets `LegacyChartType` to
  `helmopts.ChartTypeChartStub`, inside the bounded render), which reads
  the chart's files and never builds a dependency, so `Template` never
  downloads one (`helm_template` without `dependency_update`): a declared
  dependency `charts/` lacks fails with Helm's `CheckDependencies` error,
  wrapped in `ErrChartDependenciesMissing`. The stub type's defaults for a
  missing apiVersion, name or version are never used:
  `requireChartMetadata` first fails such a chart with the validation error
  of Nelm's default type. A chart archive keeps the default type.

plus two package functions: `SetValueStrings(setType, arg)`, the strings
Nelm's own `--set*` parsing (helm strvals) makes of a `set`/`set_sensitive`
argument — what a chart can render, and so what `releaseModel.sensitiveValues`
scrubs (seam 2) — and `StoredSetValueStrings(setType, arg, values)`, the
strings a release's stored values (`ReleaseInfo.Values`) hold at the
positions that argument assigns (`releaseModel.storedSensitiveValues`).

`Render` and `Template` share one private helper, `chartRender` (per-op
directory, values files, registry config, remote-chart fetch, log capture,
`OutputFilePath`); it never checks `clusterErr` itself, so each caller
guards before calling it. It bounds the whole render by the operation's
timeout: `action.ChartRender` has no timeout of its own, and its chart
loading, template engine and (with `Remote`) kube client construction take
no context, so the remote-chart download and `action.ChartRender` each run
in a worker goroutine, and when the timeout fires `chartRender` returns
the context's error at once and abandons the worker, which runs on in the
background until it finishes. Such a worker may only touch the
per-operation directory (removed by the deferred cleanup by then, which
makes its late writes fail; whatever slips in before is removed with the
temp root by `Shutdown`) and the mutex-guarded log capture; anything new
the render path does must stay as harmless when it happens after the call
returned.

An error a Nelm action that renders the chart returns (`Plan`, `Install`,
and `chartRender` for `Render` and `Template`) passes through
`redactManifestErr` (`errors.go`) at the action call, before the log tail
is appended and before it leaves the package: no such error may quote
rendered manifest content, which can hold `Secret` data that
`nelm_release`'s `resources` redacts and that a diagnostic would put in
plan and CI output. The content Nelm's render errors are known to quote
(the whole manifest of an object rendered without a `kind`, which its
decode error quotes, and goccy/go-yaml's source lines around a YAML
syntax error) is withheld; the template path and the reason stay. The
provider's `scrubSensitive` of `set_sensitive` values (seam 2) still runs
on top. A new Nelm action call that renders a chart must wrap its error
the same way.

Two clients carry no Kubernetes connection settings at all, and Configure
hands them to resources and data sources alike:

- `nelmclient.NewUnknownConfigClient(registries, registriesKnown)` while the
  provider configuration is not fully known at plan time
  (`ConfigUnknown()` reports it). `registries` are the provider's OCI
  credentials when that attribute is fully known (`registriesKnown`).
- `nelmclient.NewNoConnectionClient(registries, err)` when the
  configuration names no cluster at all (`resolveKubeconfig`'s
  "No Kubernetes connection configured" case, which is no longer a provider
  `Configure` error). `NoConnection()` returns `err` for it and nil for any
  other client. `nelm_release`'s resource `Configure` turns a non-nil
  `NoConnection()` into the "No Kubernetes connection configured" error, so
  every operation on a release still fails closed (the framework calls
  resource `Configure` before every resource RPC that can reach the client;
  `MoveResourceState` needs none). The data source does not: an offline
  render needs no cluster.

Both set `clusterErr` (`ErrConfigUnknown`, or the no-connection `err`), and
**every cluster-facing method — `Plan`, `Install`, `Uninstall`, `Get`,
`Render`, `ensureKubeFactory` (so `History`, `HandOverHelmProviderFieldManagers`,
`LiveObjects`, `IsNamespaced`) and a `Remote` `Template` — returns
`clusterErr` before calling Nelm**: a zero `Config` must never reach Nelm,
whose defaults load `~/.kube/config`'s current-context. Any new
cluster-facing method must keep that guard (`if c.clusterErr != nil`), not
test a particular constructor's flag. An offline `Template` is the one
method that runs on these clients, because it never builds a kube client;
it refuses (`ErrConfigUnknown`) only for an `oci://` chart reference
(decided after `NormalizeChartRef`, so an `oci://` repository with a bare
chart name counts) when the provider's `registries` were not known either,
rather than pull with Nelm's default `~/.docker/config.json` instead of
the configured credentials; a local chart or a classic repository's still
renders. A new offline method must likewise pass Nelm no
`KubeConnectionOptions` and `Remote: false` (or the action's equivalent).

`*plan.ResourceChange` (from `github.com/werf/nelm/pkg/plan`) passes through
`PlanResult.Changes` **opaquely** — `internal/planconv` consumes it directly
from Nelm's own type; nothing re-derives Nelm's create/update/delete/"blind
apply" classification. Test-only exception: `internal/provider`'s unit tests
may import `github.com/werf/nelm/pkg/plan` (and `pkg/resource/spec`) to
hand-build `PlanResult.Changes` fixtures for the fake `releaseClient`
(release_plan_create_test.go); the provider code itself only passes them
through.

`Plan` reads the plan artifact `action.ReleasePlanInstall` writes with its
own reader (`readPlanArtifact`, `planartifact.go`), never
`plan.ReadPlanArtifact`. Nelm cannot read back an artifact in which the
dry-run apply of any object failed: it writes that error into
`installableResourceInfos[].dryApplyErr`, an `error` it marshals as a JSON
object and cannot unmarshal (issue #8). The error is already in the
object's "blind apply" change (`Reason`), so the reader decodes only
`deployType` and `data.changes` and skips the rest of the artifact
undecoded, unknown fields included. It follows Nelm's format (gzip, scheme
`apiVersion` `v1`, the data as the JSON string `dataRaw`) and refuses an
artifact it cannot read correctly (another scheme, an encrypted artifact,
no `deployType` or `changes` field) instead of planning no changes.
`TestPlanArtifactFormat` pins the field names and types it relies on
against Nelm's `plan.PlanArtifact` / `plan.PlanArtifactData` and must pass
on every Nelm upgrade.

## Seam 2 — `internal/planconv` consumed by both sides of the diff

`internal/planconv` exposes pure functions (no cluster access):

- `Key(ref Ref, releaseNS string, scoper KeyScoper) (string, error)`
- `NormalizeUnstructured(obj, secrets)` — planned side
- `NormalizeLiveAgainst(obj, desired, secrets)` — live side; wraps
  `NormalizeUnstructured` then projects the live object onto the planned
  shape, stripping Kubernetes' server-side defaulting generically
- `BuildPlannedResources(prior, changes, releaseNS, scoper, rendered, secrets)`
  — every non-delete change takes its value from `rendered`, the
  chart render (`BuildRenderedResources(objs, releaseNS, scoper, secrets)`)
- `BuildLiveResources(objs, releaseNS, scoper, desired, secrets)` — with
  `desired` as the projection template
- `ScrubSecrets(obj, secrets)` / `ScrubString(s, secrets, placeholder)` —
  the scrubbing step of the pipeline, and the same span replacement for
  diagnostics (`releaseModel.scrubSensitive`)
- `CompareRenders(a, b)` — what two independent renders disagree on
  (volatile objects, planned Unknown on a reinstall, see ModifyPlan step 6e)
- `NewRenderScoper(renderObjs, scoper)` — the planned side's KeyScoper: the
  real scoper, plus the scope of kinds not served yet from the CRDs in the
  render; `Unresolved()` lists the kinds whose scope had to be guessed (the
  planned map is then not known at plan time)
- `BuiltinScoper{}` — a `KeyScoper` that needs no cluster: a static table
  of the kinds built into the Kubernetes API server (`builtinKindScopes`,
  whatever the version, removed kinds and kinds behind alpha feature gates
  included, since charts still render them); every other kind is a
  `*meta.NoKindMatchError`, what a cluster that does not serve the kind
  answers, so `NewRenderScoper(objs, BuiltinScoper{})` scopes it from the
  CRDs in the render or records it as `Unresolved`. Only
  `nelm_template`'s offline namespace restoration uses it. It must never
  key a `nelm_release` `resources` map: both sides of that diff ask the
  cluster's RESTMapper (the invariant below). Only built-in kinds belong
  in its table; a custom resource's scope is whatever its CRD says on a
  given cluster

`NormalizeUnstructured` strips the release ownership metadata nelm stamps at
install (`meta.helm.sh/release-name`, `meta.helm.sh/release-namespace`,
`app.kubernetes.io/managed-by`), so a render, a create's After and a live
object of the same chart output normalize identically.

`secrets` (`releaseModel.sensitiveValues()`: the `set_sensitive` values in
every form Nelm or a template renders them) are scrubbed from every value
after redaction and cleaning (`ScrubSecrets`, on the decoded tree: string
values and map keys, deterministic `<hidden N sensitive bytes, hash ...>`
placeholders, values shorter than `MinSecretLength` skipped). ModifyPlan and
Create/Update pass the plan's, Read the state's — the same values whenever
the state was written by an apply of that configuration. A failed Update's
live read passes the plan's and the prior state's together, since its
objects can hold either; Read adds the values the release's last revision
stores at the state's `set_sensitive` names (`storedSensitiveValues`), which
after a failed rotation are the new ones that state, keeping the prior
configuration, does not list (after a successful apply they are the
state's own, so both sides still scrub the same strings).

Both `internal/provider/release_plan.go` (ModifyPlan — the **planned** side,
built from `*plan.ResourceChange`) and `internal/provider/release_crud.go`
(Read — the **live** side, built from live cluster GETs) build the same
`resources` map attribute. They MUST do so through the same normalization
pipeline and the same key function.

> **Invariant (bold on purpose): both sides of the diff MUST key through
> `Key` with the same `KeyScoper` implementation (`RenderScoper` answers
> exactly like the wrapped scoper for every kind the cluster serves, and the
> live side never sees an unserved kind), the live side MUST be
> projected onto the planned shape (`NormalizeLiveAgainst`) so server-side
> defaulting is stripped identically, and both sides MUST scrub the same
> `secrets` (a projection template included), or phantom diffs result.** A `KeyScoper`
> mismatch (e.g. one side guessing namespace-scoping instead of asking the
> cached RESTMapper) is the single most likely source of a permanent,
> un-fixable noisy diff in this provider.
> The captured live/planned golden pairs in `internal/planconv/testdata/normalize/`
> (`normalize_golden_test.go`) exist specifically to catch this class of bug.

`nelm_template`'s `manifest`, `manifests` and `crds` are deliberately
outside this pipeline: they are `TemplateObject.YAML` verbatim, cleartext
Secret data and `set_sensitive` values included (helm_template parity, and
not `Sensitive`, so `for_each` over `manifests` works), with one exception.
Nelm removes `metadata.namespace` wherever it equals the release namespace
(`spec.NewResourceSpec`; nothing after `action.ChartRender` can tell
"written as the release namespace" from "written without one"), so the
data source's Read restores it: every selected object (after
`include_crds`, `skip_tests`, `show_only`; hooks included, `StandaloneCRD`
objects and the `crds` output excepted) that has no namespace and whose
kind is namespace-scoped gets `metadata.namespace = <namespace>` on a copy,
re-encoded with `nelmclient.ManifestYAML`. Scope comes from
`NewRenderScoper(render, scoper)` over the whole render (CRDs in `crds/`
and templates): `scoper` is the client's `IsNamespaced` (the cluster's
RESTMapper) after a `Remote` render, and `BuiltinScoper{}` after an offline
one, which must never ask the client (on a client with a cluster it would
contact it), nor call its `ResetKindCache`. The client caches its
RESTMapper for the life of the provider process and shares it with
`nelm_release`, while Nelm builds a fresh kube client for every render, so
Read calls `ResetKindCache` once before scoping a `Remote` render: a kind
whose CRD an earlier resource of the same apply installed is then served
to the scoping as it was to the render. A reset only drops cached
discovery: the next lookup, `nelm_release`'s included, discovers again
and gets the cluster's current answer. A kind still unresolved keeps no
namespace, and Read adds ONE warning listing every such kind; any other
scoper error fails the read. This deliberately differs from
`helm_template`, which returns the template's text and so keeps only a
namespace the template wrote. Only the data source's diagnostics are
scrubbed, with `releaseModel.scrubSensitive` of the `releaseModel` its
inputs translate to (seam 4).

## Seam 3 — `releaseModel` frozen by `release_schema.go`

`internal/provider/release_schema.go` is the schema contract for
`nelm_release`. `internal/provider/release_model.go`'s `releaseModel`
(plus `setModel` / `metadataModel`) mirrors it field-for-field via `tfsdk`
struct tags. ModifyPlan (`release_plan.go`) and the CRUD methods
(`release_crud.go`) code against this pair of files; changing an attribute
name, type, or nesting shape requires updating both files together, since
it invalidates any fixtures/goldens already captured against the old shape.

## Seam 4 — `templateModel` frozen by `template_schema.go`

`internal/provider/template_schema.go` is the schema contract for the
`nelm_template` data source, and `internal/provider/template_model.go`'s
`templateModel` mirrors it field-for-field via `tfsdk` struct tags (change
both together, as for seam 3). Its chart and values inputs (`name`,
`namespace`, `chart`, `repository`, `version`, `values`, `set`,
`set_sensitive`) keep `nelm_release`'s names, shapes and validators:
`templateModel.releaseModel()` copies them into a `releaseModel`, and Read
builds the `ReleaseSpec` with the same `toReleaseSpec` (including the rule
that drops a non-sensitive `set` entry colliding with a `set_sensitive`
one) and scrubs its diagnostics with the same `scrubSensitive`. A change to
how `releaseModel` translates those attributes therefore changes the data
source too, and a new `releaseModel` input that `toReleaseSpec` reads must
get a value (or its documented default) in that adapter.

## Global-state rules

- `nelmclient.Init` (`bootstrap.go`) is the **only** caller of
  `log.SetupLogging` and any `featgate.*.Enable()` / `Disable()` in this
  codebase. It runs its setup exactly once per process (`sync.Once`), never
  per-CRUD-call, and pins EVERY nelm feature gate (remote-charts on, all
  others off) so ambient `NELM_FEAT_*` variables cannot change behavior.
  `planconv`'s redaction must not depend on gate state either. `Init` is
  also the provider-side writer of Nelm's Helm-loader global
  `loader.NoChartLockWarning` (`pkg/helm/pkg/chart/loader`), the only one
  in this codebase: it clears it before any chart load. The loader
  `fmt.Println`s it to the process stdout when it loads a local chart
  directory (`LoadDir`; never an archive, so never a fetched remote chart)
  that declares dependencies (in `Chart.yaml` or `requirements.yaml`) and
  has neither `Chart.lock` nor `requirements.lock`.
  Nelm's own `ReleaseGet` / `ReleaseList` write the same empty string on
  every call, without synchronization (a data race the race detector
  reports, with no visible effect, documented in known-limitations).
  Clearing it changes no render: a dependency missing from `charts/` still
  fails with Nelm's `found in Chart.yaml, but missing in charts/ directory`
  error.
- `main` calls `nelmclient.Shutdown()` after `providerserver.Serve` returns
  (and before any `log.Fatal`), removing the per-process temp root `Init`
  created. No `Client` method may run after it.
- No `SecretKey` / `WERF_SECRET_KEY` anywhere in this codebase (werf secret
  values are out of scope for v1; this also avoids the `os.Setenv` race that
  encrypted plan artifacts would otherwise require). `readPlanArtifact`
  refuses an encrypted artifact. Nelm itself still reads them: no action
  sets `SecretValuesOptions`, so for a chart that carries
  `secret-values.yaml` or a `secret/` directory Nelm looks for a key in
  `WERF_SECRET_KEY`, `.werf_secret_key` in the process working directory
  (Terraform's) and `~/.werf/global_secret_key`, and fails without one.
  That is documented as unsupported (known-limitations), not worked
  around: ignoring the secret values would install a chart without them.
- Every Nelm action call passes its own per-operation `TempDirPath` (a fresh
  0700 subdirectory under `nelmclient.TempRoot()`) and `OutputNoPrint: true`
  discipline. `OutputNoPrint` alone is NOT enough for `action.ChartRender`:
  nelm ignores it there and prints every rendered manifest (Secret data
  included) to the process stdout — a pipe Terraform core logs — unless
  `OutputFilePath` is set, so `ChartRender` always gets an `OutputFilePath`
  inside its per-operation directory (set once, in `chartRender`, for
  `Render` and `Template` alike). Plan artifacts and render output are
  written and deleted in the same function call frame that created them —
  they contain cleartext Secret data and must never outlive the call that
  produced them (a timed-out action's abandoned worker aside, see
  `chartRender` in seam 1). The no-stdout guarantee covers Nelm's manifest
  printout and the `NoChartLockWarning` line, not the dependency build
  below.
- Nelm's chart loader builds a local chart directory's missing
  dependencies itself, outside the provider's control: when its lock
  (`Chart.lock`, overridden by a `requirements.lock` that lists
  dependencies: the loader decodes both into one value) lists a dependency
  from an `https://` or `oci://` repository that `Chart.yaml` (or
  `requirements.yaml`) also declares, and the chart files the loader loaded
  hold no `<name>-<version>.tgz` anywhere under `charts/` (an unpacked
  directory does not match, and an archive `.helmignore` excludes is never
  loaded; the build then fetches every lock entry whose name is missing,
  declared or not),
  `LoadChartDependencies` runs Helm's `downloader.Manager`
  (`pkg/chart` `RenderChart` sets it up as `DepDownloader`, `Out:
  os.Stdout`). It reads the Helm repository configuration of the user
  running Terraform (`HELM_REPOSITORY_CONFIG`, else an XDG-aware
  `repositories.yaml`) and updates every repository in it, downloads over
  the network (`oci://` with `RegistryCredentialsPath`: the provider's
  `registries`, or Docker's default config without them), writes the build
  to `~/.werf/local_cache` (keyed by the lock's content and reused by every
  later load, without network) plus `~/.werf/service` locks and the Helm
  repository cache (`HELM_REPOSITORY_CACHE`), prints Helm's progress to the
  process stdout (no Secret data: chart names and repository URLs), and
  runs under `context.Background()`, so it never stops at an operation's
  timeout: the operation fails then (Nelm's own action goroutine returns
  for `Plan`/`Install`, `chartRender`'s worker is abandoned for
  `Render`), and the build goes on in the background.
  A chart archive (anything `fetchChart` downloads, or a local `.tgz`), a
  chart with neither lock file, and every `Template` render (which loads a
  local chart with the chart stub type, see seam 1) never reach it. This affects
  `Render`/`Plan`/`Install` (`nelm_release`) and is documented
  (known-limitations "Chart versions and CRDs"), not prevented; a pre-check
  that refuses such a chart, or a stdout guard, must cover all three.
- Remote charts never reach nelm as remote references:
  `nelmclient.fetchChart` downloads them into the per-operation directory
  first and nelm gets the archive's absolute path, because nelm's own
  download goes to the shared Helm cache under a `<name>-<version>.tgz` name
  that concurrent operations collide on. `fetchChart` mirrors nelm's
  downloader setup (`pkg/chart` `newChartDownloader`) and must be
  re-checked against it on every nelm upgrade. Both the download and the
  nelm action get the chart reference and repository URL from
  `NormalizeChartRef`, never `ReleaseSpec.Repository` itself: an `oci://`
  repository is folded into the reference and must not reach either as a
  classic repository URL.
