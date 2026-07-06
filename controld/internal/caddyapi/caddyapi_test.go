// caddyapi_test.go — unit tests for the pure decision logic: route JSON
// construction, public-server selection, and the PATCH-vs-insert upsert
// decision. The HTTP plumbing itself is not faked here — the adapter's value
// is talking to real Caddy, and the server fixture below is the literal
// output of `caddy adapt` on the production Caddyfile, so selection is
// tested against the real wire shape.
package caddyapi

import (
	"fmt"
	"strings"
	"testing"
)

// adaptedServers is the "servers" object Caddy 2.10 generates from
// stack/edge/Caddyfile: srv0 is the metrics-only :2019 listener, srv1 is
// the public :80 one. Names are adapter-assigned and must not be relied on.
const adaptedServers = `{
  "srv0": {
    "listen": [":2019"],
    "routes": [
      {"group": "group2", "match": [{"path": ["/metrics"]}],
       "handle": [{"handler": "subroute", "routes": [{"handle": [{"handler": "metrics"}]}]}]},
      {"group": "group2",
       "handle": [{"handler": "subroute", "routes": [{"handle": [{"handler": "static_response", "status_code": 404}]}]}]}
    ]
  },
  "srv1": {
    "listen": [":80"],
    "routes": [
      {"match": [{"path": ["/healthz"]}], "handle": [{"handler": "static_response", "status_code": 200}]},
      {"handle": [{"body": "qincloud edge ok", "handler": "static_response", "status_code": 200}]}
    ],
    "logs": {"default_logger_name": "log0"}
  }
}`

func TestRouteJSONWireShape(t *testing.T) {
	got, err := routeJSON("whoami", "whoami.example.com", "qc-whoami-3:80")
	if err != nil {
		t.Fatalf("routeJSON: %v", err)
	}
	// Exact wire shape is the contract with Caddy — field order is
	// deterministic (struct declaration order), so compare the full string.
	want := `{"@id":"qc-whoami","match":[{"host":["whoami.example.com"]}],` +
		`"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"qc-whoami-3:80"}]}],` +
		`"terminal":true}`
	if string(got) != want {
		t.Fatalf("route JSON = %s, want %s", got, want)
	}
}

func TestPickPublicServerFromAdaptedConfig(t *testing.T) {
	name, err := pickPublicServer([]byte(adaptedServers))
	if err != nil {
		t.Fatalf("pickPublicServer: %v", err)
	}
	if name != "srv1" {
		t.Fatalf("picked %q, want srv1 (the :80 listener, not the :2019 metrics one)", name)
	}
}

func TestPickPublicServerMultipleListenAddresses(t *testing.T) {
	// One server covering both :80 and :443 is a fine target: a route
	// inserted there serves both schemes.
	blob := `{"edge": {"listen": [":443", ":80"]}}`
	name, err := pickPublicServer([]byte(blob))
	if err != nil {
		t.Fatalf("pickPublicServer: %v", err)
	}
	if name != "edge" {
		t.Fatalf("picked %q, want edge", name)
	}
}

func TestPickPublicServerFailsLoudWhenAbsent(t *testing.T) {
	blob := `{"srv0": {"listen": [":2019"]}}`
	_, err := pickPublicServer([]byte(blob))
	if err == nil {
		t.Fatal("want error when no server listens on :80")
	}
	// The error must name the servers it saw — that's the diagnosis.
	if !strings.Contains(err.Error(), ":80") || !strings.Contains(err.Error(), "srv0") {
		t.Fatalf("error %q should mention :80 and the servers present", err)
	}
}

// adaptedServersTLS is the "servers" object Caddy 2.10 generates from the
// TLS-era Caddyfile (sparboard.com site block + :80 redirect + :2019
// metrics) — captured from `caddy adapt` on the box, routes elided since
// selection only reads listen addresses.
const adaptedServersTLS = `{
  "srv0": {"listen": [":2019"]},
  "srv1": {"listen": [":443"], "routes": [{"match": [{"host": ["sparboard.com"]}], "terminal": true}]},
  "srv2": {"listen": [":80"]}
}`

func TestPickPublicServerTargetsTLSServer(t *testing.T) {
	// The auto-HTTPS topology a real site block creates: routes must land in
	// the :443 server (per-host certs auto-provision there); the :80 server
	// only bounces HTTP up to it, and a route there would be dark over HTTPS.
	name, err := pickPublicServer([]byte(adaptedServersTLS))
	if err != nil {
		t.Fatalf("pickPublicServer: %v", err)
	}
	if name != "srv1" {
		t.Fatalf("picked %q, want srv1 (the :443 server, not the :80 redirect or :2019 metrics)", name)
	}
}

func TestPickPublicServerRejectsAmbiguousTLS(t *testing.T) {
	// Two :443 servers means this client cannot know where app traffic
	// terminates — an arbitrary pick would silently blackhole apps, so the
	// deploy must fail loud instead.
	blob := `{"a": {"listen": [":443"]}, "b": {"listen": [":443", ":80"]}}`
	_, err := pickPublicServer([]byte(blob))
	if err == nil {
		t.Fatal("want error when multiple servers listen on :443")
	}
	if !strings.Contains(err.Error(), ":443") {
		t.Fatalf("error %q should name the ambiguous :443 topology", err)
	}
}

func TestPickPublicServerFailsLoudOnEmptyConfig(t *testing.T) {
	for _, blob := range []string{`{}`, `null`} {
		if _, err := pickPublicServer([]byte(blob)); err == nil {
			t.Fatalf("blob %s: want error, got nil", blob)
		}
	}
}

func TestPickPublicServerRejectsGarbage(t *testing.T) {
	_, err := pickPublicServer([]byte(`not json`))
	if err == nil {
		t.Fatal("want parse error on garbage input")
	}
}

func TestPatchOutcomeDecision(t *testing.T) {
	// The upsert's atomicity hinges on this three-way call: replace in place
	// on 2xx, insert only on 404 (route doesn't exist yet), and anything
	// else is a hard failure that must leave the previous route serving.
	tests := []struct {
		name     string
		status   int
		body     string
		replaced bool
		wantErr  bool
	}{
		{name: "2xx means replaced in place", status: 200, replaced: true},
		{name: "404 means first deploy, fall back to insert", status: 404, replaced: false},
		{name: "500 is a hard failure", status: 500, body: "config mutex poisoned", wantErr: true},
		{name: "400 rejected config is a hard failure", status: 400, body: `{"error":"unknown field"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replaced, err := patchOutcome("qc-whoami", tt.status, []byte(tt.body))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("status %d: want error, got nil", tt.status)
				}
				// Caddy's status + error body are the whole diagnosis — the
				// error must carry both.
				if !strings.Contains(err.Error(), fmt.Sprint(tt.status)) || !strings.Contains(err.Error(), tt.body) {
					t.Fatalf("error %q should carry status %d and body %q", err, tt.status, tt.body)
				}
				return
			}
			if err != nil {
				t.Fatalf("status %d: unexpected error: %v", tt.status, err)
			}
			if replaced != tt.replaced {
				t.Fatalf("status %d: replaced = %v, want %v", tt.status, replaced, tt.replaced)
			}
		})
	}
}
