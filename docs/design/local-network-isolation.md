# Design: host-enforced network isolation for local docks

Status: Proposed, awaiting owner decisions on the open questions (section 7). Tracks
[#98](https://github.com/usedock/docklet/issues/98). History: #91 (loopback exposure and the IPv6
question), #93 and #97 (the guest-side block of 10.0.2.2), #94 (`ipv6=off`). Related:
`docs/design/windows-fleet-hosts.md` (#37), whose option D this note turns into one shared design.

Every claim about our code names the file it comes from. Statements marked **(unverified)** were
not checked against running code or a live machine while writing this note (no builds, tests or
VMs were run for it). Each one is a phase 0 spike item in section 6.

## 1. Where we are

### 1.1 Networking of a local dock today

- `internal/local/qemu.go` `BuildArgv` gives every dock one NIC,
  `-netdev user,id=n0,ipv6=off,hostfwd=tcp:127.0.0.1:<port>-:2222` and
  `-device virtio-net-pci,netdev=n0`. `Network: off` adds `restrict=on`. `ipv6=off` is always set
  (#94). 2222 is `guestagent.Port` (`internal/guestagent/guestagent.go`).
- QEMU user-mode networking (libslirp) maps the gateway 10.0.2.2 to the host's 127.0.0.1. QEMU's
  `-netdev user` has no option for libslirp's `disable_host_loopback` (checked in `net/slirp.c` and
  `qapi/net.json` for #93 and #97). `restrict=on` is all or nothing: it drops every guest-initiated
  connection that has no `guestfwd` rule, and local docks configure none.
- With `Network: on`, the engine sets `block_gateway` in the fw_cfg guest config
  (`internal/local/engine.go` `boot`, `GuestOptions{BlockGateway: rec.Network != "off"}`;
  `internal/local/qemu.go` `GuestConfig.BlockGateway`). `images/overlay/dock-guest-init.sh` (lines
  67 to 84) then refuses NEW connections to 10.0.2.2 with iptables, in OUTPUT for the guest and in
  mangle PREROUTING for its containers, and lets DHCP to 10.0.2.2:67 through. Root in the guest can
  remove the rule, images released before it ignore the flag, and the README says so (README,
  "Local network isolation, trust properties"; `--network` help in `internal/cli/cmd_docks.go`).
- The guest gets its address by DHCP from slirp (`images/overlay/99-dock-dhcp.network`, with
  `UseDomains=no`). It uses DNS at 10.0.2.3, which slirp answers from the host resolver.
- Apart from host loopback, a network-on dock reaches everything the host can reach: the LAN and
  other RFC1918 networks, VPN routes, and link-local addresses such as a cloud metadata service at
  169.254.169.254 when the host is a cloud VM. Nothing in `internal/local` restricts these.

### 1.2 Processes today

This decides where a user-space stack can live.

- `internal/local/qemu.go` `Start` runs `qemu-system-*` with `detach(cmd)` and returns its pid. On
  Unix `detach` is `Setsid` (`internal/local/proc_unix.go`). On Windows it is
  `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP` with a hidden window
  (`internal/local/proc_windows.go`). A goroutine reaps the child only if the CLI outlives it.
- There is no supervisor, daemon or QEMU monitor (`-monitor none` in `BuildArgv`). The `dock`
  process exits once `Engine.New` or `Engine.Resume` returns. The VM is then an orphan QEMU process
  that `dock` tracks only through files in the dock directory: `pid`, `pidstart` (the process start
  stamp, so a reused pid is never trusted or killed; `livePID` in `internal/local/engine.go`),
  `port` (the agent forward) and `token`.
- `bootOnce` (`engine.go`) picks a port with `freePort()` (bind and close), writes it, starts QEMU
  and polls the agent. `boot` retries up to `bootAttempts` times when the QEMU log mentions "host
  forwarding", because another process can take the port in between.
- Stop (`stopVM`) asks the agent to `poweroff`, waits up to `StopTimeout` (30 s by default), then
  kills the pid if it still has the recorded identity, and `clearRuntime` removes `pid`, `pidstart`
  and `port`. Resume is a fresh `boot`. Branch stops the source, copies the disk and boots both.
- `dock port` (`internal/cli/cmd_local.go` `localPortForward`) is a foreground loop in the CLI. It
  listens on host `127.0.0.1:<localPort>` without authentication. For each accepted connection it
  opens an agent `tcp` tunnel (`Engine.OpenPort`, `internal/guestagent/tunnel.go` `NewTunnel`),
  which the agent connects to the port on the guest's loopback. It does not use `hostfwd`.

So the netstack cannot live in the CLI process that ran `dock new`, because that process exits
right after boot. It needs a process that lives as long as the VM (section 4.2).

### 1.3 What the alignment with fleet hosts needs

`docs/design/windows-fleet-hosts.md` section 3.1 option D proposes the same thing for Windows fleet
hosts: QEMU `-netdev stream` to a per-dock user-space TCP/IP stack in the docklet. Section 3.2
step 2 found that a chardev `guestfwd` cannot carry the per-dock CONNECT proxy, because all guest
connections share one chardev stream. Section 3.2 step 6 notes that slirp enforcement runs inside
QEMU. The Linux fleet policy is in `internal/net/net.go`: `denyRanges`, which covers link-local,
RFC1918, CGNAT and loopback with operator opt-ins, and `guestRestore`. The CONNECT proxy is in
`internal/egress/egress.go` (`Allowed`, `public`, `dialable`, `PrivateAllow` from
`DOCKLET_EGRESS_ALLOW_PRIVATE` in `cmd/docklet/main.go`). A design for local docks should produce
a stack and a policy package that the Windows fleet backend can reuse unchanged.

## 2. Goals and non-goals

Goals

1. **Host-enforced isolation.** Policy runs on the host, outside the guest and outside QEMU, so
   root in the guest cannot undo it. By default a dock cannot reach host loopback (127.0.0.0/8),
   link-local (169.254.0.0/16, so cloud metadata too), RFC1918, CGNAT (100.64.0.0/10), unspecified,
   multicast or broadcast addresses, or the host's own interface addresses. An explicit opt-in can
   allow some of these.
2. **One policy shared with fleet semantics.** The same range list and admission functions as
   `internal/net` `denyRanges` and `internal/egress` `public`/`dialable`/`Allowed`, in one package.
   The modes map onto fleet modes: off = no egress; on = fleet "unconfined" (any public address,
   private ranges opt-in); allowlist = fleet "confined" (`EgressAllow` in `api/v1/types.go`, HTTPS
   hosts through a CONNECT proxy).
3. **The agent channel and `dock port` keep working**: same token, same `guestagent` client, same
   CLI behaviour (#93 acceptance).
4. **DNS works** for network-on docks, from the host resolver, without opening the host's
   loopback resolver to arbitrary guest traffic.
5. **Linux (KVM), Windows (WHPX) and macOS (HVF)**, with the QEMU builds users install today
   (`InstallHint` in `internal/local/qemu.go`: distro packages, the Windows installer or winget,
   Homebrew), and a release binary built with `CGO_ENABLED=0` (`.github/workflows/release.yml`).
6. Existing docks and older Dockyard images keep working without an image release.

Non-goals

- IPv6 for guests. The image is IPv4-only and `ipv6=off` is set today. The new stack attaches no
  IPv6 protocol, which fails closed the same way.
- ICMP to the outside world (ping). The candidate stacks do not forward it (gvisor-tap-vsock's README
  says so for its stack). A behaviour change to note; slirp forwards some ICMP echo **(unverified
  on Windows)**.
- Throughput parity with TAP networking. The target is parity with slirp (section 3.5).
- Protecting a dock from its own host, or local users from each other beyond what the token and
  the data directory ACLs give today (`internal/local/acl_windows.go` `secureDir`).

## 3. Transport and stack options

### 3.1 Option A: QEMU frames to a user-space stack in `dock`

QEMU's virtio-net backend sends raw Ethernet frames over a socket to a stack we own. That stack
answers ARP and DHCP, terminates the guest's TCP and UDP flows, and opens matching host sockets only
when the policy allows. Nothing maps a guest address to host loopback unless we write that code.

QEMU backends that can carry frames:

| Backend | Since | Address types | Windows |
|---|---|---|---|
| `-netdev socket,fd=N` (or `connect=`/`listen=` TCP) | long before 7.2 | an inherited fd, or TCP | `fd=` is not usable from `dock`: Go's `exec.Cmd.ExtraFiles` is not supported on Windows, so there is no fd 3 to hand over; TCP works |
| `-netdev stream` | 7.2 | `inet`, `unix`, `fd` (QAPI `SocketAddress`, through `qio_channel_socket_*` in `net/stream.c`); client or `server=on`. Client reconnect: `reconnect` (seconds) since 8.0, replaced by `reconnect-ms` in 9.2 (`reconnect` deprecated) | built on every host OS (`net/meson.build` lists `stream.c` unconditionally); `unix` on Windows uses native AF_UNIX (QEMU commit d409373b9d, "util/qemu-sockets: Enable unix socket support on Windows", August 2022, first released in 7.2, needs Windows 10 build 17063 or later). `unix_listen_saddr`/`unix_connect_saddr` in `util/qemu-sockets.c` have no Windows guard |
| `-netdev dgram` | 7.2 | `inet`, `unix`, `fd` (`net/dgram.c`) | built everywhere, but the Unix-socket path uses `unlink`/`dup2`-style code without Windows guards **(unverified on Windows)** |
| `-netdev vhost-user` | old | chardev (unix socket) | not on Windows **(unverified)** |

Framing is the same for `socket` and `stream`: each frame is a 4-byte big-endian length followed by
the frame (`net/socket.c` and `net/stream_data.c` write `htonl(size)`; `net_fill_rstate` in
`net/net.c` reassembles). `net/stream_data.c` exists only since 10.1 (commit adf684ce69, "net:
Refactor stream logic for reuse in '-net passt'"); before that the same code is in `net/stream.c`.
gvisor-tap-vsock's "qemu" protocol (`pkg/tap/protocols.go`, `qemuProtocol`) is the same 4-byte
big-endian prefix. Its reader does not bound the length, so ours must: reject any length above
QEMU's own limit (`NET_BUFSIZE`, 4096 + 65536) and close the stream. One reader and writer in Go
covers both backends.

**Named pipes.** QEMU's `-netdev stream` takes a `SocketAddress`, which has no named-pipe type.
Named pipes exist only as the chardev backend `pipe:` (`chardev/char-pipe.c`), which no netdev
uses. So on Windows the choice is between AF_UNIX and loopback TCP, and AF_UNIX is better (below).

Per-OS transport for option A:

| Host | Transport | Why |
|---|---|---|
| Linux, macOS | `socketpair(AF_UNIX, SOCK_STREAM)`: one end stays in the helper, the other is passed to QEMU as fd 3 through `exec.Cmd.ExtraFiles`, `-netdev stream,id=n0,addr.type=fd,addr.str=3` on QEMU 7.2 or later, else `-netdev socket,id=n0,fd=3` | No filesystem name and no listener, so no other process can connect. Works on every QEMU we support: with no current monitor, QEMU's `socket_get_fd` (`util/qemu-sockets.c`) parses the fd number and checks `fd_is_socket`. gvisor-tap-vsock's `qemu-wrapper` also hands QEMU fd 3, but that fd is a connection to gvproxy's listening unix socket; a socketpair has no listener at all |
| Windows | AF_UNIX socket file `net.sock` in the dock directory, which already has the protected DACL from `secureDir`. The helper removes any stale file, listens, starts QEMU, which connects with `-netdev stream,id=n0,server=off,addr.type=unix,addr.path=<path>`, accepts exactly one connection, then closes the listener and deletes the file. It checks that the peer is its own QEMU child with `WSAIoctl(SIO_AF_UNIX_GETPEERPID)` (the ioctl QEMU itself defines in `include/system/os-win32.h`), so frame injection does not rest on the DACL alone. Needs QEMU 7.2 or later | Go's `net.Listen("unix", ...)` works on Windows. A filesystem object is governed by the directory ACL **(unverified: whether Windows checks the socket file ACL on connect; phase 0, together with the peer-pid ioctl)**. The helper listens rather than QEMU (`server=on`), so no QEMU-side listener can be raced by another process. Path length: `sun_path` is 108 bytes, and `%LOCALAPPDATA%\dock\docks\dk_<16 hex>\net.sock` fits unless the profile path is very long **(unverified)**; fall back to a short path under the data dir |
| Windows fallback | loopback TCP: the helper listens on `127.0.0.1:0`, QEMU uses `-netdev stream,addr.type=inet,...` or `-netdev socket,connect=` | Any local process could connect first and inject frames as if it were the guest. The helper accepts exactly one connection and checks that the peer is the dock's QEMU pid and start stamp (`GetExtendedTcpTable`), the same check `windows-fleet-hosts.md` section 3.5 proposes for agent forwards |

Candidate stacks:

- **A1. gVisor netstack used directly** (`gvisor.dev/gvisor/pkg/tcpip`: `stack`, the
  `network/ipv4` and `network/arp` protocols, `transport/tcp` and `transport/udp` forwarders,
  `adapters/gonet`). This is pure Go and already builds for Windows, macOS and Linux, since
  gvisor-tap-vsock ships Windows and macOS binaries built on it. We write our own link endpoint for
  the length-prefixed frames, DHCP and DNS responders, and TCP/UDP forwarders that call our policy.
  gVisor publishes the Go-buildable tree on its `go` branch without semver tags, so we pin a
  pseudo-version.
- **A2. gvisor-tap-vsock as a library** (`github.com/containers/gvisor-tap-vsock`). This is the
  stack Podman machine uses (gvproxy) and the basis of Lima's user-mode network **(from those
  projects' docs; not checked against their code)**. It provides a virtual switch, DHCP, DNS, port
  forwarding and QEMU transports (its README shows `-netdev stream,addr.type=unix` for QEMU 7.2 and
  later, and `-netdev socket,fd=3` through `qemu-wrapper`). Gap: its TCP forwarder
  (`pkg/services/forwarder/tcp.go`) has no allow or deny hook. It reads the destination from the
  connection id, applies the `NAT` map (whose purpose, per `pkg/types/configuration.go`, is "reaching
  the host itself (localhost) from the virtual network"), refuses 169.254.0.0/16 unless
  `Ec2MetadataAccess` is set, and otherwise calls `net.DialTimeout`. Using it means an upstream
  change (a dial or policy callback in `Configuration`) or a fork of the forwarders, and carrying
  features we do not want: the host-side HTTP API, vsock and VPNKit compatibility, and the NAT to
  loopback.
- **A3. gvproxy as a separate binary.** It has the same missing policy hook and adds a second binary
  per OS to ship, sign (`release.yml` signs every artifact) and keep in step. Rejected.

### 3.2 Option B: `-netdev passt` / `pasta`

`-netdev passt` is in QEMU 10.1 (`NetdevPasstOptions` in `qapi/net.json`, "Since: 10.1"), with a
`map-host-loopback` option. The QEMU version is not the blocker, though: passt also works through
`-netdev stream,addr.type=unix` on QEMU 7.2 and later, which is how passt documents its QEMU use
(the 10.1 netdev only starts the passt process for you). passt runs as a separate process and is
GPL-2.0-or-later. It only runs on Linux: it relies on Linux namespaces and syscalls, and there are
no Windows or macOS builds **(unverified for macOS; no port is known)**. passt removes the loopback
mapping when configured to, but it has no per-destination policy, so it does not deny RFC1918,
link-local or the host's own addresses **(unverified; passt's options were not reviewed in full)**.
That would need host firewall rules, which `dock` (run as the user) cannot install. Linux-only and
only a partial fix.

### 3.3 Option C: keep slirp with a patched build

libslirp has a `disable_host_loopback` switch. QEMU does not expose it, but a patch could. We would
then ship our own QEMU builds for three OSes and two architectures, and track QEMU security
releases. The policy would still run inside the QEMU process (C code, the weakness
`windows-fleet-hosts.md` section 3.2 step 6 describes), and adding RFC1918 or allowlist rules means
more C patches. Users today bring their own QEMU (`findTool`, `DOCK_QEMU`). Getting the switch
upstream as a `-netdev user` option is still worth a small upstream patch on its own, but it would
reach users only after several QEMU releases and distro cycles, and it would only cover loopback.

### 3.4 Option D (for completeness): TAP plus host firewall

TAP needs root or a pre-created device on Linux, a third-party driver on Windows, and a
privileged helper on macOS. `dock` runs unprivileged, so this does not fit local docks.

### 3.5 Comparison

| | A1 gVisor netstack in `dock` | A2 gvisor-tap-vsock library | B passt | C patched slirp |
|---|---|---|---|---|
| Linux / macOS / Windows | yes / yes / yes (Windows needs QEMU 7.2 or later for AF_UNIX, or the TCP fallback) | same as A1 | Linux only | yes, but only with our own QEMU builds |
| Where policy runs | our Go process, outside QEMU | same, after a hook is added upstream or in a fork | passt process (C), loopback only | inside QEMU (C) |
| RFC1918, link-local, host-address denies; allowlist | yes, our code | needs a hook | no (host firewall needed) | more patches |
| Shared with Windows fleet option D | yes (one package) | yes | no | no |
| License | Apache-2.0. Compatible with our AGPL-3.0 (`LICENSE`): Apache-2.0 code can be combined into GPLv3/AGPLv3 works (one-way, per the FSF's license list). Keep its notices | Apache-2.0, same | GPL-2.0-or-later, separate process, no linking | QEMU GPL-2.0, libslirp BSD-3-Clause **(unverified)**; we would distribute QEMU source for our builds |
| Binary size | grows the `dock` binary by a few MB **(unverified; phase 0 measures with the release flags `-trimpath -s -w`)** | larger than A1 (switch, API, vsock code) **(unverified)** | none for us | none in `dock`; we ship QEMU |
| Throughput | gvisor-tap-vsock reports 1.6 to 2.3 Gbit/s with iperf3 (MTU 4000, QEMU on macOS, its README). Expect similar for A1 **(unverified)**; slirp is the baseline to beat or match | same | high (passt is designed for it) | slirp baseline |
| Per-dock cost | one helper process: Go runtime plus stack, tens of MB **(unverified)** | same | one passt process | none extra |
| Maintenance | gVisor `go` branch pseudo-versions, occasional API churn. We own DHCP, DNS and the forwarders (a few hundred lines each **(estimate)**) | an extra dependency layer, plus upstreaming a hook or carrying a fork | a second program for users to install; Linux only | a QEMU fork |

**Choice: A1, with gvisor-tap-vsock as reference code.** We use gVisor netstack directly behind our
own small package. We can borrow designs from gvisor-tap-vsock, its DHCP responder and its forwarder
structure, with Apache-2.0 attribution wherever code is copied. A2 stays the fallback if writing the
DHCP and DNS pieces turns out to cost more than expected. In that case we propose a policy hook
upstream first (Q2).

## 4. Recommended design

### 4.1 Shape

```
dock new / resume (exits after boot)
  └─ starts, detached:  dock __vmnet <dock-id>        (per-dock helper, the same dock binary)
        ├─ internal/usernet  gVisor stack 10.0.2.0/24, DHCP, DNS, TCP/UDP forwarders
        │     └─ internal/netpolicy  (shared with internal/egress and internal/net)
        ├─ 127.0.0.1:<port>  agent listener -> gonet dial 10.0.2.15:2222
        └─ child: qemu-system-*  -netdev stream|socket (fd 3 or AF_UNIX) -device virtio-net-pci
```

New packages, OS-neutral and usable by the docklet later:

- `internal/netpolicy`: the deny ranges (today `denyRanges` in `internal/net/net.go`), `Public`
  (today `public` in `internal/egress/egress.go`), the private opt-in (`PrivateAllow`, `ParseCIDRs`)
  and host-pattern matching (`Allowed`). `internal/net` and `internal/egress` switch to it with no
  behaviour change.
- `internal/usernet` (name open): `New(Config) (*Net, error)`, `Attach(conn net.Conn)` for the
  QEMU frame stream, `DialGuest(ctx, port)` for host-to-guest connections, and `Close`. `Config`
  carries the subnet, the guest address, a `netpolicy.Policy`, the DNS upstream and logging.

### 4.2 Process model

The netstack has to outlive the CLI. Alternatives considered:

| Option | Verdict |
|---|---|
| In the `dock` process that ran `new` | Impossible: that process exits once `Engine.New` returns (section 1.2) |
| One per-user daemon serving every dock | Needs auto-start, IPC, upgrade handling and a single point of failure for all docks. One compromise reaches every dock's traffic. More than local docks need |
| A per-dock helper next to QEMU (siblings) | Simple, but QEMU keeps running with a dead netdev if the helper dies, and the engine has to notice |
| **A per-dock helper that is QEMU's parent (recommended)** | One helper per running dock. The helper starts the stack, then starts QEMU as its child, and exits when QEMU exits |

The helper is the `dock` binary itself, started as a hidden subcommand (`dock __vmnet <id>`, name
open) through `os.Executable()`, so there is still one binary to ship. Boot sequence (replacing
`QEMU.Start` and part of `bootOnce`):

1. `bootOnce` writes `net.json` (policy, mode, agent port, network mode) into the dock directory
   next to `guest.json`. The token is not in it: the helper does not need the token.
2. `dock` starts the helper detached (`detach` as today) with its log at `net.log`.
3. The helper binds the agent listener on `127.0.0.1:0` and keeps it. This replaces `freePort` and
   the "host forwarding" retry in `boot`, because the port is never released between choosing and
   use. It then creates the socketpair (Unix) or `net.sock` listener (Windows) and starts QEMU with
   the argv from `BuildArgv`.
   - Linux: `SysProcAttr.Pdeathsig = SIGKILL`, so QEMU dies with the helper. Go ties this to the
     thread that forked, so the helper must start QEMU from a locked OS thread that stays alive
     **(unverified detail; phase 0)**.
   - Windows: a Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, held only by the helper, so
     the helper's death (even `TerminateProcess`) kills QEMU. Go's `os/exec` cannot start a process
     suspended and resume it, so assigning QEMU after `Start` leaves a short window in which it runs
     outside the job. The helper avoids that by assigning itself to the job before starting QEMU;
     the child is then in the job from creation (nested jobs, Windows 8 and later).
   - macOS: there is no parent-death signal. QEMU sees EOF on its netdev. The engine treats a dead
     helper as a dead dock (below) and kills the QEMU pid if its start stamp still matches.
4. The helper writes `pid`/`pidstart` (QEMU, as today, so `livePID`, `Kill` and the e2e `pgrep`
   checks keep their meaning) and `netpid`/`netpidstart` (itself), then signals readiness to the
   waiting `dock`: the CLI polls for the files, then for the agent ping as `waitReady` does now.
5. The helper serves until QEMU exits, closes the stack and exits.

Engine changes: `livePID` returns the QEMU pid only if the helper pid is also alive with its stamp.
If QEMU is alive and the helper is dead, the dock is reported `archived` after killing QEMU, and
`dock doctor` and `dock ls` say why. `clearRuntime` also removes `netpid`, `netpidstart` and
`net.sock`. `logTail` adds `net.log`.

### 4.3 Lifecycle and crash behaviour

| Event | Behaviour |
|---|---|
| `dock new`, `dock resume`, branch boot | A new helper, then a new QEMU. The policy is read from the record at every boot, so a policy change applies on the next resume |
| `dock stop` | Agent `poweroff`, QEMU exits, the helper sees its child exit and exits. The forced path kills QEMU by pid. In both paths `stopVM` then waits briefly for the helper (`netpid` with its stamp) and kills it if it is still there, for example hung, before `clearRuntime`. Otherwise a quick `dock resume` could collide with the old helper on the agent port, `net.sock` or `net.log` |
| `dock rm` | `stopVM(force)` as today, then the directory is removed. The retry loop in `removeFiles` covers a helper still closing `net.log` on Windows |
| Helper crash | No frames are processed, so the guest has no network and no agent channel: fail closed. Linux and Windows: QEMU is killed with it. macOS: QEMU stays up without network until the next `dock` command kills it. In every case the dock shows as stopped, and `dock resume` boots it again |
| QEMU crash | The helper's child exits and the helper exits. Same as today, with no network left behind |
| Host reboot or logout | Both processes are gone. Same as today (no autostart) |
| `dock` upgraded while docks run | Running helpers keep the old binary until stop and resume. `scripts/install.sh` writes a temp file and `mv -f`s it over `dock`, which is safe while helpers run on Linux and macOS. `scripts/install.ps1` overwrites in place (`Copy-Item ... dock.exe -Force`), and Windows refuses to overwrite a running image, so today's installer fails on Windows whenever any dock is running. Today nothing long-lived runs from `dock.exe` (only a foreground `dock port`), so this is new. Either the installer renames the running `dock.exe` aside before copying (Windows allows renaming a running image), or the helper runs from a per-version copy in the data dir (Q7). A newer CLI must also read the `netpid`/`net.json` files of a helper started by the previous release, so their format stays compatible across one release |
| Helper and QEMU versions | The helper checks the QEMU version (`-version`) once per boot and chooses `stream` or `socket` (Linux, macOS), or refuses (Windows below 7.2, Q4) |

QEMU's `reconnect-ms` is not used. A restarted helper would have lost all TCP state anyway, and
reconnecting would hide a crash.

### 4.4 Guest-facing network

The stack copies slirp's addressing, so guests need no change:

- Subnet 10.0.2.0/24, guest 10.0.2.15 by DHCP, router 10.0.2.2, DNS 10.0.2.3, lease time similar
  to slirp's, MTU 1500. `99-dock-dhcp.network` and the fleet's static unit work unchanged.
- ARP is answered for the stack's own addresses only. The DHCP server answers on 10.0.2.2:67 (the
  unicast renew that `dock-guest-init.sh` lets through).
- IPv4 only. No IPv6 protocol is registered, so IPv6 frames are dropped, as `ipv6=off` does today.
- Images with `block_gateway` keep refusing NEW connections to 10.0.2.2, which is harmless: the
  only services on 10.0.2.2 are DHCP (allowed by that rule) and nothing else. The flag stays set as
  defense in depth and for docks booted by older `dock` binaries.
- Virtual service addresses that the stack itself answers (no host socket): 10.0.2.3:53 (DNS), and
  in allowlist mode 10.0.2.100:3128 (the CONNECT proxy, the address `windows-fleet-hosts.md`
  section 3.2 already uses). Connections to any other address in 10.0.2.0/24 are refused.

### 4.5 Enforcement

Every guest-initiated TCP SYN and new UDP flow goes through one decision,
`policy.Admit(dstIP, proto, port) error`, before any host socket is opened. The forwarder then
dials that exact IP: no host name, no re-resolution, so DNS rebinding cannot change what was
checked.

Denied unless the opt-in list (`--allow-cidr`, the equivalent of `DOCKLET_EGRESS_ALLOW_PRIVATE` and
the operator opt-ins of `guestRestore`) contains the address:

- 0.0.0.0/8, 127.0.0.0/8, 169.254.0.0/16, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16,
  100.64.0.0/10 (this is `denyRanges` plus "this network"), 224.0.0.0/4, 240.0.0.0/4 and
  255.255.255.255.
- The host's own addresses: every address on every host interface (`net.InterfaceAddrs`), refreshed
  at most every few seconds and on a miss. Without this, a public address on the host would reach
  host services bound to 0.0.0.0. Because that list can be stale (section 5.2), each forwarded TCP
  connection is also checked after connect: if `LocalAddr` and `RemoteAddr` have the same IP, the
  host dialled itself, and the connection is closed before any guest byte is sent **(unverified
  that every OS picks the destination as the source for a self-connect; phase 2 tests it)**.
- 10.0.2.0/24 itself except the virtual service addresses above: the guest's subnet maps to nothing
  on the host.

There is no code path from a guest address to host loopback: the stack has no NAT table. That is
the property #93 asked for, and it cannot be undone from the guest.

Resource limits, so a guest cannot exhaust the host: a cap on concurrent forwarded flows per dock
(gvisor-tap-vsock uses 128 in-flight TCP connects by default, `TCPMaxInFlight`), a connect timeout,
and idle timeouts for UDP flows. ICMP is not forwarded.

### 4.6 Allowlist and egress modes, shared with fleet

| Local mode (`--network`) | Policy | Fleet equivalent |
|---|---|---|
| `off` | Nothing is forwarded, and DNS is not answered. Only the agent channel works. | No egress (`restrict=on` on Windows hosts) |
| `on` (default) | Any public IPv4 address, any port, TCP and UDP. Private ranges are opt-in. DNS answered. | Unconfined (`denyRanges` plus opt-ins in `guestRestore`) |
| `allowlist` with `--allow-host` patterns (new, Q5) | Only the CONNECT proxy at 10.0.2.100:3128, using `egress.Proxy` with the same `Allowed` patterns, port 443, and `dialable` checks. Exec and terminal get `ProxyEnv` as fleet does (`internal/host/fast.go`, `internal/host/terminal.go`). DNS not answered. | Confined (`EgressAllow`, `internal/egress`) |

Sharing details:

- `egress.Proxy` keys policy by guest source IP (`ServeHTTP` uses `r.RemoteAddr`). Every local
  dock's guest is 10.0.2.15, so each dock gets its own `Proxy` instance inside its own helper. The
  Windows fleet backend (one docklet process, many stacks) does the same per stack. That is
  simpler and stricter than the per-listener keying in `windows-fleet-hosts.md` section 3.2 step 2,
  and it removes that section's cross-dock listener concern (step 5), because a dock's proxy is
  reachable only from inside its own stack.
- `internal/egress` does not build on Windows or macOS today, because `Listen` uses
  `syscall.IP_FREEBIND` (`egress.go` line 264, also listed in `windows-fleet-hosts.md` section
  2.1), and on Windows `syscall.SetsockoptInt(int(fd), ...)` does not match its `syscall.Handle`
  parameter either. `Listen` moves to `egress_linux.go`. The proxy is served on a `gonet` listener inside the
  stack instead.
- A DNS-based allowlist for tools that are not proxy-aware (answer only allowed names, admit only
  the IPs returned for them) was considered. It is weaker against shared CDN addresses and does not
  match fleet, so it is not proposed for the first version.

### 4.7 Agent channel and `dock port`

- The helper owns `127.0.0.1:<port>` and writes `port` as today. For each accepted connection it
  calls `DialGuest(ctx, 2222)`, which opens a TCP connection from the stack (source 10.0.2.2,
  as slirp's hostfwd connections appear today) to 10.0.2.15:2222, and splices the two. The
  `guestagent` client, the token and `Engine.agentAddr` do not change. Guest-side conntrack sees the
  same direction as today, so the `block_gateway` rule still lets replies through.
- `dock port` does not change. It tunnels through the agent's `tcp` op, so it needs only the agent
  channel. Its unauthenticated `127.0.0.1` listener is no longer reachable from any dock, because no
  dock can reach host loopback, which was the concern in #93.
- Later option (not in this design's phases): the agent listener moves from loopback TCP to an
  AF_UNIX socket in the dock directory. Other local users then cannot even attempt a token, and the
  port-squatting risk that `windows-fleet-hosts.md` section 3.5 describes goes away. This needs
  `guestagent.Dial` to accept a unix address, which is a small, separate change.

### 4.8 DNS

- The stack answers UDP and TCP on 10.0.2.3:53. It parses queries with
  `golang.org/x/net/dns/dnsmessage` (`golang.org/x/net` is already in `go.mod`) and forwards only
  standard queries: opcode QUERY, one question, class IN. Anything else (UPDATE, NOTIFY, zone
  transfers, malformed messages) gets REFUSED without reaching the host resolver, and the forwarder
  has a per-dock rate and in-flight cap. Forwarding arbitrary raw messages would contradict goal 4,
  because the upstream is often a loopback resolver or the LAN router. These are read from `/etc/resolv.conf` on Linux and
  macOS, and from `GetAdaptersAddresses` on Windows. The helper is a host process, so it may send
  to a loopback resolver such as systemd-resolved's 127.0.0.53. The guest never gets a socket to
  it. On macOS, `/etc/resolv.conf` may miss VPN-scoped resolvers, so falling back to the system
  resolver through Go's `net.Resolver` for A/AAAA records is part of the spike **(unverified)**.
- Answers are passed through unfiltered in `on` mode. Admission happens on the connection, not on
  the name, so a name that resolves to 127.0.0.1 or a LAN address gives the guest an address it
  still cannot connect to.
- Guest DNS to other servers (for example 8.8.8.8:53, or DNS over HTTPS) is ordinary public
  traffic in `on` mode and is denied in `allowlist` mode.
- Exfiltration through DNS lookups remains possible in `on` mode, as it is with slirp. In `off` and
  `allowlist` modes there is no resolver for the guest. In `allowlist` mode the proxy still resolves
  the CONNECT host on the host side, after `Allowed` has accepted it (`egress.go` `ServeHTTP`), so a
  wildcard pattern such as `*.example.com` lets the guest send chosen labels to that domain's
  authoritative servers. This is the same in fleet and is acceptable for allowlists the user picked.
- The host's upstream resolver is often the LAN router (for example 192.168.1.1). DNS through the
  stack is therefore the one path by which a guest's bytes reach a private address in `on` mode,
  limited to well-formed standard queries (above).

### 4.9 Network off

`--network off` can stay on slirp with `restrict=on` until the new stack has shipped one release
as the default for `on`. Then it moves to the stack with a deny-all policy, so one network path
covers every dock. That also drops slirp's in-QEMU restrict logic, which had the IPv6 TODO noted
in #91 (Q3).

### 4.10 Migration and compatibility

- `Record` (`internal/local/store.go`) gains `NetStack string` (`"slirp"` or `"usernet"`; empty
  means slirp for old records) and the policy fields (`AllowCIDRs`, `AllowHosts`). The stack is
  chosen at each boot, so an existing dock moves to the new stack on its next resume, with no disk
  or image change. The addressing is the same (section 4.4).
- Rollout: phase 3 adds `DOCK_LOCAL_NET=usernet` (opt-in). Phase 5 makes `usernet` the default for
  network-on docks, and `DOCK_LOCAL_NET=slirp` stays as an escape hatch for one or two releases,
  printing that it is weaker.
- Behaviour changes users will notice, to be called out in the release notes and README: LAN and
  other private addresses are no longer reachable by default (`--allow-cidr` restores them), ping to
  the outside world fails, and the host's own non-loopback addresses are unreachable.
- Old images keep working: DHCP, DNS and the agent are unchanged. `block_gateway` matters only for
  the slirp fallback.
- QEMU versions: Linux and macOS work with any QEMU we support today (fd transport). Windows needs
  QEMU 7.2 or later for AF_UNIX. `dock doctor` gains a "network" check that reports the QEMU version
  and the transport it will use. Whether Windows falls back to slirp with a warning or refuses
  network-on docks below 7.2 is Q4.
- README and `--network` help: the "guest-side control" text for #97 is replaced by the new trust
  properties once `usernet` is the default.

### 4.11 Reuse by the Windows fleet backend

`internal/usernet` and `internal/netpolicy` are what `windows-fleet-hosts.md` option D needs. The
docklet runs one stack per dock in its own process, with no helper (the docklet already lives as
long as its docks). It uses the same AF_UNIX transport in its protected state dir, `egress.Proxy`
per stack for confined docks, and the `on` policy for unconfined docks. That resolves its Q3 and Q4
in favour of D: unconfined Windows docks would get arbitrary TCP/UDP and DNS like Linux, not
HTTPS-only. The WFP second line in that note (section 3.2 step 5) would then filter the docklet
process instead of QEMU, and QEMU itself would need no network access at all **(unverified that
WFP can express "QEMU: no connects except to its AF_UNIX peer"; phase 0 of that note)**.

## 5. Security analysis

### 5.1 What a network-on guest can still reach

| Target | Before (slirp, `block_gateway`) | After (`usernet`) | Why acceptable |
|---|---|---|---|
| Host loopback services | Blocked by a guest rule that root can remove | Unreachable: no mapping exists | The goal |
| Other docks' agent forwards and `dock port` listeners | Through 10.0.2.2, unless the guest rule holds; the agent needs a token | Unreachable | |
| LAN, RFC1918, CGNAT, link-local and cloud metadata | Reachable | Denied unless opted in | Opt-in is explicit and per dock |
| Host's own public or LAN addresses | Reachable | Denied | |
| Public internet, TCP and UDP | Reachable | Reachable | The meaning of `--network on`; use `off` or `allowlist` for untrusted code |
| Hosts that are public addresses but internal by routing (VPN routes to public ranges, corporate networks on public space) | Reachable | Reachable | Not detectable from the address. Documented; users on such networks use `allowlist` or `off` |
| DNS through the host resolver (internal names resolve) | Yes | Yes, in `on` mode | Resolving gives an address, not access. Connections are still checked |
| The host's resolver itself, often the LAN router | Any DNS message, through 10.0.2.3 | Standard queries only, rate-capped (section 4.8) | DNS is needed; the router sees only well-formed queries |
| Host services through the router's public address (hairpin NAT of a port forward the user set up) | Reachable | Reachable | The public address is not the host's, so no address check can see it. Documented with the VPN row |
| IPv6 | Off (`ipv6=off`) | No IPv6 protocol in the stack; frames dropped | Fails closed |
| ICMP | Some echo forwarded by slirp | None (Q9) | |
| Timing and other covert channels on allowed traffic | Possible | Possible | Out of scope; `on` mode allows the internet anyway |
| The stack itself (DHCP, DNS, proxy) | slirp in QEMU (C) | Go code in the helper | Smaller and memory-safe compared with slirp in C |

### 5.2 Residual risks

1. **A bug in the stack or helper.** The helper runs as the user, with the user's network
   access. A guest that gains code execution in the helper can reach host loopback. This is the
   same blast radius as a QEMU escape today, since QEMU runs as the same user. gVisor netstack is
   Go, so memory corruption is far less likely than in libslirp's C. The stack replaces slirp as
   the code that parses guest packets; it does not add a second parser next to it. A malformed
   frame that triggers a Go panic kills the helper, which takes QEMU down with it (fail closed, a
   denial of service of that one dock only); the helper does not `recover` and continue. The frame
   reader rejects lengths above `NET_BUFSIZE` before allocating. Policy bugs (a missed range)
   are the realistic risk. Mitigation: one admission function with table tests for every range,
   plus an e2e test that removes the guest rule as root and still fails to connect (section 6).
2. **Frame injection by another local process.** This is not possible with the socketpair. On Windows
   AF_UNIX it rests on the directory DACL plus the one-connection, peer-pid check of section 3.1. The loopback TCP fallback needs the peer-pid
   check. Same class as the existing token-protected agent forward.
3. **Resource exhaustion.** A guest opening many flows uses helper file descriptors and memory.
   Mitigated by the per-dock caps in section 4.5. The host still pays for one Go process per
   running dock.
4. **Policy staleness.** The host's addresses change (Wi-Fi roam, VPN up). The refresh-on-miss in
   section 4.5 bounds the window to a few seconds. Treat it as a known limit.
5. **The slirp fallback.** While `DOCK_LOCAL_NET=slirp` exists, docks using it have today's weaker
   properties. `dock ls` or `dock doctor` should show the stack in use.
6. **macOS orphan window.** After a helper crash, QEMU runs without network until the next `dock`
   command. It can reach nothing in that state, so this is a cleanup issue, not an exposure.

## 6. Phased plan

Each phase is one issue under #98 (#98 becomes the epic), with its own PR. No phase changes the
default until phase 5.

**Phase 0: spikes (no product code; results written into this note).**
Each item gets a yes or no with evidence:
- `-netdev stream` with `addr.type=unix` on the Windows QEMU installer build and on a scoop build:
  the minimum version, whether the socket file's DACL is enforced, and whether
  `SIO_AF_UNIX_GETPEERPID` returns QEMU's pid to the helper.
- `-netdev socket,fd=3` and `stream addr.type=fd` from Go `ExtraFiles` on Linux and macOS.
- Throughput and CPU against slirp: a 1 GiB download in the guest and iperf3 both ways, on Linux
  KVM and Windows WHPX.
- `dock` binary size delta with the release flags.
- Linux `Pdeathsig` from a locked thread, and the Windows Job Object kill.
- DNS forwarding on all three OSes, including systemd-resolved and a macOS VPN resolver.
- That renaming a running `dock.exe` aside and copying the new one works with helpers running (the
  current `install.ps1` overwrites in place and will fail, section 4.3).
- Whether GitHub-hosted Windows and macOS runners can run a local dock at all: WHPX and HVF nested
  virtualization are not expected on hosted runners **(unverified)**, so the fallback is
  `DOCK_LOCAL_ACCEL=tcg`, and its boot time decides whether a TCG e2e fits the CI budget.

Acceptance: a go/no-go per item. A no-go on Windows AF_UNIX moves Windows to the TCP fallback with
the peer-pid check. Throughput below the slirp baseline by more than an agreed margin (Q6) stops
phase 5.

**Phase 1: `internal/netpolicy`, no behaviour change.**
Move `denyRanges`, `public`, `dialable`, `ParseCIDRs` and `Allowed` into `internal/netpolicy`, add
the host-address and 10.0.2.0/24 rules for local use, and switch `internal/net` and `internal/egress`
to it. Move `egress.Listen` into `egress_linux.go` so `internal/egress` builds on Windows and macOS.
Acceptance: existing `internal/net` and `internal/egress` tests pass unchanged. New table tests cover
every range, boundary addresses and the opt-in list. `GOOS=windows` and `GOOS=darwin`
`go build ./internal/egress ./internal/netpolicy` pass. `./internal/netpolicy/...` (and later
`./internal/usernet/...`) are added to the `cli-portable` job's `go test` list, which today covers
only `internal/local`, `internal/guestagent` and `internal/cli`.

**Phase 2: `internal/usernet`.**
Add the gVisor netstack dependency (pinned), the length-prefixed frame endpoint, ARP, DHCP, DNS
forwarding, and TCP/UDP forwarders through `netpolicy`, plus `DialGuest`.
Acceptance: in-process tests that use a second gVisor stack as the fake guest (fakes only in
`_test.go`) and run on Linux, Windows and macOS in CI. They prove that the guest gets 10.0.2.15 by
DHCP and resolves a name; that a guest connection to a test listener on host 127.0.0.1 is refused,
both by address and through 10.0.2.2; that RFC1918, link-local and a host interface address are
refused, and allowed once opted in; that a public-address dial goes through an injected dialer;
that `DialGuest` reaches a guest listener; that the flow cap holds; that IPv6 frames are dropped;
that the frame reader rejects oversized or truncated frames; that the DNS responder refuses
non-QUERY opcodes and multi-question messages; and that a self-connect to a host address is closed.

**Phase 3: helper and engine integration, opt-in.**
Add the `dock __vmnet` helper and the `BuildArgv` netdev variants (`stream` fd, `socket` fd,
`stream` unix), with argv tests per OS and per QEMU version. Add the `netpid` files, the
`livePID` change, `net.log` in `logTail`, the `Record.NetStack` field, `DOCK_LOCAL_NET=usernet`, and
a `dock doctor` network check. Remove the `freePort` race for `usernet` docks.
Acceptance: engine tests with the fake driver cover the boot order, a helper death marking the dock
stopped, and stop/resume/branch/rm. `scripts/e2e-local.sh` runs its full lifecycle twice in the
`e2e-local` job, once per stack. For `usernet` it adds:
1. A network-on dock cannot reach a host `127.0.0.1` listener through 10.0.2.2, *after*
   `sudo iptables -F; sudo iptables -t mangle -F` in the guest. This is the host-enforced proof.
2. It cannot reach the runner's own non-loopback address or 169.254.169.254.
3. `getent hosts github.com` and `curl https://github.com` work.
4. The agent, `exec`, the terminal and `dock port` (a guest HTTP server fetched through the
   forward) work.
5. Killing the helper stops the dock, and `dock resume` recovers it.

**Phase 4: Windows and macOS.**
Add the Windows Job Object and AF_UNIX paths and the macOS orphan cleanup. Add a CI job per OS that
runs the phase 3 e2e assertions, with WHPX or HVF where the runner supports it and TCG otherwise
(per the phase 0 finding), or on a self-hosted runner if the owner provides one (Q8).
Acceptance: the same five assertions pass on Windows and macOS, or the job is documented as
TCG-only with its known slowness. `cli-portable` covers the per-OS argv and process code.

**Phase 5: default to `usernet` for network-on docks.**
Flip the default. Update the README and `--network` help to state the host-enforced properties and
the behaviour changes in section 4.10. Add `--allow-cidr` to `dock new`. Keep the slirp escape hatch
with a warning. Move `--network off` onto the deny-all policy if Q3 says so.
Acceptance: `DOCK_E2E_GATEWAY_BLOCK` becomes unconditional for `usernet`, and the e2e asserts the
default stack is `usernet`. A docks.json from the previous release resumes on the new stack in the
e2e (create with the old binary, resume with the new one).

**Phase 6: allowlist mode and fleet reuse.**
Add `--network allowlist --allow-host <pattern>` with `egress.Proxy` per stack and `ProxyEnv` on
exec. Feed the result into the Windows fleet design (#37, option D).
Acceptance: an allowlisted dock reaches an allowed HTTPS host through the proxy, is refused for
other hosts and ports, has no DNS and no direct TCP, and 20 parallel CONNECTs succeed (the case
`windows-fleet-hosts.md` section 3.2 found broken with `guestfwd`).

## 7. Open questions for the owner

- **Q1. Process model.** Is a per-dock helper that is QEMU's parent (recommended) acceptable, or
  is a per-user daemon wanted for other reasons, such as persistent `dock port` forwards or a
  future local API?
- **Q2. Stack dependency.** Use gVisor netstack directly with our own DHCP, DNS and forwarders
  (recommended), or depend on `gvisor-tap-vsock` and propose a policy hook upstream?
- **Q3. Network off.** Keep slirp `restrict=on`, or move it to the new stack's deny-all policy
  once the stack is the default?
- **Q4. Old QEMU on Windows (before 7.2).** Refuse network-on docks with a doctor fix, fall back to
  slirp with the guest-side block and a warning, or use the loopback TCP transport with a peer-pid
  check?
- **Q5. Defaults and flags.** Deny LAN and private ranges by default (a behaviour change for users
  who reach LAN services today), with `--allow-cidr` to opt in? Ship `--network allowlist` in this
  epic or later?
- **Q6. Performance budget.** What throughput and per-dock memory, relative to slirp, are
  acceptable before the default flips?
- **Q7. Upgrades on Windows.** `install.ps1` overwrites `dock.exe` in place today, which fails
  while any helper runs. Change the installer to rename the running binary aside first (and clean
  up old copies later), or run the helper from a per-version copy in the data dir?
- **Q8. CI.** Is TCG-only e2e on hosted Windows and macOS runners enough, or will a self-hosted
  WHPX or HVF runner be provided?
- **Q9. ICMP.** Is losing outbound ping acceptable, or should the stack answer ICMP echo through
  unprivileged ICMP sockets where the OS allows it (Linux `ping_group_range`, macOS)?

## 8. Upstream sources read for this note

- QEMU: `net/stream.c` (`reconnect-ms`, client and server modes), `net/stream_data.c` (4-byte
  `htonl` length framing), `net/dgram.c` (address types), `net/meson.build` (stream, dgram and socket
  built on every host OS), `qapi/net.json` (`NetdevPasstOptions` since 10.1, `map-host-loopback`;
  `NetdevStreamOptions` at v7.2.0, v8.0.0 and v10.0.0 for `reconnect`/`reconnect-ms`),
  `util/qemu-sockets.c` (`socket_get_fd`, unguarded unix sockets), `include/system/os-win32.h`
  (`SIO_AF_UNIX_GETPEERPID`), `net/net.c` (`net_fill_rstate`, `NET_BUFSIZE`), commit d409373b9d
  (AF_UNIX on Windows; not in v7.1.0, in v7.2.0), commit adf684ce69 (`stream_data.c`).
- gvisor-tap-vsock: README (QEMU transports, Windows/macOS/Linux use, throughput figures, Apache-2.0),
  `pkg/types/configuration.go` (`NAT`, `Ec2MetadataAccess`, `TCPMaxInFlight`, no filter options),
  `pkg/services/forwarder/tcp.go` (no policy hook), `pkg/tap/protocols.go` (`qemuProtocol`, 4-byte
  big-endian length).
- Go: `os/exec` `Cmd.ExtraFiles` ("not supported on Windows").
- This repo: `scripts/install.sh` (temp file and `mv -f`), `scripts/install.ps1`
  (`Copy-Item -Force` over `dock.exe`).
- passt: GPL-2.0-or-later SPDX headers; the QEMU 10.1 `passt` netdev patches.
