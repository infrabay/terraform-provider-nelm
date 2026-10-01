# scripts/smoke — live fixture-capture harness

Everything under `scripts/smoke/` captures fixtures **live**, against the
local `orbstack` Kubernetes cluster, using the real `nelm` Go library +
CLI + the local `helm` v4.2.3 CLI. Nothing under
`internal/planconv/testdata/` or `internal/nelmclient/testdata/` is
hand-written; every file there was produced by running one of these
programs and is reproducible by re-running the exact command recorded
below.

## Cluster safety (read this first)

A developer's kubectl current-context may well point at a remote or
production cluster. Nothing here ever touches it:

- `guard.sh` asserts the kubeconfig context named **`orbstack`** (never
  current-context) resolves to a `https://127.0.0.1*`/`https://localhost*`
  API server, and hard-exits otherwise. Source or run it standalone before
  anything else:
  ```
  source scripts/smoke/guard.sh
  ```
- `smokelib.MustGuardOrbstack` (`scripts/smoke/smokelib/guard.go`) is the
  Go-side equivalent: it loads the `orbstack` context via nelm's own
  `kube.NewKubeConfig` loader and hard-`os.Exit(1)`s unless the resolved
  API server host is local. **Every** `//go:build smoke` program in this
  directory calls it as the very first line of `main()`, before doing
  anything else that could reach a cluster.
- Every `kubectl`/`helm`/`nelm` CLI invocation goes through
  `smokelib.RunKubectl`/`RunHelm`/`RunNelm`, which unconditionally prepend
  `--context orbstack` / `--kube-context orbstack` — no caller can omit it,
  and current-context is never consulted anywhere in this package.
- Every namespace created is named `tfnelm-fix-<tag>-<random hex>` and is
  deleted (`helm`/`nelm uninstall` + `kubectl delete ns`) by the same
  program that created it, in a `defer`red cleanup so it runs even on a
  panic/assertion failure.

All captures in this document were run against `orbstack`
(`https://127.0.0.1:26443`, Kubernetes v1.34.8+orb1) on 2026-07-16
(UTC timestamps below are the exact `PlanArtifact.timestamp` / capture
time recorded by each fixture). No other kube-context was ever contacted.

## Provenance note: why a CLI subprocess, not `pkg/action` in-process

At capture time, this module's `go.sum` was missing entries for three
transitive dependencies of `github.com/werf/nelm/pkg/action`
(`github.com/alecthomas/chroma/v2`, `github.com/dustin/go-humanize`,
`github.com/jedib0t/go-pretty/v6` — all pulled in by `pkg/action`'s
CLI-output-formatting files, e.g. `release_list.go`, `chart_ts_build.go`,
`common.go`, none of which are on any code path the harness calls). This
made `github.com/werf/nelm/pkg/action` (the package containing
`ReleasePlanInstall`, `ReleaseInstall`, `ReleaseGet`, `ReleaseUninstall`)
fail to compile in this module:

```
missing go.sum entry for module providing package github.com/alecthomas/chroma/v2 (imported by github.com/werf/nelm/pkg/action)
```

The capture was kept independent of `go.mod`/`go.sum` changes. The gap was
narrow: `github.com/werf/nelm/pkg/plan`, `pkg/resource`,
`pkg/resource/spec`, `pkg/kube`, and `github.com/wI2L/jsondiff` all
compiled cleanly in this module — **only** `pkg/action` was blocked.

**Workaround used (does not touch this module's go.mod/go.sum):** built a
`nelm` CLI binary directly from the exact pinned commit
(`github.com/werf/nelm v1.26.2`, a local read-only checkout whose
`git describe` is
`v1.26.2-2-gda9a86a`) via `go build ./cmd/nelm` **inside nelm's own,
separately-`go.sum`'d module** (read-only; `git status` there remained
clean — nothing in that checkout was modified, only compiled to a binary
elsewhere). That CLI is a thin wrapper around the exact same
`action.ReleasePlanInstall`/`ReleaseInstall`/`ReleaseGet`/`ReleaseUninstall`
functions the harness would otherwise call in-process, so the
`PlanArtifact`/`ReleaseGetResultV1` JSON it produces is byte-for-byte what
those functions would have produced. Every mutating/planning CLI call in
this package goes through `smokelib.RunNelm` (`scripts/smoke/smokelib/exec.go`,
which documents this in full); **reading/decoding/analyzing** the results
(`plan.ReadPlanArtifact`, `spec.CleanUnstruct`, `resource.GetSensitiveInfo`/
`RedactSensitiveData`, `pkg/kube` live GETs) all run in-process through the
real Go library — only the mutating actions are worked around.

