# Known limitations

This is a young (v0.x) provider. The items below are known, mostly narrow,
correctness/UX limitations surfaced by five rounds of adversarial code review
(three model families plus two independent CLI-agent passes). Each notes the
impact and the intended direction for v1.0. Everything the reviews rated
higher-severity has been fixed in code, except the `auto_rollback`-on-timeout
gap (see "Readiness tracking" below), which is documented until the
provider-side rollback lands. Most notably, the planned side of the
`resources` diff for *updated* resources is now taken from the chart's own
client render (`nelm`'s chart-render machinery) rather than reconstructed
from the server's dry-run merge — after review demonstrated that no heuristic
over (dry-run result, live object, stored state) can reliably separate
chart-managed fields from live ones.

## Diff surface (`resources`)

- **List projection is positional, not merge-key aware.** The live→desired
  projection pairs list elements by index (`containers[i]`, `env[i]`, …).
  Kubernetes strategic-merge pairs them by an identity key (usually `name`).
  If something injects a list element *before* a chart-managed one (e.g. an
  admission webhook prepends a sidecar container), positional projection
  misaligns and can misreport that element or hide drift in the real one.
  Appended injections are handled correctly. A merge-key-aware projection is
  planned for v1.0.

- **A resources map seeded from a full live read** (the first `Read` after
  `terraform import`, or an apply whose plan ran with the cluster unreachable)
  contains live-only fields until each resource's next chart-driven update
  replaces its entry with the rendered desired shape. Until then those fields
  produce state-refresh churn (no spurious plan diffs).

- **A chart-rendered field the API server refuses to persist (dropped via
  `omitempty`/pruning) shows as permanent drift.** The desired side always
  renders the field; the live side never has it. Rare in practice (the field
  is doing nothing anyway); fix requires schema-aware comparison.

- **A resource whose only change is its `apiVersion`** (e.g. an HPA moving
  `autoscaling/v1` → `v2`) leaves the old versioned map key in state for one
  refresh cycle; the next `Read` rebuilds the map from live refs and it
  clears. Cosmetic, self-healing.

- **Secret redaction placeholders embed a truncated unsalted SHA-256 and the
  value's byte length.** Deterministic placeholders are what make Secret
  drift visible without cleartext, but they also let someone with plan
  output/state verify a GUESS of a low-entropy secret offline. Use
  high-entropy secrets (which are immune); a salted scheme is being
  considered for v1.0.

## Release lifecycle

- **A release-only change that renders no manifests produces an empty plan.**
  If you change something that alters the coalesced release config or
  `NOTES.txt` but no rendered resource — while every Terraform attribute
  stays identical — the plan can come out empty and `Update` is not invoked.
  Rare in practice; a fix depends on surfacing nelm's own "release up to
  date" signal. (A *failed or pending* release is NOT affected: it always
  re-plans as an update until deployed. Nor is a `timeouts`-only edit: it
  marks status/revision/metadata unknown like any other update.)

- **Import assumes the `secret` storage backend.** `terraform import` seeds
  `release_storage_driver = "secret"`. Importing a `configmap`-backed release
  will report it missing. Set the driver in configuration before importing
  such a release (driver auto-detection on import is planned).

- **First plan after import is an in-place update.** `chart` is not
  recoverable from release storage, so it is null right after import; any
  real configuration supplies a chart, making the first post-import plan a
  metadata-only update. Inherent to Helm/nelm storage.

- **`werf.io/resource-policy` skip policies are invisible in the diff.**
  Since nelm 1.26, a resource annotated with a `skip-create`, `skip-update`
  or `skip-recreate` policy gets no planned change, so `resources` keeps its
  prior/live entry and a chart change to it is not shown. A `keep` /
  `skip-delete` resource removed from the chart is left in the cluster (as
  with `helm.sh/resource-policy: keep`); its key stays in state until the
  next refresh drops it. An invalid policy value now fails the plan.

