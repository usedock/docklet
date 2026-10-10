// Package egress confines restricted guests to an allowlist of HTTPS hosts:
// host iptables drop their forwarded traffic and every host-bound packet
// except the proxy port, and a CONNECT proxy admits only allowlisted hosts.
// Enforcement lives on the host because guests run with root.
package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	dockexec "github.com/usedock/docklet/internal/exec"
)

// Proxy is a CONNECT-only forward proxy keyed by guest source IP.
type Proxy struct {
	mu       sync.RWMutex
	policies map[string][]string
	// PrivateAllow admits these non-public ranges, for control planes on a private network.
	PrivateAllow []*net.IPNet
	// Resolve and Dial are overridable for tests.
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
}

// New returns a proxy with no admitted guests.
func New() *Proxy {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &Proxy{
		policies: map[string][]string{},
		Resolve: func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			ips := make([]net.IP, 0, len(addrs))
			for _, a := range addrs {
				ips = append(ips, a.IP)
			}
			return ips, err
		},
		Dial: dialer.DialContext,
	}
}

// Allow admits a guest with its host patterns, replacing any prior policy.
func (p *Proxy) Allow(guestIP string, hosts []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.policies[guestIP] = append([]string(nil), hosts...)
}

// Revoke forgets a guest so a reused IP never inherits its policy.
func (p *Proxy) Revoke(guestIP string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.policies, guestIP)
}

func (p *Proxy) policy(guestIP string) ([]string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	hosts, ok := p.policies[guestIP]
	return hosts, ok
}

// Allowed matches exact hosts, and subdomains for patterns written "*.example.com".
func Allowed(patterns []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, pattern := range patterns {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == pattern {
			return true
		}
	}
	return false
}

// public rejects addresses a guest must never reach through the proxy.
func public(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return !cgnat.Contains(ip)
}

// ParseCIDRs parses a comma-separated CIDR list; empty input yields none.
func ParseCIDRs(list string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		_, n, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("egress private allow %q: %w", item, err)
		}
		nets = append(nets, n)
	}
	return nets, nil
}

func (p *Proxy) dialable(ip net.IP) bool {
	if public(ip) {
		return true
	}
	for _, n := range p.PrivateAllow {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "only CONNECT is supported", http.StatusMethodNotAllowed)
		return
	}
	src, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		http.Error(w, "bad source", http.StatusForbidden)
		return
	}
	hosts, ok := p.policy(src)
	if !ok {
		http.Error(w, "guest has no egress policy", http.StatusForbidden)
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || port != "443" || !Allowed(hosts, host) {
		http.Error(w, fmt.Sprintf("egress to %s denied", r.Host), http.StatusForbidden)
		return
	}
	upstream, err := p.dialPublic(r.Context(), host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		upstream.Close()
		return
	}
	// Bytes the client sent after the CONNECT line are already buffered.
	if n := buf.Reader.Buffered(); n > 0 {
		pending, _ := buf.Reader.Peek(n)
		if _, err := upstream.Write(pending); err != nil {
			client.Close()
			upstream.Close()
			return
		}
	}
	splice(client, upstream)
}

// dialPublic resolves once and dials a vetted address, so DNS rebinding cannot swap in a private target.
func (p *Proxy) dialPublic(ctx context.Context, host string) (net.Conn, error) {
	ips, err := p.Resolve(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if p.dialable(ip) {
			return p.Dial(ctx, "tcp", net.JoinHostPort(ip.String(), "443"))
		}
	}
	return nil, errors.New("no permitted address for " + host)
}

func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tcp, ok := dst.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(a, b)
	go pipe(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}

// rules is the per-guest firewall, listed in final top-to-bottom order.
func rules(guestIP string, port int) [][]string {
	return [][]string{
		{"INPUT", "-s", guestIP, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"INPUT", "-s", guestIP, "-p", "tcp", "--dport", fmt.Sprint(port), "-j", "ACCEPT"},
		{"INPUT", "-s", guestIP, "-j", "DROP"},
		{"FORWARD", "-s", guestIP, "-j", "DROP"},
	}
}

// Restrict installs the guest firewall idempotently: forwarded traffic and
// host-bound traffic drop, except the proxy port and replies to host dials.
func Restrict(run dockexec.Runner, guestIP string, port int) error {
	list := rules(guestIP, port)
	// Insert bottom-up at the chain head so the final order matches the list.
	for i := len(list) - 1; i >= 0; i-- {
		rule := list[i]
		if run("iptables", append([]string{"-C"}, rule...)...) == nil {
			continue
		}
		if err := run("iptables", append([]string{"-I"}, rule...)...); err != nil {
			return fmt.Errorf("egress rule %v: %w", rule, err)
		}
	}
	return nil
}

// Unrestrict removes every copy of the guest firewall rules.
func Unrestrict(run dockexec.Runner, guestIP string, port int) {
	for _, rule := range rules(guestIP, port) {
		for run("iptables", append([]string{"-D"}, rule...)...) == nil {
		}
	}
}

// ProxyEnv points standard proxy variables at the host proxy for exec calls.
func ProxyEnv(gateway string, port int) map[string]string {
	url := fmt.Sprintf("http://%s:%d", gateway, port)
	return map[string]string{
		"HTTPS_PROXY": url, "https_proxy": url,
		"HTTP_PROXY": url, "http_proxy": url,
		"NO_PROXY": "localhost,127.0.0.1", "no_proxy": "localhost,127.0.0.1",
	}
}

// Listen binds the proxy on the guest-facing gateway address only. FREEBIND
// lets it bind before the first TAP carries that address, so the host never
// exposes the proxy on public or loopback interfaces.
func Listen(addr string) (net.Listener, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var opErr error
		if err := c.Control(func(fd uintptr) {
			opErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_FREEBIND, 1)
		}); err != nil {
			return err
		}
		return opErr
	}}
	return lc.Listen(context.Background(), "tcp4", addr)
}
