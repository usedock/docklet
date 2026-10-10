# CLAUDE.md

Read [AGENTS.md](AGENTS.md): layout, Linux-only checks (on Windows `GOOS=linux go vet ./...` and `go test -exec=true ./...`; real tests run in CI; never in WSL), fakes only in `*_test.go`, and product truth.

Workflow: `.claude/skills/github-workflow/SKILL.md`. Review: `.claude/skills/pr-review/SKILL.md` with the `reviewer` agent. Agents live in `.claude/agents/`.
