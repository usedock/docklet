// Command docklet serves the host agent HTTP API for one bare-metal host.
// All paths come from the environment; readiness fails closed per call.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/agent"
	"github.com/usedock/docklet/internal/container"
	"github.com/usedock/docklet/internal/cpufamily"
	"github.com/usedock/docklet/internal/disk"
	"github.com/usedock/docklet/internal/egress"
	"github.com/usedock/docklet/internal/exec"
	"github.com/usedock/docklet/internal/fc"
	"github.com/usedock/docklet/internal/fleetauth"
	"github.com/usedock/docklet/internal/host"
	"github.com/usedock/docklet/internal/idem"
	"github.com/usedock/docklet/internal/ready"
	"github.com/usedock/docklet/internal/server"
	"github.com/usedock/docklet/internal/snapmgr"
	"github.com/usedock/docklet/internal/stats"
	"github.com/usedock/docklet/internal/store"
	"github.com/usedock/docklet/internal/uid"
)

// version is set at release build time with -ldflags "-X main.version=<tag>".
var version = "dev"

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getint(key string, fallback int) int {
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
	}
	return fallback
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(version)
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "image" {
		p, err := imagePuller(os.Args[2:], getenv("DOCKLET_IMAGE_DIR", "/var/lib/docklet/images"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "docklet: %v\n", err)
			os.Exit(2)
		}
		os.Exit(runImage(os.Args[2:], os.Stdout, os.Stderr, p))
	}
	// The SDK gives firecracker 3s to bind its socket by default; cold
	// binaries on small hosts need longer. Overridable per host.
	if os.Getenv("FIRECRACKER_GO_SDK_INIT_TIMEOUT_SECONDS") == "" {
		_ = os.Setenv("FIRECRACKER_GO_SDK_INIT_TIMEOUT_SECONDS", getenv("DOCKLET_FC_INIT_TIMEOUT", "60"))
	}
	addr := getenv("DOCKLET_ADDR", "127.0.0.1:8081")
	if !isLoopbackBind(addr) {
		// The fleet agent dials out, so the local API never needs to be
		// reachable from the network.
		log.Printf("docklet: warning: %s is not a loopback bind; inbound exposure is not required and not recommended", addr)
	}
	hostID := getenv("DOCKLET_HOST_ID", "host-1")
	stateDir := getenv("DOCKLET_STATE_DIR", "/var/lib/docklet")
	imageDir := getenv("DOCKLET_IMAGE_DIR", "/var/lib/docklet/images")
	vmsDir := getenv("DOCKLET_VMS_DIR", "/var/lib/docklet/vms")
	gensDir := getenv("DOCKLET_GENS_DIR", "/var/lib/docklet/gens")

	family, err := cpufamily.Current("/proc/cpuinfo")
	if err != nil {
		log.Printf("docklet: cpu family unreadable: %v", err)
		family = "unknown"
	}

	driver := fc.NewSDKDriver()
	driver.FirecrackerBin = getenv("DOCKLET_FC_BIN", "/usr/bin/firecracker")
	runtime := getenv("DOCKLET_RUNTIME", apiv1.RuntimeMicroVM)
	var backend host.Backend = driver
	if runtime == apiv1.RuntimeGVisor {
		// gvisor hosts run containers from the OCI image under runsc;
		// the SDK driver serves microvm hosts.
		backend = &container.Driver{Image: getenv("DOCKLET_GVISOR_IMAGE", "dock-gvisor:latest"), StateDir: stateDir}
	}
	fleetAuth := fleetauth.Open(
		getenv("DOCKLET_FLEET_CREDENTIAL_FILE", filepath.Join(stateDir, "fleet-credential")),
		os.Getenv("DOCKLET_FLEET_TOKEN"),
	)
	objectStore := openStore(stateDir, fleetAuth)

	basePointer := filepath.Join(imageDir, "base.ext4")
	isMicroVM := runtime == apiv1.RuntimeMicroVM
	if isMicroVM {
		// Pulled Dockyard images are served from the image dir (gvisor hosts run an OCI image).
		host.SetImageDir(imageDir)
	}
	snaps := &snapmgr.Coordinator{
		Driver: backend,
		Store:  objectStore,
		Mount:  disk.ExecMounter{},
		// microvm generations are deltas against the dock's base image, held
		// content-addressed beside this pointer; shipping it per capture is
		// what made stop slow.
		BaseImage:  basePointer,
		VMsDir:     vmsDir,
		GensDir:    gensDir,
		HostFamily: family,
		Runner:     exec.Run,
	}
	// The SDK resolves relative log paths against our working directory;
	// parking it at the state dir keeps per-dock host logs in one place.
	if err := os.Chdir(stateDir); err != nil {
		log.Fatalf("docklet: chdir state: %v", err)
	}
	for _, dir := range []string{
		filepath.Join(stateDir, "sockets"),
		filepath.Join(stateDir, "logs"),
		filepath.Join(stateDir, "idem"),
		vmsDir,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("docklet: state dir %s: %v", dir, err)
		}
	}
	// The local API is bearer-authenticated: DOCKLET_LOCAL_TOKEN or a random token, kept 0600 in the
	// state dir; the fleet agent relays with it and direct-mode backends must be configured with it.
	localToken, err := server.NewToken(stateDir, os.Getenv("DOCKLET_LOCAL_TOKEN"))
	if err != nil {
		log.Fatalf("docklet: local api token: %v", err)
	}
	// Confined guests reach the internet only through this CONNECT proxy;
	// sources without a per-guest policy are refused before any dial.
	egressProxy := egress.New()
	egressProxy.PrivateAllow, err = egress.ParseCIDRs(os.Getenv("DOCKLET_EGRESS_ALLOW_PRIVATE"))
	if err != nil {
		log.Fatalf("docklet: %v", err)
	}
	var allowPrivate []string
	for _, n := range egressProxy.PrivateAllow {
		allowPrivate = append(allowPrivate, n.String())
	}
	egressPort := getint("DOCKLET_EGRESS_PORT", 3128)
	memReserveMiB := int64(getint("DOCKLET_MEMORY_RESERVE_MIB", 0))
	capVCPU, capMemMiB := agent.Capacity("/", memReserveMiB)
	diskOvercommit, err := parseOvercommit(os.Getenv("DOCKLET_DISK_OVERCOMMIT"))
	if err != nil {
		log.Fatalf("docklet: %v", err)
	}
	iso, err := parseIsolation(os.Getenv)
	if err != nil {
		log.Fatalf("docklet: %v", err)
	}
	if isMicroVM {
		if iso.UIDBase > 0 {
			if err := uid.CheckRange(iso.UIDBase, iso.UIDSize, "/etc/passwd", "/etc/group", "/etc/subuid", "/etc/subgid"); err != nil {
				log.Fatalf("docklet: %v; pick another DOCKLET_UID_BASE", err)
			}
			log.Printf("docklet: jailer uid/gid per dock from %d-%d", iso.UIDBase, iso.UIDBase+iso.UIDSize-1)
			if os.Getenv("DOCKLET_UID") != "" {
				log.Printf("docklet: DOCKLET_UID/GID no longer set the jailer id; they only own guest workspace files")
			}
		} else {
			log.Printf("docklet: WARNING DOCKLET_UID_BASE=0 runs every microvm as shared uid %d (deprecated), a jailer escape reaches every dock", getint("DOCKLET_UID", 1000))
		}
	}
	for _, cpus := range []int{1, 2, 4} {
		log.Printf("docklet: limits for %d vcpu: %s", cpus, iso.Limits.For(apiv1.Resources{CPUs: cpus}))
	}
	jailBase := getenv("DOCKLET_JAIL_BASE", "/srv/jailer")
	// Work disks live in the state dir (gvisor); rootfs images live in the jail base (microvm).
	diskDir := stateDir
	if runtime != apiv1.RuntimeGVisor {
		diskDir = jailBase
	}
	orchestrator, err := host.New(host.Config{
		HostID:      hostID,
		Family:      family,
		ImageBase:   basePointer,
		KernelImage: filepath.Join(imageDir, "vmlinux"),
		// Distro initrd required: without it guests stall before init on
		// minimal-MMIO platforms. Its cloud metadata waits only delay boot
		// (~30-60s) once the NIC has an address.
		InitrdImage:        getenv("DOCKLET_INITRD", filepath.Join(imageDir, "initrd.img")),
		VMsDir:             vmsDir,
		FirecrackerBin:     getenv("DOCKLET_FC_BIN", "/usr/bin/firecracker"),
		JailerBin:          getenv("DOCKLET_JAILER_BIN", "jailer"),
		JailBase:           jailBase,
		CgroupBase:         "/sys/fs/cgroup",
		Subnet:             getenv("DOCKLET_SUBNET", "192.168.127.0/24"),
		UID:                getint("DOCKLET_UID", 1000),
		GID:                getint("DOCKLET_GID", 1000),
		UIDBase:            iso.UIDBase,
		UIDSize:            iso.UIDSize,
		Limits:             iso.Limits,
		PoolSize:           getint("DOCKLET_POOL_SIZE", 0),
		Egress:             egressProxy,
		EgressPort:         egressPort,
		EgressAllowPrivate: allowPrivate,
		CapacityVCPU:       capVCPU,
		CapacityMemMiB:     capMemMiB,
		DiskTotalBytes:     func() int64 { return agent.TotalDiskBytes(diskDir) },
		DiskOvercommit:     diskOvercommit,
		DiskFreeBytes:      func() int64 { return agent.FreeDiskBytes(stateDir) },
		Driver:             backend,
		Snaps:              snaps,
		Stats:              stats.Reader{Root: "/"},
		NetRunner:          exec.Run,
		Mount:              disk.ExecMounter{},
	}, stateDir)
	if err != nil {
		log.Fatalf("docklet: %v", err)
	}

	// Bound to the guest gateway only, keeping the host free of inbound ports.
	egressListener, err := egress.Listen(fmt.Sprintf("%s:%d", orchestrator.EgressGateway(), egressPort))
	if err != nil {
		log.Fatalf("docklet: egress proxy: %v", err)
	}
	go serveEgress(egressListener, egressProxy, log.Fatalf)

	idemTTL := idem.DefaultTTL
	if raw := os.Getenv("DOCKLET_IDEM_TTL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			log.Fatalf("docklet: DOCKLET_IDEM_TTL %q must be a positive duration like 24h", raw)
		}
		idemTTL = d
	}
	journal, err := idem.OpenWith(filepath.Join(stateDir, "idem"), idem.Config{TTL: idemTTL})
	if err != nil {
		log.Fatalf("docklet: idempotency journal: %v", err)
	}

	readyFn := func() []string {
		var allow []string
		for _, a := range strings.Split(getenv("DOCKLET_ALLOW_VULNERABLE", ""), ",") {
			if a = strings.TrimSpace(a); a != "" {
				allow = append(allow, a)
			}
		}
		return ready.Check(ready.Host{Root: "/", MinMemMiB: 4096, Runtime: runtime, Allow: allow})
	}
	if reasons := readyFn(); len(reasons) > 0 {
		log.Printf("docklet: host unready, mutating calls will 503: %v", reasons)
	}
	// Periodic rescan heals registry drift: VMs missed while booting
	// during the last pass get adopted once their socket answers.
	go func() {
		tick := time.NewTicker(60 * time.Second)
		defer tick.Stop()
		for range tick.C {
			if adopted := orchestrator.Rescan(); len(adopted) > 0 {
				log.Printf("docklet: rescan adopted %v", adopted)
			}
		}
	}()
	// Leaked uids are freed on a timer; it stops with the process.
	sweepCtx, stopSweep := context.WithCancel(context.Background())
	defer stopSweep()
	go orchestrator.RunSweeper(sweepCtx, host.SweepInterval)
	// The fleet agent is the only outbound client: it registers this host,
	// heartbeats inventory, and relays queued commands into the loopback API.
	if fleetURL := os.Getenv("DOCKLET_FLEET_URL"); fleetURL != "" {
		fleet := agent.Config{
			Features:         registerFeatures(egressPort > 0, isMicroVM),
			FleetURL:         fleetURL,
			Token:            os.Getenv("DOCKLET_FLEET_TOKEN"),
			Auth:             fleetAuth,
			HostID:           hostID,
			Runtime:          runtime,
			MemoryReserveMiB: memReserveMiB,
			Family:           family,
			LocalAPIBase:     localAPIBase(addr),
			LocalToken:       localToken,
			Host:             orchestrator,
			OpenTerminal: func(ctx context.Context, dockID string, cols, rows int) (agent.TerminalStream, error) {
				// Returned directly, a nil *host.Terminal would be a non-nil interface.
				term, err := orchestrator.OpenTerminal(ctx, dockID, cols, rows)
				if err != nil {
					return nil, err
				}
				return term, nil
			},
			OpenTunnel: func(ctx context.Context, dockID string, port int) (net.Conn, error) {
				conn, err := orchestrator.OpenPort(ctx, dockID, port)
				if err != nil {
					return nil, tunnelError(err)
				}
				return conn, nil
			},
		}
		if isMicroVM {
			fleet.Images = func() []string { return host.ServedImages(imageDir) }
			fleet.Bases = func() []string {
				fps, err := disk.ListBases(basePointer)
				if err != nil {
					log.Printf("docklet: listing base images: %v", err)
				}
				return fps
			}
		}
		log.Printf("docklet: fleet agent enabled: control plane %s host %s", fleetURL, hostID)
		go func() {
			if err := fleet.Run(context.Background()); err != nil {
				log.Printf("docklet: fleet agent stopped: %v", err)
			}
		}()
	} else {
		log.Printf("docklet: standalone mode: DOCKLET_FLEET_URL unset, serving the local API only")
	}
	log.Printf("docklet %s %s listening on %s (family %s)", version, hostID, addr, family)
	if err := http.ListenAndServe(addr, server.NewMux(server.Deps{
		Host:    orchestrator,
		Journal: journal,
		Ready:   readyFn,
		HostID:  hostID,
		Token:   localToken,
	})); err != nil {
		log.Fatalf("docklet: %v", err)
	}
}

