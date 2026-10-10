# Contributing

Thanks for helping build Dock.

## Before you start

- For anything bigger than a small fix, open an issue first so we can agree on the approach.
- Report security problems to [security@usedock.io](mailto:security@usedock.io), never in an issue. See [SECURITY.md](SECURITY.md).
- Follow the [code of conduct](CODE_OF_CONDUCT.md).

## Contributor License Agreement

Every contributor signs the [Dock CLA](https://github.com/usedock/dock/blob/main/CLA.md) once. It covers all repositories in the usedock organization. On your first pull request the CLA bot posts a comment; reply with the sentence it gives you to sign. You keep the copyright in your work.

## Development

Linux only, Go 1.25.

```sh
go build ./...
go test ./...       # every behavior change ships with go test coverage
GOOS=linux go vet ./...
```

Fakes live in `*_test.go`, never in production code. A change to `api/v1` updates its JSON contract fixtures in the same pull request.

## Pull requests

- One change per pull request, with tests.
- Conventional commit titles: `feat(scope): ...`, `fix(scope): ...`, `docs: ...`.
- Short comments only where the why is not obvious; no emojis in code, comments or commits.
- A maintainer reviews every pull request before merge.
