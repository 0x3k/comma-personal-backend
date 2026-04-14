---
name: projd-end
description: "Session wrap-up: commit work, update feature status, push feature branch, create PR if complete, write handoff if incomplete. Run this when all acceptance criteria are met or the session is ending."
user-invocable: false
---

You are wrapping up a work session. Commit, verify, deliver.

## Context

Current branch: !`./.projd/scripts/skill-context.sh branch`

Working tree:
!`./.projd/scripts/skill-context.sh git-status`

Changes:
!`./.projd/scripts/skill-context.sh git-diff-stat`

.projd/agent.json:
!`./.projd/scripts/skill-context.sh agent-json`

Features:
!`./.projd/scripts/skill-context.sh features`

Token usage: !`./.projd/scripts/skill-context.sh token-usage`

## Instructions

### Step 1: Commit

Check `.projd/agent.json` for `auto_commit`.

If there are uncommitted changes (staged or unstaged):
- If `auto_commit` is `true`: stage relevant files and commit with a descriptive message. Make incremental commits if there are logically separate changes.
- If `auto_commit` is `false`: stage files but do NOT commit. Note that changes are staged for the operator.

### Step 2: Identify and update feature

Match the current branch to a feature file (compare to `branch` field in .projd/progress/*.json).

If matched, read the full feature file and verify EACH acceptance criterion:
- For each criterion, check the codebase to confirm it is met.
- If ALL criteria pass, mark for completion.
- If any fail, keep status as `in_progress` and add notes about what was done and what remains.

### Step 3: Smoke test

Run `./.projd/scripts/smoke.sh`. If it fails:
- Do NOT mark the feature as `complete`, even if acceptance criteria pass.
- Document failures in the feature notes.

### Step 4: Update feature file

If all criteria pass AND smoke passes:
- Set `"status": "complete"` in the feature file.

Otherwise:
- Keep `"status": "in_progress"`.
- Update `"notes"` with what was accomplished and what remains.

### Step 5: Push and PR

Read `allow_push` from `.projd/agent.json`.

If feature is `complete` AND (`allow_push` is `"feature"` or `true`):
1. Push the branch: `git push -u origin {branch}`
2. Create a PR using `gh pr create`:
   - Title: the feature name
   - Body: include a summary of changes, the acceptance criteria as a checked checklist, a **Test plan** section, and a **Token Economy** section with the token usage from the context above (input, output, total).
   - Base branch: read `base_branch` from the feature file. If not set, default to `main`.
   - **Test plan**: list verifiable checks for the PR. Run every test you can execute yourself (unit tests, smoke tests, linting, syntax checks, grep-based verifications) and mark the results with `[x]` (passed) or `[ ]` (failed/skipped). Only leave items unchecked if they require manual or E2E testing you cannot perform. Example:
     ```
     ## Test plan
     - [x] `smoke.sh` passes
     - [x] Unit tests pass (`go test ./...`)
     - [x] No lint errors
     - [ ] E2E: verify login flow in browser

     ## Token Economy
     1.2M input, 45.3k output (1.3M total, 8.5M cache read)
     ```
3. Record the PR URL in the feature file `notes` field.

If `allow_push` is `false`: skip push and PR. Note in output that the operator should handle pushing.

If `gh` is not available: push the branch only, skip PR creation, warn about missing `gh` CLI.

### Step 6: Handoff

If feature is incomplete:
- Create or overwrite `.projd/HANDOFF.md` with:
  - What was done this session
  - Current state (what works, what is broken)
  - Prioritized next steps

If feature is complete:
- Delete `.projd/HANDOFF.md` if it exists.

## Edge Cases

- **No matching feature** (ad-hoc work): Skip step 2 and step 4. Still commit (step 1), smoke test (step 3), and write handoff (step 6).
- **Detached HEAD**: Warn and skip push/PR. Still commit and write handoff.

## Output

If complete:

> Feature `{id}` complete. PR created: {url}. Merge when ready.

If incomplete:

> Session wrapped. .projd/HANDOFF.md written for next session.
