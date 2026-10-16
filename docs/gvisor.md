# gVisor runtime for local docks

On a Linux machine without usable KVM (for example a cloud VM without nested virtualization), `dock` can run local docks as gVisor (`runsc`) containers through Docker instead of QEMU. This page covers setup, how a gVisor dock differs from a QEMU dock, the host network block, `--network off`, `dock branch`, and push and pull.

## Turn it on

Opt in with `dock new --runtime gvisor` or `DOCK_LOCAL_RUNTIME=gvisor`. There is no silent fallback: without the flag, local docks stay QEMU only.

On Linux, the `no_accelerator` error and `dock doctor` point to this option when KVM is unusable. `dock doctor` shows an informational `gvisor` row that never makes it fail.

## Requirements

- Docker Engine and the gVisor `runsc` runtime registered with it: [install gVisor](https://gvisor.dev/docs/user_guide/install/), then `sudo runsc install` and `sudo systemctl restart docker`. `dock new` checks both before building anything.
- For `sudo` inside docks, also register a second runtime once (see [sudo and a persistent root filesystem](#sudo-and-a-persistent-root-filesystem)).
- The first dock builds a small `dock-local-gvisor:<hash>` image with `docker build` (Ubuntu 24.04, python3, git, curl, sudo); nothing is published.

## sudo and a persistent root filesystem

gVisor ignores setuid bits unless runsc runs with `--allow-suid`, and by default it keeps rootfs writes in an overlay that is discarded when the container stops (`--overlay2=none` sends them to the container layer instead). To get `sudo` and a persistent rootfs, register a separate runtime once:

```sh
sudo runsc install --runtime=runsc-suid -- --allow-suid --overlay2=none && sudo systemctl restart docker
```

- It is a second entry for the same runsc binary, so your other runsc containers keep the default and are not affected.
- Only docks created after it exists use it. `dock new` prints a note when it is missing, and `dock doctor` shows which case you are in.
- A dock keeps the runtime it was created with for life, so existing docks do not gain `sudo`.
- `runsc-suid` is only used when it points at a runsc binary and was registered with both flags; otherwise it is treated as missing.
- Without it, gVisor docks work but have no `sudo` (no root, no `apt-get install`) and only `/home/user/work` persists across stop and resume.
- With it, `sudo` is passwordless and is root inside the sandbox. `no-new-privileges` is not set on these docks (it would block sudo).

## Lifecycle and persistence

- With the `runsc-suid` runtime, like a QEMU dock, the whole filesystem persists (with plain `runsc` only `/home/user/work` does): `dock stop` stops the container and keeps it, `dock resume` starts it again, so packages you installed with `sudo apt-get install` survive.
- `/home/user/work` is also a named Docker volume, so that data outlives a lost container. `dock rm` removes the container, volume and network.
- A stopped dock holds no Docker network, and keeps its local port: if another program takes that port, `dock resume` fails with `port_in_use` and works again once the port is free.
- `exec`, `ssh` and `port` work as for QEMU docks, through the guest agent on `127.0.0.1`. `dock ls --json` shows `runtime: gvisor`. Commands run as `user`.
- If the `docker` binary is gone, `dock rm` still removes the record and warns with the names to clean up by hand. If Docker is installed but its daemon is down, it fails with `docker_unavailable` and you can retry after starting Docker.

## Isolation

gVisor is a strong sandbox. A user-space kernel written in Go intercepts the dock's system calls and only a small, filtered set reaches the host kernel; Google runs untrusted code on it (GKE Sandbox). It is a different boundary from a hardware VM, not a weak one.

- Root in the dock is root inside the sandbox, not on the host.
- The container runs with Docker's default capabilities, never `--privileged`, and with CPU, memory and process limits.
- Being able to use Docker is root-equivalent on most setups, so anyone who can run Docker on the machine can reach every gVisor dock.

## Differences from QEMU docks

- Each dock gets its own Docker bridge network (`dock-local-<id>-net`), so it cannot reach your other containers or other docks. It reaches the internet, but not the host's own network services; see [Host block](#host-block).
- DNS uses the host's upstream resolvers.
- No Docker or systemd inside by default, and one built-in image instead of Dockyard images.
- `--disk` is not enforced: the work volume and, with `runsc-suid`, everything written elsewhere in the dock live in Docker's storage (`/var/lib/docker`) with no size limit. `docker container prune` or `docker system prune` deletes a stopped dock's container and everything installed in it; only `/home/user/work` (a named volume) survives.
- Some syscall-heavy or file-I/O-heavy workloads are slower than in a VM.

## Host block

A network-on gVisor dock reaches the internet but not the host's own network services. `dock` adds one rule to the host firewall that drops anything the dock starts towards an address the host owns (the bridge gateway, `docker0`, your LAN IP; `127.0.0.1` is not routable from a dock anyway), like a QEMU dock, whose guest refuses the host loopback gateway.

### How it is installed

- Docker runs a short-lived helper container (`--network host`, only `NET_ADMIN` and `NET_RAW`, not `--privileged`; the image already built for the docks, no extra pull) that adds a per-dock chain `DOCK-<id>` and one jump at the top of the host's `INPUT` chain for the dock's bridge (`dkl<id>`, set when the network is created).
- It lives on the host, outside the sandbox, so root in the dock (sudo) cannot remove it, and it needs no `sudo` from you: whoever can run Docker already has this access.
- Only that bridge is matched; your other containers and the rest of the firewall are untouched.
- It is installed before the container starts and removed on `dock stop`, `dock rm` and a failed start. `dock resume` and a reboot-then-resume install it again, and installing twice changes nothing.

### What it blocks

- Anything the dock starts towards an address the host owns. Replies to connections the host opened (the published agent port, `dock port`) still pass.
- Ports your other containers publish on the host: Docker's userland proxy receives them on `INPUT`, so they are blocked the same way. With the proxy off, Docker's own network isolation drops them.
- IPv6 gets the same rule when `ip6tables` works (docks have no global IPv6 address anyway).

### What stays reachable

- The internet and the rest of your LAN (the block is about this machine, not other hosts).
- DNS on the host's upstream resolvers from `/etc/resolv.conf`: only those addresses on port 53 are allowed through, even if one is an address of the host itself (such as a local dnsmasq). A router or LAN resolver is a different machine and is not affected.

### Limits

- A firewall reload or flush on the host (for example `firewall-cmd --reload`, `ufw reload`, `iptables -F`) removes the rule from a running dock until it is stopped and resumed. `dock doctor` reports a running network-on dock whose rule is gone, with the fix `dock stop <dock> && dock resume <dock>`.
- A dock started by an older `dock` has no rule until it is resumed once.
- If Docker cannot run the helper (some restricted or rootless setups), `dock new` and `dock resume` fail with `host_block_failed` rather than start an unblocked dock. In that case use `--network off`, or set `DOCK_GVISOR_ALLOW_HOST=1` to start docks that can reach services on your machine (a warning is printed).

### Why not Docker's gateway_mode_ipv4

This is not Docker's `gateway_mode_ipv4`: `isolated` needs an internal network (no internet), `routed` and `inhibit_ipv4` cut internet access, and `--internal` still leaves the gateway reachable, so none of them keeps egress while hiding the host.

## Network off

`--network off` works: the container runs with `--network none`, so it has no internet, no DNS and no route to the host or to other docks (the sandbox keeps only its own loopback).

- With no published port, `dock` reaches the guest agent through `docker exec -i <container> python3` (a small relay to the agent on the sandbox loopback), so `dock exec`, `dock ssh` and `dock port` work as on any other dock.
- Each agent call costs one `docker exec`, so they are slower than on a network-on dock.
- Stop, resume and branch keep the dock network-off, and a network-off dock never creates or connects a Docker network.

## Branch

`dock branch` works: the source is stopped if running, its container filesystem is committed to a `dock-local-gvisor-branch:<id>` image and its work volume copied (by a short-lived, network-less runsc container), then the source is resumed and the branch starts as a new dock with its own token, port and network.

- With the `runsc-suid` runtime the branch carries rootfs changes (installed packages) too; with plain `runsc` only `/home/user/work` carries over.
- `dock rm` removes the branch image.
- A committed image keeps a copy of the source's rootfs changes on disk until the source and all branches made from that commit are removed.

## Push and pull

A gVisor dock has no disk image, so push and pull move only `/home/user/work`. The command line is in the README; the notes are here.

### Push from a gVisor dock

- `dock push` sends only `/home/user/work`, written onto a fresh Dockyard base (`--image`, default `ubuntu-24.04`); packages and files outside it do not carry over, and the CLI prints a note saying so.
- `--disk <size>` (for example `20gb`) sets the remote disk (default: the dock's disk size, else 10 GiB).
- A running dock needs `--stop`, as for QEMU. The size and free-space checks run before the dock is stopped.
- A `--disk` smaller than the base image, or too small for the work directory, fails with `push_too_big` and a hint to pass a larger `--disk`; these checks run before the dock is stopped.
- A missing work volume fails (`push_failed`), and a file name or symlink target containing a newline or carriage return is refused (`push_unsupported_name`).
- Device, fifo and socket files are skipped with a warning.
- `--image` and `--disk` on a QEMU dock are a usage error.

### Pull into a gVisor dock

- `dock pull <dock> --from <context> [--name N] [--runtime qemu|gvisor]` picks the runtime of the new local dock: the flag, else `DOCK_LOCAL_RUNTIME`, else `qemu`.
- The runtime's preflight runs before the export, so a pull fails early on a machine that cannot run it. On a machine where QEMU/KVM cannot run, the QEMU path fails with a hint to pass `--runtime gvisor`; the gVisor path fails without Docker or `runsc`.
- With `gvisor`, the CLI prints a note that only `/home/user/work` is copied. That directory is copied out of the pulled disk into a new stopped gVisor dock (owner mapped to the dock user), and the first `dock resume` boots it.
- The disk is read inside the gVisor sandbox and is never mounted by the host kernel.
- Pulling a remote dock that itself runs on gVisor into a gVisor dock works with a dock release after v0.3.7, and a control plane and host that support it; see [Pull a fleet gVisor dock](#pull-a-fleet-gvisor-dock) below. Into a QEMU dock it works the same way (only `/home/user/work` is copied onto a fresh base).
- The import is size-capped. It fails with `import_too_big` when the work directory grows past the cap (the disk size plus a little slack; hard links on the source disk become separate copies), and with `insufficient_space` when Docker's data root does not have enough free space.

### Pull a fleet gVisor dock

A dock that runs on gVisor in the fleet has no disk image; its durable state is the work directory, which the host saves as `state.tar.zst` on every stop. `dock pull` of such a dock needs a dock release after v0.3.7, and a control plane and dock host that support exporting gVisor docks.

- `dock pull <dock> --from <context>` (with `--runtime gvisor` or `DOCK_LOCAL_RUNTIME=gvisor` for a gVisor target; QEMU is the default) asks the control plane for the work directory (`formats: ["work-tar"]`), downloads and verifies it (size and sha256), and unpacks it into a new stopped dock. The new dock starts from a fresh image with only `/home/user/work` copied, and the CLI prints a note saying so. `--json` reports `"source":"work"` (`"disk"` for a pulled disk).
- With a control plane or host that cannot export gVisor docks yet, the pull fails with `gvisor_export_unsupported` ("this control plane (or the dock's host) cannot export gVisor docks yet") before anything is downloaded.
- Into a QEMU dock, the CLI makes a new dock from the Dockyard base the export names (`baseImage`, else the remote dock's image, else `ubuntu-24.04`; the note names the one used) with the export's CPUs and memory and a disk that holds the base image plus the remote dock's work disk size (at least 10 GiB). The rewritten tar is attached to the first boot as a read-only raw virtio drive; the guest agent unpacks it into the empty `/home/user/work` as root (`tar -x -p --numeric-owner`), moves the owners from 1001 to 1000 (the `user` of a Dockyard guest), and the dock is stopped. The next boot has no such drive. Everything outside `/home/user/work` on the remote dock is not copied, and the new dock's own sshd keys, packages and settings are the base image's. The QEMU preflight (QEMU and an accelerator) runs before the export.
- The archive is untrusted. The CLI rewrites it into a canonical tar and refuses the whole pull (`invalid_archive`) on an absolute path, a `..` component, an entry below a symlink or file, a path reused for a different kind of entry (for example a directory replaced by a symlink), a hard link that does not point at an earlier file, or an entry of an unknown type. Named pipes and device nodes are left out with a warning that names them (`skipped N special files`); nothing may be written below their paths either; unix sockets are not in the archive. The archive is capped at the recorded data size (plus slack) and the dock's disk size: one that expands past the cap fails with `decompress_failed`, and one whose files declare more than the cap with `import_too_big`. Into a gVisor dock the rewritten tar is unpacked by the same kind of helper as a disk import: read-only root filesystem, no network, only the new dock's work volume writable, and the same size cap and `import_incomplete` marker. Into a QEMU dock the guest VM is the sandbox: it unpacks only the rewritten tar, from a read-only drive. Owners are set to the dock user (1001 in a gVisor dock, 1000 in a QEMU dock). An archive larger than the cap fails with `the decompressed work archive is larger than ...`.

### Interrupted imports

If a pull is killed while the work directory is being copied (into a gVisor or a QEMU dock), the new dock is left in the state `import_incomplete`. `dock ls` and `dock doctor` show it. `dock resume`, `dock branch` and `dock push` refuse it with `import_incomplete`. Remove it with `dock rm <name>`, then pull again.
