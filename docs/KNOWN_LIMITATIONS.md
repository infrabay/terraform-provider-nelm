# Known limitations

This is a young (v0.x) provider. The items below are known, mostly narrow,
correctness/UX limitations surfaced by three rounds of adversarial code review
(two model families, plus an independent CLI-agent pass). Each notes the impact
and the intended direction for v1.0. Everything the reviews rated
higher-severity has been fixed in code — including a provider-crashing panic,
several secret-leak paths (error diagnostics, the
`last-applied-configuration` annotation, blind-apply warnings), state loss on
transient refresh failures, a failed release becoming permanently
un-retryable, live-mutable fields (e.g. HPA-owned `replicas`) breaking saved
plans, a `repository`-bypassing chart-reference hijack, silently-ignored
inline connection credentials, and hooks causing a perpetual diff.

## Diff surface (`resources`)

- **List projection is positional, not merge-key aware.** Both live→desired
  projection and the update-change three-way projection pair list elements by
  index (`containers[i]`, `env[i]`, …). Kubernetes strategic-merge pairs them
  by an identity key (usually `name`). If something injects a list element
  *before* a chart-managed one (e.g. an admission webhook prepends a sidecar
  container), positional projection misaligns and can misreport that element
  or hide drift in the real one. Appended injections are handled correctly. A
  merge-key-aware projection is planned for v1.0.

- **Custom resources whose CRD is not yet installed can fail keying at plan
  time.** The `resources` map key is resolved through a live RESTMapper. A
  first-install chart that ships both a CRD and a custom resource of that kind
  can fail to key the CR during `terraform plan` (the CRD is not discoverable
  yet). Workaround: install CRDs in a separate apply/release first. (The
  reverse case — a CRD *removed* out-of-band — is handled: the orphaned custom
  resource reads as absent and surfaces as a re-create diff.)

- **A chart-rendered field the API server refuses to persist (dropped via
  `omitempty`/pruning) shows as permanent drift.** The desired side always
  renders the field; the live side never has it. Rare in practice (the field
  is doing nothing anyway); fix requires schema-aware comparison.

- **A resource whose only change is its `apiVersion`** (e.g. an HPA moving
  `autoscaling/v1` → `v2`) leaves the old versioned map key in state for one
  refresh cycle; the next `Read` rebuilds the map from live refs and it
  clears. Cosmetic, self-healing.

## Release lifecycle

- **A release-only change that renders no manifests produces an empty plan.**
  If you change something that alters the coalesced release config or
  `NOTES.txt` but no rendered resource — while every Terraform attribute
  (chart path, values, flags) stays identical — the plan can come out empty
  and `Update` is not invoked. Rare in practice; a fix depends on surfacing
  nelm's own "release up to date" signal. (A *failed or pending* release is
  NOT affected: it now always re-plans as an update until deployed.)

- **Import assumes the `secret` storage backend.** `terraform import` seeds
  `release_storage_driver = "secret"`. Importing a `configmap`-backed release
  will report it missing. Set the driver in configuration before importing
  such a release (driver auto-detection on import is planned).

- **First plan after import is an in-place update.** `chart` is not
  recoverable from release storage, so it is null right after import; any
  real configuration supplies a chart, making the first post-import plan a
  metadata-only update. Inherent to Helm/nelm storage.

## Values precedence

- **Ordering across `set` types is not preserved.** nelm merges the underlying
  `--set` / `--set-string` / `--set-literal` / `--set-json` categories in a
  fixed order, not in the order entries appear in a single `set` list. So two
  entries with the *same name* but different `type` in one list do not honor
  list order (the later category wins, not the later list position). The
  provider does guarantee that `set_sensitive` wins over `set` on a name
  conflict. Avoid same-name-different-type entries in a single list.

- **Sensitive-value scrubbing is whole-value.** Errors that echo a
  *transformed fragment* of a `set_sensitive` value (e.g. helm's strvals
  splitting on an unescaped comma inside the value) may leak that fragment
  into a diagnostic. Escape commas in sensitive `set` values, or prefer
  `values` + a Kubernetes `Secret`.

## Timeouts / nelm internals

- **`timeouts.read` cannot bound everything inside `ReleaseGet`.** Several
  nelm-internal calls use their own background contexts, so a half-open API
  server can still stall the release-storage read beyond the configured read
  timeout (the live-object phase of `Read` *is* bounded). Upstream in nelm.

- **A timed-out operation's nelm worker is not joined before cleanup.** nelm
  runs an action in a goroutine and returns when its timeout fires without
  joining the worker; the provider then removes the per-operation temp dir. A
  genuinely stuck worker could briefly outlive cleanup (the provider's log
  capture buffer is mutex-guarded, so this is not a data race on our side).
  Set generous `timeouts` for very large releases. Upstream in nelm.

- **The per-process 0700 temp root survives abnormal termination.** Ordinary
  error paths clean per-operation directories in the same call frame, but a
  SIGKILL mid-operation can leave values/plan-artifact files (0600, inside a
  0700 root) until the OS temp cleaner runs.

- **First kube-factory construction serializes concurrent reads.** The cached
  factory is built under a mutex whose critical section includes a
  connectivity check; parallel refreshes of many resources briefly queue
  behind the first one on a slow cluster. Performance-only.

- **A nelm global (`loader.NoChartLockWarning`) is written by ReleaseGet
  without synchronization**, so highly-parallel `Read` + chart-loading is a
  data race whose only visible effect today is nondeterministic suppression
  of a chart-dependency warning. Upstream in nelm.
