package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/fleetauth"
)

const okHeartbeat = `{"ok":true,"data":{"hostId":"host-1"}}`

func registerReply(credential string) string {
	if credential == "" {
		return `{"ok":true,"data":{"hostId":"host-1","heartbeatSeconds":30,"pollWaitSeconds":25}}`
	}
	return `{"ok":true,"data":{"hostId":"host-1","heartbeatSeconds":30,"pollWaitSeconds":25,"credential":"` + credential + `"}}`
}

func bearerOf(call recorded) string { return call.header.Get("authorization") }

func TestRegisterPersistsIssuedCredentialAndLaterCallsUseIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet-credential")
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		if strings.Contains(call.url, "/register") {
			return jsonResponse(http.StatusOK, registerReply("hc_issued")), nil
		}
		return jsonResponse(http.StatusOK, okHeartbeat), nil
	}
	cfg := testAgent(t, transport, inventory{})
	cfg.Auth = fleetauth.Open(path, cfg.Token)

	if _, err := cfg.register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	cfg.heartbeat(context.Background())

	if got := bearerOf(transport.callsMatching("/v1/fleet/register")[0]); got != "Bearer fleet-token" {
		t.Fatalf("register bearer = %q, want the enrollment token", got)
	}
	if got := bearerOf(transport.callsMatching("/v1/fleet/heartbeat")[0]); got != "Bearer hc_issued" {
		t.Fatalf("heartbeat bearer = %q, want the issued credential", got)
	}
	if got := fleetauth.Open(path, "").Held(); got != "hc_issued" {
		t.Fatalf("persisted credential = %q", got)
	}
}

func TestOlderControlPlaneWithoutCredentialKeepsSharedToken(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		if strings.Contains(call.url, "/register") {
			return jsonResponse(http.StatusOK, registerReply("")), nil
		}
		return jsonResponse(http.StatusOK, okHeartbeat), nil
	}
	cfg := testAgent(t, transport, inventory{})
	cfg.Auth = fleetauth.Open("", cfg.Token)

	if _, err := cfg.register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	cfg.heartbeat(context.Background())

	if got := bearerOf(transport.callsMatching("/v1/fleet/heartbeat")[0]); got != "Bearer fleet-token" {
		t.Fatalf("heartbeat bearer = %q, want the shared token", got)
	}
}

func TestRejectedCredentialNeverFallsBackToTheSharedToken(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		if bearerOf(call) == "Bearer hc_stale" {
			return jsonResponse(http.StatusUnauthorized, `{"ok":false,"code":"host_credential_unknown","message":"unknown"}`), nil
		}
		return jsonResponse(http.StatusOK, registerReply("hc_taken_over")), nil
	}
	cfg := testAgent(t, transport, inventory{})
	cfg.Auth = fleetauth.Open("", cfg.Token)
	_ = cfg.Auth.Set("hc_stale")

	cfg.heartbeat(context.Background())
	var out registerResponse
	err := cfg.registerOnce(context.Background(), cfg.registerBody(), &out)

	var re *responseError
	if !errors.As(err, &re) || re.status != http.StatusUnauthorized {
		t.Fatalf("register err = %v, want 401", err)
	}
	for _, call := range transport.callsMatching("/v1/fleet/") {
		if bearerOf(call) != "Bearer hc_stale" {
			t.Fatalf("%s presented %q, want only the held credential", call.url, bearerOf(call))
		}
	}
	if got := cfg.Auth.Held(); got != "hc_stale" {
		t.Fatalf("held credential = %q, must be unchanged", got)
	}
}

func TestRevokedCredentialMessageNamesTheAdminReset(t *testing.T) {
	msg := describe(&responseError{status: http.StatusUnauthorized, code: "host_credential_unknown"})
	if !strings.Contains(msg, "reset-credential") {
		t.Fatalf("describe = %q, want the admin reset instruction", msg)
	}
}

func TestEnrolledHostConflictIsPermanent(t *testing.T) {
	if !permanent(&responseError{status: http.StatusConflict, code: "host_already_enrolled"}) {
		t.Fatal("409 host_already_enrolled must not be retried")
	}
}

