# docklet

The host agent for [Dock](https://github.com/usedock/dock). One docklet runs on each machine that hosts docks. It boots, pauses, snapshots, resumes and destroys docks, and reports what the host actually has.

> **Status:** the docklet is here. The `dock` CLI and its local engine will live in this repo too; they are not built yet.

## What it does

- Runs docks as Firecracker microVMs (under `jailer`) on Linux with KVM, or as gVisor (`runsc`) containers on Linux with Docker.
- Enforces each dock's CPU and memory with cgroups, gives each dock a fixed-size disk (a sparse rootfs on microvm, a size-limited work volume at `/home/user/work` on gVisor), and gives it a TAP or veth network on a host-local subnet.
- Moves snapshot data to the object store over presigned URLs the control plane mints, or through its object proxy; the host never holds store credentials.
- Connects **outbound only** to the control plane fleet API: it registers, sends heartbeats with its inventory and stats, long-polls for commands and posts results. No inbound port, no per-host address.
- Serves its own HTTP API on loopback only.
- Fails closed: a host is not offered work until its readiness checks pass.

## Platforms

| Host | Engine | Status |
| --- | --- | --- |
| Linux with KVM | Firecracker | Target: first release |
| Linux with Docker | gVisor | Target: first release |
| Windows x86_64 | libkrun on Windows Hypervisor Platform | Planned |
| macOS, Apple silicon | libkrun on Hypervisor.framework | Planned |

A snapshot moves only between hosts with the same engine and CPU architecture.

## Layout

- `cmd/docklet/` - the daemon. `cmd/gen-fast-agent/` - guest agent generator. `cmd/dock/` + `internal/cli/` - the portable `dock` CLI.
- `api/v1/` - the versioned contract with the control plane.
- `internal/` - `agent` (fleet channel), `fc` (Firecracker), `container` (gVisor), `host` (orchestration), `snapmgr` (snapshots), `net`, `egress`, `disk`, `store`, `server` and more.
- `scripts/` - host bootstrap, image and kernel builders.
- `DEPLOY.md` - the full host deploy guide.

## Add a host

One command per machine, run as root on Ubuntu from a checkout of this repo. It installs the runtime, the signature-verified docklet release and a systemd unit, then enrolls the host with your control plane ([usedock/dock](https://github.com/usedock/dock)):

```sh
sudo bash scripts/bootstrap-host.sh \
  --fleet-url https://dock.example.com \
  --fleet-token "$FLEET_TOKEN" \
  --runtime microvm    # or gvisor
```

Pick `microvm` on Linux with KVM or `gvisor` on any Linux with Docker. The script is re-runnable. See [DEPLOY.md](DEPLOY.md) for flags, base images and troubleshooting.

## dock CLI (early, not released yet)

`cmd/dock` is a client for the Dock control plane `/v1` API (not for this daemon). It builds on Linux, macOS and Windows and is not packaged or versioned yet.

```sh
go build -o dock ./cmd/dock
echo "$DOCK_API_KEY" | dock login        # stores the key; input is echoed on a terminal, so pipe it
dock new web --cpus 4 --memory 8gb --disk 40gb --env staging
dock ls
dock exec web -- uname -a                # exits with the command's exit code
dock ssh web                             # runs ssh against the Dock SSH gateway (--print to show it)
dock port web 3000                       # prints the public URL
dock branch web --count 2
dock stop web && dock resume web && dock rm web
```

- Every command accepts `--json` and prints the API data. Errors print `error: <code>: <message>` to stderr and exit 1; usage errors exit 2.
- Sizes: `8gb`, `8g`, `8192mb`; a plain number is MiB for memory and GiB for disk.
- `<dock>` is a name or id. `dock new` reads `./dock.toml` (`name`, `image`, `cpus`, `memory`, `disk`, `environment`); flags override it and unknown keys are an error.
- Contexts: `dock context add|use|ls|rm`. The built-in `cloud` context points at `https://api.usedock.io`. Config lives in `os.UserConfigDir()/dock/config.toml` (mode 0600). `DOCK_API_URL` and `DOCK_API_KEY` override the active context.
- Not yet: `dock cp` (the API has no file endpoint for live docks) and streaming `exec` output (the exec API returns the bounded result when the command ends).

## Development

Linux only, Go 1.25.

```sh
go build ./...
go vet ./...
go test ./...
go test ./internal/host/    # one package
```

On Windows or macOS, `GOOS=linux go vet ./...` and `go test -exec=true ./...` check that everything compiles; CI runs the real tests. Every behavior change ships with `go test` coverage. Fakes live in `*_test.go`, never in production code. `api/v1` is the versioned contract with the control plane; publishing JSON fixtures for every call is planned. Agents working in this repo: see [AGENTS.md](AGENTS.md).

## Contributing

Contributions are welcome. Every contributor signs the CLA; the CLA bot asks on your first pull request in each repository. See `CONTRIBUTING.md`.

## Security

Report vulnerabilities to [security@usedock.io](mailto:security@usedock.io), not in public issues. See `SECURITY.md`. Releases are signed with cosign (keyless, GitHub OIDC) and ship `checksums.txt`; the install script stops if the checksum or signature is missing or bad. See the release verification section of [DEPLOY.md](DEPLOY.md).

## License

[AGPL-3.0-only](LICENSE).

Copyright (C) 2026 Paranjay.
