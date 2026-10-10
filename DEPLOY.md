# Deploying docklet hosts

How to add a machine to a Dock control plane. A docklet host is a **dumb machine that dials
out**: it runs the docklet, connects to the control plane's public URL (for the
hosted cloud `https://api.usedock.io`; for a self-hosted stack see the
[usedock/dock](https://github.com/usedock/dock) repository), and never
accepts an inbound connection. Restate and the snapshot store live on the control
plane, never on a host.

```
              control plane (--fleet-url)
              ├── backend            REST API + fleet API + workflows (9080)
              ├── restate            durable dock workflows
              ├── minio              snapshot generations
              └── postgres / redis   state + rate limits
                        ▲
                        │ outbound HTTPS only (443)
                        │
        ┌───────────────┴───────────────┐
   microvm host (bare metal)      gvisor host (any cheap VM)
   docklet + Firecracker          docklet + Docker/runsc
   → Firecracker microVM docks    → gVisor container docks
```

One host = one `docklet` binary + one systemd unit + one env file. There is no
inbound port, no firewall rule per host, and no database row to insert by hand.

---

## 1. Control plane prerequisites

Do this once, before onboarding hosts.

**Shared fleet token.** It authorizes enrollment only, and only for hosts that
are not enrolled yet: enrolling or registering an existing hostId with it returns
`409 host_already_enrolled`. Enrollment returns the host's own credential once
(only a SHA-256 hash is stored); bootstrap writes it to
`/var/lib/docklet/fleet-credential` (`0600`) and does not store the shared token on
the host. The host then uses its credential for registration, heartbeats, command
polling, results, object transfer and terminal/tunnel relays. Generate and keep
the shared token in the control plane's `.env.production`:

```sh
openssl rand -hex 24     # → FLEET_TOKEN
```

```ini
# control plane .env.production
RUNTIME_DRIVER=restate
RESTATE_INGRESS_URL=http://restate:8080
FLEET_TOKEN=<the token>
SNAPSHOT_STORE_ACCESS_KEY=<minio access key>
SNAPSHOT_STORE_SECRET_KEY=<minio secret>
SNAPSHOT_STORE_BUCKET=dock-snapshots
```

`RUNTIME_DRIVER=restate` is required. Left unset, the backend silently runs the
in-memory `fake` driver and no real dock ever reaches a host.

**Central services.** `docker-compose.prod.yml` starts Restate and MinIO and
creates the bucket. They stay on the internal compose network.

**Any S3-compatible bucket works.** Set `SNAPSHOT_STORE_ENDPOINT` (default
`http://minio:9000`), `SNAPSHOT_STORE_SSL`, and the credentials to point the
control plane at AWS S3 or another provider instead of the bundled MinIO. The
store is deployment config, not a code path: nothing else changes.

Hosts then move artifacts **directly to that bucket** over presigned URLs the
control plane mints per transfer, rather than relaying every byte through the
API. Set `SNAPSHOT_STORE_PUBLIC_ENDPOINT` to the address a host resolves for the
store — with an in-compose MinIO that is an internal hostname hosts cannot
reach, so hosts would otherwise be handed unusable URLs. Related knobs:
`SNAPSHOT_STORE_REGION` (SigV4 signing region, default `us-east-1`),
`SNAPSHOT_STORE_PATH_STYLE` (default `true`), and
`SNAPSHOT_STORE_PRESIGN_TTL_SECONDS` (default `900`).

Direct transfer is a large win. Measured from the AWS test host: **113 MB/s**
straight to S3 versus **2.6-3.2 MB/s** through the object proxy, about 36x. The
presigned URL carries its own authorization, so the bucket credentials still
never reach a host, and the fleet token is never sent to the store.

```sh
# from the control plane's deployment directory (https://github.com/usedock/dock)
docker compose --env-file .env.production -f docker-compose.prod.yml up -d --build
docker compose --env-file .env.production -f docker-compose.prod.yml --profile tools run --rm migrate
```

**Register the workflow endpoint** with Restate (served on `9080` in restate
mode):

```sh
curl -X POST http://127.0.0.1:9070/deployments \
  -H 'content-type: application/json' -d '{"uri": "http://backend:9080"}'
curl -s http://127.0.0.1:9070/deployments    # → services: dock, dockTtl
```

---

## 2. Deploy a host

On each new machine, from the docklet checkout root:

```sh
sudo bash scripts/bootstrap-host.sh \
  --fleet-url https://dock.example.com \
  --fleet-token "$FLEET_TOKEN"
```

That single command is the whole deployment. It is **idempotent** — re-run it any
time to install an update, change a setting, or repair a host.

### What it does

1. Installs base packages (`curl`, `ca-certificates`, `iproute2`, `iptables`,
   `bzip2`, `jq`, `python3`).
2. Installs the runtime:
   - `gvisor` → requires Docker, installs gVisor `runsc`, registers it in
     `/etc/docker/daemon.json`, and **reloads** the daemon (never restarts it, so
     unrelated containers on that host survive).
   - `microvm` → delegates to `provision-host.sh` for Firecracker, `jailer`, device
     setup, and cgroupsv2 checks.
3. Calls `POST /v1/fleet/enroll` to obtain this host's `hostId` and its one-time
   credential, stored in `/var/lib/docklet/fleet-credential` (mode `0600`). No
   object-store endpoint, bucket, or credential is returned — hosts never hold store
   secrets. A re-run on an enrolled host keeps its identity and credential.
4. Writes `/etc/docklet/docklet.env` (mode `0600`) without the shared fleet token.
5. Builds the docklet from the checkout (`go build ./cmd/docklet`) or installs a
   prebuilt binary, then installs it to `/usr/local/bin/docklet`.
6. Builds the image its docks boot from, if absent:
   - `gvisor` → the OCI image `dock-gvisor:latest` from
     `scripts/Dockerfile.gvisor`, rebuilt when its guest agent, entrypoint, or
     Dockerfile changed (tracked by the `dev.usedock.gvisor-inputs` label);
   - `microvm` → the microVM base image (`base.ext4` + `vmlinux` + `initrd.img`)
     via `scripts/build-image.sh` — **minutes and ~20GB**, so it is skipped once
     present.
7. Installs and starts `docklet.service`, then waits for local readiness.

### Flags

| Flag | Meaning |
|---|---|
| `--fleet-url <url>` | control plane base, e.g. `https://dock.example.com` (required; no default) |
| `--fleet-token <token>` | fleet bearer token (required) |
| `--host-id <id>` | stable identity; defaults to the enrolled id on disk, else the control plane derives one |
| `--runtime microvm\|gvisor` | defaults to `microvm` when `/dev/kvm` exists, else `gvisor` |
| `--pool-size N` | warm pool target (see §7) |
| `--egress-allow-private <cidrs>` | private ranges confined guests may reach through the egress proxy, e.g. a control plane on a LAN (see §8) |
| `--allow-vulnerable <names>` | CPU vulnerabilities readiness accepts, e.g. `spec_rstack_overflow`; single-tenant or dev hosts only |
| `--base-image <path\|url>` | microvm: install this pre-built base image (a `.zst` suffix is decompressed) instead of building one; see [Base images](#base-images-one-per-fleet) |
| `--base-image-sha256 <hex>` | sha256 of the raw image (its base fingerprint); required with `--base-image`, a mismatch refuses the install |

Env equivalents: `DOCKLET_FLEET_URL`, `DOCKLET_FLEET_TOKEN`, `DOCKLET_HOST_ID`,
`DOCKLET_RUNTIME`, `DOCKLET_POOL_SIZE`, `DOCKLET_EGRESS_ALLOW_PRIVATE`,
`DOCKLET_ALLOW_VULNERABLE`, `DOCKLET_BASE_IMAGE`, `DOCKLET_BASE_IMAGE_SHA256`.
Installer knobs: `DOCKLET_BIN` (prebuilt binary), `DOCKLET_GVISOR_IMAGE`, `DOCKLET_IMAGE_DIR` (base image location,
default `/var/lib/docklet/images`), `FC_VERSION`, `RUNSC_VERSION`,
`DOCKLET_REBUILD_IMAGE=1` (rebuild an existing
base image), `READINESS_TIMEOUT`.

### gVisor host

Any cheap VM; no KVM, no nested virt, no custom kernel.

```sh
sudo bash scripts/bootstrap-host.sh \
  --fleet-url https://dock.example.com --fleet-token "$FLEET_TOKEN" \
  --runtime gvisor --pool-size 0
```

Registers with `runtime: gvisor` and only ever receives gVisor
placements. The installer needs only Docker: it builds docklet with your own `go`
when present (as the user who ran
sudo, so the checkout stays theirs), and in `golang` containers
otherwise.

### microvm host

Bare metal (or nested-virt instance) with `/dev/kvm`:

Use Linux 5.10+ and util-linux 2.36+. Snapshot inspection mounts pass
`nosymfollow`; older hosts retry without it (the code still refuses symlinks)
but lose the kernel-level defense.

```sh
sudo bash scripts/bootstrap-host.sh \
  --fleet-url https://dock.example.com --fleet-token "$FLEET_TOKEN" \
  --runtime microvm
```

`bootstrap-host.sh` also builds the base image the microVMs boot from
(`/var/lib/docklet/images/{base.ext4,vmlinux,initrd.img}`) through
`build-image.sh`, unless `--base-image` hands it the fleet's pre-built one (the
recommended path for every host after the first, see
[Base images](#base-images-one-per-fleet)). That step is slow — several
minutes and ~20GB, because it debootstraps Ubuntu and installs Docker —
so it runs only when the image is absent. Force a rebuild after a host kernel
upgrade:

```sh
sudo DOCKLET_REBUILD_IMAGE=1 bash scripts/bootstrap-host.sh \
  --fleet-url https://dock.example.com --fleet-token "$FLEET_TOKEN" --runtime microvm
```

The image is minimal Ubuntu 24.04 (systemd, sshd, Docker, git, curl, python3); its name is set in `scripts/image-versions.env`.
`build-image.sh` derives the guest kernel from the running host
kernel (`/boot/vmlinuz-<release>` and `initrd.img-<release>` must exist) and
stamps the release it used beside the image, so a later kernel change is reported
instead of silently reused.

**Layout matters on microvm hosts.** Bootstrap creates the jail base at
`/var/lib/docklet/jail` and writes `DOCKLET_JAIL_BASE`, deliberately *inside* the
state directory. The driver hard-links the guest kernel from the image directory
into each jail root, and provisioning/snapshotting clone the rootfs with
`cp --reflink=auto`; both require the images and the jails to share one
filesystem. With the jail base left at its default (`/srv/jailer` on the root
filesystem) a microvm provision fails with `invalid cross-device link`.

Mount the state directory on one reflink filesystem (XFS with `reflink=1`, or
btrfs) so those copies become metadata-only clones instead of full 20GB data
copies. Override the jail location with `DOCKLET_JAIL_BASE` only if it stays on
the same filesystem as `DOCKLET_IMAGE_DIR`.

### Base images: one per fleet

A microvm generation is a block delta against the exact base image its dock was
provisioned from, so restoring it needs that base, byte for byte. Bases are
therefore **content-addressed** on every host:

```
/var/lib/docklet/images/
  bases/<sha256>/base.ext4     every base this host holds; never rewritten, never auto-deleted
  base.ext4 -> bases/<sha256>/base.ext4   symlink to the current base (new docks)
  vmlinux, initrd.img          guest kernel, staged from the host kernel
```

- The fingerprint is the sha256 of the raw image (`sha256sum base.ext4`), the
  same value the docklet records in each generation manifest
  (`baseFingerprint`) and on the snapshot row.
- New docks clone the current base. A dock keeps diffing against the base it
  came from, and a restore looks up `bases/<manifest fingerprint>`; it fails
  with `base_image_unavailable` (HTTP 409) only when that base is absent, before
  downloading anything.
- `build-image.sh` and `--base-image` install through `scripts/install-base.sh`:
  write to a temp file inside `bases/`, rename into `bases/<sha256>/`, then
  atomically repoint `base.ext4`. A rebuild adds a base and never touches the old
  one, so stopped docks stay restorable and a provision racing the install
  always clones a complete image.
- The guest kernel (`vmlinux`, `initrd.img`) is **not** part of the base
  identity: resume is a fresh boot, so a generation restores under whatever
  kernel the host stages (`scripts/install-kernel.sh`, written beside the target
  and renamed).
- Each host reports the fingerprints it holds on every heartbeat. The control
  plane places a resume, branch, or template boot from a microvm delta generation
  only on a host holding its base, and fails with `base_image_unavailable`
  (HTTP 503) instead of trying hosts that must refuse. Hosts whose docklet
  predates this (no report) are used only when no reporting host holds the base.

**Distribute one image to every microvm host.** `build-image.sh` is not
reproducible (fresh ext4 UUIDs, `builtAt`), so two hosts
that each build their own image hold different bases and cannot restore each
other's generations. Build once, then install that exact file everywhere:

```sh
# on the build host, after bootstrap/build-image.sh
fp=$(basename "$(dirname "$(readlink -f /var/lib/docklet/images/base.ext4)")")
zstd -T0 -o "base-$fp.ext4.zst" "/var/lib/docklet/images/bases/$fp/base.ext4"
# publish base-$fp.ext4.zst (object store, HTTPS), then on every other microvm host:
sudo bash scripts/bootstrap-host.sh --fleet-url … --fleet-token … --runtime microvm \
  --base-image "https://images.example.com/base-$fp.ext4.zst" --base-image-sha256 "$fp"
```

Copying the file by hand works too: `rsync --sparse` it onto the host (for example
to `/var/lib/docklet/incoming.ext4`), then
`sudo OUT_DIR=/var/lib/docklet/images bash scripts/install-base.sh /var/lib/docklet/incoming.ext4 <sha256>`,
which verifies the checksum, installs it under `bases/<sha256>/`, and makes it
current. Never place files into `bases/` directly: a directory there is trusted by
name. A docklet picks up added bases on its next heartbeat, with no restart.
To roll the fleet to a new image, install the new base on every host the same
way; old bases stay for the generations that need them.

---

## 3. Verify a host

Locally on the host:

```sh
systemctl status docklet                     # active (running)
curl -s http://127.0.0.1:8081/v1/readiness   # {"ok":true,"data":{"ready":true}}
curl -s -H "Authorization: Bearer $(cat /var/lib/docklet/local-api-token)" \
  http://127.0.0.1:8081/v1/status            # hostId + running inventory (token required)
journalctl -u docklet -n 30                  # expect "fleet agent online"
```

The local API is bearer-authenticated: every route except `/v1/healthz` and
`/v1/readiness` needs the random token the docklet writes (`0600`) to
`/var/lib/docklet/local-api-token` at each start, and the fleet agent relays with it
automatically. Set `DOCKLET_LOCAL_TOKEN` to use a fixed token instead (still written to that file);
a direct-mode backend (`RUNTIME_DRIVER` not `restate`) must send the same value as its `DOCKLET_LOCAL_TOKEN`. Dock, runtime, and snapshot ids are validated at that boundary.
A failed non-terminal capture (branch/fork) resumes the source dock; a terminal
capture (stop) is never resumed.

Readiness fails closed: `ready:false` lists the missing prerequisite
(`no-docker`, `no-cgroup-v2`, `no-kvm`, `low-memory`).

From the control plane — the host registers itself over the fleet API, so it
appears in the registry with a fresh heartbeat and no manual insert:

```sql
select id, runtime, state, used_docks, max_docks,
       now() - last_heartbeat_at as heartbeat_age
from hosts;
```

`address` reads `outbound`: the recorded value is informational and is never
dialed. A host that misses three heartbeats (30s each) is marked `offline` and
receives no new placements.

Then confirm it can actually run a dock, using the public API:

```
POST /v1/docks  {workspaceId, resources:{cpus:2, memoryMiB:4096, diskGiB:20}}   → poll to `ready`
GET  /v1/docks/:id/stats                                    → source:"host"
POST /v1/docks/:id/stop                                     → `archived`
POST /v1/docks/:id/resume                                   → `ready`
POST /v1/docks/:id/branch                                   → new dock, same state
```

On a microvm host, also check the disk the user gets: inside the dock, `df -h /`
should show about `diskGiB` (the image's root filesystem grows at boot through
`x-systemd.growfs` in its `/etc/fstab`). If it shows the base size (about 20G), the
image predates that entry; rebuild it with `build-image.sh`.

---

## 4. Day-2 operations

**Upgrade a host.** Ship the new code to the machine, then re-run bootstrap:

```sh
git pull        # or rsync the checkout
sudo bash scripts/bootstrap-host.sh --fleet-url … --fleet-token …
```

An old untracked `versions.env` in the docklet directory is unused and can be deleted.

Or ship a binary only, skipping the Go toolchain requirement:

```sh
sudo DOCKLET_BIN=/tmp/docklet bash scripts/bootstrap-host.sh …
```

Bootstrap keeps an already-installed `/usr/local/bin/docklet` when there is no
checkout *and* no Go on `PATH`, so verify the binary timestamp after an upgrade
if you rely on building from source.

**After a kernel upgrade** on a microvm host, the base image still boots its own
kernel, so a running host keeps working; rebuild when you want the image to match
the new kernel (`DOCKLET_REBUILD_IMAGE=1`, above). Bootstrap reports the mismatch
without rebuilding on its own. A rebuild adds a new content-addressed base and
keeps the old one, so stopped docks are unaffected; to keep the fleet on one
image, distribute the new base to every microvm host instead (see
[Base images](#base-images-one-per-fleet)).

**Prune unreferenced bases (manual).** Nothing deletes a base automatically: a
base is needed for as long as any generation built against it exists. List the
fingerprints still referenced, from the control plane database:

```sql
SELECT DISTINCT base_fingerprint FROM snapshots WHERE base_fingerprint IS NOT NULL
UNION SELECT DISTINCT base_fingerprint FROM templates WHERE base_fingerprint IS NOT NULL;
```

and compare with what a host holds (`ls /var/lib/docklet/images/bases`, also
reported per host in `hosts.base_fingerprints`). A base is safe to remove from a
host when it is not current (`readlink /var/lib/docklet/images/base.ext4`) and not in
the list above. If a microvm snapshot row has no `base_fingerprint`, read its
manifest first (`manifest.json` under `snapshots/<storeSnapshotId>/` in the
snapshot bucket, field `baseFingerprint`). Running docks keep their own rootfs
copy, so removing an unreferenced base never affects them; it only makes their
next capture diff against the current base (a larger delta). Remove with
`sudo rm -r /var/lib/docklet/images/bases/<sha256>`.

An install interrupted by a crash or power loss can leave temp files in
`bases/` (`.incoming.*`, `.download.*`, `.raw.*`); they are never used and are
safe to delete once no install is running. `install-base.sh` re-hashes an
existing `bases/<sha256>/` before trusting it and moves a mismatching one aside to
`bases/.corrupt-<sha256>-<time>`; inspect and delete those too.

**Delta format.** Captures write `DOCKDLT2` deltas (record-count trailer)
and manifests with `sha256`/`rawSize`. A docklet that predates this format fails
safely with `not a dock delta` on such a delta.

**Drain a host** (stop taking new work, keep running docks):

```sh
sudo systemctl stop docklet
```

Its heartbeats stop, the control plane marks it `offline` within ~90s, and the
scheduler places nothing new on it. Running docks keep running.

**Decommission a host.** Drain it first, then remove its state:

```sh
sudo systemctl disable --now docklet
sudo rm -rf /etc/docklet /var/lib/docklet
docker rm -f $(docker ps -aq --filter name=dock-)
docker volume rm $(docker volume ls -q --filter name=dock-)
```

Then delete or archive its docks through the API so their generations are pruned
from the central store.

**Rotate the fleet token.** Change `FLEET_TOKEN` in `.env.production` and restart
the backend. Enrolled hosts keep working on their own credentials; the new token is
only needed when enrolling a new machine.

**Reset a host credential.** A host that lost `/var/lib/docklet/fleet-credential`,
or whose credential leaked, is recovered by an instance admin in an interactive
session: `POST /v1/hosts/<id>/reset-credential` replaces the credential and returns
the new one once. Install it on the host and restart:

```sh
echo "<credential>" | sudo tee /var/lib/docklet/fleet-credential >/dev/null
sudo chmod 600 /var/lib/docklet/fleet-credential && sudo systemctl restart docklet
```

A host never falls back to the shared token after its credential is rejected; it
logs that the credential needs an admin reset.

---

## 5. Host configuration reference

`/etc/docklet/docklet.env`, mode `0600`, written by bootstrap:

| Variable | Meaning |
|---|---|
| `DOCKLET_FLEET_URL` | control plane base; unset disables fleet mode |
| `DOCKLET_FLEET_TOKEN` | legacy hosts only: stand-in until the host has its credential; bootstrap no longer writes it |
| `DOCKLET_FLEET_CREDENTIAL_FILE` | where the per-host credential is kept (default `/var/lib/docklet/fleet-credential`) |
| `DOCKLET_HOST_ID` | identity returned by enrollment |
| `DOCKLET_RUNTIME` | `microvm` \| `gvisor` |
| `DOCKLET_LOCAL_TOKEN` | optional fixed local API bearer token (default: random per start, written to `local-api-token`) |
| `DOCKLET_ADDR` | local API bind, always `127.0.0.1:8081` |
| `DOCKLET_GVISOR_IMAGE` | gVisor OCI image tag (gvisor runtime only) |
| `DOCKLET_POOL_SIZE` | warm pool target (present only when set) |
| `DOCKLET_MEMORY_RESERVE_MIB` | RAM held back from guests for the host itself (default: the larger of 1 GiB and 10% of RAM). The docklet registers the remainder as its guest memory budget, the backend places against it, and the docklet refuses a provision with `capacity_exhausted` when the summed requested vCPUs and memory would exceed it |
| `DOCKLET_EGRESS_PORT` | egress proxy port on the guest gateway address, default `3128` |
| `DOCKLET_EGRESS_ALLOW_PRIVATE` | comma-separated CIDRs that are reachable despite being private, e.g. `10.0.0.0/24`: the egress proxy may dial them and every dock may forward to them (see Guest network isolation); empty refuses every private address |
| `DOCKLET_IDEM_TTL` | how long an idempotency token stays deduplicated, a Go duration (default `168h`, 7 days; Go has no `d` unit); expired journal entries are dropped when docklet starts and as the journal grows. Responses over 64 KiB and all `fast-run` read results are never written to disk, only a hash marker: a retried token gets `409 result_not_retained` instead of re-executing (the control plane surfaces it as `exec_result_expired` for exec, and re-reads with a fresh token for reads) |
| `DOCKLET_ALLOW_VULNERABLE` | comma-separated `/sys/devices/system/cpu/vulnerabilities` names readiness accepts (present only when set); empty fails closed on any `Vulnerable` entry |

`docklet.service` runs `EnvironmentFile=/etc/docklet/docklet.env`,
`ExecStart=/usr/local/bin/docklet`, `Restart=always`, `RestartSec=5`,
`LimitNOFILE=1048576`, logging to the journal.

The docklet's HTTP API binds loopback only. Setting `DOCKLET_FLEET_URL` starts
the agent; leaving it unset runs the docklet standalone (local API only, no
control plane), which is useful for manual bring-up but never how a fleet host
should run.

---

## 6. Troubleshooting

Start with the journal and readiness — they name the cause.

| Symptom | Cause and fix |
|---|---|
| `ready:false`, reason `no-docker` | Docker absent or not reachable. Install/start Docker, then re-run bootstrap. |
| `ready:false`, reason `no-cgroup-v2` | Kernel without cgroupsv2. Use a newer kernel image. |
| `ready:false`, reason `cpu-vulnerable:<name>` | Kernel or microcode lacks a mitigation (common on WSL2 and some cloud VMs, e.g. `spec_rstack_overflow` on AMD). Update microcode/kernel. On a single-tenant or dev host, accept it: re-run bootstrap with `--allow-vulnerable <name>`. |
| `ready:false`, reason `no-kvm` | `/dev/kvm` missing on a `microvm` host: not bare metal, or nested virt disabled. Use `--runtime gvisor`, or enable nested virt. |
| `runsc` not registered | `docker info` must list `runsc`. Bootstrap reloads the daemon; if the daemon had a hand-edited `daemon.json`, re-run it. |
| Enroll or register returns `401` | Wrong or rotated `DOCKLET_FLEET_TOKEN`. Re-run bootstrap with the current token. |
| Poll or heartbeat returns `401 host_credential_unknown` | The credential was reset or revoked. An instance admin resets it (§4) and the new credential is installed on the host. |
| Bootstrap/enroll returns `409 host_already_enrolled` | That hostId is enrolled and its credential is not on this machine. Reset it as in §4. |
| Poll fails `502`/`503` | Control plane restarting or unreachable. The agent retries with backoff; check the backend container is up. |
| Commands stay `claimed` and never complete | The host is not posting results. Check the journal; if the control plane rejects the result payload, the host logs the validation error. |
| Docks vanish seconds after creation | A **second** control plane or reconciler owns that `hostId` and is fencing the runtimes as orphans. Guarantee exactly one owner of each host. |
| Stray containers with no dock | Expected to be cleaned automatically: the reconciler compares the host's reported inventory against `docks` and fences runtimes with no row. Confirm via the `Fenced orphan runtime …` log line. |
| Provision fails `no such volume` | The host is running a stale docklet binary. Rebuild/reinstall and re-run bootstrap. |
| Bootstrap dies `build-image.sh: missing /boot/vmlinuz-<release>` | The host kernel has no matching `/boot` image. Install `linux-image-$(uname -r)` or boot a kernel that ships one. |
| microvm host warns the base image was built for another kernel | Harmless (the guest boots its own kernel), but rebuild with `DOCKLET_REBUILD_IMAGE=1` when you want them aligned. |
| microvm provision fails `stage kernel … invalid cross-device link` | `DOCKLET_JAIL_BASE` and `DOCKLET_IMAGE_DIR` are on different filesystems, so the kernel cannot be hard-linked into the jail. Keep the default (`/var/lib/docklet/jail`). |
| microvm stop is slow | A capture is proportional to what the guest changed, and a busy guest (heavy logging, package installs) writes a large delta that must upload. Expect seconds for an idle dock and minutes for a churned one. Keeping the base image and dock rootfs on a reflink filesystem (XFS with `reflink=1`, or btrfs) lets unchanged blocks be skipped by extent maps; without it the same delta is computed by comparing every block, which is slower but still correct. |
| Capture fails with "store refused the upload" | The host could not reach `SNAPSHOT_STORE_PUBLIC_ENDPOINT`. The dock is left running and the stop reports `service_unavailable`. Minted URLs point at that address, so it must be resolvable and reachable from the host, and its certificate trusted; check it against the bucket's real endpoint. The upload is attempted a bounded number of times, each with a freshly signed URL, then fails rather than retrying until the operation times out. |
| microvm provision fails on a freshly bootstrapped host | Check `/var/lib/docklet/images/{base.ext4,vmlinux,initrd.img}` all exist and are non-zero (`ls -lL`; `base.ext4` is a symlink into `bases/`); a skipped build with an empty directory means the earlier failure above was ignored. |
| Resume/branch fails with `base_image_unavailable` | The generation is a delta against a base no eligible host holds (`snapshots.base_fingerprint`, or `baseFingerprint` in its manifest). Install that base on a microvm host with `install-base.sh` or `--base-image` (see Base images); hosts report it on the next heartbeat. |

---

## 7. What each runtime persists

**gvisor.** A generation is a tar of the dock's **workspace volume**
(`/home/user/work`) — the directory agents work in. Captures take seconds and
generations are kilobytes-to-megabytes rather than hundreds of megabytes, because
the base image is never copied. The trade-off is deliberate: under gVisor the
container's own filesystem layers are not visible to `docker diff`, so writes
outside the workspace (for example `apt-get install` inside the dock) do not
survive stop/resume. Docker-inside is unsupported under gvisor; microvm docks
carry those workloads.

**microvm.** A generation is a **block delta against the base
image** plus optional memory images, so the whole guest filesystem survives
with reboot semantics without shipping the base. Hosts hold bases
content-addressed (see [Base images](#base-images-one-per-fleet)), and a
dock's unchanged blocks still share physical extents with its base (reflink), so
unchanged blocks are skipped without even being read, and only the blocks the
guest actually wrote are stored. A base identity travels with each delta, restore
looks that base up by fingerprint and refuses (`base_image_unavailable`) rather
than apply the delta to a different base and produce a corrupt filesystem.
Legacy whole-image generations still restore.

A generation has to cross the network, so its size dominates. Measured on a
20GB image with an idle guest, a capture is about 2-5s wall clock and produces a
~150KB generation, against a timeout and a ~2.2GB whole-image generation before.
A guest that has churned heavily writes a larger delta and takes proportionally
longer, so treat capture time as churn divided by upload bandwidth, not a
constant — the same dock measured 2.2s and, under cold caches with heavier
churn, 86s.

The changed-block search costs about 4ms rather than seconds because it never
reads the image: extent maps identify blocks still tied to the base's own
physical blocks, and only the remaining ranges are compared. Two details carry
that: `FIEMAP_FLAG_SYNC` is mandatory (without it XFS returns a stale map and a
just-written block still looks shared, which would omit a real change), and an
extent only counts as shared when its physical blocks belong to *this* base — a
dock cloned from a sibling reports blocks shared with that sibling, whose bytes
can differ from the base. Skipping those would restore the wrong bytes.

A base image is hashed once and the result is cached beside it
(`bases/<sha256>/base.ext4.fingerprint`), so a host reboot costs nothing extra. The cache is
keyed on size, mtime, ctime and inode, and a digest of an image modified within
the last 2 seconds is never cached (timestamps are too coarse to prove it is
unchanged).

The reflink requirement is a speed requirement, not a correctness one: on a
filesystem without it the same delta is computed by comparing every block, which
is slower but still correct.

Warm pools (`--pool-size`) keep pre-booted fast runtimes ready for sub-second
claims. Leave `0` on a gvisor host until the in-container agent sidecar has
been validated for your image.

---

## 8. Security properties

- **No inbound exposure.** Hosts connect outbound on 443 and listen on loopback
  only. There is no per-host port to open and no address to dial, so a host can
  sit behind NAT on a private subnet.
- **No store credentials on hosts.** Enrollment returns identity and cadence
  only. Generations move on short-lived presigned URLs that the control plane
  mints per transfer, so a host can write and read exactly one object and never
  holds the bucket credentials, which stay inside the control plane.
- **Confined guests leave only through the egress proxy.** Docks booted with an
  egress allowlist (issue-to-PR runs) have their forwarded traffic dropped and
  every host-bound packet refused except the proxy port, by host iptables rules
  keyed to the guest IP, so root inside the guest cannot lift them. The proxy is
  CONNECT-only, admits allowlisted hosts on 443, refuses private, loopback, and
  link-local resolutions (except ranges an operator opts into with
  `DOCKLET_EGRESS_ALLOW_PRIVATE`, for a control plane on a private network),
  and binds only the guest gateway address, never a
  public or loopback interface. Rules are removed on stop and destroy so a
  reused IP never inherits them. Allowlists name hosts, not tenants: an
  allowlisted multi-tenant service (GitHub, a package registry) can still
  receive data pushed with credentials the guest brings itself.
- **Guest network isolation** applies to every dock, confined or not, and is
  re-applied atomically (`iptables-restore --noflush`) on every provision and
  snapshot load, so removing an opt-in takes effect at the next one.
  - `DOCKLET-GUEST` (hooked first in `FORWARD` for the guest subnet) drops
    forwarded traffic to other docks on the subnet, `169.254.0.0/16` (cloud
    metadata), `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`
    and `127.0.0.0/8`; public egress is kept.
  - `DOCKLET-GUEST-IN` (hooked first in `INPUT` for the guest subnet) accepts
    only replies (`ESTABLISHED,RELATED`), ICMP echo requests, and TCP to the
    egress proxy port on the gateway address, then drops everything else, so guests cannot reach host
    services bound to `0.0.0.0` or the host's LAN addresses. Host-initiated
    traffic (fast agent, terminals, tunnels) still works through replies.
  - `DOCKLET_EGRESS_ALLOW_PRIVATE=cidr,cidr` lets guests forward to those
    private ranges; it never overrides the sibling-dock drop and does not open
    host `INPUT`.
  - gvisor containers get a pinned public resolver (`1.1.1.1`) instead of the host
    resolver, which is usually a private address.
  - IPv6 is disabled on each guest-facing host interface
    (`net.ipv6.conf.<iface>.disable_ipv6=1`) because the chains are IPv4 only.
  - To remove: `iptables -D FORWARD -s <subnet> -j DOCKLET-GUEST`,
    `iptables -D INPUT -s <subnet> -j DOCKLET-GUEST-IN`, then `-F` and `-X` both
    chains.
- **The shared fleet token** can only enroll hosts that do not exist yet; it cannot
  take over an enrolled host. Each host authenticates everything else with its own
  credential, and the control plane derives the host from it, so one host cannot
  poll another's commands, post its results, report its stats, or touch generations
  it did not write or was not told to load. Generation ids are bound to the first
  host and capture command that write them, and recorded generations are immutable
  to hosts. Recover with the admin reset in §4.
- **gvisor isolation is process-level on a shared kernel.** Containers run as the
  unprivileged `user`, with all capabilities dropped, `no-new-privileges`, the
  `runsc` runtime, and the requested CPU/memory limits. Steer untrusted code and
  sensitive credentials to `microvm` docks.

## 9. Do not

- Do not install Restate or MinIO on a host. They are central; a per-host copy
  splits workflow state and snapshot storage across machines.
- Do not run a control plane beside a docklet for fleet work. Test against the
  control plane's public URL, because that is the only path a real dock takes.
- Do not bind the docklet's API to `0.0.0.0`. Inbound access is never required.
- Do not point two control planes at one `hostId`; they will fence each other's
  docks.
