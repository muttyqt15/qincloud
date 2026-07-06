// caddyapi.go — Caddy admin API client over its unix socket, implementing
// deploy.Router. Routes are JSON route objects with "@id": "qc-<app>" so
// upsert/delete address them directly via /id/qc-<app>. On first deploy the
// route is INSERTED AT INDEX 0 of the :80 server's route list — the Caddyfile
// boot config ends in a catch-all respond, so appended routes would never
// match; on redeploy it is PATCHed in place, one atomic request.
package caddyapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

// requestTimeout bounds every admin API call. The socket is local, so
// anything slower than this is a hung Caddy, not a slow network.
const requestTimeout = 10 * time.Second

// baseURL's host is a placeholder: the transport dials the unix socket for
// every request, but net/http still requires a syntactically valid URL.
const baseURL = "http://caddy-admin"

type Client struct {
	http *http.Client
}

// New returns a client for the admin socket (shared caddy_admin volume,
// /run/caddy/admin.sock inside both the caddy and controld containers).
func New(sockPath string) *Client {
	return &Client{http: &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}}
}

// UpsertRoute makes requests for host reverse-proxy to dial, replacing any
// existing route for app. Replace-in-place first, insert only when absent:
// each admin call must be atomic, because deploy.go's routing-failure cleanup
// assumes "route not switched → the OLD container keeps serving". A
// delete-then-insert would break that promise — a failed insert after a
// successful delete (Caddy restarting between the calls, client timeout,
// rejected config) leaves the app with NO route, silently serving the
// catch-all while both the store and deploy.go believe the old container is
// still routed. PATCH swaps the route in one request, so any single failure
// leaves either the old or the new route fully in place, never neither. It
// also removes the happy-path window where the host briefly had no route.
func (c *Client) UpsertRoute(ctx context.Context, app, host, dial string) error {
	serversJSON, err := c.expect2xx(ctx, http.MethodGet, "/config/apps/http/servers", nil)
	if err != nil {
		return fmt.Errorf("read servers config: %w", err)
	}
	// The Caddyfile adapter assigns arbitrary server names (srv0, srv1, …)
	// and also defines a metrics-only server on :2019 — select the public
	// one by its listen address, never by name. Checked before any mutation,
	// even on the PATCH path: a topology this client can't route correctly
	// (see pickPublicServer) must fail the deploy loud, not blackhole traffic.
	server, err := pickPublicServer(serversJSON)
	if err != nil {
		return err
	}

	route, err := routeJSON(app, host, dial)
	if err != nil {
		return err
	}

	status, respBody, err := c.do(ctx, http.MethodPatch, "/id/"+routeID(app), route)
	if err != nil {
		return fmt.Errorf("replace route for %s: %w", app, err)
	}
	replaced, err := patchOutcome(routeID(app), status, respBody)
	if err != nil {
		return fmt.Errorf("replace route for %s: %w", app, err)
	}
	if replaced {
		return nil
	}

	// 404: no route registered under this @id yet — first deploy, insert it.
	// PUT at an array index INSERTS there (POST would append). Index 0 is
	// load-bearing: Caddy evaluates routes in order and the boot config ends
	// in a catch-all respond, so an appended route would sit behind the
	// catch-all and never receive a request.
	insertPath := fmt.Sprintf("/config/apps/http/servers/%s/routes/0", server)
	if _, err := c.expect2xx(ctx, http.MethodPut, insertPath, route); err != nil {
		return fmt.Errorf("insert route for %s: %w", app, err)
	}
	return nil
}

// patchOutcome classifies the PATCH /id/<routeID> response into the upsert
// decision: 2xx = replaced in place (done), 404 = no such route yet (fall
// back to insert), anything else = a real failure surfaced with Caddy's
// error body. Pure so the three-way decision is unit-testable without
// faking the admin API.
func patchOutcome(routeID string, status int, body []byte) (replaced bool, err error) {
	if status >= 200 && status <= 299 {
		return true, nil
	}
	if status == http.StatusNotFound {
		return false, nil
	}
	return false, fmt.Errorf("PATCH /id/%s: caddy returned %d: %s", routeID, status, strings.TrimSpace(string(body)))
}

// DeleteRoute removes app's route. Absent is success: Destroy must be
// idempotent, and the route is legitimately gone after a forced Caddyfile
// reload (the boot Caddyfile knows nothing about API-added routes; caddy
// runs with --resume so plain restarts keep them — see stack/edge).
func (c *Client) DeleteRoute(ctx context.Context, app string) error {
	return c.deleteID(ctx, routeID(app))
}