- **`werf.io/deploy-dependency-*: state=ready` targets are always
  readiness-tracked** (nelm 1.26.2+), even when unchanged, so a release whose
  dependency target is unhealthy fails its apply even if nothing about that
  target changed. Charts without werf.io annotations are unaffected.

## Readiness tracking and `helm_release` parity

Details and workarounds for all of these are in the resource docs
(`docs/resources/release.md`, "Readiness tracking and `wait`" and
"`auto_rollback` vs `helm_release`'s `atomic`").

- **`auto_rollback` does not roll back when `timeouts` expires.** It only
  acts on failures nelm detects inside the `timeouts.create`/`update`
  budget. Pending/unschedulable pods, init containers waiting on a
  dependency, slow-but-progressing rollouts and long Jobs end only when the
  budget runs out; nelm then aborts the operation and its rollback runs on
  the expired context, so nothing is rolled back (the release stays
  `failed`/`pending-upgrade` with the new revision live). An early-detected
  failure's rollback also only gets the rest of the same budget. Helm's
  `atomic` rolls back after a timeout with a fresh timeout. Planned: after a
  timed-out install with `auto_rollback = true`, roll back to the last
  deployed revision from the provider under its own bounded context.

- **`auto_rollback` does not uninstall a failed first install.** With no
  prior deployed revision the failed release and its resources stay in the
  cluster and the resource is tainted; the next apply replaces it
  (uninstall + fresh install). `atomic` uninstalls instead. An optional
  provider-side uninstall of a failed first install (with nothing
  persisted to state) is being considered.

- **Readiness tracking is stricter than `helm_release`'s `wait = true`.**
  nelm awaits non-hook Jobs (no failure allowed) and custom resources with
  a kubedog status rule (`ExternalSecret`, cert-manager `Certificate`, Argo
  CD `Application`, Flux, Prometheus Operator `Prometheus`/`Alertmanager`,
  Kyverno, …), plus any resource whose `status.phase`/`state`/`status`/
  `health` holds a recognized pending or failed word; Helm treats all of
  those as ready at once. `ExternalSecret` `Ready=False` and a `Degraded`
  Argo `Application` fail the apply immediately, and the generic tracker
  fails a resource after 4 minutes without activity regardless of
  `timeouts`. Conversely, a custom resource that only reports a `Ready`
  condition is treated as ready at once. Per-resource `werf.io/*`
  annotations (`track-termination-mode: NonBlocking`, `fail-mode`,
  `no-activity-timeout`, `failures-allowed-per-replica`) tune it; there is
  no per-release override besides `wait = false` yet.

- **`wait = false` is not exactly `helm_release`'s `wait = false`.** It maps
  to nelm's `NoFinalTracking`, which only drops tracking that no later
  deploy step depends on: pre-install/pre-upgrade hooks, earlier weight
  groups, `state=ready` dependency targets and — unlike Helm — every main
  resource of a chart with a post-install/post-upgrade hook are still
  awaited, while a post-install hook without a `hook-succeeded` delete
  policy is not. Untracked resources cannot fail the apply, so
  `auto_rollback` does not trigger for them.

- **`OnDelete` StatefulSets block the apply until their pods are deleted
  by hand.** Helm skips readiness checks for `updateStrategy: OnDelete`;
  nelm waits until every replica runs the new revision, so a pod-template
  change fails after `timeouts` (and every retry waits again) unless the
  StatefulSet carries `werf.io/track-termination-mode: NonBlocking`.
  `werf.io/fail-mode` does not help.

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
  timeout. The live-object phase of `Read` *is* bounded, and the shared
  kube-client construction is capped (at `kube_request_timeout`, default 30s)
  so a black-holed endpoint cannot wedge the whole process. Upstream in nelm.

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

- **A nelm global (`loader.NoChartLockWarning`) is written by ReleaseGet
  without synchronization**, so highly-parallel `Read` + chart-loading is a
  data race whose only visible effect today is nondeterministic suppression
  of a chart-dependency warning. Upstream in nelm.
