package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAllowedMatchesExactAndWildcardOnly(t *testing.T) {
	patterns := []string{"github.com", "*.npmjs.org"}
	cases := map[string]bool{
		"github.com":           true,
		"GitHub.com.":          true,
		"api.github.com":       false,
		"registry.npmjs.org":   true,
		"npmjs.org":            false,
		"evilgithub.com":       false,
		"registry.npmjs.org.x": false,
	}
	for host, want := range cases {
		if got := Allowed(patterns, host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// proxyFor serves the proxy on loopback and routes every dial to target.
func proxyFor(t *testing.T, target string, resolved net.IP) (*Proxy, string) {
	t.Helper()
	p := New()
	p.Resolve = func(context.Context, string) ([]net.IP, error) { return []net.IP{resolved}, nil }
	p.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return p, strings.TrimPrefix(srv.URL, "http://")
}

func connect(t *testing.T, proxyAddr, target string) (int, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, conn
}

func TestProxyTunnelsAllowlistedHostsEndToEnd(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello from upstream")
	}))
	defer upstream.Close()
	p, addr := proxyFor(t, strings.TrimPrefix(upstream.URL, "https://"), net.ParseIP("140.82.112.3"))
	p.Allow("127.0.0.1", []string{"github.com"})
	status, conn := connect(t, addr, "github.com:443")
	defer conn.Close()
	if status != http.StatusOK {
		t.Fatalf("allowlisted CONNECT status = %d", status)
	}
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	fmt.Fprint(tlsConn, "GET / HTTP/1.1\r\nHost: github.com\r\nConnection: close\r\n\r\n")
	body, _ := io.ReadAll(tlsConn)
	if !strings.Contains(string(body), "hello from upstream") {
		t.Fatalf("tunnel did not reach upstream: %q", body)
	}
}

func TestProxyDeniesEverythingOutsidePolicy(t *testing.T) {
	p, addr := proxyFor(t, "127.0.0.1:1", net.ParseIP("140.82.112.3"))
	if status, _ := connect(t, addr, "github.com:443"); status != http.StatusForbidden {
		t.Fatalf("guest without policy must be refused, got %d", status)
	}
	p.Allow("127.0.0.1", []string{"github.com"})
	for _, target := range []string{"evil.example:443", "github.com:22", "github.com:80"} {
		if status, _ := connect(t, addr, target); status != http.StatusForbidden {
			t.Fatalf("%s must be refused, got %d", target, status)
		}
	}
	p.Revoke("127.0.0.1")
	if status, _ := connect(t, addr, "github.com:443"); status != http.StatusForbidden {
		t.Fatalf("revoked guest must be refused, got %d", status)
	}
}

func TestProxyRefusesPrivateResolutions(t *testing.T) {
	for _, ip := range []string{"10.0.0.5", "127.0.0.1", "169.254.169.254", "100.64.1.1", "192.168.1.1"} {
		p, addr := proxyFor(t, "127.0.0.1:1", net.ParseIP(ip))
		p.Allow("127.0.0.1", []string{"github.com"})
		if status, _ := connect(t, addr, "github.com:443"); status != http.StatusBadGateway {
			t.Fatalf("allowlisted host resolving to %s must not be dialed, got %d", ip, status)
		}
	}
}

func TestProxyDialsOnlyOptedInPrivateRanges(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(upstream.Close)
	target := strings.TrimPrefix(upstream.URL, "https://")
	allow, err := ParseCIDRs("10.20.0.0/16, 192.168.50.0/24")
	if err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]int{
		"10.20.3.4":       http.StatusOK,
		"192.168.50.9":    http.StatusOK,
		"10.21.0.1":       http.StatusBadGateway,
		"127.0.0.1":       http.StatusBadGateway,
		"169.254.169.254": http.StatusBadGateway,
	} {
		p, addr := proxyFor(t, target, net.ParseIP(ip))
		p.PrivateAllow = allow
		p.Allow("127.0.0.1", []string{"dock.lan"})
		status, conn := connect(t, addr, "dock.lan:443")
		conn.Close()
		if status != want {
			t.Errorf("resolution %s: got %d, want %d", ip, status, want)
		}
	}
}

func TestParseCIDRsRejectsGarbage(t *testing.T) {
	if nets, err := ParseCIDRs(" "); err != nil || len(nets) != 0 {
		t.Fatalf("empty list: %v %v", nets, err)
	}
	if _, err := ParseCIDRs("10.0.0.0/8,nope"); err == nil {
		t.Fatal("invalid CIDR must fail")
	}
}

func TestProxyRejectsPlainHTTP(t *testing.T) {
	_, addr := proxyFor(t, "127.0.0.1:1", net.ParseIP("140.82.112.3"))
	res, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("non-CONNECT must be refused, got %d", res.StatusCode)
	}
}

func TestRestrictInstallsOrderedRulesIdempotently(t *testing.T) {
	installed := map[string]bool{}
	var inserts []string
	run := func(name string, args ...string) error {
		key := strings.Join(args[1:], " ")
		switch args[0] {
		case "-C":
			if installed[key] {
				return nil
			}
			return errors.New("no rule")
		case "-I":
			installed[key] = true
			inserts = append(inserts, key)
		case "-D":
			if !installed[key] {
				return errors.New("no rule")
			}
			delete(installed, key)
		}
		return nil
	}
	if err := Restrict(run, "172.16.0.9", 3128); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"FORWARD -s 172.16.0.9 -j DROP",
		"INPUT -s 172.16.0.9 -j DROP",
		"INPUT -s 172.16.0.9 -p tcp --dport 3128 -j ACCEPT",
		"INPUT -s 172.16.0.9 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
	}
	if strings.Join(inserts, "|") != strings.Join(want, "|") {
		t.Fatalf("inserts must leave accepts above drops:\n got %v\nwant %v", inserts, want)
	}
	if err := Restrict(run, "172.16.0.9", 3128); err != nil || len(inserts) != 4 {
		t.Fatalf("restrict must be idempotent, inserts=%d err=%v", len(inserts), err)
	}
	Unrestrict(run, "172.16.0.9", 3128)
	if len(installed) != 0 {
		t.Fatalf("unrestrict must remove every rule, left %v", installed)
	}
}

func TestProxyEnvCoversBothCases(t *testing.T) {
	env := ProxyEnv("172.16.0.1", 3128)
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if env[key] != "http://172.16.0.1:3128" {
			t.Fatalf("%s = %q", key, env[key])
		}
	}
}

func TestListenBindsAnAddressNoInterfaceCarriesYet(t *testing.T) {
	ln, err := Listen("192.0.2.77:0")
	if err != nil {
		t.Fatalf("freebind listen must succeed before the gateway exists: %v", err)
	}
	defer ln.Close()
	if !strings.HasPrefix(ln.Addr().String(), "192.0.2.77:") {
		t.Fatalf("listener must stay on the gateway address, got %s", ln.Addr())
	}
}
