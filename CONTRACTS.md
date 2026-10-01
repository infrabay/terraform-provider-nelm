# CONTRACTS.md

This file records the seams between packages that later phases (and parallel
tasks within a phase) must not break without updating this document and
getting orchestrator sign-off. See the implementation plan for the full design;
this is the load-bearing summary that ships with the repo.

## Seam 1 — `internal/nelmclient.Client` consumed by `internal/provider`

`internal/nelmclient` is the **single point of contact** with the Nelm
(`github.com/werf/nelm`) Go library. Nothing outside this package may import
`github.com/werf/nelm/pkg/action`.

`internal/provider` consumes `*nelmclient.Client` only through its exported
methods, declared as the `releaseClient` interface in
`internal/provider/release_client.go` (the resource holds that interface so
unit tests can substitute an offline fake; `Configure` always stores a
`*nelmclient.Client`):

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
  pending-* lock check
- `LiveObjects(ctx, refs)` and `IsNamespaced(gvk)` (the `planconv.KeyScoper`)

plus one package function, `SetValueStrings(setType, arg)`: the strings
Nelm's own `--set*` parsing (helm strvals) makes of a `set`/`set_sensitive`
argument — what a chart can render, and so what `releaseModel.sensitiveValues`
scrubs (seam 2).

`*plan.ResourceChange` (from `github.com/werf/nelm/pkg/plan`) passes through
`PlanResult.Changes` **opaquely** — `internal/planconv` consumes it directly
from Nelm's own type; nothing re-derives Nelm's create/update/delete/"blind
apply" classification. Test-only exception: `internal/provider`'s unit tests
may import `github.com/werf/nelm/pkg/plan` (and `pkg/resource/spec`) to
hand-build `PlanResult.Changes` fixtures for the fake `releaseClient`
(release_plan_create_test.go); the provider code itself only passes them
through.

## Seam 2 — `internal/planconv` consumed by both sides of the diff

`internal/planconv` exposes pure functions (no cluster access):

- `Key(ref Ref, releaseNS string, scoper KeyScoper) (string, error)`
- `NormalizeUnstructured(obj, secrets)` (Phase B, T-planconv) — planned side
- `NormalizeLiveAgainst(obj, desired, secrets)` (Phase D) — live side; wraps
  `NormalizeUnstructured` then projects the live object onto the planned
  shape, stripping Kubernetes' server-side defaulting generically
- `BuildPlannedResources(prior, changes, releaseNS, scoper, rendered, secrets)`
  (Phase B) — every non-delete change takes its value from `rendered`, the
  chart render (`BuildRenderedResources(objs, releaseNS, scoper, secrets)`)
- `BuildLiveResources(objs, releaseNS, scoper, desired, secrets)` (Phase B;
  `desired` projection template added in Phase D)
- `ScrubSecrets(obj, secrets)` / `ScrubString(s, secrets, placeholder)` —
  the scrubbing step of the pipeline, and the same span replacement for
  diagnostics (`releaseModel.scrubSensitive`)
- `CompareRenders(a, b)` — what two independent renders disagree on
  (volatile objects, planned Unknown on a reinstall, see ModifyPlan step 6e)
- `NewRenderScoper(renderObjs, scoper)` — the planned side's KeyScoper: the
  real scoper, plus the scope of kinds not served yet from the CRDs in the
  render; `Unresolved()` lists the kinds whose scope had to be guessed (the
  planned map is then not known at plan time)

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
objects can hold either.

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
> The `T-fixtures` golden-pair task (design §7 risk #1) exists specifically
> to catch this class of bug before it reaches Phase B wave 2.

## Seam 3 — `releaseModel` frozen by `release_schema.go`

`internal/provider/release_schema.go` is the frozen Phase A schema contract
for `nelm_release`. `internal/provider/release_model.go`'s `releaseModel`
(plus `setModel` / `metadataModel`) mirrors it field-for-field via `tfsdk`
struct tags. Every later task (`T-resplan`, `T-rescrud`) codes against this
pair of files as given; changing an attribute name, type, or nesting shape
after Phase A requires updating both files together and orchestrator
sign-off, since it invalidates any fixtures/goldens already captured against
the old shape.

## Global-state rules

- `nelmclient.Init` (`bootstrap.go`) is the **only** caller of
  `log.SetupLogging` and any `featgate.*.Enable()` in this codebase. It runs
  its setup exactly once per process (`sync.Once`), never per-CRUD-call.
- No `SecretKey` / `WERF_SECRET_KEY` anywhere in this codebase (werf secret
  values are out of scope for v1; this also avoids the `os.Setenv` race that
  encrypted plan artifacts would otherwise require).
- Every Nelm action call passes its own per-operation `TempDirPath` (a fresh
  0700 subdirectory under `nelmclient.TempRoot()`) and `OutputNoPrint: true`
  discipline. Plan artifacts are read and then deleted in the same function
  call frame that created them — they contain cleartext Secret data and must
  never outlive the call that produced them.
