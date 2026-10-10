# Contributing

Thanks for helping build Dock.

## Before you start

- For anything bigger than a small fix, open an issue first so we can agree on the approach.
- Report security problems to [security@usedock.io](mailto:security@usedock.io), never in an issue. See [SECURITY.md](SECURITY.md).
- Follow the [code of conduct](CODE_OF_CONDUCT.md).

## Contributor License Agreement

Every contributor signs the [Dock CLA](https://github.com/usedock/dock/blob/83279d37875c012493ef59b52f91a88c63bbae94/CLA.md). Signatures are stored per repository, so the CLA bot asks on your first pull request in each repository; reply with the sentence it gives you to sign. You keep the copyright in your work.

## Development

These steps apply once the code lands in this repo. Linux only, Go 1.25.

```sh
go build ./...
go test ./...       # every behavior change ships with go test coverage
GOOS=linux go vet ./...
```

Fakes live in `*_test.go`, never in production code. Once contract fixtures exist, a change to `api/v1` updates them in the same pull request.

## Pull requests

- One change per pull request, with tests.
- Conventional commit titles: `feat(scope): ...`, `fix(scope): ...`, `docs: ...`.
- Short comments only where the why is not obvious; no emojis in code, comments or commits.
- A maintainer reviews every pull request before merge.
