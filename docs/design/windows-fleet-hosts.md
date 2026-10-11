# Design: Windows machines as fleet hosts

Status: draft for review. Tracks [#37](https://github.com/usedock/docklet/issues/37) (part of #31).
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
`github.com/vishvananda/netns`), `internal/stats` (`syscall.Stat_t`) and `internal/egress`
(`syscall.IP_FREEBIND`). The import of `internal/fc` from `internal/host` pulls the SDK in, which is
why neutral types are a prerequisite rather than a cleanup.

### 2.2 What the local engine does today

`internal/local` already runs Linux docks on Windows:

- `qemu.go` `Accelerator` picks `whpx` on Windows, and `CheckAccel` boots a paused probe machine
  because WHPX can be installed but unusable. `BuildArgv` uses `-accel whpx,kernel-irqchip=off -cpu
  qemu64` ("WHPX has no in-kernel irqchip and cannot expose the host CPU model"), `q35`,
  `-nodefaults -no-user-config -display none -monitor none`, a virtio qcow2 disk, `virtio-rng`, and a
  user-mode (slirp) netdev with one `hostfwd` from `127.0.0.1:<port>` to the guest agent port
  (`guestagent.Port`, 2222); `Network: off` adds `restrict=on`.
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
   Firecracker generations and Windows generations apart by default. Today the control plane rejects
   any runtime other than `microvm` or `gvisor` (`RUNTIMES` in
   `apps/backend/src/fleet/dto/fleet.dto.ts` and `fleet.service.ts`), so this needs a matching
   control plane release.

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

Recommendation: **A for phases 1 to 3, always with `restrict=on`**. B and C stay open (Q3).

Details of A:

- Every dock runs `-netdev user,id=n0,restrict=on,hostfwd=tcp:127.0.0.1:<agentPort>-:2222`. Outbound
  traffic leaves only through explicit `guestfwd` rules (section 3.2). QEMU documents that `restrict`
  does not affect explicit forwarding rules **(unverified on the Windows QEMU build)**.
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
  disks.

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
3. **Unconfined docks** get the same mechanism with an "any public host" policy, so the private,
   link-local, CGNAT and loopback block that Linux gets from `guestRestore` also holds on Windows.
   Consequence: on Windows, unconfined docks only get HTTPS/CONNECT egress plus DNS, not arbitrary
   TCP/UDP. That is a product difference (Q4). A general TCP forwarder in the docklet would close it,
   at the cost of more code in the trusted path.
4. **DNS.** slirp's built-in DNS at 10.0.2.3 forwards to the host resolver. With `restrict=on` it is
   **(unverified)** whether 10.0.2.3 still answers. If it does not, the proxy resolves names itself
   (CONNECT carries host names), and guests only need DNS for non-proxied tools.
5. **WFP as the second line.** The Linux design has two lines (iptables plus proxy). On Windows the
   second line is a set of persistent Windows Filtering Platform filters, added by the docklet under
   its own provider and sublayer, on `FWPM_LAYER_ALE_AUTH_CONNECT_V4/V6` for the QEMU binary's app id:
   block every outbound connect from QEMU except to the loopback proxy ports. That means a slirp bug
   that ignores `restrict` still cannot reach the network or other loopback services. Whether ALE
   connect filters see loopback connects from a user-mode process is **(unverified)** and is a phase 0
   spike item; if they do not, the docklet API and proxy need named-pipe or token-only protection and
   the review must weigh that.
6. **Trust properties.** On Linux, enforcement sits outside the VMM process (host kernel netfilter),
   so a compromised Firecracker process still meets iptables in a jailed network setup. On Windows,
   enforcement step 1 lives **inside the QEMU process**. A guest that gains code execution in QEMU
   (device model or slirp bug) bypasses `restrict` and talks to the network with QEMU's privileges.
   WFP (step 5) is then the only network control left, and it filters by process, not by dock. This is
   weaker than Linux and must be stated as such to operators, and it is why section 5 limits Windows
   hosts to a single tenant.
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
| pids | `JOB_OBJECT_LIMIT_ACTIVE_PROCESS` = 1: QEMU never needs a child process |
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
- **Credentials with DPAPI.** The fleet credential, the local API token and the S3 credentials are
  stored encrypted with `CryptProtectData`, entropy bound to the host id, in the service account's
  scope, inside the protected state dir. `fleetauth.Credential` gets a storage interface with a plain
  file implementation (Linux, unchanged) and a DPAPI implementation (Windows). DPAPI protects against
  offline disk reads and other non-admin users; it does not protect against a local administrator,
  who can run code as the service. That limit is stated in the operator docs.
