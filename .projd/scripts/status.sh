#!/usr/bin/env bash
set -euo pipefail

# status.sh -- Show project state at a glance.
#
# Displays git state, feature progress, and handoff context.
# Run at the start of every session for quick orientation.
#
# Usage:
#   ./.projd/scripts/status.sh          # show status for this project (+ sub-projects if any)
#   ./.projd/scripts/status.sh --local  # skip sub-projects, show only this directory

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
cd "$PROJECT_DIR"

LOCAL_ONLY=false
if [ "${1:-}" = "--local" ]; then
    LOCAL_ONLY=true
fi

# --- Sub-project aggregation ---
if [ "$LOCAL_ONLY" = false ] && [ -f projects.json ]; then
    for dir in $(jq -r '.projects[].path' projects.json); do
        if [ -x "$dir/.projd/scripts/status.sh" ]; then
            echo ""
            echo "=============================="
            echo "=== $dir"
            echo "=============================="
            (cd "$dir" && ./.projd/scripts/status.sh --local)
        fi
    done
    echo ""
    echo "=============================="
    echo "=== root"
    echo "=============================="
fi

# --- Git ---
echo ""
echo "--- Git ---"
if git rev-parse --is-inside-work-tree &>/dev/null; then
    BRANCH=$(git branch --show-current 2>/dev/null || echo "detached")
    echo "Branch: $BRANCH"

    DIRTY=$(git status --porcelain 2>/dev/null | head -20)
    if [ -n "$DIRTY" ]; then
        echo "Dirty files:"
        echo "$DIRTY"
    else
        echo "Working tree clean"
    fi

    if git rev-parse --abbrev-ref '@{upstream}' &>/dev/null 2>&1; then
        LOCAL=$(git rev-parse HEAD)
        REMOTE=$(git rev-parse '@{upstream}' 2>/dev/null || echo "unknown")
        if [ "$LOCAL" = "$REMOTE" ]; then
            echo "Up to date with remote"
        else
            AHEAD=$(git rev-list '@{upstream}..HEAD' --count 2>/dev/null || echo "?")
            BEHIND=$(git rev-list 'HEAD..@{upstream}' --count 2>/dev/null || echo "?")
            echo "Ahead: $AHEAD, Behind: $BEHIND"
        fi
    else
        echo "No upstream tracking branch"
    fi

    echo ""
    echo "--- Recent Commits ---"
    git log --oneline -5 2>/dev/null || echo "(no commits yet)"
else
    echo "Not a git repository"
fi

# --- .projd/HANDOFF.md ---
echo ""
if [ -f .projd/HANDOFF.md ]; then
    echo "--- .projd/HANDOFF.md (previous session left context) ---"
    head -10 .projd/HANDOFF.md
    LINES=$(wc -l < .projd/HANDOFF.md | tr -d ' ')
    if [ "$LINES" -gt 10 ]; then
        echo "  ... ($LINES lines total)"
    fi
else
    echo "--- No .projd/HANDOFF.md (clean start) ---"
fi

# --- Progress ---
echo ""
if [ -d .projd/progress ] && ls .projd/progress/*.json &>/dev/null; then
    TOTAL=0
    DONE=0
    IN_PROGRESS=0
    PENDING=0

    for f in .projd/progress/*.json; do
        [ -f "$f" ] || continue
        TOTAL=$((TOTAL + 1))
        STATUS=$(jq -r '.status // "pending"' "$f")
        case "$STATUS" in
            complete) DONE=$((DONE + 1)) ;;
            in_progress) IN_PROGRESS=$((IN_PROGRESS + 1)) ;;
            *) PENDING=$((PENDING + 1)) ;;
        esac
    done

    echo "--- Progress: $DONE/$TOTAL complete ($IN_PROGRESS in progress, $PENDING pending) ---"

    for f in .projd/progress/*.json; do
        [ -f "$f" ] || continue
        STATUS=$(jq -r '.status // "pending"' "$f")
        NAME=$(jq -r '.name' "$f")
        BRANCH=$(jq -r '.branch // empty' "$f")
        BLOCKED=$(jq -r '.blocked_by // [] | length' "$f")

        case "$STATUS" in
            complete)    TAG="DONE" ;;
            in_progress) TAG="WORK" ;;
            *)           TAG="TODO" ;;
        esac

        LINE="  [$TAG] $NAME"
        if [ -n "$BRANCH" ]; then
            LINE="$LINE ($BRANCH)"
        fi
        if [ "$BLOCKED" -gt 0 ] && [ "$STATUS" != "complete" ]; then
            BLOCKERS=$(jq -r '.blocked_by | join(", ")' "$f")
            LINE="$LINE [blocked by: $BLOCKERS]"
        fi
        echo "$LINE"
    done
else
    echo "--- No features in .projd/progress/ ---"
fi

# --- Worktrees ---
if git rev-parse --is-inside-work-tree &>/dev/null; then
    WT_COUNT=$(git worktree list 2>/dev/null | wc -l | tr -d ' ')
    if [ "$WT_COUNT" -gt 1 ]; then
        echo ""
        echo "--- Active Worktrees ---"
        git worktree list 2>/dev/null | while IFS= read -r line; do
            # Skip the main worktree (first line)
            if echo "$line" | grep -qE '\[bare\]|'"$PROJECT_DIR"''; then
                continue
            fi
            echo "  $line"
        done
        echo "  ($((WT_COUNT - 1)) worktree(s) -- run git worktree list for details)"
    fi
fi

# --- Agent config ---
if [ -f .projd/agent.json ]; then
    echo ""
    echo "--- Agent Controls ---"
    PREFIX=$(jq -r '.git.branch_prefix // empty' .projd/agent.json)
    PUSH=$(jq -r '.git.allow_push // false' .projd/agent.json)
    PROTECTED=$(jq -r '.git.protected_branches // [] | join(", ")' .projd/agent.json)
    echo "Branch prefix: ${PREFIX:-none}  |  Push: $PUSH  |  Protected: ${PROTECTED:-none}"
fi
