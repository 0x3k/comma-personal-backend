#!/usr/bin/env bash
set -euo pipefail

# cleanup-agent.sh -- Remove a parallel-agent worktree and its feature branch
# after the PR has been merged.
#
# Must be run from the main repo (not from inside the worktree) -- `gh` and
# `git worktree remove` both refuse when the current process is inside the
# worktree being removed.
#
# Usage:
#   ./.projd/scripts/cleanup-agent.sh <branch>
#
# Example:
#   ./.projd/scripts/cleanup-agent.sh agent/my-feature

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
cd "$PROJECT_DIR"

BRANCH="${1:-}"
if [ -z "$BRANCH" ]; then
    echo "Usage: $0 <branch>" >&2
    exit 2
fi

# Find the worktree path tied to this branch, if any.
WORKTREE_PATH=$(git worktree list --porcelain | awk -v b="refs/heads/$BRANCH" '
    $1 == "worktree" { path = $2 }
    $1 == "branch"   && $2 == b { print path; exit }
')

if [ -n "$WORKTREE_PATH" ]; then
    echo "Removing worktree: $WORKTREE_PATH"
    # Try a single -f first; if the worktree is locked (the Claude Code harness
    # locks its agent worktrees), retry with -f -f.
    if ! git worktree remove -f "$WORKTREE_PATH" 2>/dev/null; then
        git worktree remove -f -f "$WORKTREE_PATH"
    fi
else
    echo "No worktree found for branch '$BRANCH' (skipping worktree removal)."
fi

if git show-ref --verify --quiet "refs/heads/$BRANCH"; then
    echo "Deleting local branch: $BRANCH"
    git branch -D "$BRANCH"
else
    echo "No local branch '$BRANCH' (skipping branch delete)."
fi

# Clean up stale worktree metadata from worktrees removed manually.
git worktree prune

echo "Cleanup complete for $BRANCH."
