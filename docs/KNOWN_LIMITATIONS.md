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

- **Objects whose render changes on every render** (random or time-based
  template functions: `rollme` annotations, `genCA` certificates, generated
  passwords, deploy timestamps) are detected by rendering the chart twice per
  plan — a second chart render on every plan. Such an object is
  `(known after apply)` whenever the release is reinstalled, so the plan
  does not show which of its fields change, and out-of-band changes to its
  random fields are not drift. Two kinds of templates escape the check: a
  `.Release.Revision`-dependent one renders the next revision against the
  live one on every plan (a perpetual update and rollout), and one whose
  output changes only once a second or slower (`now | unixEpoch`,
  `now | date ...`) can render identically twice within one plan and
  differently at apply, which aborts the apply with "Provider produced
  inconsistent final plan". Use `diff_mode = "none"` for those (no object
  diff and no drift detection for that release, like `helm_release`), or
  make the chart deterministic. See "Non-deterministic charts" in the
  resource docs.

- **Kinds the cluster does not serve at plan time.** When a chart renders
  objects of a kind that is not served yet and whose
  CustomResourceDefinition the chart itself does not contain — typically a
  CRD installed by another release earlier in the same apply (cert-manager
  and a chart of ClusterIssuers) — the whole `resources` map, with
  `status`/`revision`/`metadata`, is known after apply, with a warning.
  Templates gated on `.Capabilities.APIVersions.Has`, or on a `lookup` of
  objects another release creates in the same apply, give no such signal:
  they can still render differently at apply and abort it. Apply the
  providing release first (`-target`) or apply twice.

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
  re-plans as an update until deployed — though the apply refuses to run
  over a pending revision until it is stale, see below. Nor is a
  `timeouts`-only edit: it marks status/revision/metadata unknown like any
  other update.)

- **`create_before_destroy` cannot replace a release under the same name.**
  The new and the old object are the same Helm release — across a
  `release_storage_driver` change too — so the create half of such a
  replacement is refused (`already exists`, or `already exists in the
  <driver> storage backend` for a driver change): the apply fails and the
  release stays installed. Remove `create_before_destroy` — Terraform also
  enables it implicitly when a dependent resource has it (`+/-` in the plan
  instead of `-/+`). Only the apply refuses: the plan does not warn for a
  driver change or a `-replace`. With `adopt_existing = true` the
  same-backend refusal is off and such a replacement would uninstall the
  release, so set that flag only for the apply that adopts a release. The
  driver-change check reads the other backend's Secrets or ConfigMaps; if
  the provider's credentials may not list them, Create only warns, and a
  `create_before_destroy` driver change would again install into the new
  backend and then uninstall the release from the old one.

- **The pending-release lock is a fixed-age heuristic.** The apply refuses
  to install over a `pending-*` revision younger than the operation timeout
  (at least 15 minutes, not configurable) and takes an older one over. The
  age comes from the timestamp the writer (helm, nelm) stored, so large
  clock skew between machines shifts it; an operation that starts between
  the provider's history read and nelm's install is not detected; destroy
  does not check the lock (like `helm uninstall`). See "Pending releases" in
  the resource docs for manual recovery.

- **Replacing a release whose chart `lookup`s live objects can abort.** A
  replacement's create is planned while the old release still exists and
  re-planned after its destroy removed it; a template whose output depends
  on `lookup` (a `lookup`-guarded generated password included) renders
  differently in the two, and Terraform aborts with "Provider produced
  inconsistent final plan" after the uninstall ran; the next apply installs
  the release. To replace such a release without the failed apply, do it in
  two steps: `terraform destroy -target=...`, then `terraform apply`.

- **A replacement's conflicts with live objects are only warnings at plan
  time.** The create half of a replacement (a `name`/`namespace`/
  `release_storage_driver` change, a tainted resource) is planned while the
  old release still owns its objects, so Nelm's ownership and
  immutable-field checks against live objects only warn there and run for
  real after the destroy. A conflict with an object the old release does
  NOT own (another release's, or one created outside Helm) therefore fails
  the apply after the old release was uninstalled. Read the objects the
  `re-checked at apply` warning names before applying. Telling the two
  apart needs owner information Nelm's error does not carry in structured
  form.

- **`-replace` cannot get past an immutable-field change.** Terraform plans
  `-replace` as an update of the same release first, and Nelm's
  immutable-field check fails that plan. Use `terraform taint` (planned as
  a plain create) or `terraform destroy -target=...` then `terraform apply`.

- **A create plan lists the chart's `crds/` CRDs even with
  `no_install_crds = true`.** The planned `resources` of a create is the
  chart's render, which includes `crds/`; the first refresh after the apply
  drops them again. Cosmetic.

- **Import assumes the `secret` storage backend.** `terraform import` seeds
  `release_storage_driver = "secret"`. Importing a `configmap`-backed release
  will report it missing. Set the driver in configuration before importing
  such a release (driver auto-detection on import is planned).

- **First plan after import is an in-place update.** `chart` is not
  recoverable from release storage, so it is null right after import; any
  real configuration supplies a chart, making the first post-import plan an
  update whose apply runs `nelm install` for real: a new revision, and the
  chart's upgrade hooks (migration Jobs, webhook certificate patch Jobs) run
  as on any `helm upgrade`. Inherent to Helm/nelm storage.

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
