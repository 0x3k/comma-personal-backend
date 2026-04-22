# projd Workflow

This file is auto-loaded by Claude Code alongside the root CLAUDE.md.
It contains projd workflow instructions, agent controls, and session conventions.

## Agent Controls

Read `.projd/agent.json` before any git operation. It defines what you are allowed to do:

```json
{
  "git": {
    "branch_prefix": "agent/",
    "protected_branches": ["main", "master"],
    "allow_push": "feature",
    "allow_force_push": false,
    "auto_commit": true
  },
  "dispatch": {
    "max_agents": 20,
    "auto_review": false
  }
}
```

Git rules:
- **Branch prefix**: Always create branches with the configured prefix (e.g., `agent/feature-name`)
- **Protected branches**: Never commit directly to these branches. Always work on a feature branch.
- **Push**: `"feature"` allows pushing branches that start with the configured prefix. `false` blocks all pushes. `true` allows all pushes.
- **Force push**: If `allow_force_push` is false, never force-push under any circumstances.
- **Auto commit**: If true, commit after each meaningful unit of work. If false, stage changes but let the operator commit.

Dispatch rules:
- **Max agents**: Maximum number of parallel agents that `/projd-hands-off` will spawn concurrently. If more features are eligible, dispatch in waves.
- **Auto review** (vibes mode): If true, `/projd-hands-off` spawns a reviewer agent for each completed PR. The reviewer runs smoke tests, verifies acceptance criteria, fixes trivial issues inline (or spawns a subagent for larger fixes), and merges passing PRs automatically -- updating `.projd/progress/{id}.json` to `"complete"` and cleaning up the worktree after each merge. If false, PRs are left for the operator to review.

These rules are enforced by a PreToolUse hook in `.claude/hooks/check-git-policy.sh`. The hook blocks violations before commands execute.

## Session Conventions

### Skills (projd)

This project includes a skill family called `projd` (project daemon) that automates the workflow:

**Human-facing** (invoked by the operator):
- `/projd-plan` -- Create feature files from requirements. Does not implement.
- `/projd-hands-on` -- Select a feature, create its branch, present acceptance criteria. You stay in the loop.
- `/projd-hands-off` -- Launch parallel agents on parallelizable features. Tests are the quality gate.
- `/projd-adopt` -- Add projd to an existing project (user-level, install with `install-skill.sh`).

**Agent-facing** (auto-triggered by Claude):
- `projd-start` -- Orient at session start: read state, handoff, smoke test, identify feature.
- `projd-end` -- Wrap up: commit, update feature, push branch, create PR if complete.

### Session Start

The `projd-start` skill handles orientation automatically. It:

1. Runs `./.projd/scripts/status.sh` to see git state, progress, and handoff context
2. Reads `.projd/HANDOFF.md` for context from the previous session (if it exists)
3. Runs `./.projd/scripts/smoke.sh` to verify nothing is currently broken
4. Identifies the current feature from the branch, or finds the highest-priority pending unblocked feature

### Session End

The `projd-end` skill handles wrap-up automatically. It:

1. Commits completed work with descriptive messages (if `auto_commit` is true)
2. Updates the feature file in `.projd/progress/`:
   - Sets `"status": "complete"` if all acceptance criteria are met and smoke passes
   - Keeps `"status": "in_progress"` if partially done, with notes
3. Pushes the feature branch and creates a PR (if `allow_push` permits)
4. Writes `.projd/HANDOFF.md` if work is incomplete, or deletes it if complete
5. Runs `./.projd/scripts/smoke.sh` as a final gate

### Feature Workflow

Each feature is a JSON file in `.projd/progress/`. The lifecycle:

```
pending --> in_progress --> complete --> PR merged
```

Use `/projd-hands-on` to start a feature:
1. Checks that its `blocked_by` list is resolved (all blockers are `complete`)
2. Creates a branch: `git checkout -b {branch_prefix}{feature-id}`
3. Sets `"status": "in_progress"` and `"branch"` in the feature file
4. Presents acceptance criteria as the work plan

Implementation:
5. Implement against the acceptance criteria
6. Verify with `./.projd/scripts/smoke.sh` and tests
7. `projd-end` marks complete and creates a PR when all criteria pass

### Parallel Sessions

Use `/projd-hands-off` to run multiple agents in parallel:

