# Design: Windows machines as fleet hosts

Status: Proposed, awaiting owner decisions on the open questions (section 7). Tracks [#37](https://github.com/usedock/docklet/issues/37) (part of #31).
No code lands for this until the note is agreed and the outside security review in section 5 is scoped.

Every claim below names the file it comes from. Statements marked **(unverified)** were not checked
against running code or a Windows host while writing this note; each one is a phase 0 spike item.

## 1. Goals and non-goals

Goals

- A Windows 10/11 or Windows Server x86_64 machine with Windows Hypervisor Platform (WHPX) can enroll
  with a control plane and run docks the same way a Linux host does: provision, pause, stop
  (snapshot plus destroy), resume, exec, terminal, port tunnels and the egress allowlist.
- The fleet protocol does not change shape. The Windows host speaks the same outbound channel
  (`internal/agent/agent.go`: register, heartbeat, long-poll, relay into the loopback API) and the same
  `api/v1` routes (`api/v1/types.go`).
- Generations a Windows host writes use the existing manifest format (`internal/store/store.go`
  `Manifest`): `rootfs.ext4.zst`, manifest `sha256`, `sizeBytes` and `dataBytes`. A dock can therefore
  leave a Windows host through the same export path a Linux dock uses, and a Linux export can be
  imported onto a Windows host.
- One QEMU/WHPX implementation, shared by the `dock` CLI local engine (`internal/local`) and the host
  backend, instead of two.
- A readiness gate (`internal/ready`) for Windows, so a host that cannot run docks is never offered
  work, as already required for Linux hosts ("A host is not offered work until readiness passes",
  `AGENTS.md`).

Non-goals (for this design)

- Multi-tenant use. A Windows host runs only docks belonging to the operator who enrolled it
  (section 5). Shared or hosted fleets with Windows hosts are out of scope until the outside review
  is complete and its findings are fixed.
- Memory snapshots, fast fork from memory, and the Fast Run warm pool (`internal/host/pool.go`) on
  Windows.
- The `gvisor` runtime on Windows.
- Windows guests. Guests stay the Linux Dockyard images (`internal/dockyard`).
- arm64 Windows hosts. `internal/local/qemu.go` `BuildArgv` only configures WHPX on the x86 path;
  WHPX support in QEMU for arm64 is **(unverified)**.
- Hyper-V (HCS/vmcompute) as the VMM. It is listed as an alternative in section 3.1 but not designed
  here.
- Browsing generation files (`generation.tree`, `generation.file`) on Windows hosts in the first
  phases (section 4).

## 2. Architecture

### 2.1 What the host looks like today

- `cmd/docklet/main.go` wires one `host.Host` (`internal/host/host.go`) with a `host.Backend`
  (`internal/host/backend.go`), the snapshot coordinator (`internal/snapmgr`), an object store
  (`internal/store`), the egress proxy (`internal/egress`), and the fleet agent (`internal/agent`).
- `host.Backend` is:

  ```go
  Boot(ctx, fc.BootSpec) (*fc.Handle, error)
  Adopt(dockID, jailRoot string, res apiv1.Resources) (*fc.Handle, error)
  Pause / Resume / CreateSnapshot / SnapshotPaths / Terminate
  ```

  Both of its implementations (Firecracker `fc.SDKDriver` and the gVisor `container.Driver`) take and
  return types from package `fc`. `fc.BootSpec` (`internal/fc/fc.go`) carries Firecracker and Linux
  fields: `Firecracker`, `Jailer`, `JailBase`, `UID`, `GID`, `TapName`, `GuestMAC`, `TrackDirty`, and
  `AttachNetwork func(pid int) error`. `fc.Handle` is `{DockID, JailRoot, SocketPath, Pid}`.
- The host already adapts to a second backend through optional interfaces: `usesVeth()` and
  `diskOnly()` in `internal/host/host.go` check for `UseVeth()` / `DiskOnly()` on the driver
  (`internal/container/driver.go`). That pattern is the precedent for a third backend.
- `host.Host` itself does Linux work directly around `Driver.Boot`: TAP and gateway
  (`internal/net/net.go` `EnsureTAP`, `EnsureGateway`, `EnsureNAT` with iptables), static guest
  networking written into a loop-mounted rootfs (`gateRootfs`, `writeGateFiles`), guest agent
  installation into the mounted rootfs (`installFastAgent` in `internal/host/fast.go`), per-dock uids
  (`internal/uid`), cgroup v2 limits (`enforceLimits`, `fc.ApplyLimits`, `internal/host/cgroup.go`),
  and egress iptables (`internal/egress` `Restrict`).
- `internal/server/server.go` `Deps.Host` is the concrete `*host.Host`, not an interface.

Today `GOOS=windows go build ./cmd/docklet` fails (checked on main at 26aa749). The first errors are
in the Firecracker SDK dependencies (`github.com/containernetworking/plugins/pkg/ns`,
`github.com/vishvananda/netns`), `internal/stats` (`syscall.Stat_t`), `internal/egress`
(`syscall.IP_FREEBIND`) and `internal/agent` (`syscall.Statfs`, used for disk capacity). The import of `internal/fc` from `internal/host` pulls the SDK in, which is
why neutral types are a prerequisite rather than a cleanup.

### 2.2 What the local engine does today

`internal/local` already runs Linux docks on Windows:

- `qemu.go` `Accelerator` picks `whpx` on Windows, and `CheckAccel` boots a paused probe machine
  because WHPX can be installed but unusable. `BuildArgv` uses `-accel whpx,kernel-irqchip=off -cpu
  qemu64` ("WHPX has no in-kernel irqchip and cannot expose the host CPU model"), `q35`,
  `-nodefaults -no-user-config -display none -monitor none`, a virtio qcow2 disk, `virtio-rng`, and a
  user-mode (slirp) netdev with one `hostfwd` from `127.0.0.1:<port>` to the guest agent port
  (`guestagent.Port`, 2222); `Network: off` adds `restrict=on`. With `Network: on` the netdev is
  unrestricted, so a local dock can reach the Windows machine's loopback services through 10.0.2.2
  (libslirp `sotranslate_out4` maps the slirp host address to 127.0.0.1). That is the CLI's
  existing trust model and out of scope here, but it is why a fleet host must never use that mode.
  The netdev also leaves IPv6 on (QEMU's default).
- The guest config (agent token, workspace, hostname) reaches the guest as the fw_cfg file
  `opt/usedock/cfg` (`FWCfgName`, `WriteGuestConfig`), never argv. `images/overlay/dock-guest-init.sh`
  copies it to `/etc/dock-fast-agent.json` on every boot.
- Disks: `CreateOverlay` (qcow2 on a cached `base.qcow2`), `ResizeDisk`, `ConvertRaw` (raw to qcow2,
  used by pull `Import`), `FlattenDisk` (qcow2 chain to sparse raw, pre-flagged sparse on NTFS via
  `markSparse` in `disk_windows.go`).
- Processes: `proc_windows.go` starts QEMU detached (`DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP`),
  tracks it by pid plus creation time (`procStart`) so a reused pid is never killed, and kills with
  `TerminateProcess`. Stop is a guest `poweroff` through the agent, then a kill
  (`engine.go` `stopVM`). There is no QEMU monitor, so no pause.
- `acl_windows.go` `secureDir` gives the data dir a protected DACL (current user, SYSTEM,
  Administrators), because `os.MkdirAll` ignores the mode on Windows.

### 2.3 Proposed shape

```
cmd/docklet (windows build)
  host.Host  (unchanged orchestration: locking, capacity, idempotency, generations)
    Backend      -> qemuvm.Driver      (new; QEMU/WHPX, QMP control)
    Network      -> slirpnet           (new seam; Linux keeps tapnet = internal/net + egress iptables)
    Limits       -> jobobject          (new seam; Linux keeps cgroup)
    DiskFormat   -> qcow2disk          (new seam; Linux keeps ext4/reflink in internal/disk)
    GuestConfig  -> fw_cfg             (new seam; Linux keeps rootfs writes)
  snapmgr / store / agent / server      (shared, unchanged wire format)
internal/qemu (new, extracted from internal/local: argv, accel, qemu-img, proc)
  used by internal/local (dock CLI) and qemuvm (docklet)
```

Decisions this asks for:

1. **Extract, do not import.** Move the reusable QEMU pieces out of `internal/local` into
   `internal/qemu`: `BuildArgv` (extended), `Accelerator`, `CheckAccel`, tool discovery, the qemu-img
   calls, and `proc_*.go`. `internal/local` keeps its `Engine` (records, names, ports, CLI output) on
   top of it. The host backend must not depend on `local.Engine`, which is the CLI's user-data model.
2. **Neutral boot types.** Add `internal/vmm` (name open) with:

   ```go
   type BootSpec struct {
       DockID    string
       Resources apiv1.Resources
       Kernel, Initrd string
       Dir       string          // per-dock state dir (jail root on Linux)
       Disk      string          // working disk path
       Limits    Limits          // resolved I/O, net, process limits (today fc.Effective)
       Net       NetAttachment   // tap name + MAC, veth attacher, or user-mode forwards
       Guest     GuestConfig     // token, workspace, hostname, network mode (fw_cfg on QEMU)
       Ext       any             // backend-specific: Firecracker jailer/uid/gid/TrackDirty
   }
   type Handle struct {
       DockID  string
       Dir     string
       Control string           // Firecracker API socket, QMP pipe name
       Pid     int
       Start   uint64           // process creation stamp, guards pid reuse
   }
   ```

   `fc.SDKDriver` and `container.Driver` convert from these at their boundary; `host.Backend`
   switches to them. `Pid` plus `Start` replaces "fence by recorded pid" logic that today only
   exists in the local engine.
3. **Seams on `host.Host`, not a second orchestrator.** The idempotency journal, per-dock locks,
   capacity reservation, generation answers (`snapshotAnswer` in `internal/server/server.go`) and
   the fleet relay are platform-neutral and should stay single. The Linux-only steps around
   `Driver.Boot` in `Provision` and `SnapshotLoad` move behind the four seams above, chosen at
   start-up. Linux behaviour must be byte-for-byte the same after the refactor (phase 1 acceptance).
   The alternative, a separate Windows orchestrator implementing the same routes, is simpler at first
   but duplicates every lifecycle fix; listed as open question Q1.
4. **A new runtime name.** The Windows host registers `runtime: "qemu"` (name open, Q2), not
   `microvm`. The control plane's runtime filter (`holdingBase` and `placeAndRun` inputs in
   `apps/backend/src/hosts/hosts.service.ts` and `apps/backend/src/docks/docks.service.ts`) then keeps
   Firecracker generations and Windows generations apart by default. The filter only applies when a
   runtime is pinned: `placeForDock` in `hosts.service.ts` lets "hosts of any runtime qualify unless
   `runtime` pins one (a restore)", so a fresh provision can land on a `qemu` host. That is the
   intent, but it means the tenancy gate (section 4) must cover fresh provisions too, not only
   restores. Today the control plane rejects any runtime other than `microvm` or `gvisor` in three
   places: `RUNTIMES` in `apps/backend/src/fleet/dto/fleet.dto.ts`, `RUNTIMES` plus `checkedRuntime`
   in `fleet.service.ts`, and `checkedRuntime` in `hosts/hosts.service.ts`, all of which follow the
   `Runtime` type from `RUNTIMES` in `apps/backend/src/database/schema/docks.ts`. That type also keys
   `PERSISTENT_PATHS` (`Record<Runtime, ...>`), which needs a `qemu` entry (`['/']`). The `runtime`
   columns are `text`, so no database migration is needed, but it is a matching control plane
   release.

## 3. Mapping each Linux-only mechanism to Windows

### 3.1 Networking (TAP, gateway, NAT)

Linux today: one TAP per dock (`EnsureTAP`), a /32 gateway, MASQUERADE and guest chains that drop
private, link-local, CGNAT and loopback destinations unless opted back in (`internal/net/net.go`,
list at line 76), a stable guest IP from `net.Allocator`, and a static networkd unit written into the
rootfs. The host reaches the guest agent at `guestIP:2222` (`internal/host/tunnel.go`,
`guestagent.Addr`).

Options on Windows:

| Option | How | For | Against |
|---|---|---|---|
| A. QEMU user-mode (slirp) | `-netdev user` per dock, `hostfwd` for the agent | No driver, no admin network setup, already proven by `internal/local` | All docks share one guest address (10.0.2.15); guest sees the host loopback as 10.0.2.2 unless `restrict=on`; no per-guest host IP for source-keyed policy; slirp is extra code in the VMM process |
| B. TAP + Hyper-V internal vSwitch + WinNAT | QEMU `-netdev tap` through the OpenVPN `tap-windows6` driver, bridged to an internal vSwitch, NAT through `New-NetNat` | Real per-guest IPs, closest to the Linux model, WFP can filter by guest IP | Third-party kernel driver; Hyper-V role and admin setup; one WinNAT per host **(unverified limit)**; more moving parts to break on Windows updates |
| C. Hyper-V (HCS) VMs instead of QEMU | vmcompute / HCS API with a Hyper-V switch | First-party VMM, strongest isolation story on Windows | Different VMM, disk format (VHDX), no fw_cfg; does not reuse `internal/local`; a separate design |

| D. User-mode stack in the docklet | QEMU `-netdev stream` (or `dgram`) carries the NIC's frames to a per-dock user-space TCP/IP stack in the docklet (the gvisor-tap-vsock / gvproxy approach) | Network policy runs outside the QEMU process, per dock and per connection; no slirp in QEMU; arbitrary TCP for unconfined docks is possible | A user-space TCP/IP stack becomes trusted docklet code; `-netdev stream` on the Windows QEMU build is **(unverified)** |

Recommendation: **A for phases 1 to 3, always with `restrict=on` and `ipv6=off`**, with D as the
phase 0 alternative for egress (section 3.2 explains why A's `guestfwd` cannot carry the proxy).
B, C and D stay open (Q3).

Details of A:

- Every dock runs `-netdev user,id=n0,restrict=on,ipv6=off,hostfwd=tcp:127.0.0.1:<agentPort>-:2222`.
  In libslirp (`tcp_input.c`, `udp.c`, `ip_icmp.c`, `udp6.c`), `restrict` drops a guest TCP SYN
  unless it matches a `guestfwd` rule, drops all guest UDP except DHCP and TFTP to slirp itself, and
  drops ICMP echo except to slirp's own addresses; `hostfwd` keeps working because its socket
  already exists. This is read from the libslirp source; the Windows QEMU build still needs the
  phase 0 check.
- **`ipv6=off` is mandatory.** libslirp's restricted-mode check in `tcp_input` compares the IPv4
  view of the header (`ti_dst`) with the `guestfwd` list even for IPv6 segments (the code carries
  `/* TODO: IPv6 */`). In `struct tcpiphdr` (`tcpip.h`) `ti_dst` overlays bytes 4 to 7 of the IPv6
  *source* address, which the guest picks. A guest that adds an IPv6 address whose bytes 4 to 7 are
  10.0.2.100 (for example `2001:db8:a00:264::1`) and opens TCP to any IPv6 host on the `guestfwd`
  port passes the check; the segment then reaches `tcp_fconnect` as a real outbound IPv6 connect,
  or to the host's `::1` if it targets slirp's own IPv6 address. With `ipv6=off` slirp drops IPv6
  input before TCP. Phase 0 reproduces this on the Windows build and pins the fix with an argv test.
- Without `restrict=on`, slirp maps 10.0.2.2 to the host's own loopback. A guest could then reach the
  docklet API on `127.0.0.1:8081`, other docks' agent forwards, and any unauthenticated service the
  machine owner runs on loopback. This is the reason unrestricted slirp is never used on a host.
- `vm.guestIP` becomes the agent endpoint (`127.0.0.1:<port>`), held per dock, instead of a routable
  guest IP. `ProvisionResponse.IP` is stored by the control plane as `dock.ip`
  (`docks.service.ts` line 492) and shown in the dock view (`dock-view.ts`); SSH, terminal and tunnels
  go through the fleet relay (`dock-ssh-gateway.service.ts` uses the exec API, `dock-tunnel.service.ts`
  uses `tunnel.open`), so nothing dials it. The Windows host should report the guest's slirp address
  for display only. Whether any client depends on `ip` being unique is **(unverified)**.
- Port allocation must not race: bind and hold the port, or retry on "host forwarding" failure as
  `internal/local/engine.go` `boot` does. Agent ports bind on 127.0.0.1 only.
- Guest networking config: the guest uses DHCP from slirp. A disk that came from a Linux host carries
  a static `/etc/systemd/network/10-dock.network` matching the dock's MAC (`writeGateFiles`). If the
  Windows backend reuses `MacFor(dockID)`, that stale unit wins and the guest has no network. The
  Windows backend must pass `network: dhcp` in the fw_cfg config, and `dock-guest-init.sh` must then
  delete the static unit and the stale machine-id (today done by `writeGateFiles` on a mounted disk).
  This is an image change and needs a Dockyard image release before Windows hosts can resume Linux
  disks. A cheaper option for the network part alone: the static unit matches on
  `MACAddress=` only, so a Windows backend that derives a different MAC from the dock id never
  matches it, and the image's DHCP unit applies (`dock-guest-init.sh` header). The stale
  machine-id still needs the guest-side step.

### 3.2 Egress allowlist

Linux today (`internal/egress/egress.go`): a restricted guest's forwarded traffic is dropped by
iptables, every host-bound packet except the proxy port is dropped, and a CONNECT-only proxy keyed by
**guest source IP** admits allowlisted HTTPS hosts and refuses non-public addresses unless
`PrivateAllow` lists them. The rules are installed before the guest boots, and again after
(`Provision` in `host.go`, "Confine before the guest exists").

Windows design (enforcement in QEMU's netdev, backed by WFP):

1. **Default-deny in the netdev.** `restrict=on` on every dock. With no `guestfwd`, the guest has no
   route out at all.
2. **One proxy listener per confined dock.** slirp connections to the host all come from 127.0.0.1,
   so source-IP keying does not work. The proxy gets one loopback listener per dock, and the policy is
   keyed by listener. The guest reaches it through
   `guestfwd=tcp:10.0.2.100:3128-tcp:127.0.0.1:<dockProxyPort>`. `egress.ProxyEnv` then points at
   `10.0.2.100:3128`. The admission code (`Allowed`, `dialable`, `public`) is reused unchanged.

   **Blocker for this step as written.** A chardev `guestfwd` cannot carry a proxy. In QEMU's
   `net/slirp.c` `slirp_guestfwd`, a non-`cmd:` rule opens one chardev when the netdev is created,
   every guest connection to `10.0.2.100:3128` writes into that one stream (`guestfwd_write`), and
   bytes coming back are delivered by `slirp_socket_recv` to the first socket that
   `slirp_find_ctl_socket` finds for that address and port. Concrete failure: `npm install` opens
   several CONNECT tunnels at once; the proxy sees their bytes interleaved on one TCP stream and
   replies reach only one of them, so installs hang or fail. There is also no per-connection close.
   The `cmd:` form spawns a helper process per connection, which conflicts with the one-process Job
   Object limit (section 3.3) and puts a new process in the trusted path. So the per-dock proxy needs
   either option D in section 3.1 (frames to a user-space stack in the docklet, which then applies
   the same admission code per connection) or another transport, and that choice is a phase 0 spike
   with a go/no-go before phase 4.
3. **Unconfined docks** get the same mechanism with an "any public host" policy, so the private,
   link-local, CGNAT and loopback block that Linux gets from `guestRestore` also holds on Windows.
   Consequence: on Windows, unconfined docks only get HTTPS/CONNECT egress, without guest DNS
   (step 4), not arbitrary TCP/UDP. That is a product difference (Q4). A general TCP forwarder in the docklet would close it,
   at the cost of more code in the trusted path.
4. **DNS.** slirp's built-in DNS at 10.0.2.3 forwards to the host resolver. With `restrict=on` it does
   not answer: libslirp `udp_input` drops every guest datagram after the DHCP and TFTP cases, and a
   TCP connect to 10.0.2.3:53 is dropped like any other SYN without a `guestfwd`. The proxy resolves
   names itself (CONNECT carries host names), so proxy-aware tools work; tools that resolve before
   connecting fail. That matches Linux confined docks, whose per-guest rules (`rules` in
   `internal/egress/egress.go`) also drop everything but the proxy port. The difference is for
   unconfined docks (step 3), which have DNS on Linux and none here unless option D answers it.
5. **WFP as the second line.** The Linux design has two lines (iptables plus proxy). On Windows the
   second line is a set of persistent Windows Filtering Platform filters, added by the docklet under
   its own provider and sublayer, on `FWPM_LAYER_ALE_AUTH_CONNECT_V4/V6` for the QEMU binary's app id:
   block every outbound connect from QEMU except to the loopback proxy ports. That means a slirp bug
   that ignores `restrict` still cannot reach the network or other loopback services. Whether ALE
   connect filters see loopback connects from a user-mode process is **(unverified)** and is a phase 0
   spike item; if they do not, the docklet API and proxy need named-pipe or token-only protection and
   the review must weigh that.
   An app-id filter cannot tell docks apart: every dock runs the same `qemu-system-x86_64.exe`, so
   "loopback proxy ports" means all of them. A confined dock's compromised QEMU can therefore connect
   to an unconfined dock's listener (policy "any public host") and leave its allowlist. The proxy
   must check, per accepted connection, that the loopback peer's owning process (from
   `GetExtendedTcpTable`) is that dock's QEMU pid with the recorded creation stamp, and refuse
   otherwise. If QEMU runs under a per-dock SID (section 5.1), WFP can also filter on
   `FWPM_CONDITION_ALE_USER_ID`, which does separate docks.
6. **Trust properties.** On Linux, enforcement sits outside the VMM process (host kernel netfilter),
   so a compromised Firecracker process still meets iptables in a jailed network setup. On Windows,
   enforcement step 1 lives **inside the QEMU process**. A guest that gains code execution in QEMU
   (device model or slirp bug) bypasses `restrict` and talks to the network with QEMU's privileges.
   WFP (step 5) is then the only network control left, and it filters by process, not by dock. This is
   weaker than Linux and must be stated as such to operators, and it is why section 5 limits Windows
   hosts to a single tenant. WFP is still a meaningful second line against a slirp logic bug (like
   the IPv6 case in section 3.1) because the kernel applies it whatever QEMU believes; against code
   execution in QEMU it limits destinations, not which dock's policy applies. Option D moves step 1
   out of the QEMU process, which is the main security argument for it.
7. **Order.** As on Linux: the proxy listener and policy exist before QEMU starts, and the dock is
   tracked as confined only after both. `egress-on-resume` requires that `SnapshotLoad` applies the
   same path, which it does by construction because the netdev is built from the spec at boot.

### 3.3 Resource limits (cgroups to Job Objects)

Linux today: `fc.ApplyLimits` writes `cpu.max` (CPUs x 100000 over 100000) and `memory.max`;
`fc.Limits.For` resolves per-size disk MB/s, disk IOPS, net MB/s and a pids limit
(`internal/fc/limits.go`); Firecracker applies the disk and net rate limiters; `killCgroup` kills
every process of a dock.

Windows mapping, one Job Object per dock, created before QEMU starts and assigned at creation
(`CREATE_SUSPENDED`, assign, resume) so no QEMU instruction runs outside it:

| Linux | Windows |
|---|---|
| `cpu.max` | `JobObjectCpuRateControlInformation` with `JOB_OBJECT_CPU_RATE_CONTROL_HARD_CAP`, rate = CPUs / host logical CPUs. `-smp` already bounds vCPU threads |
| `memory.max` | `JOB_OBJECT_LIMIT_JOB_MEMORY` set to guest RAM plus a fixed QEMU overhead (value to be measured in phase 0) |
| pids | `JOB_OBJECT_LIMIT_ACTIVE_PROCESS` = 1: QEMU never needs a child process (true for the current argv; it rules out `guestfwd=...-cmd:` and `smb=`, which spawn processes) |
| kill all of a dock | `TerminateJobObject`, then the pid plus creation-stamp check from `proc_windows.go` |
| disk MB/s, IOPS | QEMU `-drive ...,throttling.bps-total=,throttling.iops-total=` (QEMU block throttling) |
| net MB/s | `JobObjectNetRateControlInformation` on the job **(unverified that it applies to slirp's host sockets)**; otherwise not enforced in phase 1 and reported as such |
| stats CPU, memory | job accounting (`JobObjectBasicAccountingInformation`) and the QEMU process's working set; `internal/stats` today reads cgroup files and `syscall.Stat_t` |
| stats disk used | `allocatedBytes` (`internal/local/disk_windows.go`) on the working disk |
| stats rx/tx | no slirp counters known; report 0 in phase 1 **(unverified whether QMP exposes any)** |

Do not set `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`: Linux docks survive a docklet restart and are
adopted (`Rescan`, `Adopt`), and Windows docks must too. The job is named per dock under a
docklet-only namespace with a DACL so a restarted service can reopen it.

The capacity budget (`CapacityVCPU`, `CapacityMemMiB`, `memReserveMiB` in `cmd/docklet/main.go`) is
platform-neutral and reused. On a workstation the default reserve (larger of 1 GiB and 10% of RAM,
`internal/agent/agent.go`) is too small for an interactive user; the Windows default should be higher
(Q6).

### 3.4 Disk, snapshots and manifest compatibility

Linux today: the working disk is a raw ext4 file (`fc.RootfsName`, `rootfs.ext4`), provisioned with
`cp --reflink=auto --sparse=always` from a content-addressed base (`internal/disk/disk.go`,
`bases.go`), grown with a sparse truncate (`disk.Grow`), and loop-mounted for gating, agent install and
generation browsing (`disk.ExecMounter`). Captures are either a 1 MiB block delta against the base
(`disk.WriteDelta`, `rootfs.delta.zst`, `baseFingerprint`) or, for `Full` requests, a whole image
(`snapmgr.FreezeRootfsFull`) stored as `rootfs.ext4.zst` with manifest `sha256` and `sizeBytes` of the
compressed object and `dataBytes`, the bytes of non-zero 64 KiB blocks (`store.SparseBlock`,
`dataCounter`).

Windows design:

- **Working disk: qcow2 overlay** on a per-image `base.qcow2`, exactly as `internal/local/image.go`
  `Cache` and `CreateOverlay` do today. `growDisk` becomes `qemu-img resize`; the guest grows its
  filesystem on boot (`dock-guest-init.sh` step 3).
- **Wire format: unchanged.** A capture flattens the overlay to a sparse raw ext4 image
  (`FlattenDisk`, already sparse-safe on NTFS) and hands that file to the same
  `store.PutFileCompressed` the Linux path uses. `sha256` is over the compressed bytes and `dataBytes`
  is counted from the decompressed stream, so neither depends on how the raw image was produced.
  `snapmgr.FreezeRootfsFull` takes a raw source path today and then copies it with `disk.Freeze`
  (`cp`); it needs an entry point that accepts an already-frozen raw file so the Windows path does not
  copy twice.
- **Restore: unchanged verification.** `snapmgr.MaterializeDiskWithin` (manifest `sha256`/`sizeBytes`
  check, import size cap at `diskGiB`, family check) writes a raw image; the Windows backend then
  converts it with `ConvertRaw` to a standalone qcow2, as local `Import` does. The verification code
  path is the same Go code on both platforms, which is what lets a Windows host advertise
  `verified-imports`.
- **Deltas: not in phases 1 to 3.** A Windows host captures whole images even for stop/resume
  generations (`Full: false`). `MaterializeDisk` already restores whole-image generations. It reports
  `baseFingerprints: []` on the heartbeat, never `nil`: the control plane treats `nil` as "unknown" and
  may place delta restores there (`holdingBase` in `hosts.service.ts`). The cost is upload size: every
  stop uploads the compressed whole disk. Phase 5 adds deltas by flattening and running
  `disk.WriteDelta` against the raw base it keeps; `fiemap_other.go` means that compare reads every
  block. Keeping a raw ext4 working disk instead of qcow2 (so all of `internal/disk` applies) is the
  alternative (Q5).
- **Consistency of a capture.** Linux pauses the VM and copies the file. On QEMU the backend issues
  QMP `stop` (QEMU drains and flushes block I/O when it stops the VM **(unverified on the Windows
  build)**) and then flattens while paused. For `resumeAfter` captures a better path is QMP
  `blockdev-snapshot-sync`: switch the running VM to a new overlay, resume immediately, and flatten the
  now read-only previous chain in the background **(unverified on the Windows build)**.
- **Generation browsing** (`GenerationTree`/`GenerationFile`, ext4 mount): not available. Parsing a
  guest-controlled ext4 image in a user-space reader inside the docklet is new attack surface and is
  not proposed. The routes return a stable code (proposed `unsupported_on_host`).
- **CPU family.** `cpufamily.Current` reads `/proc/cpuinfo`, which does not exist on Windows, and
  `cpufamily.Compatible` requires an exact, non-empty match. Under WHPX the guest always sees `qemu64`
  (`BuildArgv`), and generations are disk-only, so host CPU identity does not matter for restore. The
  Windows host reports a fixed family, proposed `qemu64-whpx` (Q2), so docks move freely between
  Windows hosts.
- **Moving docks between Linux and Windows.** With different runtime and family values, the control
  plane will not place a Linux generation on a Windows host or the reverse. Moving is done by export
  and import: a `Full` capture on the source host (manifest with `sha256`, `dataBytes`, `image`), then
  an import onto the target, where the control plane writes the manifest with the placed host's
  `cpuFamily` (`wholeImageManifest` in `apps/backend/src/dock-transfers/dock-transfers.service.ts`).
  Exports are refused for host-built bases (`EXPORT_BASE_UNSUPPORTED` in the same file), and a Windows
  host only serves Dockyard images, so every Windows dock is exportable. A direct "re-home" that skips
  the round trip through the user is a later control plane feature (Q7).

### 3.5 Guest agent channel

Linux today: the host calls the agent at `guestIP:2222` with a per-dock token
(`internal/host/fast.go` `fastAgentCall`, `internal/host/tunnel.go`). For microVM docks the token and
the agent program are written into the mounted rootfs (`installFastAgent`).

Windows design:

- The agent and `dock-guest-init` are baked into Dockyard images (`images/overlay`), so nothing is
  written into the disk. Host-built bases without the agent are not supported on Windows.
- The token, workspace, hostname and network mode go through fw_cfg (`opt/usedock/cfg`), as the local
  engine does. The config file is created in the dock's state dir under the protected DACL and
  never appears in argv.
- The host reaches the agent through the dock's `hostfwd` on `127.0.0.1:<port>`. Any local process on
  the Windows machine can connect to that port; the token is the only control. Section 5 lists this.
  The reverse also holds: if QEMU exits (crash, kill) and another local process binds the freed
  port, the docklet's next call sends the dock's token to that process and trusts its answers.
  Before each new connection the docklet checks that the listener's owning pid
  (`GetExtendedTcpTable`) is the dock's QEMU with its creation stamp.
- Later option: a virtio-serial port backed by a named pipe with a DACL for the service account only,
  which removes the loopback listener. This needs a guest agent change and is not in phase 1.
- Pool rekey (`rekeyFastAgent`) and `terminal.open`/`tunnel.open` work unchanged once the address is
  the forward.

### 3.6 Service model and credentials

Linux today: the docklet runs as root under systemd (see `DEPLOY.md`), configured by environment;
the per-host fleet credential is a plain file at `DOCKLET_FLEET_CREDENTIAL_FILE` (default
`<stateDir>/fleet-credential`, `internal/fleetauth/fleetauth.go`), and the enrollment token comes from
`DOCKLET_FLEET_TOKEN`.

Windows design:

- **A Windows service** (`golang.org/x/sys/windows/svc`), installed by `docklet service install`,
  running as a virtual service account (`NT SERVICE\docklet`), not LocalSystem. Whether a virtual
  account can use WHPX without extra rights is **(unverified)**; the local engine shows a normal
  interactive user can.
- **State dir** under `%ProgramData%\docklet` with a protected DACL: the service SID, SYSTEM and
  Administrators only (same approach as `secureDir`, with the service SID instead of the current
  user).
- **Credentials with DPAPI.** The fleet credential and the local API token are stored encrypted with
  `CryptProtectData` in the service account's scope, inside the protected state dir. There are no S3
  credentials to protect on a fleet host: in fleet mode `openStore` (`cmd/docklet/main.go`) sends
  generations through the control plane's object proxy with the fleet credential and ignores
  `SNAPSHOT_STORE_*`. `fleetauth.Credential` gets a storage interface with a plain file
  implementation (Linux, unchanged) and a DPAPI implementation (Windows). Limits to state plainly:
  - DPAPI protects against offline disk reads and other non-admin users. It does not protect against
    a local administrator, or against anything running as the service account, which can call
    `CryptUnprotectData` itself. QEMU processes run as that account today (section 5.1), so a QEMU
    escape gets the credential despite DPAPI. DPAPI only helps against threat 1 once QEMU runs under
    a different, restricted identity.
  - Entropy "bound to the host id" adds nothing against those callers, since the host id sits in the
    same state dir; drop it or describe it as a mix-up guard only.
  - A virtual account (`NT SERVICE\docklet`) has no password, so its DPAPI master key can only be
    protected by a machine secret, not a user secret; anyone who can read that secret (SYSTEM,
    administrators) can decrypt offline. How the key is protected, whether user-scope DPAPI works for
    a virtual account without a loaded profile, and whether it survives profile resets are
    **(unverified)** (phase 0).
    Losing the master key means re-enrolling the host (`resetCredential` exists on the control plane).
- **Enrollment** is a one-shot `docklet enroll --fleet-url ... --token-file ...` run by an
  administrator. It registers, stores the per-host credential, and does not persist the shared
  enrollment token. That matches Linux, where bootstrap writes only the per-host credential
  (`DEPLOY.md`); the difference is that the Windows service never reads `DOCKLET_FLEET_TOKEN` at all,
  where the Linux docklet still accepts it from its environment (`fleetauth.Open`).
- **Local API** stays on `127.0.0.1:8081` behind its bearer token, as on Linux
  (`isLoopbackBind` warning in `cmd/docklet/main.go`). A named pipe is an option for later.
- **QMP channel.** QEMU's Windows `pipe:` chardev creates its named pipe with `CreateNamedPipe(...,
  NULL)` (`chardev/char-pipe.c`): the default DACL (full control for SYSTEM, Administrators and the
  owner, read for Everyone and Anonymous) and no `FILE_FLAG_FIRST_PIPE_INSTANCE`. Read access does not
  let another user issue QMP commands, but the name can be squatted: a local process that creates
  `\\.\pipe\<name>` first, with a permissive DACL, either makes QEMU fail to start (denial of
  service) or ends up as an instance the docklet may connect to, and then answers QMP as if it were
  QEMU (for example, reporting `stop` done while the guest keeps writing during a capture). The
  docklet must check `GetNamedPipeServerProcessId` against the dock's QEMU pid and creation stamp
  after connecting, or use an AF_UNIX socket in the protected state dir if the Windows build
  supports it **(unverified)**.
- **Updates and restarts**: docks survive a service restart (section 3.3). `Rescan` enumerates the
  per-dock state dirs and reopens each dock's job and QMP pipe instead of reading
  `<jailBase>/firecracker`. Updating QEMU itself is different: a running dock keeps the old binary
  until it is stopped and resumed, so a QEMU security fix needs a drain (stop/resume each dock, or
  refuse new work and wait). Readiness refuses a QEMU version below a minimum the docklet release
  pins, and the docklet reports its QEMU version so the control plane can see which hosts lag.
- **Readiness** (`internal/ready`), Windows branch: WHPX usable (the `CheckAccel` probe), QEMU and
  qemu-img present with a supported version, the WFP filters installed, enough memory after the
  reserve, the state dir DACL correct, Hyper-V features consistent. Failing reasons produce 503s on
  mutating routes as today.

## 4. Advertised features and placement

What a Windows host registers (`registerFeatures` in `cmd/docklet/main.go`, features in
`api/v1/types.go` and `apps/backend/src/hosts/host-features.ts`):

| Capability | Windows phase 1-3 | Notes |
|---|---|---|
| `runtime` | `qemu` (new) | Needs the control plane `RUNTIMES` change |
| provision, pause, destroy, stop/resume (disk-first) | yes | Pause via QMP `stop`/`cont` |
| `snapshot.create` `Full` with `sha256`, `dataBytes`, `baseImage` | yes | Wire format unchanged |
| delta generations, `baseFingerprints` | no (reports `[]`) | Phase 5 |
| `verified-imports` | yes, from phase 3 | Same `MaterializeDiskWithin` checks; advertise only once the import e2e passes on Windows. `registerFeatures(egressProxy, microVM)` adds it only for `microvm` today, so it needs a change |
| `egress-on-resume` | yes, from phase 4 | Only with the per-dock proxy plus WFP filters in place, and only when readiness confirms them |
| exec, `terminal.open`, `tunnel.open`, SSH (via control plane) | yes | Through the agent forward |
| memory snapshots (`Memory: true`) | no | Refuse with a stable code |
| `generation.tree` / `generation.file` | no | Refuse with `unsupported_on_host` |
| Fast Run warm pool | no | `PoolSize` forced to 0 |
| net MB/s limit, rx/tx stats | not enforced / zero | Until verified (section 3.3) |
| arbitrary TCP/UDP egress for unconfined docks | no | HTTPS CONNECT and DNS only (Q4) |

Placement rules the control plane must enforce:

- Runtime filter: generations captured with runtime `qemu` place only on `qemu` hosts and the reverse,
  as `holdingBase` already does for `cpuFamily` and `runtime`.
- Features: the existing `withFeature` filter (`hosts.service.ts`) handles `egress-on-resume` and
  `verified-imports` without change.
- Capabilities without a feature flag today (generation browsing, memory capture, Fast Run pool) must
  not be routed to `qemu` hosts. Either the control plane keys them off runtime, or the docklet adds
  new feature names that Linux hosts start advertising (`generation-files`, `memory-snapshots`) so
  absence means "no". The second is more general but every older Linux host would look incapable
  until upgraded (Q8).
- Tenancy: a `qemu` host is only eligible for docks of one organization. Today nothing records who
  a host belongs to: the `hosts` table has no owner column
  (`apps/backend/src/database/schema/hosts.ts`), enrollment uses the single shared `FLEET_TOKEN`
  (`enrollHost` in `hosts.service.ts` creates the row with only an id and a runtime), and
  `placeForDock` takes no organization. So "the account that enrolled it" does not exist yet, and
  "self-hosted" does not mean single-tenant: a self-hosted control plane can hold several
  organizations, and fresh provisions from any of them can place on any runtime (section 2.3).
  Two workable gates, in order of strength:
  1. Owner binding (recommended before anything beyond a lab): an org-scoped enrollment token issued
     from the dashboard by an org admin, a `hosts.organization_id` set at enrollment and immutable
     after, and a placement filter so a host with an owner takes only that org's docks (restores,
     fresh provisions, imports and branch loads alike). Hosts without an owner stay shared, as today.
  2. Interim instance gate: refuse `qemu` registrations unless an instance setting allows them, and
     while it is on, refuse to create a second organization (or refuse the setting when more than
     one exists). A setting alone, without that check, does not keep other tenants off the host.
  Either gate ships in the same control plane release that adds `qemu` to `RUNTIMES`, default off,
  not later.

## 5. Security model and threats

### 5.1 Isolation compared with Linux hosts

| Layer | Linux host (`microvm`) | Windows host (`qemu`) |
|---|---|---|
| Hypervisor | KVM | Hyper-V via WHPX (the hypervisor is strong; the VMM is not Hyper-V's) |
| VMM | Firecracker: small Rust device model, virtio-mmio block/net/vsock, serial, i8042 and little else; no PCI, no firmware | QEMU: C, a full `q35` PC (PCIe root, the ICH9 LPC bridge and its on-board controllers, legacy timers and RTC, ACPI) plus virtio-pci net/blk/rng, fw_cfg and slirp; with `kernel-irqchip=off` the interrupt controllers (PIC, IOAPIC, LAPIC) are emulated in QEMU user space too. `-nodefaults` removes the default display, NIC and drives, not the chipset |
| VMM confinement | jailer: chroot, seccomp, per-dock uid (`internal/uid`), cgroup, network namespace | none today; Job Object for resources only; no seccomp equivalent; same service account for all docks |
| Network enforcement | host netfilter, outside the VMM | inside the VMM (slirp `restrict`), plus WFP by process |
| Host owner | operator-controlled server | possibly a personal workstation with other users and services |

Conclusion: a guest that exploits QEMU on Windows gets the service account's rights on the machine,
can read every other dock's disk and token on that host, and can reach the network subject only to
WFP. On Linux the same bug would land in a jailed, per-dock-uid, seccomp-filtered process.

Hardening to evaluate (phase 0 spikes, each with a go/no-go):

- Run each QEMU under a per-dock restricted token or AppContainer, with WHPX access, read access to
  its own disk and fw_cfg file, and nothing else. Whether WHPX works from an AppContainer is
  **(unverified)**.
- Keep the device set minimal: `-nodefaults` already, no USB, no display, no audio; pin the QEMU
  version and track its security advisories, and refuse to start with an unpinned version.
- Shrink the chipset: try `-machine microvm` (virtio-mmio, no PCI, optional ACPI) under WHPX, or
  failing that turn off the `q35` on-board devices the guest does not need (SATA, SMBus, HPET, PIT
  where the guest kernel copes). Both are **(unverified with WHPX)**; record the device tree
  (`info qtree` over QMP) of the final argv in this note so the outside review scopes it.
- An argv allow-list test: the backend's `BuildArgv` output must contain no `-virtfs`, `-fsdev`, 9p
  device, `smb=`, `-chardev` other than the QMP and serial ones, extra `-drive`, USB or audio
  device. Shared folders between the Windows machine and a guest are not offered on hosts at all.
- Turn on Windows process mitigations for QEMU (CFG, ACG where QEMU tolerates it, no child processes
  via the job) **(unverified compatibility)**.

### 5.2 Threats

1. Guest to host escape through the QEMU device model or slirp. Impact: all docks on the host, the
   fleet credential, the machine. Mitigations: section 5.1 hardening, single tenant only.
2. Guest reaching host loopback services through slirp (10.0.2.2, or slirp's IPv6 host address,
   which maps to `::1`). Mitigation: `restrict=on` and `ipv6=off` always, egress only through the
   chosen per-dock transport (section 3.2), WFP block on QEMU connects.
3. Local users on the Windows machine reaching agent forwards or the docklet API on loopback.
   Mitigation: per-dock agent tokens and the API bearer token stored under the protected DACL; named
   pipes as a later option.
4. Theft of the fleet credential (offline disk access, another local user, malware running as the
   owner). Mitigation: DPAPI plus DACL, no enrollment token at rest, credential rotation and host
   revocation on the control plane (existing `credentialHash` per host). A local administrator is
   out of scope by definition.
5. A compromised or malicious Windows host misusing the control plane: fake inventory and stats, or
   poisoned generations. The control plane scopes object reads and writes to the host's own captures
   (`apps/backend/src/fleet/fleet-object-access.ts`), but a generation it writes is later restored
   elsewhere with the host's own `sha256`, which proves integrity in transit, not honesty of the
   writer. Mitigation: runtime pinning keeps those generations on `qemu` hosts; single tenant keeps
   the blast radius inside the owner's account.
6. Multi-tenant risk. If another account's docks ran on a workstation, the machine owner (and any
   malware on it) could read their disks, memory and tokens. Hosted, shared use is not acceptable for
   this design.
7. Denial of service of the workstation: job limits, the memory reserve, and a cap on concurrent
   docks. `MaxDocks` today is derived from capacity by the docklet (`maxDocks` in
   `internal/agent/agent.go`), not set by the operator, so a workstation cap is a new setting (Q6).
   Also cap the guest-written files the docklet keeps: the serial log (`-serial file:`) grows without
   bound from guest output, and the per-dock state dir sits on the machine's own disk.
8. Supply chain and patching: QEMU for Windows is a third-party binary build. Pin the version and
   its sha256, verify at start (readiness), and document the source. QEMU publishes security fixes
   often; the docklet release that pins a version owns the update, and running docks keep the old
   binary until stop/resume (section 3.6), so a fix needs a drain.
9. Slirp restricted-mode bypass over IPv6 (section 3.1): a guest reaches arbitrary IPv6 hosts, or the
   machine's `::1`, on the `guestfwd` port. Mitigation: `ipv6=off`, an argv test, and WFP blocking
   QEMU's non-loopback connects.
10. Cross-dock policy: one dock's QEMU using another dock's proxy listener or agent forward
    (section 3.2 step 5, section 3.5). Mitigation: peer-pid checks on every loopback channel.
11. QMP named pipe squatting by another local process (section 3.6). Mitigation: verify the pipe
    server pid, or a socket in the protected state dir.
12. Shared folders: QEMU can expose host directories to the guest (`-virtfs`/9p, `smb=`). Never used;
    the argv allow-list test enforces it.

### 5.3 Recommendation

Windows hosts are **single-tenant and self-host only** until the outside review is done and its
findings are closed: the docks on a Windows host belong to the account that enrolled it, and a hosted
multi-tenant fleet does not accept `qemu` hosts. The feature stays labelled experimental in docs and
in `dock` CLI output.

### 5.4 Scope for the outside security review

- QEMU/WHPX on Windows as a guest boundary: the device set from the final `BuildArgv`, the
  `kernel-irqchip=off` path, slirp with `restrict=on` and `guestfwd`, fw_cfg, and the pinned QEMU
  version's open advisories.
- The QEMU process confinement actually achieved (restricted token or AppContainer, job, mitigations),
  with an attempt to reach another dock's files and the credential from a QEMU-context shell.
- Egress: proof that a confined guest cannot reach anything but allowlisted hosts, including through
  DNS, IPv6 (including spoofed IPv6 source addresses), 10.0.2.2/10.0.2.3, `guestfwd` targets,
  another dock's proxy listener, and the proxy's private-range checks; WFP filter
  persistence across reboot and service crash; behaviour when the proxy dies (`serveEgress` exits the
  process on Linux).
- Service account rights, state dir DACLs, DPAPI use, enrollment token handling, local API and agent
  ports and the QMP channel from another local user's session (including squatting a freed port or
  a pipe name).
- Restart and adoption: no window in which a dock runs unconfined or unlimited after a service restart
  or a crash.
- Fleet trust: what a malicious Windows host can do to the control plane and to other hosts through
  the store, with the control plane changes from section 4 applied.

## 6. Phased plan

Each phase is one issue under #37 (or a small epic), each with its own PR. No phase marks Windows
hosts available; that needs phase 6.

**Phase 0: spikes (no product code, results written into this note).**
Acceptance: a written yes/no with evidence for each **(unverified)** item above, at minimum: WFP ALE
filters on QEMU loopback connects; `restrict=on` with `hostfwd`/`guestfwd` and slirp DNS on the
Windows QEMU build; QMP `stop` flush and `blockdev-snapshot-sync` on Windows; Job Object CPU, memory,
network-rate behaviour on a QEMU process; WHPX from a virtual service account and from an
AppContainer; QEMU memory overhead per dock size. Added in review: a live repro of the IPv6
restricted-mode bypass with and without `ipv6=off`; the egress transport (option D with
`-netdev stream` on the Windows build, or another design that carries concurrent CONNECTs, with a
test that runs 20 parallel CONNECTs through it); DPAPI from the virtual account; peer-pid lookup for
loopback TCP and named pipes; `-machine microvm` or a reduced `q35` under WHPX. Phase 0 ends with a
go/no-go per item, and a no-go on the egress transport or on QEMU confinement stops phases 3 and 4
until the owner re-decides Q3.

**Phase 1: neutral types and seams, Linux unchanged.**
Add `internal/vmm` `BootSpec`/`Handle`, convert `fc.SDKDriver` and `container.Driver` at their
boundary, introduce the Network, Limits, DiskFormat and GuestConfig seams inside `host.Host`, and stop
importing the Firecracker SDK from packages a Windows build needs.
Acceptance: `go vet`, `go test`, gofmt clean on Linux CI; the existing host tests pass unchanged; the
self-host e2e (`scripts/e2e-selfhost.sh`) passes; `GOOS=windows go build ./internal/host ./internal/server
./internal/agent ./internal/snapmgr ./internal/store` succeeds.

**Phase 2: extract `internal/qemu` from `internal/local`.**
Move argv, accelerator, qemu-img and process code; `internal/local` uses it.
Acceptance: no behaviour change in the `dock` CLI; the local engine tests and the local e2e
(`internal/local/e2e_test.go`) pass; `BuildArgv` gains QMP, throttling, `restrict=on` and
the chosen egress-transport options covered by argv tests, including that `ipv6=off` is always set
on hosts and the device and shared-folder allow-list (section 5.1).

**Phase 3: `qemuvm` backend, Windows service, readiness, whole-image generations.**
Windows build of `cmd/docklet`, `docklet service install|uninstall`, `docklet enroll`, DPAPI
credential storage, Job Objects, QMP pause/resume, Rescan/Adopt from state dirs, whole-image captures
through `store.PutFileCompressed`, restores through `MaterializeDiskWithin` plus `ConvertRaw`,
`baseFingerprints: []`, Windows readiness. No egress allowlist yet: confined provisions are refused
(fail closed, as `restrictEgress` does today without a proxy).
Acceptance: on a Windows CI or lab host, provision, exec, terminal, tunnel, stop (full capture),
resume, destroy all pass through a real control plane; a generation captured on Windows restores on
another Windows host; a dock exported from Linux imports onto Windows and the reverse, with `sha256`
verified and `dataBytes` equal to what the Linux path records for the same disk; a service restart
adopts running docks; `verified-imports` advertised and the import size cap tested; the agent and
QMP channels refuse a peer that is not the dock's QEMU (tested by squatting the port and the pipe
name); a guest with network off cannot reach host loopback over IPv4 or IPv6.

**Phase 4: egress on Windows.**
Per-dock proxy listeners, `guestfwd`, WFP provider and filters, readiness check for them,
`egress-on-resume` advertised.
Acceptance: tests that a confined dock reaches only allowlisted hosts and nothing on host loopback,
private ranges, link-local or IPv6; parallel CONNECTs from one dock all work; a confined dock cannot
use an unconfined dock's listener; filters survive service restart and reboot; proxy death stops the
service; confinement holds on `snapshot.load`.

**Phase 5: delta generations on Windows (optional, after Q5).**
Keep the raw base, flatten and `disk.WriteDelta` on capture, `ApplyDelta` plus `ConvertRaw` on
restore, report real `baseFingerprints`.
Acceptance: a delta captured on Windows restores on another Windows host with the same base, and the
upload size for a small change is proportional to the change.

**Phase 6: outside security review and availability.**
Acceptance: review report delivered against section 5.4; every high and critical finding fixed with a
test; the control plane changes in section 4 shipped (runtime, placement exclusions, tenancy gate);
docs (`DEPLOY.md` Windows section) published; only then is #37 closed and Windows hosts marked
available as single-tenant, self-host.

Control plane work (usedock/dock) runs alongside: accept the new runtime (phase 3), keep unsupported
operations away from `qemu` hosts (phase 3), the tenancy gate in the same release that accepts the
runtime, default off (section 4), and an optional re-home flow (Q7).

## 7. Open questions for the owner

- Q1. One orchestrator with seams (recommended) or a separate Windows orchestrator behind the same
  routes?
- Q2. Names: runtime `qemu` (or `whpx`), and the fixed CPU family string for Windows hosts.
- Q3. Networking: slirp (`restrict=on`, `ipv6=off`) for the agent channel, plus which egress
  transport, given that a chardev `guestfwd` cannot carry concurrent proxy connections: a user-space
  stack in the docklet over `-netdev stream` (option D, also moves enforcement out of QEMU), TAP plus
  WinNAT (B), or a Hyper-V (HCS) backend instead of QEMU (C)?
- Q4. Is HTTPS-CONNECT-only egress (no guest DNS) acceptable for unconfined docks on Windows, or must
  Windows match Linux's arbitrary TCP/UDP egress before release? Option D can close the gap.
- Q5. Working disk: qcow2 overlays (recommended, reuses proven local code, whole-image captures) or
  raw ext4 like Linux (all of `internal/disk` including deltas applies, but provisioning is a full
  sparse copy without reflink on NTFS)?
- Q6. Default memory reserve and concurrent dock cap on workstations.
- Q7. Is moving docks between Linux and Windows through export/import enough, or is a control-plane
  re-home that rewrites the manifest's family and runtime wanted?
- Q8. Gate unsupported operations by runtime in the control plane, or add new feature names that
  Linux hosts start advertising?
- Q9. Who performs the outside security review, and is it scoped to phases 3 and 4 together, or
  per phase?
- Q10. Minimum Windows versions and editions (Windows 10 22H2, Windows 11, Server 2022/2025), and
  whether Windows Server hosts should be allowed in hosted fleets at all after the review.
- Q11. Tenancy gate: owner binding (org-scoped enrollment token, `hosts.organization_id`, placement
  filter) before the first release, or the interim instance setting that also refuses a second
  organization (section 4)?
- Q12. QEMU distribution: does the docklet installer ship a pinned QEMU build (and who rebuilds it
  for advisories), or does the operator install it and readiness only enforce a minimum version?
- Q13. Are DPAPI's limits acceptable as stated (no protection from QEMU while it runs as the service
  account), or is per-dock QEMU identity (restricted token or AppContainer) a release requirement
  rather than a phase 0 "evaluate"?