// routeID is the "@id" under which an app's route is registered — the
// stable admin-API handle that makes upsert/delete order-independent.
func routeID(app string) string { return "qc-" + app }

// Route JSON is built from typed structs (not maps) so the wire shape is
// pinned at compile time and deterministic for tests.
type route struct {
	ID       string        `json:"@id"`
	Match    []routeMatch  `json:"match"`
	Handle   []routeHandle `json:"handle"`
	Terminal bool          `json:"terminal"`
}

type routeMatch struct {
	Host []string `json:"host"`
}

type routeHandle struct {
	Handler   string     `json:"handler"`
	Upstreams []upstream `json:"upstreams"`
}

type upstream struct {
	Dial string `json:"dial"`
}

// routeJSON builds the route object routing host to dial. terminal:true
// stops route evaluation on a match, so the catch-all respond below never
// touches app traffic.
func routeJSON(app, host, dial string) ([]byte, error) {
	body, err := json.Marshal(route{
		ID:       routeID(app),
		Match:    []routeMatch{{Host: []string{host}}},
		Handle:   []routeHandle{{Handler: "reverse_proxy", Upstreams: []upstream{{Dial: dial}}}},
		Terminal: true,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal route for %s: %w", app, err)
	}
	return body, nil
}

// pickPublicServer returns the name of the server whose listen addresses
// include ":80", given the GET /config/apps/http/servers response. Fails
// loud in two cases, because a wrong target silently blackholes app traffic:
// no :80 server at all (e.g. only the metrics-only :2019 one), or a SEPARATE
// server listening on :443. The latter is the auto-HTTPS topology a real
// site block creates (M6): the :80 server becomes the HTTP-redirect server,
// so a route programmed there leaves the app dark over HTTPS while deploys
// still report live. This client only understands the plain-HTTP baseline —
// extend its targeting before enabling TLS. A single server listening on
// both :80 and :443 is fine: one route there serves both.
func pickPublicServer(serversJSON []byte) (string, error) {
	var servers map[string]struct {
		Listen []string `json:"listen"`
	}
	if err := json.Unmarshal(serversJSON, &servers); err != nil {
		return "", fmt.Errorf("parse servers config: %w", err)
	}
	// Sort names for a deterministic pick: map iteration order is random,
	// and a nondeterministic route target would be an un-debuggable edge.
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	picked := ""
	for _, name := range names {
		if slices.Contains(servers[name].Listen, ":80") {
			picked = name
			break
		}
	}
	if picked == "" {
		return "", fmt.Errorf("no server listening on :80 in caddy config (servers: %s)", strings.Join(names, ", "))
	}
	for _, name := range names {
		if name != picked && slices.Contains(servers[name].Listen, ":443") {
			return "", fmt.Errorf(
				"server %s listens on :443 separately from the :80 server %s: TLS topology detected — routes into :80 would leave apps dark over HTTPS; extend caddyapi route targeting before enabling TLS",
				name, picked)
		}
	}
	return picked, nil
}

// deleteID removes the config object registered under id. 404 (unknown
// object id) is success: DeleteRoute must be idempotent, and an app that was
// never deployed (or whose route predates a Caddyfile force-reload) has no
// route to delete.
func (c *Client) deleteID(ctx context.Context, id string) error {
	status, respBody, err := c.do(ctx, http.MethodDelete, "/id/"+id, nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("DELETE /id/%s: caddy returned %d: %s", id, status, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// expect2xx sends a request and fails loud on any non-2xx, surfacing Caddy's
// status + error body — that body names the config path that rejected the
// change, which is the whole diagnosis.
func (c *Client) expect2xx(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	status, respBody, err := c.do(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, fmt.Errorf("%s %s: caddy returned %d: %s", method, path, status, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}

// do sends one request over the socket and returns the status + body.
// Transport failures are errors here; HTTP status judgment belongs to the
// caller, because deleteID treats 404 as success while everything else
// demands 2xx.
func (c *Client) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reqBody)
	if err != nil {
		return 0, nil, fmt.Errorf("build request %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	// Cap the read: admin responses are small config blobs or error strings;
	// an unbounded ReadAll would let a misbehaving endpoint balloon memory.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	return resp.StatusCode, respBody, nil
}
