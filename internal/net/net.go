// Package net owns host networking for microVMs: TAP devices, guest IP
// allocation inside one host-local subnet, and guest kernel IP arguments.
package net

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	dockexec "github.com/usedock/docklet/internal/exec"
)

// EnsureTAP creates and raises a TAP device owned by uid, idempotently.
// A stale device from a crashed boot is deleted and recreated once.
func EnsureTAP(run dockexec.Runner, tap string, uid int) error {
	if err := run("ip", "tuntap", "add", "dev", tap, "mode", "tap", "user", fmt.Sprint(uid)); err != nil && !isExists(err) {
		return fmt.Errorf("add tap: %w", err)
	}
	disableIPv6(run, tap)
	if err := run("ip", "link", "set", "dev", tap, "up"); err == nil {
		// A resurrected TAP keeps its old addresses; flush so the gateway
		// below converges instead of colliding with a stale assignment.
		if err := run("ip", "addr", "flush", "dev", tap); err != nil {
			return fmt.Errorf("flush tap: %w", err)
		}
		return nil
	}
	if err := DeleteTAP(run, tap); err != nil {
		return fmt.Errorf("clear stale tap: %w", err)
	}
	if err := run("ip", "tuntap", "add", "dev", tap, "mode", "tap", "user", fmt.Sprint(uid)); err != nil {
		return fmt.Errorf("re-add tap: %w", err)
	}
	disableIPv6(run, tap)
	if err := run("ip", "link", "set", "dev", tap, "up"); err != nil {
		return fmt.Errorf("raise tap: %w", err)
	}
	return nil
}

// DeleteTAP removes a TAP device, ignoring absence.
func DeleteTAP(run dockexec.Runner, tap string) error {
	if err := run("ip", "tuntap", "del", "dev", tap, "mode", "tap"); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// EnsureGateway gives a TAP a point-to-point first hop: the gateway as /32
// plus a /32 route for exactly this guest. A shared /24 on every TAP would
// overlap and misroute host-to-guest traffic once two docks run.
func EnsureGateway(run dockexec.Runner, tap, gateway, guestIP string) error {
	_ = run("ip", "addr", "del", gateway+"/24", "dev", tap)
	if err := run("ip", "addr", "add", gateway+"/32", "dev", tap); err != nil && !isExists(err) && !isAssigned(err) {
		return fmt.Errorf("add gateway: %w", err)
	}
	if err := run("ip", "route", "replace", guestIP+"/32", "dev", tap); err != nil {
		return fmt.Errorf("add guest route: %w", err)
	}
	return nil
}

// GuestChain filters guest-forwarded traffic; GuestInChain filters guest-to-host traffic.
const (
	GuestChain   = "DOCKLET-GUEST"
	GuestInChain = "DOCKLET-GUEST-IN"
)

// denyRanges are never reachable from a guest: metadata and link-local, RFC1918,
// CGNAT and loopback. Operators opt specific ranges back in.
var denyRanges = []string{
	"169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "127.0.0.0/8",
}

// guestRestore renders both chains as one iptables-restore document: sibling
// drop, opt-in returns, then denies for forwarding; replies, echo and the proxy
// port, then drop for host input. Callers validate every CIDR first.
func guestRestore(subnet string, allowPrivate []string, proxyPort int) string {
	var b strings.Builder
	b.WriteString("*filter\n:" + GuestChain + " - [0:0]\n:" + GuestInChain + " - [0:0]\n")
	b.WriteString("-A " + GuestChain + " -d " + subnet + " -j DROP\n")
	for _, cidr := range allowPrivate {
		b.WriteString("-A " + GuestChain + " -d " + cidr + " -j RETURN\n")
	}
	for _, cidr := range denyRanges {
		b.WriteString("-A " + GuestChain + " -d " + cidr + " -j DROP\n")
	}
	b.WriteString("-A " + GuestInChain + " -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT\n")
	b.WriteString("-A " + GuestInChain + " -p icmp --icmp-type echo-request -j ACCEPT\n")
	if _, n, err := net.ParseCIDR(subnet); err == nil && proxyPort > 0 {
		fmt.Fprintf(&b, "-A %s -d %s -p tcp --dport %d -j ACCEPT\n", GuestInChain, gatewayOf(n), proxyPort)
	}
	b.WriteString("-A " + GuestInChain + " -j DROP\nCOMMIT\n")
	return b.String()
}

// gatewayOf is the host-side first hop, the .1 of the subnet like the allocator.
func gatewayOf(n *net.IPNet) string {
	b := n.IP.To4()
	return fmt.Sprintf("%d.%d.%d.1", b[0], b[1], b[2])
}

// ipv4Only drops IPv6 opt-ins, which iptables-restore rejects; the proxy still honours them.
func ipv4Only(cidrs []string) []string {
	var out []string
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil && n.IP.To4() != nil {
			out = append(out, c)
		}
	}
	return out
}

// ensureGuestChains replaces both chains' contents atomically, so removed
// opt-ins vanish and traffic never sees an empty chain, then hooks them.
func ensureGuestChains(run dockexec.Runner, subnet string, allowPrivate []string, proxyPort int) error {
	for _, cidr := range append([]string{subnet}, allowPrivate...) {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("guest chain cidr %q: %w", cidr, err)
		}
	}
	allowPrivate = ipv4Only(allowPrivate)
	doc := guestRestore(subnet, allowPrivate, proxyPort)
	if err := run("sh", "-c", "iptables-restore --noflush <<'DOCKLET_EOF'\n"+doc+"DOCKLET_EOF"); err != nil {
		return fmt.Errorf("restore guest chains: %w", err)
	}
	for _, hook := range [][]string{
		{"FORWARD", "-s", subnet, "-j", GuestChain},
		{"INPUT", "-s", subnet, "-j", GuestInChain},
	} {
		if run("iptables", append([]string{"-C"}, hook...)...) == nil {
			continue
		}
		if err := run("iptables", append([]string{"-I"}, hook...)...); err != nil {
			return fmt.Errorf("hook %s: %w", hook[0], err)
		}
	}
	return nil
}

