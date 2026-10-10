# AGENTS.md

Guidance for AI coding agents working in `usedock/docklet`. `CLAUDE.md` points here.

## What this is

The host agent for [Dock](https://github.com/usedock/dock), the open-source sandbox infrastructure. One docklet runs per machine. It boots, pauses, snapshots, resumes and destroys docks, and reports what the host has. The control plane lives in [usedock/dock](https://github.com/usedock/dock); this repo never contains it.

## Layout

- `cmd/docklet/` - the daemon entry point. `cmd/gen-fast-agent/` - generator for the guest agent.
- `api/v1/` - the versioned contract with the control plane (types shared with the fleet API).
- `internal/` - `agent` (fleet channel, outbound only), `fc` (Firecracker driver), `container` (gVisor driver), `host` (orchestration, allocator, cgroups, pool, reconciler), `snapmgr` (snapshots and deltas), `net`, `egress`, `disk`, `store`, `server` (loopback API), `ready`, `stats`, `idem`, `fleetauth`, `cpufamily`, `exec`, `ids`.
- `scripts/` - `bootstrap-host.sh` (one-command host install), image and kernel builders, gVisor image.
- `DEPLOY.md` - the host deploy guide. `docker-compose.test.yml` - a test-host stack (Restate + MinIO).

## Platform and checks

Linux only (KVM, cgroups, netns). Go version comes from `go.mod`.

- Linux and CI: `go vet ./...`, `go test -timeout 8m ./...`, `gofmt -l .` (must print nothing), `for f in scripts/*.sh; do bash -n "$f"; done`.
- Windows: `$env:GOOS='linux'; go vet ./...` and compile-only `go test -exec=true ./...`. Real tests run in GitHub Actions (`.github/workflows/ci.yml`).
- Never run Go tests in WSL on the owner's PC (it lags the machine), and never `wsl --shutdown`.

## Conventions

- Every behavior change ships with `go test` coverage that fails without it.
- Fakes live only in `*_test.go`, never in production code.
- Comments are short one-liners for the why; no emojis in code, comments or commits.
- Conventional commits: `feat(scope): ...`, `fix(scope): ...`, `docs: ...`. Shell scripts keep LF endings.
- Never commit secrets or real tokens; fixtures are fake.
- A change to `api/v1` must stay compatible with the control plane release that supports this docklet, or say so in the PR.

## Product truth

- Dock sizes are `Resources{cpus, memoryMiB, diskGiB}`.
- Runtime is `microvm` (Firecracker, KVM) or `gvisor` (runsc, Docker).
- Images are minimal. Dock never installs agents or tools into a guest.
- The fleet channel is outbound only: no inbound port, no per-host address. A host is not offered work until readiness passes.
- Vocabulary: Dock, Snapshot, Branch, Template; never `box` or `computer`.
- Plan: https://claude.ai/code/artifact/c93a7b9f-45d6-45f7-aeed-15e8ed204830

## Workflow

Work follows `.claude/skills/github-workflow/SKILL.md` (issue, branch, implement, checks, review, PR, merge) and every PR is reviewed with `.claude/skills/pr-review/SKILL.md` by the `reviewer` agent (`.claude/agents/`). Merge only on a clean review, green CI, and the owner's go-ahead.