// openStore picks the generation store: the control plane's chunked object
// proxy on a fleet host, a local S3 endpoint for standalone deployments, and
// in-memory last (generations then stay host-local).
func openStore(stateDir string, auth *fleetauth.Credential) store.Store {
	fleetURL, fleetToken := os.Getenv("DOCKLET_FLEET_URL"), auth.Bearer()
	if fleetURL != "" && fleetToken != "" {
		if os.Getenv("SNAPSHOT_STORE_ENDPOINT") != "" {
			log.Printf("docklet: fleet mode ignores SNAPSHOT_STORE_*: generations go through the control plane")
		}
		fleet, err := store.DialFleet(fleetURL, fleetToken)
		if err != nil {
			log.Printf("docklet: fleet store unavailable, generations stay host-local: %v", err)
			return store.NewMemStore()
		}
		// An artifact of unknown length spills beside the rest of the state
		// rather than into a possibly small system temp dir.
		fleet.Bearer = auth.Bearer
		fleet.ScratchDir = stateDir
		return fleet
	}
	if endpoint := os.Getenv("SNAPSHOT_STORE_ENDPOINT"); endpoint != "" {
		s3, err := store.DialS3(
			endpoint,
			getenv("SNAPSHOT_STORE_BUCKET", "dock-snapshots"),
			os.Getenv("SNAPSHOT_STORE_ACCESS_KEY"),
			os.Getenv("SNAPSHOT_STORE_SECRET_KEY"),
			getenv("SNAPSHOT_STORE_SSL", "true") == "true",
		)
		if err != nil {
			log.Printf("docklet: store unreachable, generations stay host-local: %v", err)
			return store.NewMemStore()
		}
		return s3
	}
	return store.NewMemStore()
}

