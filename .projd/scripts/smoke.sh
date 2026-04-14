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

run_check "lint" npx eslint src --ext .ts
run_check "typecheck" npx tsc --noEmit

run_check "vet" go vet ./...
run_check "fmt" bash -c 'test -z "$(gofmt -l .)"'

# --- Summary ---
echo ""
echo "=== smoke.sh: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
