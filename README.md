# docklet

Isolated Linux machines for agents. On your laptop, your servers, or our cloud.

This repo holds the `dock` CLI, with its built-in local engine, and the docklet, the host agent for [Dock](https://github.com/usedock/dock). One docklet runs on each machine that hosts docks. It boots, pauses, snapshots, resumes and destroys docks, and reports what the host actually has.

## Install the CLI

```sh
curl -fsSL https://usedock.io/install | sh     # Linux, macOS
irm https://usedock.io/install.ps1 | iex       # Windows (PowerShell)
```

Both installers verify the release checksums and cosign signatures before installing the `dock` CLI.

## Quickstart

With no account, `dock` runs docks on your own machine (the built-in `local` context). You need QEMU and KVM on Linux (x86_64 or arm64), or QEMU and Windows Hypervisor Platform on Windows x86_64; `dock doctor` tells you what is missing.

```sh
curl -fsSL https://usedock.io/install | sh
dock new demo
dock exec demo -- uname -a
```

The first `dock new` downloads the default `ubuntu-24.04` Dockyard image once. On Windows, install with the PowerShell line above.

![dock quickstart: install, dock doctor, dock new api, dock exec, dock branch, dock ls](docs/assets/quickstart.gif)


The first `dock new` downloads the image; the recording skips that wait.

Docs: [quickstart](https://docs.usedock.io/quickstart), [CLI](https://docs.usedock.io/cli), [local docks](https://docs.usedock.io/local-docks), [Windows](https://docs.usedock.io/local-docks/windows), [Dockyard images](https://docs.usedock.io/dockyard).

Questions and ideas: [GitHub Discussions](https://github.com/usedock/dock/discussions).

> **Status:** the `dock` CLI ships in signed releases (since v0.1.0). From v0.2.0 it runs docks locally with no account on Linux (x86_64, and arm64 from v0.3.0) with KVM and Windows x86_64 with Windows Hypervisor Platform, via QEMU; macOS builds but is untested. The docklet host agent is early, and Windows and macOS fleet hosts are planned.

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
| Linux with KVM | Firecracker | Early, in signed releases |
| Linux with Docker | gVisor | Early, in signed releases |
| Windows x86_64 (fleet host) | docklet daemon on Windows | Planned |
| macOS, Apple silicon (fleet host) | docklet daemon on macOS | Planned |

A snapshot moves only between hosts with the same engine and CPU architecture.

### Local docks (the `dock` CLI, no account)

The `dock` binary carries its own engine and runs docks on your own machine with QEMU. The engine needs QEMU and the machine's hardware accelerator; it never falls back to software emulation on its own.

| Machine | Accelerator | Status |
| --- | --- | --- |
| Linux x86_64 | KVM (`/dev/kvm`) | Available |
| Linux arm64 | KVM (`/dev/kvm`) | Available from v0.3.0 (tested under emulation so far) |
| Windows x86_64 | Windows Hypervisor Platform (WHPX) | Available |
| macOS | Hypervisor.framework (HVF) | Untested, planned |

`dock doctor` checks what is missing. On Windows, enable the platform once (elevated PowerShell, then reboot) and install QEMU:

```powershell
Enable-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform
winget install SoftwareFreedomConservancy.QEMU
```

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

A microvm host boots from the signed Dockyard `ubuntu-24.04` (pulled at bootstrap; `--build-base` or an unreachable Dockyard falls back to building it on the host), so a dock's whole disk can move between hosts and contexts. A snapshot with `full: true` writes a checksummed `rootfs.ext4.zst`, and restoring one verifies it. See [DEPLOY.md](DEPLOY.md#full-snapshots-and-checksums-dock-push--dock-pull).

## dock CLI reference

`cmd/dock` runs docks locally (the built-in `local` context, no account) or talks to the Dock control plane `/v1` API (the `cloud` context and added URLs; not this daemon). It builds on Linux, macOS and Windows; released binaries come from the installer above.

```sh
go build -o dock ./cmd/dock                # or use the installed release
dock doctor                              # what local docks need: QEMU, accelerator, disk, Dockyard
dock images                              # Dockyard images; `dock images pull ubuntu-24.04` downloads one
dock new web                             # with no account this boots a local dock (downloads the image once)
dock login                               # prompts without echo; or: echo "$DOCK_API_KEY" | dock login
dock new web --cpus 4 --memory 8gb --disk 40gb --env staging   # returns once the dock is ready
dock push web --to lab                  # copy a stopped local dock's disk to a control plane context (--name, --stop, --rm)
dock pull web --from lab --name web-local   # copy a fleet dock's disk to a new stopped local dock (Dockyard-image docks only)
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
- Local context: the default until `dock login` stores a key or `DOCK_API_KEY`/`DOCK_API_URL` is set; switch with `dock context use local|cloud`. Local docks live under `$XDG_DATA_HOME/dock` (Linux), `~/Library/Application Support/dock` (macOS) or `%LOCALAPPDATA%\dock` (Windows) (`DOCK_DATA_DIR` overrides): a qcow2 overlay on the cached Dockyard base image, the agent token, forwarded port and pid. `--json` keeps the API's keys; `runtime` is `qemu` and a stopped dock is `archived`. Local differences: `dock new --network off` blocks the dock's network, `dock port` forwards a dock port to `127.0.0.1` in the foreground (no public URL), `dock ssh` opens a shell through the guest agent, `dock branch` copies the stopped disk (a running dock is stopped, copied and resumed; there is no live fork), and `dock stop` powers the guest off and keeps the disk. `exec` returns when the command ends. Settings: `DOCK_QEMU` (qemu-system binary), `DOCK_DOCKYARD_URL` and `DOCK_DOCKYARD_SHA256` (use another image index; it is unpinned and warned about, and verified only when the hash is given). The dock binary pins the images release tag and the sha256 of its `dockyard.json`, and refuses an index that does not match. `DOCK_LOCAL_ACCEL=tcg` allows slow software emulation (tests only; local docks never fall back to it on their own).
- Contexts: `dock context add|use|ls|rm`. The built-in `cloud` context points at `https://api.usedock.io`. Config lives in `os.UserConfigDir()/dock/config.toml` (mode 0600). `DOCK_API_URL`, `DOCK_API_KEY` and `DOCK_WORKSPACE` override the active context.
- Workspace for `dock new`: `--workspace`, then `DOCK_WORKSPACE`, then the context default (`dock context use <name> --workspace <id>`), then your only workspace. With several workspaces and none chosen, `dock new` fails and lists them.
- Push and pull move the disk only (no memory, no running processes), between `local` and a control plane context with a microVM host. `dock push` flattens the stopped dock's disk, compresses it with zstd, checks it with sha256 and uploads it in retried parts; `dock pull` exports, downloads with resume, verifies, and registers a stopped local dock on the Dockyard image it came from. The local token and runtime files are never sent, and a pulled dock gets a new local token.
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
