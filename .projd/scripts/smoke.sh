#!/usr/bin/env bash
set -euo pipefail

# smoke.sh -- Fast verification that the project is not broken.
#
# Runs lint + type-check (same checks as pre-commit hooks).
# Should complete in under 30 seconds.
#
# Usage:
#   ./.projd/scripts/smoke.sh           # run all checks (local + sub-projects)
#   ./.projd/scripts/smoke.sh lint      # run only the "lint" check
#   ./.projd/scripts/smoke.sh typecheck # run only the "typecheck" check

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
cd "$PROJECT_DIR"

PASS=0
FAIL=0
TARGET="${1:-all}"

run_check() {
    local name="$1"
    shift
    if [ "$TARGET" != "all" ] && [ "$TARGET" != "$name" ]; then
        return
    fi
    echo "--- $name ---"
    if "$@"; then
        echo "[PASS] $name"
        PASS=$((PASS + 1))
    else
        echo "[FAIL] $name"
        FAIL=$((FAIL + 1))
    fi
}

# --- Sub-project aggregation ---
if [ -f projects.json ] && [ "$TARGET" = "all" ]; then
    for dir in $(jq -r '.projects[].path' projects.json); do
        if [ -x "$dir/.projd/scripts/smoke.sh" ]; then
            echo ""
            echo "=== $dir ==="
            if (cd "$dir" && ./.projd/scripts/smoke.sh); then
                PASS=$((PASS + 1))
            else
                FAIL=$((FAIL + 1))
            fi
        fi
    done
    echo ""
    echo "=== root ==="
fi

# --- Local checks (activated by ./setup.sh) ---

# Autodetect the TS project dir: repo root or a common frontend subdir.
_ts_dir="."
for _d in . web frontend client app ui; do
    if [ -f "$_d/package.json" ]; then _ts_dir="$_d"; break; fi
done
_ts_has() { [ -f "$_ts_dir/package.json" ] && jq -e --arg s "$1" '.scripts[$s] // empty' "$_ts_dir/package.json" >/dev/null 2>&1; }
if _ts_has lint; then
    run_check "lint" sh -c "cd '$_ts_dir' && npm run lint"
elif [ -d "$_ts_dir/src" ]; then
    run_check "lint" sh -c "cd '$_ts_dir' && npx eslint src --ext .ts,.tsx"
fi
if _ts_has type-check; then
    run_check "typecheck" sh -c "cd '$_ts_dir' && npm run type-check"
elif _ts_has typecheck; then
    run_check "typecheck" sh -c "cd '$_ts_dir' && npm run typecheck"
else
    run_check "typecheck" sh -c "cd '$_ts_dir' && npx tsc --noEmit"
fi

run_check "vet" go vet ./...
run_check "fmt" bash -c 'test -z "$(gofmt -l .)"'

# Migration up/down + plate_hash CHECK constraint test. The test auto-skips
# when DATABASE_URL is unset or unreachable, so this is safe to leave in
# the always-on smoke run -- it only exercises the DB when one is wired up.
run_check "alpr-schema" go test -count=1 -run 'TestALPRSchema_' ./internal/db/...

# --- Summary ---
echo ""
echo "=== smoke.sh: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
