---
name: projd-hands-off
description: "Launch parallel agents on parallelizable features. Each agent gets an isolated worktree and works autonomously, with automated tests as the quality gate."
user-invocable: true
disable-model-invocation: true
argument-hint: "[--dry-run]"
---

You are a coordinator launching parallel agents to work on independent features autonomously.

## Context

.projd/agent.json:
!`./.projd/scripts/skill-context.sh agent-json`

Features:
!`./.projd/scripts/skill-context.sh features`

Current branch: !`./.projd/scripts/skill-context.sh branch`

CLAUDE.md overview:
!`./.projd/scripts/skill-context.sh claude-md`

## Arguments

`$ARGUMENTS`

## Instructions

### 1. Read dispatch config

Read `.projd/agent.json` and extract the dispatch settings:
- `dispatch.max_agents`: maximum concurrent agents (default 20 if missing)
- `dispatch.auto_review`: whether to auto-review and merge PRs (default false if missing)

Also read the git settings for branch prefix and push permissions.

### 2. Identify parallelizable features

From the feature summary above:
- Filter: `status == "pending"`
- Filter: `blocked_by` is empty, OR every ID in `blocked_by` has `status: "complete"`
- Group into parallelizable sets: features that have no mutual `blocked_by` relationships can run simultaneously

### 3. Dry run check

If `$ARGUMENTS` contains `--dry-run`:
- Present the dispatch plan as a table: feature id, name, priority, will be dispatched in parallel?
- Show dispatch config: max_agents, auto_review
- Note how many waves are needed if eligible features exceed max_agents
- Do NOT spawn any agents. Stop here.

### 4. Read full feature files

For each feature to dispatch, read the full `.projd/progress/{id}.json` to get acceptance criteria and description.

### 5. Spawn worker agents

For each feature (up to `max_agents` concurrent):

Spawn an Agent with:
- `isolation: "worktree"` -- gives each agent its own git working directory
- `run_in_background: true` -- agents work in parallel

Determine the **base branch** from the "Current branch" in the Context section above. This is the branch all feature branches will be created from and all PRs will target.

Each agent prompt must include:
- The feature ID, name, and full description
- All acceptance criteria
- The branch prefix from .projd/agent.json (agent must create branch `{prefix}{feature-id}`)
- The **base branch** (so the agent knows what to target for PRs)
- Instructions to:
  1. Create the feature branch: `git checkout -b {prefix}{feature-id}`
  2. Update `.projd/progress/{feature-id}.json` with `status: "in_progress"`, `branch`, and `"base_branch": "{base_branch}"`
  3. Implement all acceptance criteria
  4. Run `./.projd/scripts/smoke.sh` to verify
  5. Commit all changes with descriptive messages
  6. If smoke passes and all criteria met: set `status: "complete"`, push branch, create PR via `gh pr create --base {base_branch}`. Include a **Test plan** section in the PR body: run every test you can (smoke, unit tests, lint, syntax checks) and mark results with `[x]`/`[ ]`. Only leave unchecked items that require manual testing.
  7. If incomplete: set notes with progress, write .projd/HANDOFF.md
- Key conventions from CLAUDE.md (code style, test patterns)

If more eligible features than `max_agents`, dispatch in waves -- wait for the current batch to finish before starting the next.

### 6. Collect worker results

Wait for all worker agents to complete. For each:
- Check if the feature was marked complete
- Note any failures or partial completions
- Collect PR URLs if created

### 7. Auto-review (conditional)

Skip this step entirely if `auto_review` is `false`.

For each worker that completed successfully and created a PR, spawn a **review agent**:

- `isolation: "worktree"` -- isolated working directory
- `run_in_background: true` -- reviewers work in parallel

Each review agent prompt must include:
- The PR number and URL
- The feature ID and all acceptance criteria
- Instructions to:

  **Step A -- Checkout and verify:**
  1. Check out the PR: `gh pr checkout <number>`
  2. Run `./.projd/scripts/smoke.sh`
  3. Review the diff (`gh pr diff <number>`) against each acceptance criterion
  4. Determine: PASS (all criteria met, smoke passes) or FAIL (with specific issues)

  **Step B -- If PASS:**
  1. Merge the PR: `gh pr merge <number> --squash --delete-branch`
  2. In the **main repo** (not the worktree), pull the latest base branch and update `.projd/progress/{feature-id}.json`: set `"status": "complete"`; commit this update to the base branch
  3. Remove the feature's worktree: `git worktree remove --force <path>` (if it exists)
  4. Report: merged successfully

  **Step C -- If FAIL:**
  1. Assess each issue: is the fix trivial (1 line change) or non-trivial?
  2. **Trivial fix** (1 LOC): fix it directly, commit with a descriptive message, push to the PR branch
  3. **Non-trivial fix**: spawn a subagent (with `isolation: "worktree"`) that receives:
     - The PR branch name
     - The specific issues found
     - The acceptance criteria
     - Instructions to: check out the branch, fix the issues, run smoke, commit, and push
  4. After fixes (direct or via subagent): re-run `./.projd/scripts/smoke.sh`
  5. If smoke passes now:
     a. Merge the PR via `gh pr merge <number> --squash --delete-branch`
     b. In the **main repo** (not the worktree), pull the latest base branch and update `.projd/progress/{feature-id}.json`: set `"status": "complete"`; commit this update to the base branch
     c. Remove the feature's worktree: `git worktree remove --force <path>` (if it exists)
  6. If still failing: leave a review comment on the PR (`gh pr review <number> --comment --body "<issues>"`) and report as needs-attention

### 8. Collect review results

Wait for all review agents to complete. For each:
- Record whether the PR was merged, fixed-and-merged, or flagged for attention
- Collect any review comments or fix descriptions

### 9. Report

Present a summary table:

| Feature | Worker | Review | PR | Notes |
|---------|--------|--------|----|-------|
| feature-id | complete/partial/failed | merged/fixed/needs-attention/skipped | URL | ... |

## Guardrails

- Respect `max_agents` from `.projd/agent.json` dispatch config. Default to 20 if not set.
- Do NOT retry failed worker agents automatically. Report failures for the operator to decide.
- Each agent gets a focused, single-feature prompt. Do not include unrelated features or exploration instructions.
- Review agents must not modify code unrelated to the issues they found.
- If `allow_push` is `false` in .projd/agent.json, skip both pushing and auto-review (there are no PRs to review).

## Output

End with:

> Dispatched [N] agents. [M] completed, [K] merged, [J] need attention.
