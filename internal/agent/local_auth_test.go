package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestRelayCarriesLocalTokenButFleetCallsDoNot(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		if strings.Contains(call.url, "/v1/fleet/") {
			return jsonResponse(http.StatusOK, `{"ok":true,"data":{"accepted":true}}`), nil
		}
		if call.url == "http://127.0.0.1:8081/v1/destroy" {
			return jsonResponse(http.StatusOK, `{"ok":true,"data":{"dockId":"d1"}}`), nil
		}
		return nil, fmt.Errorf("unexpected %s %s", call.method, call.url)
	}
	cfg := testAgent(t, transport, inventory{})
	cfg.LocalToken = "local-secret"

	cfg.execute(context.Background(), command{ID: "c1", Op: "destroy", IdempotencyToken: "t", Payload: json.RawMessage(`{"dockId":"d1"}`)})

	relays := transport.callsMatching("127.0.0.1:8081")
	if len(relays) != 1 || relays[0].header.Get("authorization") != "Bearer local-secret" {
		t.Fatalf("relays = %+v, want one call carrying the local bearer token", relays)
	}
	for _, call := range transport.callsMatching("/v1/fleet/") {
		if strings.Contains(call.header.Get("authorization"), "local-secret") {
			t.Fatalf("local token leaked to the control plane: %s", call.url)
		}
	}
}

func TestRelayRefusesTraversalIDsInGetOps(t *testing.T) {
	for _, tc := range []struct{ op, payload string }{
		{"generation.tree", `{"snapshotId":"../../etc"}`},
		{"generation.file", `{"snapshotId":"a/b","path":"x"}`},
		{"stats", `{"dock":"../x"}`},
	} {
		transport := &fakeTransport{}
		transport.handler = func(call recorded) (*http.Response, error) {
			if strings.Contains(call.url, "/result") {
				return jsonResponse(http.StatusOK, `{"ok":true,"data":{"accepted":true}}`), nil
			}
			return nil, fmt.Errorf("local api must not be called: %s", call.url)
		}
		cfg := testAgent(t, transport, inventory{})
		cfg.execute(context.Background(), command{ID: "c1", Op: tc.op, Payload: json.RawMessage(tc.payload)})
		if relays := transport.callsMatching("127.0.0.1:8081"); len(relays) != 0 {
			t.Fatalf("%s %s reached the local api", tc.op, tc.payload)
		}
		results := transport.callsMatching("/result")
		if len(results) != 1 || !strings.Contains(string(results[0].body), `"ok":false`) {
			t.Fatalf("%s: result posts = %+v, want one failure", tc.op, results)
		}
	}
}
