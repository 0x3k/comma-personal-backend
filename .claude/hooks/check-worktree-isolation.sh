#!/usr/bin/env bash
set -euo pipefail

# check-worktree-isolation.sh -- PreToolUse hook that blocks Write/Edit
# operations targeting paths outside the active worktree.
#
# When /projd-hands-off spawns a parallel-agent worker with
# isolation: worktree, the worker runs in .claude/worktrees/agent-<id>/.
# Some tools (e.g. sqlc generate, go run, build caches) can resolve files
# relative to the enclosing module root and cause Write/Edit calls that
# target the main repo tree instead of the worktree -- producing stray
# modifications in main after the worker finishes.
#
# This hook detects a worktree session from CWD and rejects any Write/Edit
# whose file_path resolves inside the main repo but outside the active
# worktree. Paths outside the repo entirely (e.g. ~/.claude, /tmp) are
# allowed: those are not "leaks".
#
# No-op when the session CWD is not inside a worktree.

INPUT=$(cat)
TOOL=$(echo "$INPUT" | jq -r '.tool_name // empty')
CWD=$(echo "$INPUT" | jq -r '.cwd // empty')

# Only guard file-writing Claude tools. Bash file writes are out of scope;
# the git-policy hook and path-guard hook handle shell commands.
case "$TOOL" in
    Write|Edit|NotebookEdit) ;;
    *) exit 0 ;;
esac

[ -z "$CWD" ] && exit 0

# Detect worktree root from CWD. Must live under .claude/worktrees/<name>/.
# Everything above that is the main repo root.
if [[ "$CWD" =~ ^(.*)/\.claude/worktrees/([^/]+)(/|$) ]]; then
    MAIN_REPO_ROOT="${BASH_REMATCH[1]}"
    WORKTREE_ROOT="${MAIN_REPO_ROOT}/.claude/worktrees/${BASH_REMATCH[2]}"
else
    # Not in a worktree -- nothing to enforce.
    exit 0
fi

FILE_PATH=$(echo "$INPUT" | jq -r '.tool_input.file_path // empty')
[ -z "$FILE_PATH" ] && exit 0

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

# Resolve relative paths against the session CWD.
if [[ "$FILE_PATH" != /* ]]; then
    FILE_PATH="${CWD}/${FILE_PATH}"
fi

# Canonicalize: resolve ".." and symlinks by walking to the nearest existing
# ancestor (the target may be a new file). Then join with the tail.
dir="$FILE_PATH"
while [ ! -d "$dir" ]; do
    dir=$(dirname "$dir")
done
tail="${FILE_PATH#"$dir"}"
tail="${tail#/}"
if [ -n "$tail" ]; then
    RESOLVED="$(cd "$dir" && pwd -P)/${tail}"
else
    RESOLVED="$(cd "$dir" && pwd -P)"
fi

# Canonicalize the worktree/main anchors too so symlinks in either side
# do not cause spurious mismatches.
if [ ! -d "$WORKTREE_ROOT" ] || [ ! -d "$MAIN_REPO_ROOT" ]; then
    # Anchors missing -- be conservative and allow. The worktree may have
    # just been removed; do not block legit edits on the main repo.
    exit 0
fi
RESOLVED_WORKTREE="$(cd "$WORKTREE_ROOT" && pwd -P)"
RESOLVED_MAIN="$(cd "$MAIN_REPO_ROOT" && pwd -P)"

# Allow anything inside the active worktree.
if [[ "$RESOLVED" == "${RESOLVED_WORKTREE}/"* ]] || [[ "$RESOLVED" == "$RESOLVED_WORKTREE" ]]; then
    exit 0
fi

# Deny writes that fall inside the main repo tree (including sibling
# worktrees) but outside the active worktree.
if [[ "$RESOLVED" == "${RESOLVED_MAIN}/"* ]] || [[ "$RESOLVED" == "$RESOLVED_MAIN" ]]; then
    deny "Blocked: '$RESOLVED' is inside the main repo ($RESOLVED_MAIN) but outside the active worktree ($RESOLVED_WORKTREE). Parallel-agent workers must only write inside their worktree. Retarget the path under $RESOLVED_WORKTREE."
fi

# Outside the project tree entirely -- not this hook's concern.
exit 0