// isLoopbackBind reports whether addr binds only the loopback interface.
func isLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// localAPIBase points the fleet agent at the API this process serves; a
// wildcard bind falls back to loopback, which is always reachable locally.
// An unparsable address leaves the agent on its contract default.
func localAPIBase(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// tunnelError tags a guest port failure for the control plane; only an outdated
// guest agent is "unavailable", since stop and resume is its fix.
func tunnelError(err error) error {
	switch {
	case errors.Is(err, host.ErrPortClosed):
		return &agent.TunnelError{Code: "port_closed", Err: err}
	case errors.Is(err, host.ErrTunnelUnavailable):
		return &agent.TunnelError{Code: "tunnel_unavailable", Err: err}
	default:
		return &agent.TunnelError{Code: "tunnel_failed", Err: err}
	}
}

// parseOvercommit reads DOCKLET_DISK_OVERCOMMIT: a finite positive ratio, 1.0 when unset.
func parseOvercommit(raw string) (float64, error) {
	if raw == "" {
		return 1, nil
	}
	r, err := strconv.ParseFloat(raw, 64)
	if err != nil || r <= 0 || math.IsInf(r, 0) || math.IsNaN(r) {
		return 0, fmt.Errorf("DOCKLET_DISK_OVERCOMMIT %q must be a positive number like 1.0 or 1.5", raw)
	}
	return r, nil
}

// serveEgress runs the egress proxy and calls fatal if it stops for any reason
// but a clean shutdown. Confined guests lose all egress once the proxy is gone
// while the host keeps advertising egress-on-resume, so the process exits and
// the service manager restarts it.
func serveEgress(l net.Listener, h http.Handler, fatal func(format string, args ...any)) {
	err := http.Serve(l, h)
	if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return
	}
	fatal("docklet: egress proxy stopped, confined guests lose egress: %v", err)
}

// registerFeatures is what the host advertises at registration: egress-on-resume needs the egress
// proxy, verified-imports a microvm host (uploaded disks are whole ext4 images).
func registerFeatures(egressProxy, microVM bool) []string {
	var features []string
	if egressProxy {
		features = append(features, apiv1.DockletFeatures...)
	}
	if microVM {
		features = append(features, apiv1.FeatureVerifiedImports)
	}
	return features
}
