---
name: github-workflow
description: The usedock/docklet lifecycle for a piece of work on GitHub - create the issue, branch, implement with subagents, run checks, commit, open the PR, review, address feedback, merge and clean up. Use when starting any feature, fix or refactor, or when the user says "create an issue", "open a PR", "ship this" or "merge it".
---

# GitHub workflow

Repo: `usedock/docklet` (public), default branch `main`. CI (`.github/workflows/ci.yml`) runs `gofmt`, `go vet`, `go test`, shell syntax checks and gitleaks on every PR. Use `gh`. The docklet is Linux-only: on Windows never run `go test` in WSL (it lags the owner's PC); compile-only locally, real tests in CI.

## 0. Epic (big or multi-part work)

Audits, big features, migrations, and any plan with more than one PR start with a parent tracking issue (the epic) plus one sub-issue per piece of work. Small single-PR changes skip this and go straight to step 1.

1. Create the epic. Title `epic: <goal>`, labels `epic` plus the usual type label. Body:

```markdown
## Goal
The outcome, and why now.

## Scope
What is in and out. For an audit: the areas covered.

## Plan
How the work is split (each item becomes a sub-issue), the order, and dependencies between them.

## Done when
- [ ] Every sub-issue closed
- [ ] Follow-up notes from reviews filed or resolved
```

2. Create each piece of work as a normal issue (step 1 template, its own priority label), then attach it to the epic as a sub-issue. The API takes the child's numeric `id`, not its number:

```bash
child_id=$(gh api repos/usedock/docklet/issues/<child-number> -q .id)
gh api -X POST repos/usedock/docklet/issues/<epic-number>/sub_issues -F sub_issue_id="$child_id"
```

GitHub then shows progress on the epic (`sub_issues_summary`). Add the epic and every sub-issue to the project board. Findings discovered later (in reviews, during implementation) become new sub-issues of the same epic, not loose issues.

3. One sub-issue per PR where possible; group tightly coupled sub-issues into one PR and list them all with `Closes #n`.
4. Close the epic when every sub-issue is closed, with a short summary comment linking the PRs.

To see an epic's children: `gh api repos/usedock/docklet/issues/<epic-number>/sub_issues -q '.[] | "\(.number) \(.state) \(.title)"'`.

## 1. Issue

Every change starts from an issue, except trivial edits. Pieces of a larger effort are sub-issues of its epic (step 0).

```bash
gh issue create -R usedock/docklet \
  --title "<area>: <outcome in plain words>" \
  --label <bug|enhancement|documentation> \
  --body-file <file>
```

Body:

```markdown
## Problem
What is wrong or missing, and who it affects.

## Proposal
What will change. Name the packages touched (`internal/*`, `api/v1`, `cmd/`, `scripts/`, `DEPLOY.md`).

## Acceptance
- [ ] Observable outcome 1
- [ ] Observable outcome 2
- [ ] Tests: what new go tests prove it

## Notes
Links, constraints, out of scope.
```

Before writing it, dispatch the `explorer` agent to find the code involved so the Proposal names real files. Read `AGENTS.md` (Product truth) for anything user-facing.

## 2. Branch

```bash
git switch main && git pull --ff-only
git switch -c <type>/<short-kebab-summary>
```

`<type>` is `feat`, `fix`, `refactor`, `docs` or `chore`, e.g. `fix/gvisor-veth-cleanup`. One issue per branch. If the working tree has someone's uncommitted changes, stop and ask before switching.

## 3. Implement

Orchestrated with the agents in `.claude/agents/`:
1. `explorer` (Haiku) maps the code and the pattern to copy.
2. The orchestrator (Opus) splits the work into units that do not share files.
3. `implementer` (Sonnet) agents build each unit with tests, in parallel where independent.
4. The orchestrator reads the combined diff and reruns checks itself.

## 4. Checks

Run from the repo root (see `AGENTS.md`):

| Where | Commands |
|---|---|
| Linux / CI | `go vet ./...`, `go test -timeout 8m ./...`, `gofmt -l .` (must print nothing), `for f in scripts/*.sh; do bash -n "$f"; done` |
| Windows | `$env:GOOS='linux'; go vet ./...` and compile-only `go test -exec=true ./...`; never `go test` in WSL; wait for CI for the real run |

Watch CI with `gh pr checks <n> -R usedock/docklet --watch`. Any failure blocks the PR.

## 5. Self-review

Dispatch the `reviewer` agent (Opus) on the local diff (`git diff main...HEAD` plus uncommitted changes) using `/pr-review`. Send blocking findings back to an implementer, then re-review. Do not open the PR with blocking findings.

## 6. Commit

Conventional commits, matching history: `feat(scope): ...`, `fix(scope): ...`, `refactor`, `docs`, `chore`. Scope is the package (`fc`, `host`, `agent`, `snapmgr`, `scripts`). Subject is lowercase, imperative, says what changed for the user. No emojis.

```bash
git add <specific paths>      # never `git add -A`; keep .env and private files out
git commit -m "fix(host): ..."
```

Prefer a few meaningful commits over many WIP commits; the PR is squash-merged anyway.

## 7. Pull request

```bash
git push -u origin HEAD
gh pr create -R usedock/docklet --base main \
  --title "<same style as the commit subject>" \
  --body-file <file>
```

Body:

```markdown
Closes #<issue>

## What
Summary of the change in 2-4 bullets.

## Why
The problem from the issue, in one or two sentences.

## How to verify
- Checks run: <exact commands and results>
- Manual steps: <e.g. re-run bootstrap-host.sh on a test host>

## Risk
Config/env changes, host re-bootstrap or re-enroll needed, `api/v1` contract changes that need a matching control plane release.
```

Use `--draft` while work is still in progress.

## 8. Review

Run `/pr-review <number>` with the `reviewer` agent and post the result:

```bash
gh pr review <number> --comment --body-file <review.md>
```

Use `--request-changes` when there are blocking findings. Address each finding with a new commit (do not force-push over a review in progress), reply on the thread with what changed, and re-run the review until it is clean.

## 9. Merge

Only when checks pass and the review has no blocking findings, and only after the user approves the merge:

```bash
gh pr merge <number> --squash --delete-branch
git switch main && git pull --ff-only
```

The squash title must follow the commit convention. `Closes #<issue>` in the body closes the issue automatically; verify with `gh issue view <issue>`.

## 10. After merge

- Deploy-affecting changes: say what hosts need (re-run `scripts/bootstrap-host.sh`, stop + resume of docks for guest agent changes).
- Delete any local branches left over: `git branch -d <branch>`.