// EnsureNAT masquerades guest egress behind the host uplink and lets it
// through FORWARD, whose policy is DROP wherever Docker runs. Guest policy
// lives in GuestChain (forwarded) and GuestInChain (host-bound), hooked first
// so they win over existing accepts. allowPrivate lists CIDRs the operator
// opted back in; proxyPort (0 for none) is the one host port guests may dial.
func EnsureNAT(run dockexec.Runner, subnet string, allowPrivate []string, proxyPort int) error {
	if err := ensureGuestChains(run, subnet, allowPrivate, proxyPort); err != nil {
		return err
	}
	for _, rule := range natRules(subnet) {
		table, spec := rule[:2], rule[2:]
		if run("iptables", append(append(append([]string{}, table...), "-C"), spec...)...) == nil {
			continue
		}
		if err := run("iptables", append(append(append([]string{}, table...), "-A"), spec...)...); err != nil {
			return fmt.Errorf("add nat rule %v: %w", spec, err)
		}
	}
	return nil
}

// natRules lists {-t, table, chain, match...} for guest egress and its replies.
func natRules(subnet string) [][]string {
	return [][]string{
		{"-t", "nat", "POSTROUTING", "-s", subnet, "-j", "MASQUERADE"},
		{"-t", "filter", "FORWARD", "-s", subnet, "-j", "ACCEPT"},
		{"-t", "filter", "FORWARD", "-d", subnet, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
	}
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Cannot find device")
}

func isExists(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Persist-mode devices report busy instead of exists; either way the
	// device is there and the link-set step decides what happens next.
	return strings.Contains(msg, "exists") || strings.Contains(msg, "busy")
}

// isAssigned matches iproute2 refusing a duplicate address on the device:
// the gateway is already where we want it, so the call converged.
func isAssigned(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already assigned")
}

// Allocator hands out guest IPs from a subnet, persisted across restarts.
type Allocator struct {
	mu     sync.Mutex
	path   string
	subnet *net.IPNet
	used   map[string]string // ip -> dockID
}

// OpenAllocator loads or creates the allocation file for subnetCIDR.
func OpenAllocator(dir, subnetCIDR string) (*Allocator, error) {
	_, subnet, err := net.ParseCIDR(subnetCIDR)
	if err != nil {
		return nil, err
	}
	a := &Allocator{path: filepath.Join(dir, "ips.json"), subnet: subnet, used: map[string]string{}}
	raw, err := os.ReadFile(a.path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a.used); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// Assign returns the stable IP for a dock, allocating the next free host
// address (.2 and up; .1 is the gateway) on first call.
func (a *Allocator) Assign(dockID string) (guest, gateway, mask string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for ip := range a.used {
		if a.used[ip] == dockID {
			return ip, a.gateway(), a.mask(), nil
		}
	}
	next, ok := a.nextFree()
	if !ok {
		return "", "", "", fmt.Errorf("subnet %s exhausted", a.subnet)
	}
	a.used[next] = dockID
	if err := a.save(); err != nil {
		delete(a.used, next)
		return "", "", "", err
	}
	return next, a.gateway(), a.mask(), nil
}

// ByDock returns the current dock to IP assignments.
func (a *Allocator) ByDock() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]string, len(a.used))
	for ip, dock := range a.used {
		out[dock] = ip
	}
	return out
}

// Release frees a dock's IP.
func (a *Allocator) Release(dockID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for ip, owner := range a.used {
		if owner == dockID {
			delete(a.used, ip)
			return a.save()
		}
	}
	return nil
}

// Reassign moves an IP from one dock id to another, keeping the address.
// Pool claims use it so the VM keeps its TAP and guest config.
func (a *Allocator) Reassign(oldDockID, newDockID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for ip, owner := range a.used {
		if owner == oldDockID {
			a.used[ip] = newDockID
			return a.save()
		}
	}
	return fmt.Errorf("no IP assigned to dock %s", oldDockID)
}

// Gateway is the host-side first hop every guest in the subnet sees.
func (a *Allocator) Gateway() string {
	return a.gateway()
}

func (a *Allocator) gateway() string {
	base := a.subnet.IP.To4()
	return fmt.Sprintf("%d.%d.%d.1", base[0], base[1], base[2])
}

func (a *Allocator) mask() string {
	ones, _ := a.subnet.Mask.Size()
	return fmt.Sprintf("%d", ones)
}

func (a *Allocator) nextFree() (string, bool) {
	base := a.subnet.IP.To4()
	ones, bits := a.subnet.Mask.Size()
	hosts := 1 << (bits - ones)
	for i := 2; i < hosts-1; i++ {
		last := int(base[3]) + i
		if last > 254 {
			continue
		}
		ip := fmt.Sprintf("%d.%d.%d.%d", base[0], base[1], base[2], last)
		if _, taken := a.used[ip]; !taken {
			return ip, true
		}
	}
	return "", false
}

func (a *Allocator) save() error {
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(a.used)
	if err != nil {
		return err
	}
	return os.WriteFile(a.path, raw, 0o600)
}

// disableIPv6 stops the host answering IPv6 on a guest interface, which no
// guest chain filters. Best effort: absent IPv6 means nothing to disable.
func disableIPv6(run dockexec.Runner, iface string) {
	_ = run("sysctl", "-w", "net.ipv6.conf."+iface+".disable_ipv6=1")
}
