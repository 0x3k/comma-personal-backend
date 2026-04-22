#!/usr/bin/env bash
set -euo pipefail

# check-git-policy.sh -- PreToolUse hook that enforces .projd/agent.json git policies.
#
# Receives JSON on stdin from Claude Code with tool_input.command.
# Exits 0 with JSON deny decision to block, or exits 0 silently to allow.

INPUT=$(cat)
COMMAND=$(echo "$INPUT" | jq -r '.tool_input.command // empty')

# Early exit if not a git command
# Match git at the start or after shell operators (;&|), not inside quoted strings
if ! echo "$COMMAND" | grep -qE '(^|[;&|]\s*)git\s'; then
    exit 0
fi

CWD=$(echo "$INPUT" | jq -r '.cwd // empty')

# Determine the *effective* CWD for branch checks. The session CWD (what Claude
# Code sends) can lag behind the directory where the command actually runs --
# e.g., a parallel-agent worktree -- causing false blocks on merge/commit when
# the command is `cd <worktree> && git ...` but the session is still on main.
#
# Honor two shapes:
#   1. Leading `cd <path> &&|;|...`
#   2. `git -C <path> ...`
EFFECTIVE_CWD="$CWD"

_cd_target=""
if [[ "$COMMAND" =~ ^[[:space:]]*cd[[:space:]]+\"([^\"]+)\" ]]; then
    _cd_target="${BASH_REMATCH[1]}"
elif [[ "$COMMAND" =~ ^[[:space:]]*cd[[:space:]]+\'([^\']+)\' ]]; then
    _cd_target="${BASH_REMATCH[1]}"
elif [[ "$COMMAND" =~ ^[[:space:]]*cd[[:space:]]+([^[:space:]\&\|\;]+) ]]; then
    _cd_target="${BASH_REMATCH[1]}"
fi
if [ -n "$_cd_target" ]; then
    if [[ "$_cd_target" == /* ]]; then
        EFFECTIVE_CWD="$_cd_target"
    else
        EFFECTIVE_CWD="$CWD/$_cd_target"
    fi
fi

_gitc_target=""
if [[ "$COMMAND" =~ git[[:space:]]+-C[[:space:]]+\"([^\"]+)\" ]]; then
    _gitc_target="${BASH_REMATCH[1]}"
elif [[ "$COMMAND" =~ git[[:space:]]+-C[[:space:]]+\'([^\']+)\' ]]; then
    _gitc_target="${BASH_REMATCH[1]}"
elif [[ "$COMMAND" =~ git[[:space:]]+-C[[:space:]]+([^[:space:]\&\|\;]+) ]]; then
    _gitc_target="${BASH_REMATCH[1]}"
fi
if [ -n "$_gitc_target" ]; then
    if [[ "$_gitc_target" == /* ]]; then
        EFFECTIVE_CWD="$_gitc_target"
    else
        EFFECTIVE_CWD="$CWD/$_gitc_target"
    fi
fi

# Load .projd/agent.json from the session CWD (the policy file belongs to the
# project root, not the worktree copy).
AGENT_JSON="${CWD}/.projd/agent.json"
if [ ! -f "$AGENT_JSON" ]; then
    exit 0
fi

ALLOW_PUSH=$(jq -r '.git.allow_push // "false"' "$AGENT_JSON")
ALLOW_FORCE_PUSH=$(jq -r '.git.allow_force_push // false' "$AGENT_JSON")
BRANCH_PREFIX=$(jq -r '.git.branch_prefix // ""' "$AGENT_JSON")

# Read protected branches into an array
PROTECTED_BRANCHES=()
while IFS= read -r branch; do
    [ -n "$branch" ] && PROTECTED_BRANCHES+=("$branch")
done < <(jq -r '.git.protected_branches[]? // empty' "$AGENT_JSON")

deny() {
    local reason="$1"
    jq -n --arg reason "$reason" '{
        hookSpecificOutput: {
            hookEventName: "PreToolUse",
            permissionDecision: "deny",
            permissionDecisionReason: $reason
        }
    }'
    exit 0
}

is_protected() {
    local branch="$1"
    if [ ${#PROTECTED_BRANCHES[@]} -eq 0 ]; then
        return 1
    fi
    for p in "${PROTECTED_BRANCHES[@]}"; do
        if [ "$branch" = "$p" ]; then
            return 0
        fi
    done
    return 1
}

# --- Check a: Force push ---
if echo "$COMMAND" | grep -qE 'git\s+push\s+(.+\s)?(-[a-zA-Z]*f\b|--force\b)'; then
    if [ "$ALLOW_FORCE_PUSH" != "true" ]; then
        deny "Force push blocked by .projd/agent.json (allow_force_push: false)."
    fi
fi

# --- Check b: Push ---
if echo "$COMMAND" | grep -qE 'git\s+push'; then
    case "$ALLOW_PUSH" in
        false)
            deny "Push blocked by .projd/agent.json (allow_push: false). The operator handles pushing."
            ;;
        feature)
            # Get the current branch from the effective CWD (honors `cd <worktree> &&`)
            CURRENT_BRANCH=$(git -C "$EFFECTIVE_CWD" branch --show-current 2>/dev/null || echo "")
            if [ -n "$BRANCH_PREFIX" ] && [ -n "$CURRENT_BRANCH" ]; then
                if [[ "$CURRENT_BRANCH" != "${BRANCH_PREFIX}"* ]]; then
                    deny "Push blocked: branch '$CURRENT_BRANCH' does not start with prefix '$BRANCH_PREFIX'. Only feature branches can be pushed (allow_push: feature)."
                fi
            fi
            # Check push arguments for protected branch targets
            if echo "$COMMAND" | grep -qE 'git\s+push\s+\S+\s+\S+' && [ ${#PROTECTED_BRANCHES[@]} -gt 0 ]; then
                PUSH_ARGS=$(echo "$COMMAND" | sed -E 's/.*git[[:space:]]+push[[:space:]]+[^[:space:]]+[[:space:]]+(.*)/\1/')
                for arg in $PUSH_ARGS; do
                    # Skip flags
                    [[ "$arg" == -* ]] && continue
                    # Extract target from refspec (src:dst) or use arg as-is
                    if [[ "$arg" == *:* ]]; then
                        TARGET="${arg##*:}"
                    else
                        TARGET="$arg"
                    fi
                    for p in "${PROTECTED_BRANCHES[@]}"; do
                        if [ "$TARGET" = "$p" ]; then
                            deny "Push blocked: cannot push to protected branch '$p'."
                        fi
                    done
                done
            fi
            ;;
        true)
            # Allow all pushes
            ;;
        *)
            # Unknown value, treat as false
            deny "Push blocked by .projd/agent.json (allow_push has unknown value: '$ALLOW_PUSH')."
            ;;
    esac
fi

# --- Check c: Commit on protected branch ---
if echo "$COMMAND" | grep -qE 'git\s+commit'; then
    CURRENT_BRANCH=$(git -C "$EFFECTIVE_CWD" branch --show-current 2>/dev/null || echo "")
    # Allow initial commit in empty repos (no HEAD yet)
    if git -C "$EFFECTIVE_CWD" rev-parse HEAD &>/dev/null; then
        if [ -n "$CURRENT_BRANCH" ] && is_protected "$CURRENT_BRANCH"; then
            deny "Cannot commit directly to protected branch '$CURRENT_BRANCH'. Create a feature branch first: git checkout -b ${BRANCH_PREFIX}<name>"
        fi
    fi
fi

# --- Check d: Branch creation without prefix ---
if echo "$COMMAND" | grep -qE 'git\s+(checkout\s+-[bB]|switch\s+-[cC])\s+'; then
    if [ -n "$BRANCH_PREFIX" ]; then
        NEW_BRANCH=$(echo "$COMMAND" | sed -E 's/.*git[[:space:]]+(checkout[[:space:]]+-[bB]|switch[[:space:]]+-[cC])[[:space:]]+([^[:space:]]+).*/\2/')
        if [[ "$NEW_BRANCH" != "${BRANCH_PREFIX}"* ]]; then
            deny "Branch must use prefix '${BRANCH_PREFIX}'. Use: git checkout -b ${BRANCH_PREFIX}${NEW_BRANCH}"
        fi
    fi
fi

# --- Check d2: Branch creation via `git branch <name>` without prefix ---
if echo "$COMMAND" | grep -qE 'git\s+branch\s+[^-]'; then
    if [ -n "$BRANCH_PREFIX" ]; then
        NEW_BRANCH=$(echo "$COMMAND" | sed -E 's/.*git[[:space:]]+branch[[:space:]]+([^[:space:]-][^[:space:]]*).*/\1/')
        if [[ "$NEW_BRANCH" != "${BRANCH_PREFIX}"* ]]; then
            deny "Branch must use prefix '${BRANCH_PREFIX}'. Use: git branch ${BRANCH_PREFIX}${NEW_BRANCH}"
        fi
    fi
fi

# --- Check e: Merge into protected branch ---
if echo "$COMMAND" | grep -qE 'git\s+merge'; then
    CURRENT_BRANCH=$(git -C "$EFFECTIVE_CWD" branch --show-current 2>/dev/null || echo "")
    if [ -n "$CURRENT_BRANCH" ] && is_protected "$CURRENT_BRANCH"; then
        deny "Cannot merge into protected branch '$CURRENT_BRANCH'. Merge via PR instead."
    fi
fi

# Default: allow
exit 0
