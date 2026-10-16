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

#### gVisor runtime (Linux without KVM)

On a Linux machine without usable KVM (for example a cloud VM without nested virtualization), opt in to gVisor containers instead: `dock new --runtime gvisor` or `DOCK_LOCAL_RUNTIME=gvisor`. There is no silent fallback: without the flag, local docks stay QEMU only.

- Needs Docker Engine and the gVisor `runsc` runtime registered with it ([install gVisor](https://gvisor.dev/docs/user_guide/install/), then `sudo runsc install` and `sudo systemctl restart docker`). `dock new` checks both before building anything. For `sudo` inside docks, also register a second runtime once (below). The first dock builds a small `dock-local-gvisor:<hash>` image with `docker build` (Ubuntu 24.04, python3, git, curl, sudo); nothing is published.
- Optional, for `sudo` and a persistent rootfs: gVisor ignores setuid bits unless runsc runs with `--allow-suid`, and by default it keeps rootfs writes in an overlay that is discarded when the container stops (`--overlay2=none` sends them to the container layer instead). Register a separate runtime once: `sudo runsc install --runtime=runsc-suid -- --allow-suid --overlay2=none && sudo systemctl restart docker`. It is a second entry for the same runsc binary, so your other runsc containers keep the default and are not affected; only docks created after it exists use it (`dock new` prints a note when it is missing, and `dock doctor` shows which case you are in). A dock keeps the runtime it was created with for life, so existing docks do not gain sudo. Without it gVisor docks work but have no `sudo` (no root, no `apt-get install`) and only `/home/user/work` persists across stop and resume. `runsc-suid` is only used when it points at a runsc binary and was registered with both flags; otherwise it is treated as missing.
- On Linux, the `no_accelerator` error and `dock doctor` point to this option when KVM is unusable. `dock doctor` shows an informational `gvisor` row that never makes it fail.
- With the `runsc-suid` runtime, like a QEMU dock, the whole filesystem persists (with plain `runsc` only `/home/user/work` does): `dock stop` stops the container and keeps it, `dock resume` starts it again, so packages you installed with `sudo apt-get install` survive. `/home/user/work` is also a named Docker volume, so that data outlives a lost container. `dock rm` removes the container, volume and network. A stopped dock holds no Docker network, and keeps its local port: if another program takes that port, `dock resume` fails with `port_in_use` and works again once the port is free.
- `exec`, `ssh` and `port` work as for QEMU docks, through the guest agent on `127.0.0.1`. `dock ls --json` shows `runtime: gvisor`. Commands run as `user`; with the `runsc-suid` runtime `sudo` is passwordless and is root inside the sandbox. `no-new-privileges` is not set on these docks (it would block sudo).
- Isolation: gVisor is a strong sandbox. A user-space kernel written in Go intercepts the dock's system calls and only a small, filtered set reaches the host kernel; Google runs untrusted code on it (GKE Sandbox). It is a different boundary from a hardware VM, not a weak one. Root in the dock is root inside the sandbox, not on the host. The container runs with Docker's default capabilities, never `--privileged`, and with CPU, memory and process limits. Being able to use Docker is root-equivalent on most setups, so anyone who can run Docker on the machine can reach every gVisor dock.
- Real differences from QEMU docks:
  - Each dock gets its own Docker bridge network (`dock-local-<id>-net`), so it cannot reach your other containers or other docks. It reaches the internet, but not the host's own network services: `dock` adds one rule to the host firewall that drops anything the dock starts towards an address the host owns (the bridge gateway, `docker0`, your LAN IP; `127.0.0.1` is not routable from a dock anyway), like a QEMU dock, whose guest refuses the host loopback gateway. See "Host block" below.
  - Host block. Docker runs a short-lived helper container (`--network host`, only `NET_ADMIN` and `NET_RAW`, not `--privileged`; the image already built for the docks, no extra pull) that adds a per-dock chain `DOCK-<id>` and one jump at the top of the host's `INPUT` chain for the dock's bridge (`dkl<id>`, set when the network is created). It lives on the host, outside the sandbox, so root in the dock (sudo) cannot remove it, and it needs no `sudo` from you: whoever can run Docker already has this access. Only that bridge is matched; your other containers and the rest of the firewall are untouched. It is installed before the container starts and removed on `dock stop`, `dock rm` and a failed start, `dock resume` and a reboot-then-resume install it again, and installing twice changes nothing. Replies to connections the host opened (the published agent port, `dock port`) still pass. DNS keeps working because the dock uses the host's upstream resolvers from `/etc/resolv.conf` and only those addresses on port 53 are allowed through, even if one is an address of the host itself (such as a local dnsmasq); a router or LAN resolver is a different machine and is not affected. Ports your other containers publish on the host are blocked the same way (Docker's userland proxy receives them on `INPUT`; with the proxy off, Docker's own network isolation drops them). What stays reachable: the internet and the rest of your LAN (the block is about this machine, not other hosts), and DNS on the resolvers above. IPv6 gets the same rule when `ip6tables` works (docks have no global IPv6 address anyway). Limits: a firewall reload or flush on the host (for example `firewall-cmd --reload`, `ufw reload`, `iptables -F`) removes the rule from a running dock until it is stopped and resumed; a dock started by an older `dock` has no rule until it is resumed once; and if Docker cannot run the helper (some restricted or rootless setups) `dock new` and `dock resume` fail with `host_block_failed` rather than start an unblocked dock. In that case use `--network off`, or set `DOCK_GVISOR_ALLOW_HOST=1` to start docks that can reach services on your machine (a warning is printed). This is not Docker's `gateway_mode_ipv4`: `isolated` needs an internal network (no internet), `routed` and `inhibit_ipv4` cut internet access, and `--internal` still leaves the gateway reachable, so none of them keeps egress while hiding the host.
  - DNS uses the host's upstream resolvers.
  - No Docker or systemd inside by default, and one built-in image instead of Dockyard images.
  - `--disk` is not enforced: the work volume and, with `runsc-suid`, everything written elsewhere in the dock live in Docker's storage (`/var/lib/docker`) with no size limit. `docker container prune` or `docker system prune` deletes a stopped dock's container and everything installed in it; only `/home/user/work` (a named volume) survives.
  - `dock branch` works: the source is stopped if running, its container filesystem is committed to a `dock-local-gvisor-branch:<id>` image and its work volume copied (by a short-lived, network-less runsc container), then the source is resumed and the branch starts as a new dock with its own token, port and network. With the `runsc-suid` runtime the branch carries rootfs changes (installed packages) too; with plain `runsc` only `/home/user/work` carries over. `dock rm` removes the branch image. A committed image keeps a copy of the source's rootfs changes on disk until the source and all branches made from that commit are removed.
  - `--network off` works: the container runs with `--network none`, so it has no internet, no DNS and no route to the host or to other docks (the sandbox keeps only its own loopback). With no published port, `dock` reaches the guest agent through `docker exec -i <container> python3` (a small relay to the agent on the sandbox loopback), so `dock exec`, `dock ssh` and `dock port` work as on any other dock. Each agent call costs one `docker exec`, so they are slower than on a network-on dock. Stop, resume and branch keep the dock network-off, and a network-off dock never creates or connects a Docker network.
  - Not supported: `dock push` and `dock pull`.
  - Some syscall-heavy or file-I/O-heavy workloads are slower than in a VM.
- If the `docker` binary is gone, `dock rm` still removes the record and warns with the names to clean up by hand; if Docker is installed but its daemon is down, it fails with `docker_unavailable` and you can retry after starting Docker.

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
- Local context: the default until `dock login` stores a key or `DOCK_API_KEY`/`DOCK_API_URL` is set; switch with `dock context use local|cloud`. Local docks live under `$XDG_DATA_HOME/dock` (Linux), `~/Library/Application Support/dock` (macOS) or `%LOCALAPPDATA%\dock` (Windows) (`DOCK_DATA_DIR` overrides): a qcow2 overlay on the cached Dockyard base image, the agent token, forwarded port and pid. `--json` keeps the API's keys; `runtime` is `qemu` (or `gvisor`, see above) and a stopped dock is `archived`. Local differences: `dock new --network off` blocks the dock's network (QEMU `restrict=on`; IPv6 is always disabled in the guest network), `dock port` forwards a dock port to `127.0.0.1` in the foreground (no public URL), `dock ssh` opens a shell through the guest agent, `dock branch` copies the stopped disk (a running dock is stopped, copied and resumed; there is no live fork), and `dock stop` powers the guest off and keeps the disk. `exec` returns when the command ends. Settings: `DOCK_QEMU` (qemu-system binary), `DOCK_DOCKYARD_URL` and `DOCK_DOCKYARD_SHA256` (use another image index; it is unpinned and warned about, and verified only when the hash is given). The dock binary pins the images release tag and the sha256 of its `dockyard.json`, and refuses an index that does not match. `DOCK_LOCAL_ACCEL=tcg` allows slow software emulation (tests only; local docks never fall back to it on their own).

- Local network isolation, trust properties: QEMU user-mode networking (slirp) maps the gateway `10.0.2.2` to the host's `127.0.0.1`, and QEMU has no option to turn that off (its `restrict=on` is all or nothing). With `--network on` (the default) the dock therefore asks its guest to refuse connections to `10.0.2.2` (an iptables rule applied at boot from the dock's config; DNS, the internet and the agent forward are unaffected). This stops code that accidentally probes `localhost` services, `dock port` forwards of other docks included, but it is a guest-side control: root inside the dock can remove the rule, and images released before this change ignore it. Each dock's agent forward needs that dock's random token, so one dock cannot run commands in another. Treat a network-on local dock as able to reach your localhost services if it is hostile; use `--network off` for untrusted code. A user-space network stack in `dock` (a host-enforced fix) is tracked in usedock/docklet#98.
- Contexts: `dock context add|use|ls|rm`. The built-in `cloud` context points at `https://api.usedock.io`. Config lives in `os.UserConfigDir()/dock/config.toml` (mode 0600). `DOCK_API_URL`, `DOCK_API_KEY` and `DOCK_WORKSPACE` override the active context.
- Workspace for `dock new`: `--workspace`, then `DOCK_WORKSPACE`, then the context default (`dock context use <name> --workspace <id>`), then your only workspace. With several workspaces and none chosen, `dock new` fails and lists them.
- Push and pull move the disk only (no memory, no running processes), between `local` and a control plane context with a microVM host. `dock push` flattens the stopped dock's disk, compresses it with zstd, checks it with sha256 and uploads it in retried parts; `dock pull` exports, downloads with resume, verifies, and registers a stopped local dock on the Dockyard image it came from. Runtime files are never sent. The flattened disk does hold the guest's own copy of the dock's local token, so `dock push` rotates the local token (the copy that leaves stops working), and a pulled dock gets a new local token. A push or pull checks free space first, and stale `tmp/xfer-*` leftovers from a crashed run are removed on the next one.
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
