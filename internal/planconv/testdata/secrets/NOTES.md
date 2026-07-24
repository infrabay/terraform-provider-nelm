# secrets/redaction fixture evidence notes

Captured by scripts/smoke/secrets against nelm CLI (nelm-v1.26.2), release tfnelm-fix-secrets-a52104/secrets
(plan-only, no install, no cleanup needed -- namespace was never created).
Chart: testdata/charts/basic, with --set secret.password=s3cr3t-fake-9f2c --set configMap.sensitivePathsAnnotation=true.

## (1) Cleartext-in-dataRaw evidence

fake secret value "s3cr3t-fake-9f2c" found in artifact.DataRaw (the JSON string nelm gzips
onto disk as the plan artifact's dataRaw field): true

Snippet from artifact.DataRaw around the fake password (proves the Secret's
stringData is stored in PLAIN CLEARTEXT in the plan artifact, not hashed or
redacted by nelm itself -- redaction is the CONSUMER's responsibility,
exactly as design §2.1/§7 risk #7 states):

    ...},"config":{"configMap":{"sensitivePathsAnnotation":true},"secret":{"password":"s3cr3t-fake-9f2c"}},"manifest":"# Source: basic/template...

This is why CONTRACTS.md mandates: plan artifacts are read and deleted in
the SAME function call frame, under a 0700 per-op temp dir, and never
persist past that call.

### Where exactly the fake password appears (precise, checked by hand)

Searching `secret_plan.decoded.json` for the literal plaintext string
`s3cr3t-fake-9f2c` finds it in FOUR places, all as fully-reversible
plaintext (not a hash, not encrypted):

    /data/plan/operations/0/config/release/config/secret/password
    /data/plan/operations/15/config/release/config/secret/password
    /data/release/config/secret/password
    /data/releaseInfos/0/release/config/secret/password

These are the raw user-supplied Helm VALUES (`.Values.secret.password`),
stored verbatim in the release record embedded in the artifact.

Separately, the RENDERED Secret object itself
(`secret_after.raw.json`, i.e. one of `Changes[].After`) shows
`data.password: "czNjcjN0LWZha2UtOWYyYw=="` -- this is base64, which is
Kubernetes' standard (non-secret, non-cryptographic) wire encoding for the
`data` field of any Secret object, not a redaction mechanism: it decodes
trivially (`echo czNjcjN0LWZha2UtOWYyYw== | base64 -d` -> `s3cr3t-fake-9f2c`)
and carries zero security value on its own. Both forms (the plaintext
values-map copies AND the base64 Secret-object copy) are cleartext for the
purposes of this task's redaction/temp-file concern.

## (2) resource.GetSensitiveInfo results

- Secret /v1, Kind=Secret /secrets-basic: IsSensitive=true SensitivePaths=[$$HIDE_ALL$$] FullySensitive=true
  (default Secret behavior, V1/HideAll -- global FeatGateFieldSensitive is
  OFF in this codebase per CONTRACTS.md, so nelm's own default for Secret
  kind is the full-skeleton HideAll, not path-specific. The PROVIDER
  overrides this locally per design §2.1 step 2 -- see
  configmap_redacted.json below for what path-specific redaction produces,
  which is what the provider will replicate for Secrets too.)
- ConfigMap (werf.io/sensitive-paths: "data.message" annotated) /v1, Kind=ConfigMap /secrets-basic:
  IsSensitive=true SensitivePaths=[data.message] FullySensitive=false
  (annotation-driven path redaction works on ANY kind, not just Secret --
  resource.GetSensitiveInfo checks the werf.io/sensitive-paths annotation
  before falling back to any Secret-specific default.)

## (3) resource.RedactSensitiveData output

See secret_redacted.json (nelm's own default HideAll skeleton -- the
provider will NOT use this shape for Secrets, see design §2.1 step 2) and
configmap_redacted.json (path-specific: data.message replaced with a
deterministic "<hidden N sensitive bytes, hash sha256[:12]>" placeholder,
everything else -- name, labels, other data keys if any -- untouched).

## Files in this directory

- secret_plan.artifact.json.gz -- raw gzip plan artifact (as nelm wrote it)
- secret_plan.decoded.json -- decoded/indented JSON of the same artifact
- secret_after.raw.json -- the Secret's plan-After object, RAW/cleartext
- configmap_after.raw.json -- the annotated ConfigMap's plan-After object, RAW/cleartext
- secret_redacted.json -- resource.RedactSensitiveData(secret, HideAll) output
- configmap_redacted.json -- resource.RedactSensitiveData(configmap, ["data.message"]) output
