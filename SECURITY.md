# Security policy

## Supported versions

Security fixes are made on `main` and released in a new patch version of the
latest minor release. Older minor versions do not receive fixes; upgrade to
the latest release to pick them up.

| Version              | Supported |
|----------------------|-----------|
| latest minor (0.x.y) | yes       |
| older minors         | no        |

## Reporting a vulnerability

Please **do not** open a public issue, pull request or discussion for a
security problem.

Report it privately through GitHub's private vulnerability reporting:
open the repository's
[Security tab](https://github.com/infrabay/terraform-provider-nelm/security)
and choose **Report a vulnerability**
([direct link](https://github.com/infrabay/terraform-provider-nelm/security/advisories/new)).

Please include:

- the provider version, Terraform version and Nelm version involved;
- a description of the issue and its impact;
- the steps or a minimal configuration that reproduce it, with credentials,
  kubeconfigs and other secrets removed.

Reports are acknowledged as soon as possible. Once the issue is confirmed,
a fix is prepared in a private security advisory and released, and the
advisory is published with the fixed version, crediting the reporter unless
they prefer otherwise.

## Scope

This policy covers the code in this repository: the provider binary and its
release artifacts. Vulnerabilities in [Nelm](https://github.com/werf/nelm),
Helm, Terraform or other dependencies should be reported to those projects;
if one affects this provider, a report here is still welcome so the
dependency can be updated.

Some behaviour is documented rather than a vulnerability, for example which
values a plan shows: see
[Sensitive values in non-`Secret` resources](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/resources/release.md#sensitive-values-in-non-secret-resources)
and the
[known limitations](https://github.com/infrabay/terraform-provider-nelm/blob/main/docs/guides/known-limitations.md).
