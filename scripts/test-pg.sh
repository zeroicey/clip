#!/usr/bin/env bash
#
# Run the whole Go test suite INCLUDING the Postgres-backed tests.
#
# Those tests (internal/clip) are skipped by default because they need a live
# database; `pnpm run backend:test` therefore works on a machine that has none,
# and the release gate stays runnable. This script is the other half: it is what
# you run before deploying, and what proves the storage guarantees (ownership in
# the WHERE clause, expiry filtering, the quota lock, the upload sniffing)
# actually hold against a real server.
#
# Usage:
#   TEST_PG_DSN='postgres://relay:PASSWORD@127.0.0.1:5432/relay_test?sslmode=disable' \
#     pnpm run backend:test-pg
#
# The database name in the DSN MUST contain "test": these tests TRUNCATE tables,
# and a mistyped DSN must not be able to eat real clips. The test suite enforces
# that as well, but failing here first gives a clearer message.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ -z "${TEST_PG_DSN:-}" ]; then
  cat >&2 <<'EOF'
TEST_PG_DSN is not set, so the Postgres-backed tests would be skipped.

  TEST_PG_DSN='postgres://relay:PASSWORD@127.0.0.1:5432/relay_test?sslmode=disable' \
    pnpm run backend:test-pg

The password lives in /data/services/clip/clip.env on the host (mode 0600).
EOF
  exit 1
fi

case "$TEST_PG_DSN" in
  *test*) ;;
  *)
    echo "refusing to run: TEST_PG_DSN names a database without 'test' in it" >&2
    exit 1
    ;;
esac

cd "$REPO"
go test ./... -count=1 "$@"
