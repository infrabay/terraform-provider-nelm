#!/usr/bin/env bash
# guard.sh — hard safety gate for every live-cluster capture script under
# scripts/smoke/.
#
# A developer's kubectl current-context may point at a remote or production
# cluster. NOTHING under scripts/smoke/ is ever allowed to touch it. This script
# asserts that the kubeconfig context named "orbstack" (and ONLY that named
# context — current-context is never consulted) resolves to a local API
# server (https://127.0.0.1* or https://localhost*), and hard-exits
# otherwise.
#
# Usage (every capture script does one of these FIRST, before any other
# kubectl/helm/nelm invocation):
#   source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/guard.sh"
# or, standalone:
#   ./scripts/smoke/guard.sh && echo "safe to proceed"
#
# The Go-side equivalent of this exact check is
# scripts/smoke/smokelib/guard.go:MustGuardOrbstack, which every `//go:build
# smoke` Go program in this directory calls before touching the cluster in
# any way. Both checks exist independently and deliberately duplicate each
# other — belt and suspenders for the single most important invariant of
# this harness.

set -euo pipefail

_orbstack_guard_server=$(kubectl config view --context orbstack \
  -o jsonpath='{.clusters[?(@.name=="orbstack")].cluster.server}' 2>/dev/null || true)

if [[ -z "${_orbstack_guard_server}" ]]; then
  echo "ORBSTACK GUARD FAILED: kubeconfig context 'orbstack' not found (or has no cluster.server). Refusing to proceed." >&2
  exit 1
fi

case "${_orbstack_guard_server}" in
  https://127.0.0.1*|http://127.0.0.1*|https://localhost*|http://localhost*)
    ;;
  *)
    echo "ORBSTACK GUARD FAILED: context 'orbstack' resolves to server '${_orbstack_guard_server}'," >&2
    echo "expected https://127.0.0.1* or https://localhost*. This looks like it could be a remote" >&2
    echo "(possibly production) cluster. REFUSING to proceed. current-context is NEVER trusted;" >&2
    echo "only the explicitly named 'orbstack' context is ever checked or used." >&2
    exit 1
    ;;
esac

echo "orbstack guard OK: context='orbstack' server='${_orbstack_guard_server}'"
export ORBSTACK_GUARD_OK=1
export ORBSTACK_GUARD_SERVER="${_orbstack_guard_server}"