1. Scans `.projd/progress/` for pending unblocked features
2. Groups features that can run simultaneously (no mutual `blocked_by`)
3. Spawns isolated subagents, each in its own git worktree
4. Each agent works on one feature: creates branch, implements, commits, PRs

Rules:
- Each agent works in its **own worktree** on its **own branch**
- Feature files in `.projd/progress/` are per-feature, so there are no write conflicts
- Never pick a feature whose `blocked_by` dependencies are not yet `complete`
- Never pick a feature that already has `"status": "in_progress"` (another agent owns it)

Use `--dry-run` to preview the dispatch plan without spawning agents.

### Known Gotchas

- **Resolving conflicts without force-push**: `.projd/agent.json` sets `allow_force_push: false`, and GitHub's "Update branch" button returns 422 on conflicts. When a feature branch falls behind main, run `git merge origin/main` on the feature branch (NOT `git rebase`). The PR's squash-merge flattens the extra merge commit.
- **Where new backend code goes**: `cmd/server/main.go` is a thin bootstrap. New HTTP route registrations belong in `cmd/server/routes.go`; new background goroutines belong in `cmd/server/workers.go`; shared helpers (e.g. `envBool`) live in `cmd/server/env.go`. Shared deps are threaded via the `deps` struct in `cmd/server/deps.go`. Adding code to `main.go` is almost always wrong and creates merge conflicts across parallel feature branches.
- **Worktree writes are hook-guarded**: `.claude/hooks/check-worktree-isolation.sh` blocks `Write`/`Edit` calls from a worktree session whose target resolves into the main repo tree (or a sibling worktree). If the hook denies an edit, retarget the path under the active worktree rather than trying to bypass it.

### Planning Sessions

Use `/projd-plan` for planning:

1. Provide requirements (description or file path)
2. The skill breaks them into discrete features with acceptance criteria
3. Sets `blocked_by` to express dependencies
4. Identifies which features can be parallelized
5. Does NOT start implementation -- the plan is the deliverable

### Session Discipline

- **One feature per session**: Pick one item, implement it, test it, commit it. Resist the urge to refactor adjacent code or start additional features.
- **Verify before marking complete**: Run `./.projd/scripts/smoke.sh` and any relevant tests. Do not mark a feature as `complete` until all acceptance criteria pass.
- **Incremental commits**: Commit after each meaningful unit of work. Descriptive commit messages enable rollback and make the next session's context-gathering faster.
- **Leave the codebase clean**: Every commit should be mergeable to main. No half-implemented features, no commented-out code, no TODO breadcrumbs without corresponding feature files.

## Sub-Projects

If `projects.json` exists at the root, this is a multi-project workspace. Each sub-project is a self-contained directory with its own `CLAUDE.md`, `.projd/progress/`, and scripts.

When working in a sub-project:
1. Read both the root `CLAUDE.md` (this file) and the sub-project's `CLAUDE.md`
2. Use the sub-project's `.projd/progress/` for project-specific features
3. Use the root `.projd/progress/` for cross-cutting features that span sub-projects
4. Run the sub-project's `./.projd/scripts/smoke.sh` for local checks; run the root `./.projd/scripts/smoke.sh` for the full suite

## Pre-Commit Quality Gates

This project uses [Lefthook](https://github.com/evilmartians/lefthook) for pre-commit hooks. Install with:

```bash
./.projd/scripts/init.sh
```

Git policies from `.projd/agent.json` are enforced by a Claude Code PreToolUse hook (`.claude/hooks/check-git-policy.sh`). The Lefthook pre-push hook provides a secondary guard for direct git usage outside Claude Code.

## First-Time Setup

At the start of a session, check if the development environment is initialized:

1. Run: `test -f .git/hooks/pre-commit && echo "initialized" || echo "not initialized"`
2. If **initialized**, skip this section entirely.
3. If **not initialized**, check: `test -f .projd/no-init && echo "declined" || echo "not declined"`
4. If **declined**, skip this section entirely.
5. Otherwise, tell the user the dev environment has not been initialized and ask if they want to run `./.projd/scripts/init.sh` (installs git hooks and dependencies, safe to re-run).
   - If they say **yes**: run `./.projd/scripts/init.sh`
   - If they say **no**: run `touch .projd/no-init` and do not ask again.

## Code Conventions (projd)

- Never use emojis in code or comments
- Use `git -C <path>` instead of `cd <path> && git` when running git commands in another directory. This avoids compound shell commands and matches the `Bash(git *)` auto-approve rule.
