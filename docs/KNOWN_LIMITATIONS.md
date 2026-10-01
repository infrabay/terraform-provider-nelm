# Known limitations

This is a young (v0.x) provider. The items below are known, mostly narrow,
correctness/UX limitations surfaced by five rounds of adversarial code review
(three model families plus two independent CLI-agent passes). Each notes the
impact and the intended direction for v1.0. Everything the reviews rated
higher-severity has been fixed in code. Most notably, the planned side of the
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

## Field ownership

- **Fields added with `kubectl edit` are removed by the next update, and the
  diff does not show it.** With the default `no_remove_manual_changes =
  false`, nelm's plan already moves the `kubectl-edit` field manager's fields
  to its own `helm` manager (a real `managedFields` patch), and the next apply
  that updates the release for any reason removes the ones the chart does not
  render. The `resources` diff never shows them, because the live side is
  projected onto the chart's shape. `helm_release` (Helm 3 three-way merge)
  keeps such fields. Turning the flag on afterwards does not bring them back,
  and the flag change is itself an update. Set `no_remove_manual_changes =
  true` before the first plan if you rely on `kubectl edit` hotfixes; other
  managers (`kubectl patch`/`label`/`annotate`/`apply`) are not affected. A
  plan-time warning for removals the diff cannot show is being considered.

- **The `helm_release` field-manager hand-over runs at apply, not plan.**
  Objects written by hashicorp/helm carry a `terraform-provider-helm_*`
  field manager that nelm does not recognize; the provider renames it to
  `helm` right before each install so nelm's own Helm 3 hand-over prunes what
  the chart no longer renders, and `terraform plan` adds no write of its own.
  The plan already shows those removals (its planned side is the chart
  render). If an admission webhook is unavailable during the hand-over, the
  object is skipped with a warning and a later apply that updates the release
  finishes it, removing fields the chart stopped rendering in between; that
  later diff does not show them. nelm's Helm 3 hand-over also dry-runs the
  previous revision's manifest, so a `helm_release` revision that recorded a
  resource under an API version the cluster no longer serves fails that first
  apply, exactly as it fails a `helm_release` upgrade; clean up the stored
  manifest first (e.g. `helm mapkubeapis`).

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
