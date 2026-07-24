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
methods:

- `Plan(ctx, ReleaseSpec, timeout) (*PlanResult, error)`
- `Install(ctx, ReleaseSpec, timeout) error`
- `Uninstall(ctx, name, namespace, storageDriver string, timeout) error`
- `Get(ctx, name, namespace, storageDriver string) (*ReleaseInfo, error)`

`*plan.ResourceChange` (from `github.com/werf/nelm/pkg/plan`) passes through
`PlanResult.Changes` **opaquely** — `internal/planconv` consumes it directly
from Nelm's own type; nothing re-derives Nelm's create/update/delete/"blind
apply" classification.

## Seam 2 — `internal/planconv` consumed by both sides of the diff

`internal/planconv` exposes pure functions (no cluster access):

- `Key(ref Ref, releaseNS string, scoper KeyScoper) (string, error)`
- `NormalizeUnstructured(...)` (Phase B, T-planconv) — planned side
- `NormalizeLiveAgainst(obj, desired)` (Phase D) — live side; wraps
  `NormalizeUnstructured` then projects the live object onto the planned
  shape, stripping Kubernetes' server-side defaulting generically
- `BuildPlannedResources(prior, changes, releaseNS, scoper)` (Phase B)
- `BuildLiveResources(objs, releaseNS, scoper, desired)` (Phase B; `desired`
  projection template added in Phase D)

Both `internal/provider/release_plan.go` (ModifyPlan — the **planned** side,
built from `*plan.ResourceChange`) and `internal/provider/release_crud.go`
(Read — the **live** side, built from live cluster GETs) build the same
`resources` map attribute. They MUST do so through the same normalization
pipeline and the same key function.

> **Invariant (bold on purpose): both sides of the diff MUST key through
> `Key` with the same `KeyScoper` implementation, and the live side MUST be
> projected onto the planned shape (`NormalizeLiveAgainst`) so server-side
> defaulting is stripped identically, or phantom diffs result.** A `KeyScoper`
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
