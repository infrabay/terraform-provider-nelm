---
page_title: "Known limitations"
subcategory: ""
description: |-
  Known limitations of nelm_release, and the differences from helm_release that matter when migrating, with their workarounds.
---

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

## Provider configuration

- **A provider configuration that is unknown at plan time only works for new
  releases.** When `host` (or any provider attribute) depends on something
  applied in the same run — typically a GKE cluster created or replaced
  alongside its releases — new `nelm_release`s plan with an Unknown diff and
  are installed at apply, but releases already in state cannot be refreshed
  or planned and the plan fails. Apply the cluster change first with
  `-target`, or keep the cluster and its releases in separate root modules.
  (`helm_release` gets through this case only because it silently drops an
  unreadable release from state.) Terraform's experimental deferred actions
  are honoured when the CLI enables them.

- **`$KUBECONFIG` and an in-cluster service account are never used
  implicitly.** The kubeconfig comes from `kube_config_paths` /
  `KUBE_CONFIG_PATH(S)` (or `~/.kube/config` for an explicit
  `kube_context` attribute — `KUBE_CTX` alone is not enough, as with the
  `helm` provider); a configuration that names no cluster is an error by
  design. Inside a pod, pass `host` / `token` /
  `cluster_ca_certificate` explicitly.

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
  value's byte length.** This holds for `Secret` data and for scrubbed
  `set_sensitive` values alike. Deterministic placeholders are what make
  Secret drift visible without cleartext, but they also let someone with
  plan output/state verify a GUESS of a low-entropy secret offline. Use
  high-entropy secrets (which are immune); a salted scheme is being
  considered for v1.0.

- **Only `set_sensitive` values are scrubbed from non-`Secret` objects.** A
  secret that reaches a chart through `values` or `set` — even from a
  `sensitive = true` variable — appears in cleartext in `resources` (plan
  output and state) wherever the chart renders it outside a `Secret`:
  Terraform never tells a provider which inputs are sensitive.
  `hashicorp/helm`'s `helm_release` prints no rendered manifest by default,
  so this is new exposure for such configurations; pass these values through
  `set_sensitive`. `set_sensitive` values themselves are scrubbed only where
  they are rendered verbatim (or quoted, JSON-escaped or base64-encoded
  whole), only from strings and keys, and only when at least 4 bytes long; a
  hashed, partial or otherwise transformed rendering is not recognized. See
  [Sensitive values in non-`Secret` resources](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#sensitive-values-in-non-secret-resources).

- **State can hold a `set_sensitive` value in cleartext right after an
  import, or after a failed apply that changed one.** Read only knows the
  `set_sensitive` values stored in state: none after `terraform import`
  until the first apply, and the previous ones after a failed apply until
  the next successful one. Self-healing; a `moved` block from `helm_release`
  carries the values over and is not affected.

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

- **Diagnostics scrub `set_sensitive` values as Nelm parses them, not every
  fragment.** Errors and warnings are scrubbed of each value as written, the
  strings Nelm parses out of it and their quoted and base64 forms. A value
  Nelm *fails* to parse can still leak the fragment its error names (e.g.
  `key "word" has no value` for an unescaped comma in `pass,word`). Escape
  commas in sensitive values (`value = "pass\\,word"` in HCL), or use
  `type = "literal"`.

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
  Set generous `timeouts` for very large releases. Upstream in nelm. The
  same goes for a remote chart download cut off by its timeout: Helm's
  download code takes no context, so the abandoned download keeps running
  in the background — for an OCI pull, whose registry client has no HTTP
  timeout, until the registry answers or drops the connection.

- **A killed or crashed provider leaves its 0700 temp root behind.** Each
  provider process removes its `tf-nelm-*` temp root when Terraform shuts
  it down normally, and ordinary error paths clean per-operation
  directories in the same call frame, but a SIGKILL or crash mid-operation
  can leave values/plan-artifact files (0600, inside the 0700 root) until
  the OS temp cleaner runs.

- **A nelm global (`loader.NoChartLockWarning`) is written by ReleaseGet
  without synchronization**, so highly-parallel `Read` + chart-loading is a
  data race whose only visible effect today is nondeterministic suppression
  of a chart-dependency warning. Upstream in nelm.