func TestPollAndResultUseTheCredential(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"ok":true,"data":{"command":null}}`), nil
	}
	cfg := testAgent(t, transport, inventory{})
	cfg.Auth = fleetauth.Open("", cfg.Token)
	_ = cfg.Auth.Set("hc_issued")

	if _, err := cfg.pollOnce(context.Background(), 0); err != nil {
		t.Fatalf("poll: %v", err)
	}
	cfg.post(context.Background(), command{ID: "c1"}, resultRequest{HostID: cfg.HostID, Ok: true})

	for _, call := range transport.callsMatching("/v1/fleet/") {
		if got := bearerOf(call); got != "Bearer hc_issued" {
			t.Fatalf("%s bearer = %q, want the credential", call.url, got)
		}
	}
}

func TestRegisterAnnouncesCredentialSupportAndFallsBackForOlderBackends(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		if strings.Contains(string(call.body), "acceptsCredential") {
			return jsonResponse(http.StatusBadRequest, `{"ok":false,"code":"validation_error","message":"property acceptsCredential should not exist"}`), nil
		}
		return jsonResponse(http.StatusOK, registerReply("")), nil
	}
	cfg := testAgent(t, transport, inventory{})
	cfg.Features = apiv1.DockletFeatures

	if _, err := cfg.register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}

	calls := transport.callsMatching("/v1/fleet/register")
	if len(calls) != 2 || !strings.Contains(string(calls[0].body), `"acceptsCredential":true`) || strings.Contains(string(calls[1].body), "acceptsCredential") || !strings.Contains(string(calls[0].body), `"features":["egress-on-resume"]`) || strings.Contains(string(calls[1].body), "features") {
		t.Fatalf("register bodies = %d calls, want one with the flag then one without", len(calls))
	}
}

func TestCredentiallessHostClaimsOneOnUnauthorizedAfterBackendUpgrade(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		switch {
		case strings.Contains(call.url, "/register"):
			return jsonResponse(http.StatusOK, registerReply("hc_claimed")), nil
		case bearerOf(call) == "Bearer hc_claimed":
			return jsonResponse(http.StatusOK, okHeartbeat), nil
		default:
			return jsonResponse(http.StatusUnauthorized, `{"ok":false,"code":"host_credential_unknown","message":"unknown"}`), nil
		}
	}
	path := filepath.Join(t.TempDir(), "fleet-credential")
	cfg := testAgent(t, transport, inventory{})
	cfg.Auth = fleetauth.Open(path, cfg.Token)

	cfg.heartbeat(context.Background())

	regs := transport.callsMatching("/v1/fleet/register")
	if len(regs) != 1 || bearerOf(regs[0]) != "Bearer fleet-token" || !strings.Contains(string(regs[0].body), `"acceptsCredential":true`) {
		t.Fatalf("claim registrations = %+v, want one with the shared token", regs)
	}
	beats := transport.callsMatching("/v1/fleet/heartbeat")
	if len(beats) != 2 || bearerOf(beats[1]) != "Bearer hc_claimed" {
		t.Fatalf("heartbeat was not retried with the claimed credential")
	}
	if got := fleetauth.Open(path, "").Held(); got != "hc_claimed" {
		t.Fatalf("persisted credential = %q", got)
	}
}

func TestClaimIsSerializedAcrossConcurrentCalls(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		switch {
		case strings.Contains(call.url, "/register"):
			return jsonResponse(http.StatusOK, registerReply("hc_claimed")), nil
		case bearerOf(call) == "Bearer hc_claimed":
			return jsonResponse(http.StatusOK, okHeartbeat), nil
		default:
			return jsonResponse(http.StatusUnauthorized, `{"ok":false,"code":"host_credential_unknown","message":"unknown"}`), nil
		}
	}
	cfg := testAgent(t, transport, inventory{})
	cfg.Auth = fleetauth.Open("", cfg.Token)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg.heartbeat(context.Background())
		}()
	}
	wg.Wait()

	if got := len(transport.callsMatching("/v1/fleet/register")); got != 1 {
		t.Fatalf("register calls = %d, want exactly 1", got)
	}
}

func TestHeldCredentialIsNeverReplacedByAClaim(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized, `{"ok":false,"code":"host_credential_unknown","message":"unknown"}`), nil
	}
	cfg := testAgent(t, transport, inventory{})
	cfg.Auth = fleetauth.Open("", cfg.Token)
	_ = cfg.Auth.Set("hc_revoked")

	cfg.heartbeat(context.Background())

	if got := len(transport.callsMatching("/v1/fleet/register")); got != 0 {
		t.Fatalf("register calls = %d, a held credential must never trigger a claim", got)
	}
}

func TestFailedClaimLeavesTheHostCredentialless(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		if strings.Contains(call.url, "/register") {
			return jsonResponse(http.StatusConflict, `{"ok":false,"code":"host_already_enrolled","message":"enrolled"}`), nil
		}
		return jsonResponse(http.StatusUnauthorized, `{"ok":false,"code":"host_credential_unknown","message":"unknown"}`), nil
	}
	var logs []string
	cfg := testAgent(t, transport, inventory{})
	cfg.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	cfg.Auth = fleetauth.Open("", cfg.Token)

	cfg.heartbeat(context.Background())

	if !strings.Contains(strings.Join(logs, " "), "host_already_enrolled") {
		t.Fatalf("logs = %v, want the claim failure cause", logs)
	}
	if cfg.Auth.Held() != "" || len(transport.callsMatching("/v1/fleet/heartbeat")) != 1 {
		t.Fatal("a refused claim must not retry the call or invent a credential")
	}
}

func TestRegisterAdvertisesFeaturesOnlyWhenConfigured(t *testing.T) {
	for name, features := range map[string][]string{"with proxy": apiv1.DockletFeatures, "without proxy": nil} {
		t.Run(name, func(t *testing.T) {
			transport := &fakeTransport{}
			transport.handler = func(recorded) (*http.Response, error) {
				return jsonResponse(http.StatusOK, registerReply("")), nil
			}
			cfg := testAgent(t, transport, inventory{})
			cfg.Features = features
			if _, err := cfg.register(context.Background()); err != nil {
				t.Fatalf("register: %v", err)
			}
			body := string(transport.callsMatching("/v1/fleet/register")[0].body)
			if got := strings.Contains(body, "egress-on-resume"); got != (features != nil) {
				t.Fatalf("egress-on-resume advertised = %v, body %s", got, body)
			}
		})
	}
}