`scripts/smoke/smokelib.NelmBin()` resolves the binary via `$SMOKE_NELM_BIN`
(falls back to `nelm` on `PATH`, i.e. a separately-installed nelm CLI, if
the exact-pinned binary isn't provided). All captures below were run with:

```
export SMOKE_NELM_BIN=/path/to/nelm-v1.26.2   # built as described above
```

The provider itself (`internal/nelmclient/actions.go`) calls the same
`pkg/action` functions in-process; the `go.sum` gap above only affected
the module at capture time.

## Harness layout

- `guard.sh` — bash orbstack assertion (see above).
- `smokelib/` (`package smokelib`, build tag `smoke`) — shared helper
  imported by every capture program:
  - `guard.go` — `MustGuardOrbstack`, `ConnectionOptions`.
  - `bootstrap.go` — `InitLogging`, `MustTempDir`, `RandNamespace`.
  - `exec.go` — `RunNelm`/`RunKubectl`/`RunHelm` (subprocess runners,
    always pinned to `orbstack`, stdout/stderr captured separately so
    JSON output callers parse never get contaminated by warnings/logs).
  - `artifact.go` — `ReadArtifact`/`Decode`/`MarshalIndent` (plan artifact
    gzip-read + fixture-friendly nested JSON).
  - `fixtures.go` — `WriteFile`/`CopyFile`.
  - `kubeclient.go` — `MustClientFactory`/`GetLive` (live dynamic-client
    GETs via nelm's own `pkg/kube`, cached RESTMapper).
  - `paths.go` — `RepoRoot`/`BasicChartPath`/`FixtureDir`/
    `NelmclientFixtureDir` (all absolute, computed via `runtime.Caller`).
- `lifecycle/main.go` — lifecycle plan fixtures.
- `secrets/main.go` — secret/redaction fixtures.
- `helmv4probe/main.go` — import (helm v4 release storage) and
  managedFields fixtures.
- `normalize/main.go` — normalization golden pair (`STRIP_LIST.md`).
- `errorshapes/main.go` — error-shape fixtures for
  `internal/nelmclient/testdata/errors/`, used by `internal/nelmclient`'s
  error-classification unit tests. Uses a SYNTHETIC/throwaway kubeconfig
  for the unreachable-cluster case (never touches orbstack or any real
  context for that one); the bad-chart-ref and remote-chart-without-featgate cases
  need a reachable cluster to get past nelm's connectivity check, so those
  two run against guarded `orbstack` in disposable `tfnelm-fix-*`
  namespaces that are never actually created (chart loading fails first).

Every program is `//go:build smoke`, so `go build ./...` / `go vet ./...`
without `-tags smoke` skip this whole directory (verified: a directory
whose files are ALL excluded by build constraints contributes zero
packages to `./...` — no error). Run any of them with:

```
go run -tags smoke ./scripts/smoke/<lifecycle|secrets|helmv4probe|normalize> [args...]
```

`lifecycle` and `normalize` must be run in that order (normalize consumes
lifecycle's saved artifact + reads live objects from the namespace
lifecycle deliberately leaves alive) — see each file's doc comment.

## Fixture provenance

nelm Go library: `github.com/werf/nelm v1.26.2` (per `go.mod`).
nelm CLI binary used for all mutating/planning calls: built from local
checkout `git describe` = `v1.26.2-2-gda9a86a` (see provenance note above).
helm CLI: `v4.2.3+g43e8b7f`. Cluster: `orbstack`
(`https://127.0.0.1:26443`, Kubernetes v1.34.8+orb1). Capture date:
2026-07-16 (UTC).

### Lifecycle plan fixtures (`internal/planconv/testdata/lifecycle/`)

Captured by `go run -tags smoke ./scripts/smoke/lifecycle`
(namespace `tfnelm-fix-lifecycle-74a6c3`, release `lifecycle`, cleaned up
after `normalize` ran against it). Artifact timestamp `2026-07-16T23:10:32Z`
onward.

| file | what it is | command that produced it |
|---|---|---|
| `01_first_install.artifact.json.gz` / `.decoded.json` | first-install plan, fresh namespace, release never existed | `nelm release plan install --kube-context orbstack -n tfnelm-fix-lifecycle-74a6c3 -r lifecycle --save-plan <path> --no-final-tracking testdata/charts/basic` |
| `02_no_change.artifact.json.gz` / `.decoded.json` | plan captured immediately after `nelm release install` of the same chart/values | same plan command, run again after `nelm release install --kube-context orbstack -n <ns> -r lifecycle --no-show-progress testdata/charts/basic` |
| `03_drift.artifact.json.gz` / `.decoded.json` | plan captured after `kubectl --context orbstack -n <ns> patch deployment lifecycle-basic --type merge -p '{"spec":{"replicas":3}}'` | same plan command, run after the patch |
| `04_delete.artifact.json.gz` / `.decoded.json` | plan captured with `--set configMap.enabled=false` (testdata/charts/basic's delete-fixture hook, see chart's `values.yaml`) | same plan command + `--set configMap.enabled=false` |

**Results (all 4 assertions passed, 0 failures):**
- (a) all 6 resources (ClusterRoleBinding, ConfigMap, ClusterRole, Secret,
  Deployment, Service) came back `Type: "create"`, `Before: null`.
- (b) **`Changes` was empty (JSON `null`, i.e. Go `len(nil) == 0`) on the
  no-change plan** — confirms the provider's prior-map merge
  assumption for an up-to-date release. Note for planconv:
  `data.changes` can be JSON `null`, not just `[]`; `BuildPlannedResources`
  ranging over a nil slice is a no-op in Go, so no special-casing is
  needed, but golden tests should cover the `null` shape explicitly.
- (c) the drift plan contained exactly one `update` change, for the
  Deployment, with `Before.spec.replicas: 3` / `After.spec.replicas: 1`
  (Before/After both populated, as expected).
- (d) the delete plan contained a `delete` change for the ConfigMap (plus
  an incidental Deployment `update`, unrelated to the values change —
  worth planconv/resplan double-checking why an unrelated resource shows
  up as changed here; not investigated further here).

### Secret/redaction fixtures (`internal/planconv/testdata/secrets/`)

Captured by `go run -tags smoke ./scripts/smoke/secrets` (plan-only,
`tfnelm-fix-secrets-a52104`/`secrets` — namespace was never created, so no
cleanup was needed). Command:

```
nelm release plan install --kube-context orbstack -n tfnelm-fix-secrets-a52104 -r secrets \
  --save-plan <path> --no-final-tracking \
  --set secret.password=s3cr3t-fake-9f2c --set configMap.sensitivePathsAnnotation=true \
  testdata/charts/basic
```

(`s3cr3t-fake-9f2c` is an obviously-fake value, never a real secret;
`configMap.sensitivePathsAnnotation` is testdata/charts/basic's fixture-capture
hook that adds `werf.io/sensitive-paths: "data.message"` to the ConfigMap.)

| file | what it is |
|---|---|
| `secret_plan.artifact.json.gz` / `.decoded.json` | the plan artifact (raw + decoded) |
| `secret_after.raw.json` | the Secret's plan-After object, cleartext (base64 `data.password`) |
| `configmap_after.raw.json` | the annotated ConfigMap's plan-After object, cleartext |
| `secret_redacted.json` | `resource.RedactSensitiveData(secret, ["$$HIDE_ALL$$"])` output (nelm's default V1/HideAll Secret behavior) |
| `configmap_redacted.json` | `resource.RedactSensitiveData(configmap, ["data.message"])` output (path-specific) |
| `NOTES.md` | full evidence write-up, incl. the exact JSON-pointer locations of the fake password in cleartext |

**Results:** the fake password appears in cleartext in **four** locations
inside the decoded artifact (`/data/plan/operations/*/config/release/config/secret/password`,
`/data/release/config/secret/password`,
`/data/releaseInfos/0/release/config/secret/password` — all the raw Helm
values map, stored verbatim) plus base64-encoded (not encrypted — trivially
reversible) in the rendered Secret's `data.password` field. This is exactly
what justifies CONTRACTS.md's plan-artifact temp-file/delete-in-same-frame
policy. `resource.GetSensitiveInfo` confirms: Secret defaults to
`IsSensitive=true SensitivePaths=["$$HIDE_ALL$$"]` (nelm's V1 default,
global `FeatGateFieldSensitive` off per CONTRACTS.md); the
`werf.io/sensitive-paths`-annotated ConfigMap gets
`IsSensitive=true SensitivePaths=["data.message"]` — confirming
annotation-driven path redaction works on any kind, not just Secret.

### Import fixtures (`internal/planconv/testdata/helmv4import/`)

Captured by `go run -tags smoke ./scripts/smoke/helmv4probe`
(`tfnelm-fix-helmv4-b38442`/`helmv4`, cleaned up at program exit via
`defer`). Commands:

```
helm --kube-context orbstack install helmv4 testdata/charts/basic -n tfnelm-fix-helmv4-b38442 --create-namespace
kubectl --context orbstack -n tfnelm-fix-helmv4-b38442 get secret -l owner=helm -o name
kubectl --context orbstack -n tfnelm-fix-helmv4-b38442 get secret/sh.helm.release.v1.helmv4.v1 -o jsonpath='{.type}'
nelm release get --kube-context orbstack -n tfnelm-fix-helmv4-b38442 -r helmv4 --print-values --output-format json
```

| file | what it is |
|---|---|
| `release_secret_evidence.txt` | helm version + release-storage secret name/type/labels (data payload deliberately not captured — it's a base64+gzip blob of the manifest, not needed for this check) |
| `release_get.json` | full `nelm release get --print-values --output-format json` result against the helm-v4-installed release |
| `failed_release_get.json` / `failed_release_NOTES.md` | best-effort: forced a failed install (bad image, `--wait --timeout 10s`) then ran `nelm release get` against it |

**Can nelm read and import a helm v4 release: YES** — helm v4.2.3 writes the identical
`sh.helm.release.v1.<name>.v1` / type `helm.sh/release.v1` Secret format
(confirmed: `secret/sh.helm.release.v1.helmv4.v1`, type
`helm.sh/release.v1`), and `nelm release get` reads it back with **zero
conversion**: chart name `basic`, version `0.1.0`, all 6 resources
populated, values populated (6 top-level keys), release identity
round-trips exactly. All 5 assertions passed.

**Best-effort failed-release capture:** the forced-failure helm install DID
leave a stored release behind (`status: "failed"`) that `nelm release get`
reads successfully (see `failed_release_get.json`) — relevant to Create's
partial-failure handling: a failed first install is NOT
invisible to `ReleaseGet` in this scenario, at least when the failure
happens after resources were already applied (the capture used
`--wait --timeout 10s` against an unpullable image, so the Deployment
object existed and was recorded before the wait timed out).

### managedFields probe (`internal/planconv/testdata/helmv4import/managedfields_*`)

Same `helmv4probe` run as the import fixtures (same namespace/release). Commands:

```
kubectl --context orbstack -n <ns> get deployment helmv4-basic --show-managed-fields -o yaml   # before
nelm release plan install --kube-context orbstack -n <ns> -r helmv4 --save-plan <p1> --no-final-tracking testdata/charts/basic
kubectl --context orbstack -n <ns> get deployment helmv4-basic --show-managed-fields -o yaml   # after
nelm release plan install --kube-context orbstack -n <ns> -r helmv4 --save-plan <p2> --no-final-tracking testdata/charts/basic
kubectl --context orbstack -n <ns> get deployment helmv4-basic --show-managed-fields -o yaml   # after2
```

**Result: managedFields did NOT change** after either plan
(`before` == `after` == `after2`, byte-identical `managedFields:` blocks).
This **corrects** the working assumption ("first plan against
helm-created resources MergePatches managedFields") for the common case. Root cause, read directly from the local nelm
checkout (`v1.26.2-2-gda9a86a`):

- `pkg/common/common.go:109` — `DefaultFieldManager = "helm"`: nelm
  deliberately uses the SAME field-manager name Helm itself uses.
- `pkg/plan/resource_info.go`'s `fixManagedFields()` only mutates anything
  when it finds (a) a LEGACY `Manager=="helm" && Operation=="Update"` entry
  needing migration to the modern `Apply` style, (b) a ServiceAccount
  needing a secrets-field fix, or (c) a manager literally named
  `"kubectl-edit"` or prefixed `"werf"` (`common.OldFieldManagerPrefix`).
  A resource freshly created by helm v4's server-side-apply has only
  `{manager: helm, operation: Apply}` (already recognized as nelm's own)
  plus whatever the controller itself writes (e.g. `k3s`/`Update` on the
  `status` subresource, which is skipped — its `Subresource` differs) —
  none of the three trigger conditions apply, so nothing is patched.

See `managedfields_NOTES.md` for the full write-up. This is genuinely
good news for importing plain-helm releases: adopting a modern helm v3/v4
release does NOT unexpectedly rewrite managedFields on a plain
`terraform plan`. The assumption likely still applies to resources carrying
a truly legacy field manager (pre-server-side-apply werf/helm
client-side-apply, or a manually `kubectl edit`-touched resource) — this
capture did not reproduce that narrower scenario live (flagged as a gap,
not fabricated).

### Normalization golden pair (`internal/planconv/testdata/normalize/`, `internal/planconv/testdata/STRIP_LIST.md`)

Captured by `go run -tags smoke ./scripts/smoke/normalize tfnelm-fix-lifecycle-74a6c3 lifecycle`,
run immediately after `lifecycle` (reads
`internal/planconv/testdata/lifecycle/01_first_install.decoded.json` for
plan-After objects + does live GETs against lifecycle's namespace before
it was cleaned up). For each of Deployment, Service, ConfigMap, Secret,
ClusterRole, ClusterRoleBinding:

- `<Kind>.live.raw.json` / `<Kind>.planafter.raw.json` — raw (uncleaned)
  live GET vs. plan-After object.
- `<Kind>.live.cleaned.json` / `<Kind>.planafter.cleaned.json` — both run
  through `spec.CleanUnstruct(obj, CleanUnstructOptions{CleanRuntimeData:
  true, CleanHelmShAnnos: true, CleanWerfIoAnnos: true,
  CleanManagedFields: true})` — the cleaning step of the normalization
  pipeline.
- `<Kind>.cleaned-diff.json` — the RFC6902 diff (`wI2L/jsondiff`) between
  the two cleaned objects.

**See `internal/planconv/testdata/STRIP_LIST.md`** for the full summary
table (which JSON-pointer paths planconv's `NormalizeUnstructured` must
strip beyond `CleanUnstruct` alone) and field-by-field rationale.
Headline: `ClusterRole`/`ClusterRoleBinding` (the cluster-scoped key case)
needed **zero** extra stripping; `Deployment` needed the most (15 fields,
mostly apps/v1 + PodSpec server-defaulting); `Service` needed 7
(cluster-IP/family/affinity server defaulting); every namespaced kind
needed `metadata.namespace` stripped (chart manifests never set it
explicitly, live objects always have it). One entry (`/spec/replicas`) is
flagged as NOT a strip candidate — it's `lifecycle`'s own intentional
drift (step c) correctly still showing up as a real diff, not
server-defaulting noise; see the STRIP_LIST.md caveat section.

### Error-shape fixtures (`internal/nelmclient/testdata/errors/`)

Captured by `go run -tags smoke ./scripts/smoke/errorshapes` (used by
`internal/nelmclient`'s error-classification unit tests).

| file | scenario | exact error text captured |
|---|---|---|
| `unreachable_cluster.txt` | synthetic kubeconfig, server `https://127.0.0.1:1` (nothing listens there), `--kube-request-timeout 3s` | `release plan install: construct kube client factory: check kubernetes cluster version to check kubernetes connectivity: Get "https://127.0.0.1:1/version?timeout=3s": dial tcp 127.0.0.1:1: connect: connection refused` |
| `bad_chart_ref.txt` | `./this-relative-chart-path-does-not-exist-anywhere` against orbstack (reachable, so this gets past connectivity and reaches chart loading) | `release plan install: render chart: load chart at "./this-relative-...": error checking if ... is a directory: stat ...: no such file or directory` |
| `remote_chart_no_featgate.txt` | `oci://example.com/charts/does-not-matter` against orbstack, `NELM_FEAT_REMOTE_CHARTS` unset (false) | **Same generic "stat ...: no such file or directory" error as `bad_chart_ref.txt`** — no distinct/explicit "remote charts disabled" message. Important for `internal/nelmclient`'s error classifier: there is nothing feat-gate-specific to pattern-match here; an `oci://`/`repo/name` ref without the gate enabled is indistinguishable, error-text-wise, from a plain bad local path. |

Note: nelm's connectivity check (`kube.NewClientFactory`) always runs
BEFORE chart loading, so the bad-chart-ref and remote-chart-featgate
cases needed a *reachable* cluster (orbstack) to actually exercise
chart-reference validation — against the synthetic unreachable
kubeconfig they'd just re-produce the same connectivity error as case 1.
