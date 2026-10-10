# docklet

The host agent for [Dock](https://github.com/usedock/dock). One docklet runs on each machine that hosts docks. It boots, pauses, snapshots, resumes and destroys docks, and reports what the host actually has.

> **Status:** this repo is being set up. The code arrives as a fresh first commit when the private monorepo is split. The `dock` CLI and its local engine will live here too; they are not built yet.

## What it does

- Runs docks as Firecracker microVMs (under `jailer`) on Linux with KVM, or as gVisor (`runsc`) containers on Linux with Docker.
- Enforces each dock's CPU, memory and disk with cgroups, and gives it a TAP or veth network on a host-local subnet.
- Moves snapshot data through the control plane's authenticated object proxy.
- Connects **outbound only** to the control plane fleet API: it registers, sends heartbeats with its inventory and stats, long-polls for commands and posts results. No inbound port, no per-host address.
- Serves its own HTTP API on loopback only.
- Fails closed: a host is not offered work until its readiness checks pass.

## Platforms

| Host | Engine | Status |
| --- | --- | --- |
| Linux with KVM | Firecracker | Supported |
| Linux with Docker | gVisor | Supported |
| Windows x86_64 | libkrun on Windows Hypervisor Platform | In progress |
| macOS, Apple silicon | libkrun on Hypervisor.framework | Planned |

A snapshot moves only between hosts with the same engine and CPU architecture.

## Add a host

One command per machine, run as root. It installs the runtime, the docklet and a systemd unit, then enrolls the host:

```sh
sudo bash scripts/bootstrap-host.sh \
  --fleet-url https://dock.example.com \
  --fleet-token "$FLEET_TOKEN"
```

KVM hosts also need the base image the microVMs boot from; `bootstrap-host.sh` builds it, or pass `--base-image`. Images are content-addressed, so distribute one image across the fleet. See `DEPLOY.md` for the full guide.

## Development

Linux only. Requires Go 1.25.

```sh
go build ./...
go test ./...
go test ./internal/host/    # one package
```

Every behavior change ships with `go test` coverage. Fakes live in `*_test.go`, never in production code. `api/v1` is the versioned contract with the control plane; JSON fixtures for every call are published so the control plane can test against the docklet version it supports.

## Contributing

Contributions are welcome. Every contributor signs the CLA once; the CLA bot asks on your first pull request. See `CONTRIBUTING.md`.

## Security

Report vulnerabilities to [security@usedock.io](mailto:security@usedock.io), not in public issues. See `SECURITY.md`. Releases are signed with cosign and the install script stops if verification fails.

## License

[AGPL-3.0-only](LICENSE).