- **Enrollment** is a one-shot `docklet enroll --fleet-url ... --token-file ...` run by an
  administrator. It registers, stores the per-host credential, and does not persist the shared
  enrollment token. The service never needs the shared token in its environment, unlike the Linux
  default.
- **Local API** stays on `127.0.0.1:8081` behind its bearer token, as on Linux
  (`isLoopbackBind` warning in `cmd/docklet/main.go`). A named pipe is an option for later.
- **Updates and restarts**: docks survive a service restart (section 3.3). `Rescan` enumerates the
  per-dock state dirs and reopens each dock's job and QMP pipe instead of reading
  `<jailBase>/firecracker`.
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
| `verified-imports` | yes, from phase 3 | Same `MaterializeDiskWithin` checks; advertise only once the import e2e passes on Windows |
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
- Tenancy: a `qemu` host is only eligible for docks owned by the account that enrolled it. The
  `hosts` table has no owner column today (`apps/backend/src/database/schema/hosts.ts`), so until one
  exists the control plane should refuse `qemu` registrations unless an instance setting allows them,
  which in practice means self-hosted single-tenant control planes only.

## 5. Security model and threats

### 5.1 Isolation compared with Linux hosts

| Layer | Linux host (`microvm`) | Windows host (`qemu`) |
|---|---|---|
| Hypervisor | KVM | Hyper-V via WHPX (the hypervisor is strong; the VMM is not Hyper-V's) |
| VMM | Firecracker: small Rust device model, few virtio devices | QEMU: C, `q35`, virtio-pci, fw_cfg, slirp; with `kernel-irqchip=off` the interrupt controller is emulated in QEMU user space too |
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
- Turn on Windows process mitigations for QEMU (CFG, ACG where QEMU tolerates it, no child processes
  via the job) **(unverified compatibility)**.

### 5.2 Threats

1. Guest to host escape through the QEMU device model or slirp. Impact: all docks on the host, the
   fleet credential, the machine. Mitigations: section 5.1 hardening, single tenant only.
2. Guest reaching host loopback services through slirp (10.0.2.2). Mitigation: `restrict=on` always,
   egress only through `guestfwd` to the proxy, WFP block on QEMU connects.
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
7. Denial of service of the workstation: job limits, the memory reserve, and a host-side cap on
   concurrent docks (`MaxDocks`).
8. Supply chain: QEMU for Windows is a third-party binary build. Pin the version and its sha256,
   verify at start (readiness), and document the source.

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
  DNS, IPv6, 10.0.2.2/10.0.2.3, `guestfwd` targets, and the proxy's private-range checks; WFP filter
  persistence across reboot and service crash; behaviour when the proxy dies (`serveEgress` exits the
  process on Linux).
- Service account rights, state dir DACLs, DPAPI use, enrollment token handling, local API and agent
  ports from another local user's session.
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
AppContainer; QEMU memory overhead per dock size.

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
`guestfwd` options covered by argv tests.

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
adopts running docks; `verified-imports` advertised and the import size cap tested.

**Phase 4: egress on Windows.**
Per-dock proxy listeners, `guestfwd`, WFP provider and filters, readiness check for them,
`egress-on-resume` advertised.
Acceptance: tests that a confined dock reaches only allowlisted hosts and nothing on host loopback,
private ranges, link-local or IPv6; filters survive service restart and reboot; proxy death stops the
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
operations away from `qemu` hosts (phase 3), the tenancy gate (before any hosted exposure), and an
optional re-home flow (Q7).

## 7. Open questions for the owner

- Q1. One orchestrator with seams (recommended) or a separate Windows orchestrator behind the same
  routes?
- Q2. Names: runtime `qemu` (or `whpx`), and the fixed CPU family string for Windows hosts.
- Q3. Networking: confirm slirp with `restrict=on` for the first release, or invest in TAP plus
  WinNAT, or a Hyper-V (HCS) backend instead of QEMU?
- Q4. Is HTTPS-CONNECT-plus-DNS egress acceptable for unconfined docks on Windows, or must Windows
  match Linux's arbitrary TCP/UDP egress before release?
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
