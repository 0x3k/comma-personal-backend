#!/usr/bin/env bash
set -euo pipefail

# validate.sh -- Verify the boilerplate was configured correctly.
#
# Checks that placeholder content has been replaced, language-specific
# blocks have been activated, and scripts run without errors.
#
# Usage:
#   ./.projd/scripts/validate.sh           # run all checks
#   ./.projd/scripts/validate.sh --strict  # also run smoke.sh (slower)

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
cd "$PROJECT_DIR"

STRICT=false
if [ "${1:-}" = "--strict" ]; then
    STRICT=true
fi

PASS=0
FAIL=0
WARN=0

check() {
    local name="$1"
    local result="$2"
    if [ "$result" = "pass" ]; then
        echo "  [PASS] $name"
        PASS=$((PASS + 1))
    elif [ "$result" = "warn" ]; then
        echo "  [WARN] $name"
        WARN=$((WARN + 1))
    else
        echo "  [FAIL] $name"
        FAIL=$((FAIL + 1))
    fi
}

echo "=== Validating project configuration ==="
echo ""

# --- CLAUDE.md ---
echo "CLAUDE.md:"
if [ ! -f CLAUDE.md ]; then
    check "file exists" "fail"
else
    check "file exists" "pass"
    # Project overview must be filled in (setup.sh handles this)
    if grep -q '<!-- e\.g\.' CLAUDE.md 2>/dev/null; then
        check "project overview configured (run ./setup.sh)" "fail"
    else
        check "project overview configured" "pass"
    fi
    # Remaining placeholder sections are warnings (operator fills these in)
    if grep -q '<!-- Replace' CLAUDE.md 2>/dev/null; then
        check "all sections filled in (Build, Architecture, etc.)" "warn"
    else
        check "all sections filled in" "pass"
    fi
fi

# --- .claude/CLAUDE.md ---
echo ""
echo ".claude/CLAUDE.md:"
if [ ! -f .claude/CLAUDE.md ]; then
    check "file exists" "fail"
else
    check "file exists" "pass"
fi

# --- agent.json ---
echo ""
echo ".projd/agent.json:"
if [ ! -f .projd/agent.json ]; then
    check "file exists" "fail"
else
    check "file exists" "pass"
    if jq empty .projd/agent.json 2>/dev/null; then
        check "valid JSON" "pass"
    else
        check "valid JSON" "fail"
    fi
    if jq -e '.git.protected_branches | length > 0' .projd/agent.json &>/dev/null; then
        check "protected branches defined" "pass"
    else
        check "protected branches defined" "warn"
    fi
fi

# --- lefthook.yml ---
echo ""
echo "lefthook.yml:"
if [ ! -f lefthook.yml ]; then
    check "file exists" "fail"
else
    check "file exists" "pass"
    if grep -q '^\s*placeholder:' lefthook.yml 2>/dev/null; then
        check "placeholder command removed" "fail"
    else
        check "placeholder command removed" "pass"
    fi
    # Check that at least one real command exists (non-comment, non-placeholder)
    if grep -E '^\s{4}\S' lefthook.yml | grep -qv 'placeholder' 2>/dev/null; then
        check "has active hook commands" "pass"
    else
        check "has active hook commands" "fail"
    fi
fi

# --- smoke.sh ---
echo ""
echo ".projd/scripts/smoke.sh:"
if [ ! -f .projd/scripts/smoke.sh ]; then
    check "file exists" "fail"
elif [ ! -x .projd/scripts/smoke.sh ]; then
    check "is executable" "fail"
else
    check "is executable" "pass"
    if grep -q '^run_check ' .projd/scripts/smoke.sh 2>/dev/null; then
        check "has active checks (uncommented run_check)" "pass"
    else
        check "has active checks (uncommented run_check)" "fail"
    fi
fi

# --- .projd/scripts/init.sh ---
echo ""
echo ".projd/scripts/init.sh:"
if [ ! -f .projd/scripts/init.sh ]; then
    check "file exists" "fail"
elif [ ! -x .projd/scripts/init.sh ]; then
    check "is executable" "fail"
else
    check "is executable" "pass"
fi

# --- progress/ ---
echo ""
echo ".projd/progress/:"
if [ ! -d .projd/progress ]; then
    check "directory exists" "fail"
else
    check "directory exists" "pass"
    FEATURE_COUNT=$(find .projd/progress -name '*.json' -not -name 'example-*' | wc -l | tr -d ' ')
    if [ "$FEATURE_COUNT" -gt 0 ]; then
        check "has real feature files (not just example)" "pass"
    else
        check "has real feature files (not just example)" "fail"
    fi
    # Validate each feature file
    for f in .projd/progress/*.json; do
        [ -f "$f" ] || continue
        BASENAME=$(basename "$f")
        if jq -e '.id and .name and .acceptance_criteria' "$f" &>/dev/null; then
            check "$BASENAME: valid schema" "pass"
        else
            check "$BASENAME: valid schema (needs id, name, acceptance_criteria)" "fail"
        fi
    done
fi

# --- status.sh ---
echo ""
echo ".projd/scripts/status.sh:"
if [ ! -f .projd/scripts/status.sh ]; then
    check "file exists" "fail"
elif [ ! -x .projd/scripts/status.sh ]; then
    check "is executable" "fail"
else
    check "is executable" "pass"
fi

# --- Strict mode: run smoke.sh ---
if [ "$STRICT" = true ]; then
    echo ""
    echo "smoke.sh (execution):"
    if ./.projd/scripts/smoke.sh &>/dev/null; then
        check "exits cleanly" "pass"
    else
        check "exits cleanly" "fail"
    fi
fi

# --- Summary ---
echo ""
echo "=== validate.sh: $PASS passed, $FAIL failed, $WARN warnings ==="

if [ "$FAIL" -gt 0 ]; then
    echo ""
    echo "Fix the failures above, then re-run ./.projd/scripts/validate.sh"
    exit 1
fi
