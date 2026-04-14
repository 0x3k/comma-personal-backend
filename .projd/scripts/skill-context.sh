#!/usr/bin/env bash
set -euo pipefail

# skill-context.sh -- Print project context for skill load-time.
#
# Each subcommand outputs one section of context.
# Skills call this instead of inline shell to avoid permission errors
# from compound expressions in !` ` blocks.
#
# Usage:
#   ./.projd/scripts/skill-context.sh features
#   ./.projd/scripts/skill-context.sh agent-json
#   ./.projd/scripts/skill-context.sh claude-md
#   ./.projd/scripts/skill-context.sh branch
#   ./.projd/scripts/skill-context.sh git-status
#   ./.projd/scripts/skill-context.sh git-diff-stat
#   ./.projd/scripts/skill-context.sh handoff
#   ./.projd/scripts/skill-context.sh status
#   ./.projd/scripts/skill-context.sh smoke
#   ./.projd/scripts/skill-context.sh gh-auth

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
cd "$PROJECT_DIR"

cmd="${1:-help}"

case "$cmd" in
    features)
        shopt -s nullglob
        found=false
        for f in .projd/progress/*.json; do
            if [ -f "$f" ]; then
                found=true
                jq -c '{id, name, status, priority, blocked_by, branch}' "$f" 2>/dev/null || true
            fi
        done
        if [ "$found" = false ]; then
            echo "none"
        fi
        ;;
    agent-json)
        if [ -f .projd/agent.json ]; then
            cat .projd/agent.json
        else
            echo "not found"
        fi
        ;;
    claude-md)
        if [ -f CLAUDE.md ]; then
            head -30 CLAUDE.md
        else
            echo "no CLAUDE.md"
        fi
        ;;
    branch)
        git branch --show-current 2>/dev/null || echo "detached"
        ;;
    git-status)
        git status --short 2>/dev/null || echo "not a git repo"
        ;;
    git-diff-stat)
        git diff --stat 2>/dev/null
        ;;
    handoff)
        if [ -f .projd/HANDOFF.md ]; then
            cat .projd/HANDOFF.md
        else
            echo "No .projd/HANDOFF.md -- clean start."
        fi
        ;;
    status)
        if [ -x .projd/scripts/status.sh ]; then
            ./.projd/scripts/status.sh 2>&1 || echo "status.sh failed"
        else
            echo ".projd/scripts/status.sh not found"
        fi
        ;;
    smoke)
        if [ -x .projd/scripts/smoke.sh ]; then
            rc=0
            ./.projd/scripts/smoke.sh 2>&1 || rc=$?
            echo "EXIT_CODE=$rc"
        else
            echo ".projd/scripts/smoke.sh not found"
        fi
        ;;
    token-usage)
        branch=$(git branch --show-current 2>/dev/null || echo "")
        if [ -z "$branch" ]; then
            echo "no branch detected"
            exit 0
        fi
        project_slug=$(echo "$PROJECT_DIR" | sed 's|/|-|g')
        jsonl_dir="$HOME/.claude/projects/$project_slug"
        if [ ! -d "$jsonl_dir" ]; then
            echo "no session data"
            exit 0
        fi
        sums=$(grep -h '"usage"' "$jsonl_dir"/*.jsonl 2>/dev/null | \
            jq -r --arg branch "$branch" 'select(.message.usage != null and .gitBranch == $branch) |
                .message.usage |
                "\(.input_tokens // 0) \(.output_tokens // 0) \(.cache_creation_input_tokens // 0) \(.cache_read_input_tokens // 0)"' 2>/dev/null | \
            awk '{in_t += $1 + $3; out_t += $2; cache_r += $4} END {printf "%d %d %d", in_t+0, out_t+0, cache_r+0}')
        read -r tin tout tcache <<< "$sums"
        total=$((tin + tout))
        if [ "$total" -eq 0 ]; then
            echo "no token data for branch $branch"
            exit 0
        fi
        fmt() {
            local n="$1"
            if [ "$n" -ge 1000000 ]; then
                awk "BEGIN { printf \"%.1fM\", $n / 1000000 }"
            elif [ "$n" -ge 1000 ]; then
                awk "BEGIN { printf \"%.1fk\", $n / 1000 }"
            else
                printf "%d" "$n"
            fi
        }
        echo "$(fmt "$tin") input, $(fmt "$tout") output ($(fmt "$total") total, $(fmt "$tcache") cache read)"
        ;;
    mode)
        if [ -f .projd/mode ]; then
            cat .projd/mode
        else
            echo "team"
        fi
        ;;
    gh-auth)
        gh auth status 2>&1 || echo "gh: not available or not authenticated"
        ;;
    help)
        echo "Usage: $0 <subcommand>"
        echo ""
        echo "Subcommands: features agent-json claude-md branch git-status"
        echo "             git-diff-stat handoff status smoke gh-auth"
        echo "             token-usage mode"
        ;;
    *)
        echo "Unknown subcommand: $cmd"
        exit 1
        ;;
esac
