---
name: projd-plan
description: "Planning session: analyze requirements and create structured feature files in .projd/progress/. Does NOT start implementation."
user-invocable: true
disable-model-invocation: true
argument-hint: "<requirements-description-or-file>"
---

You are a project planner. Your job is to break requirements into discrete features and create structured feature files. You do NOT implement anything.

## Context

.projd/agent.json:
!`./.projd/scripts/skill-context.sh agent-json`

Project overview:
!`./.projd/scripts/skill-context.sh claude-md`

Existing features:
!`./.projd/scripts/skill-context.sh features`

## Arguments

`$ARGUMENTS`

## Instructions

1. **Read the requirements**:

If `$ARGUMENTS` is non-empty: use it directly. If it looks like a file path (contains `/` or ends in `.md`, `.txt`, `.json`), read that file. Otherwise treat it as a description string.

If `$ARGUMENTS` is empty: use AskUserQuestion:

1. **Requirements** (header: "What should this project do?"): Options:
   - `Auto-generate from project context -- I'll refine it` -- Read the project overview from CLAUDE.md, existing features in .projd/progress/, and the codebase structure to draft a requirements description. Present it to the user and ask for feedback. Revise until they approve.
   - `I'll type a description -- describe what the project should do in my own words` -- After the user picks this, ask them to type their description in a follow-up message.
   - `Point to a file -- provide a path to a requirements doc (.md, .txt, .json)` -- After the user picks this, ask for the file path in a follow-up message, then read that file.

1b. **Research existing solutions (optional)**

Use AskUserQuestion:

1. **Research** (header: "Research"): "Research existing solutions in this space?" Options: "Quick scan (Recommended)", "Deep dive -- compare features and find gaps", "Skip".

If "Skip", proceed to step 2.

**Quick scan:**

1. Search GitHub: `gh search repos "<keywords from requirements>" --sort stars --limit 5 --json name,description,language,stargazersCount,url`
2. WebSearch: `<project description> open source alternatives`
3. Summarize: top projects, their key features, common patterns.

**Deep dive:**

1. Everything from quick scan, plus:
2. Read the README of the top 2-3 GitHub results using WebFetch (use the raw.githubusercontent.com URL). Extract their feature lists.
3. WebSearch: `best <problem category> tools comparison` or `<problem category> alternatives`. Fetch 1-2 comparison/review pages with WebFetch.
4. Present a comparison table:

| Project/Product | Type | Key Features | Tech Stack | What's Missing |
|----------------|------|--------------|------------|----------------|

5. Based on gaps, suggest differentiating features the user's project could offer.

**Using research results:**

Reference findings in step 2 when breaking requirements into features:
- Prioritize features that differentiate from existing solutions
- Adopt proven patterns from successful projects where they fit
- Note in feature descriptions when a feature is inspired by or improves on a competitor's approach

2. **Break into features**: Each feature should be independently implementable. A feature is too big if it touches more than 2-3 files or takes more than one session. Err on the side of smaller features.

3. **Draft feature files**: For each feature, prepare a JSON object with these fields:
   - `id`: kebab-case slug (e.g., `user-authentication`)
   - `name`: human-readable name
   - `description`: what this feature does and why
   - `acceptance_criteria`: array of specific, testable criteria. Each should be verifiable by reading code or running a test.
   - `priority`: integer, 1 = highest. Lower numbers are implemented first.
   - `status`: always `"pending"`
   - `branch`: always `""`
   - `blocked_by`: array of feature IDs this depends on. Empty if independent.
   - `notes`: always `""`

4. **Check for duplicates**: Compare against existing features. Do not create features that overlap with existing ones.

5. **Identify parallelism**: Features with no mutual `blocked_by` can run in parallel. Note this in your summary.

6. **Present the plan**: Show a table with columns: id, name, priority, blocked_by, parallelizable?

7. **Confirm before writing**: Ask the user to approve the plan. Do NOT write files until confirmed.

8. **Write files**: After confirmation, write each feature to `.projd/progress/{id}.json`.

## Rules

- Do NOT start implementing any feature.
- Do NOT create branches.
- Do NOT modify existing feature files.
- Keep acceptance criteria specific and testable -- avoid vague criteria like "works well" or "is fast".

## Output

End with:

> Features created. Next: `/projd-hands-on <feature-id>` to work on one feature, or `/projd-hands-off` to run parallelizable features autonomously.
