#!/usr/bin/env bash
#
# The gate. Everything that must be true before a commit or a deploy.
#
# It is a script rather than a package.json "scripts" entry because this
# repository has no Node toolchain at all — a Go-only service does not need one,
# and the absence of a second package manager is one less thing to keep in sync.
#
# Usage: scripts/check.sh
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

echo "==> gofmt"
unformatted="$(gofmt -l . | grep -v '^$' || true)"
if [ -n "$unformatted" ]; then
  echo "these files are not gofmt-clean:" >&2
  echo "$unformatted" >&2
  exit 1
fi

echo "==> go vet"
go vet ./...

echo "==> sqlc (generated code must match the SQL)"
"$REPO/scripts/check-sqlc-sync.sh"

echo "==> go test"
# Postgres-backed tests skip without TEST_PG_DSN, so this stays runnable on a
# machine with no database. Run them for real with:
#   TEST_PG_DSN=... scripts/test-pg.sh
go test ./...

echo
echo "gate passed"
