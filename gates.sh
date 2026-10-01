#!/usr/bin/env bash
# gates.sh — deterministic mechanical gates. Usage:
#   ./gates.sh repo                      # full repo gate (default)
#   ./gates.sh pkg <./internal/foo>      # package-scoped
set -euo pipefail
cd "$(dirname "$0")"

fmt() { local out; out=$(gofmt -l -e .); [ -z "$out" ] || { echo "gofmt violations:"; echo "$out"; exit 1; }; }

case "${1:-repo}" in
  repo)
    fmt
    go vet ./...
    if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; fi
    go build ./...
    # unit tests only: TF_ACC unset => acceptance auto-skipped; smoke excluded by build tag
    go test -race -count=1 -timeout 10m ./...
    # file-existence gate
    for f in main.go gates.sh CONTRACTS.md \
             internal/provider/release_schema.go internal/nelmclient/types.go internal/planconv/key.go; do
      [ -e "$f" ] || { echo "missing required file: $f"; exit 1; }
    done
    ;;
  pkg)
    fmt
    go vet "$2"
    go build "$2"
    go test -race -count=1 -timeout 5m "$2"
    ;;
  *)
    echo "usage: $0 [repo | pkg <package>]" >&2
    exit 2
    ;;
esac
echo "gates OK (${1:-repo})"
