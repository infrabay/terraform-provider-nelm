# Known limitations

This is a young (v0.x) provider. The items below are known, mostly narrow,
correctness/UX limitations surfaced by adversarial code review. Each notes the
impact and the intended direction for v1.0. Nine higher-severity findings from
the same review (a provider-crashing panic, a `set_sensitive` value leaking into
error output, state loss on a transient post-install refresh failure, an
unbounded Read, an unsafe storage-driver switch, hooks causing a perpetual diff,
and others) have already been fixed.

## Diff surface (`resources`)

- **Update diffs may show server-defaulted fields.** For an `update`, nelm sets
  the change's desired object to the API server's dry-run result, which already
  carries server-side defaulting (`progressDeadlineSeconds`, `strategy`, pod
  `dnsPolicy`, empty `resources: {}`, …). A freshly *created* resource does not.
  So the first `update` to a resource can show those defaults being added even
  though the chart never set them. It is one-time noise, not a
  never-converging diff: once applied, both the stored value and subsequent live
  reads carry the defaults, so later plans are clean.

- **List projection is positional, not merge-key aware.** The live→desired
  projection pairs list elements by index (`containers[i]`, `env[i]`, `ports[i]`,
  …). Kubernetes strategic-merge pairs them by an identity key (usually `name`).
  If something injects a list element *before* a chart-managed one (e.g. an
  admission webhook prepends a sidecar container), positional projection
  misaligns and can misreport that element or hide drift in the real one.
  Appended injections are handled correctly. A merge-key-aware projection is
  planned for v1.0.

- **Custom resources whose CRD is not yet installed can fail keying.** The
  `resources` map key is resolved through a live RESTMapper. A first-install
  chart that ships both a namespaced CRD and a custom resource of that kind can
  fail to key the CR at plan time (the CRD is not discoverable yet). Workaround:
  install CRDs in a separate apply/release first.

- **A resource whose only change is its `apiVersion`** (e.g. an HPA moving
  `autoscaling/v1` → `v2`) leaves the old versioned map key in state for one
  refresh cycle; the next `Read` rebuilds the map from live refs and it clears.
  Cosmetic, self-healing.

## Release lifecycle

- **A release-only change that renders no manifests produces an empty plan.**
  If you change something that alters the coalesced release config or `NOTES.txt`
  but no rendered resource (nelm would still cut a new revision), and the
  Terraform chart reference and rendered `resources` are unchanged, the plan can
  come out empty and `Update` is not invoked. Rare in practice; a fix depends on
  surfacing nelm's own "release up to date" signal.

- **Import assumes the `secret` storage backend.** `terraform import` seeds
  `release_storage_driver = "secret"`. Importing a `configmap`-backed release
  will report it missing. Set the driver in configuration before importing such
  a release (full driver auto-detection on import is planned).

- **First plan after import is an in-place update.** `chart` is not recoverable
  from release storage, so it is null right after import; any real configuration
  supplies a chart, making the first post-import plan a metadata-only update.
  Inherent to Helm/nelm storage.

## Values precedence

- **Ordering across `set` types is not preserved.** nelm merges the underlying
  `--set` / `--set-string` / `--set-literal` / `--set-json` categories in a
  fixed order, not in the order entries appear in a single `set` list. So two
  entries with the *same name* but different `type` in one list do not honor
  list order (the later category wins, not the later list position). The
  provider does guarantee that `set_sensitive` wins over `set` on a name
  conflict. Avoid same-name-different-type entries in a single list.

## Concurrency / nelm internals

- **A timed-out operation's nelm worker is not joined before cleanup.** nelm
  runs an action in a goroutine and returns when its timeout fires without
  joining the worker; the provider then removes the per-operation temp dir. A
  genuinely stuck dependency (some nelm internals still use a background
  context) could keep the worker alive briefly after cleanup. Set generous
  `timeouts` for very large releases.

- **A nelm global (`loader.NoChartLockWarning`) is written by ReleaseGet
  without synchronization**, so highly-parallel `Read` + chart-loading is a data
  race whose only visible effect today is nondeterministic suppression of a
  chart-dependency warning. Upstream in nelm.
