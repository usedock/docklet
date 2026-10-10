package net

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"

	dockexec "github.com/usedock/docklet/internal/exec"
)

// VethFor derives a stable <=15 char host-side veth name per dock.
// The dv- prefix distinguishes container veth from microVM TAP (dt-).
func VethFor(dockID string) string {
	sum := md5.Sum([]byte(dockID))
	return "dv-" + hex.EncodeToString(sum[:])[:8]
}

// vethPeer is the temporary guest-side peer; AttachVeth moves it into the
// container netns and renames it to eth0.
func vethPeer(hostVeth string) string {
	return hostVeth + "-g"
}

// EnsureVeth creates a veth pair and raises the host side, idempotently.
// A stale pair from a crashed boot is deleted and recreated once.
func EnsureVeth(run dockexec.Runner, hostVeth string) error {
	peer := vethPeer(hostVeth)
	if err := run("ip", "link", "add", hostVeth, "type", "veth", "peer", "name", peer); err != nil && !isExists(err) {
		return fmt.Errorf("add veth: %w", err)
	}
	disableIPv6(run, hostVeth)
	if err := run("ip", "link", "set", "dev", hostVeth, "up"); err == nil {
		return nil
	}
	if err := DeleteVeth(run, hostVeth); err != nil {
		return fmt.Errorf("clear stale veth: %w", err)
	}
	if err := run("ip", "link", "add", hostVeth, "type", "veth", "peer", "name", peer); err != nil {
		return fmt.Errorf("re-add veth: %w", err)
	}
	disableIPv6(run, hostVeth)
	if err := run("ip", "link", "set", "dev", hostVeth, "up"); err != nil {
		return fmt.Errorf("raise veth: %w", err)
	}
	return nil
}

// DeleteVeth removes a veth pair, ignoring absence.
func DeleteVeth(run dockexec.Runner, hostVeth string) error {
	if err := run("ip", "link", "del", hostVeth); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// AttachVeth moves the guest peer into the container netns (pid), renames
// it eth0, addresses both sides, and routes the guest. The host side keeps
// the /32 gateway address so EnsureGateway/NAT semantics match TAP docks.
func AttachVeth(run dockexec.Runner, hostVeth string, pid int, guestIP, gateway, mask string) error {
	peer := vethPeer(hostVeth)
	pidStr := fmt.Sprint(pid)
	if err := run("ip", "link", "set", peer, "netns", pidStr); err != nil {
		return fmt.Errorf("move veth to netns: %w", err)
	}
	if err := run("nsenter", "-t", pidStr, "-n", "ip", "link", "set", "dev", peer, "name", "eth0"); err != nil {
		return fmt.Errorf("rename guest veth: %w", err)
	}
	if err := run("nsenter", "-t", pidStr, "-n", "ip", "link", "set", "dev", "eth0", "up"); err != nil {
		return fmt.Errorf("raise guest veth: %w", err)
	}
	if err := run("nsenter", "-t", pidStr, "-n", "ip", "addr", "add", guestIP+"/"+mask, "dev", "eth0"); err != nil {
		return fmt.Errorf("addr guest veth: %w", err)
	}
	if err := run("nsenter", "-t", pidStr, "-n", "ip", "route", "add", "default", "via", gateway, "dev", "eth0"); err != nil {
		return fmt.Errorf("route guest veth: %w", err)
	}
	return nil
}
