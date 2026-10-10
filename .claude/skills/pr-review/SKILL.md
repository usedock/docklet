---
name: pr-review
description: usedock/docklet pull request review checklist - correctness, security, runtime safety, API contract, tests. Use for "/pr-review <number|branch>", "review this PR", or a pre-submit review of local changes. Run by the Opus reviewer agent.
---

# PR review

Argument: a PR number, a branch name, or nothing (review local changes against `main`).

## 1. Scope

```bash
gh pr view <n> --json title,body,baseRefName,headRefName,files   # PR
gh pr diff <n>                                                   # PR diff
git diff origin/main...HEAD && git diff                      # local changes
```

Read the linked issue. Read every changed file in full, not only the hunks, plus the callers of changed functions. Note which packages are touched.

## 2. Checklist

Only check the sections the diff touches.

### All
- Does the change do what the issue's Acceptance list says, and nothing unrelated?
- Dead code, commented-out code, duplicated logic that already exists elsewhere.
- Comments are short one-liners explaining why; no paragraph comments; no emojis in code, comments or commits.
- No secrets, tokens, `.env` contents, or private files committed; test fixtures are fake.
- Product vocabulary: `Dock`, `Snapshot`, `Branch`, `Template` ... never `box` or `computer`. Dock never installs agents in a guest.

### Runtime
- `go test` coverage for behavior changes; fakes only in `*_test.go`.
- Crash safety: operations idempotent across restarts (journal/dedup tokens), no orphaned VMs, containers, TAP/veth devices, netns or mounts on error paths.
- Isolation: nothing widens what a guest can reach on the host or other docks (egress rules, forward rules, mounts, privileges).
- Fleet channel stays outbound-only; no new inbound listener.
- Changes to the guest agent or image inputs: note that existing docks need stop + resume, and that re-running `scripts/bootstrap-host.sh` rebuilds the image.

### API contract (`api/v1`)
- Request/response shape changes are backward compatible with the control plane release that supports this docklet, or called out in the PR.
- Sizes use `Resources{cpus,memoryMiB,diskGiB}`; runtime is `microvm` or `gvisor`.
- Linux-only code carries build tags where needed; fakes live only in `*_test.go`.

### Scripts and deploy (`scripts/`, `DEPLOY.md`, compose files)
- New env vars are documented in `DEPLOY.md`; defaults safe.
- `bootstrap-host.sh` stays re-runnable; shell scripts keep LF line endings and pass `bash -n`.

## 3. Verify

- Run the checks (`.claude/skills/github-workflow/SKILL.md` step 4) or read CI results (`gh pr checks <n> -R usedock/docklet`) and record them. Never run `go test` in WSL.
- For each candidate finding, confirm it in the code and write the concrete failure: input or state, then the wrong result. Drop anything you cannot confirm.

## 4. Output

```markdown
## Review: <PR title or branch>

**Verdict:** approve | changes requested

### Blocking
1. `path:line` - <defect>. Failure: <concrete scenario>. Fix: <suggestion>.

### Non-blocking
1. `path:line` - <issue and suggestion>.

### Checks
- `<command>`: pass | fail (<summary>)
```

Blocking means a correctness bug, security or tenancy hole, data loss risk, missing test for a behavior change, or a broken check. Everything else is non-blocking. Keep empty sections with "None."
