#!/usr/bin/env bash
#
# Fail if the sqlc-generated code no longer matches the SQL it came from.
#
# This is the same shape of guard as scripts/check-schema-sync.mjs: generated
# artifacts are checked into the repository (so a plain `go build` works without
# the code generator installed), and that only stays honest if drift is caught
# mechanically. `sqlc diff` regenerates in memory and reports a difference.
#
# Unlike the schema check, this one needs the sqlc binary. It fails loudly with
# the install command rather than skipping: a gate that silently skips is a gate
# that never fails.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! command -v sqlc >/dev/null 2>&1; then
  cat >&2 <<'EOF'
sqlc is not installed, so generated-code drift cannot be checked.

  go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest

(With GOPROXY=https://goproxy.cn,direct on a mainland network.)
EOF
  exit 1
fi

cd "$REPO"
if ! out="$(sqlc diff 2>&1)"; then
  echo "sqlc: generated code is out of date (run: pnpm run db:gen-sqlc)" >&2
  echo "$out" >&2
  exit 1
fi
echo "sqlc: generated code matches the queries"
