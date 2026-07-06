---
title: "Extra — Publishing the Dashboard Safely (Cloudflare)"
slug: x1-public-dashboard-cloudflare
type: milestone
milestone: X1
status: stable
difficulty: 4
tags: [qincloud, security, networking, edge]
created: 2026-07-06
updated: 2026-07-06
related: ["[[m5-dashboard]]", "[[m1-edge-and-tls]]", "[[m4-controld-deploy-engine]]", "[[x3-verified-edge-deploys]]", "[[layered-trust-defense-in-depth]]", "[[verify-the-artifact-under-test]]", "[[root-cause-over-patch]]"]
sources: ["commit 5383d66", "commit 2e6d626", "commit 8f68070", "stack/edge/Caddyfile:49-73", "runbooks/gotchas/dashboard.md"]
---

# Extra — Publishing the Dashboard Safely (Cloudflare)

> **In one sentence:** the controld dashboard can delete every container on the box, so putting it on the public internet meant wrapping it in three independent locks — and the afternoon it ate was spent chasing five *correct* fixes that a stale config file kept hiding.

*This note ramps from plain English (§1–4) to the systems level (§5+). Read as far down as you want to go.*

## 1. What we were building — and why it matters (plain English)

The dashboard ([[m5-dashboard]]) is the operator's cockpit: it lists apps, streams logs, and deploys or destroys containers with a button. It does that by talking directly to `docker.sock` — the Docker daemon's control socket. Anyone who can drive that socket effectively **is root on the whole box**. There is no smaller way to say it.

So far the cockpit was reachable only from inside our private Tailscale network — think of it as a locked staff-only hallway with no door to the street. That's safe, but it means you must be on the tailnet (laptop, phone with Tailscale on) to use it. We wanted a normal URL — `https://dash.sparboard.com` — that works from any browser.

The catch: the moment you cut a door from the street into a room that holds the master keys, you have to be *very sure* about who can walk through it. A public login page that checks a password isn't enough on its own — an attacker who never intends to log in can still hammer the door and exhaust the one shared machine that answers everyone's requests. This is a **denial-of-service (DoS)** lever: not stealing anything, just knocking the whole platform offline by making the doorman do expensive work over and over.

## 2. The plan (initial approach)

Keep the tailnet door exactly as it is — the raw, unauthenticated hallway at `TS_IP:8600` stays. Then add a *second, separate* public door, and stack independent locks on it so no single failure opens it:

1. **A password on the door** — Caddy `basic_auth`, so a browser must send `admin` + a password.
2. **A bouncer in front of the building** — put the hostname behind Cloudflare ("orange-cloud" it), so Cloudflare's WAF and DDoS mitigation absorb floods *before* they reach our machine, and the real server's IP address is hidden from public DNS.
3. **A lock that only opens for the bouncer** — because the origin IP, once discovered, could be hit directly (bypassing Cloudflare entirely), teach our own edge to refuse anyone who isn't Cloudflare.

This is [[layered-trust-defense-in-depth]] made concrete: three locks, each covering the others' failure modes. The tailnet path is trust-by-location; the public path is trust-by-proof, three times over.

## 3. Where it deviated

The design was right. The *delivery* is where the afternoon went.

I'd edit the `Caddyfile`, sync it to the box, reload Caddy, and test — and the gate wouldn't be there. Direct hits to the origin IP that should have 404'd still reached the login page. So I'd conclude the config was wrong and try the next thing. I burned through **five different, individually-correct fixes** this way, each looking like it had failed.

Two of those "fixes" were chasing real Caddy quirks that turned out to be **red herrings sitting underneath the real bug**:

- **Two `remote_ip` matchers collide.** Caddy 2.10's config adapter, given two `remote_ip` matchers in one file (the dashboard's Cloudflare gate *and* an IP guard I'd tried on the `:2019` metrics listener), silently drops one from the compiled config. `caddy validate` still passes. Real quirk — but not why my gate was missing.
- **Backticks in a comment eat directives.** A backtick or `{$…}`/`{env.}` inside a Caddyfile comment makes the lexer swallow the directives that follow it. Also real, also not the cause.

Both are documented now, but on the day they were noise. The signal was invisible.

## 4. The fix — and how I found it

The breakthrough was refusing to trust that "I edited the file" meant "the running server sees the edit." I ran the config *out of the container's own eyes*:

```bash
docker exec edge-caddy-1 grep '@cf' /etc/caddy/Caddyfile
```

The marker wasn't there. The container was serving an **old Caddyfile** — every one of my five edits was landing on a file the running Caddy could no longer see.

**Root cause:** the Caddyfile is a *bind-mounted single file*. My deploy step used `rsync`, and `rsync`'s default is to write to a temp file and then `rename()` it into place. `rename()` gives the host path a **brand-new inode**. But a bind mount is pinned to the *inode* that existed when the container started — so the container kept reading the OLD inode while every tool on the host showed the NEW one. `caddy adapt`, `caddy reload`, my `grep` on the host: all read the fresh file. Only Caddy, inside the mount, read the ghost.

That is the whole illusion. Five correct fixes "failed" because none of them were ever loaded. The fix is one flag:

```bash
rsync --inplace stack/edge/Caddyfile box:/opt/qincloud/edge/Caddyfile
```

`--inplace` writes through the existing inode instead of swapping it, so the bind mount stays valid. (`--force-recreate` on the container re-establishes the mount as an alternative.) This exact failure is why the edge later got a single self-verifying deploy command — see [[x3-verified-edge-deploys]] — that greps the marker *inside the container* after every change instead of trusting the copy.

This is [[root-cause-over-patch]] and [[verify-the-artifact-under-test]] in one lesson: I kept patching the *config* when the defect was in the *delivery*, and I only saw it once I inspected the artifact the server actually runs, not the one I thought I'd shipped.

## 5. Going deep (systems level)

The public door lives in `stack/edge/Caddyfile:49-73`. Its shape:

```caddy
dash.sparboard.com {
    tls internal
    @cf remote_ip 173.245.48.0/20 103.21.244.0/22 … 2c0f:f248::/32
    handle @cf {
        basic_auth {
            admin {$DASH_PASSWORD_HASH}
        }
        reverse_proxy qincloud-controld:8600
    }
    handle {
        respond 404
    }
}
```

- **`tls internal`** — the zone SSL mode in Cloudflare is **Full**, so Cloudflare terminates the public TLS and re-encrypts to our origin, accepting a self-signed cert. We serve `tls internal` and skip ACME entirely, because an HTTP-01 challenge can't pass the orange-cloud proxy.
- **`@cf remote_ip …`** is the origin lock: the named matcher lists Cloudflare's published ranges (from `cloudflare.com/ips`, refreshed 2026-07-06). Requests from those IPs `handle @cf` → hit auth + proxy. Everything else falls to `handle { respond 404 }` **before any bcrypt runs**. A direct-to-origin-IP flood therefore costs us a 404, not a password hash.
- **`basic_auth` runs only inside `@cf`**, so Cloudflare's WAF/rate-limiting is always in front of the CPU-expensive check.

Two more hardening details from `runbooks/gotchas/dashboard.md`:

- **bcrypt cost 10, not Caddy's default 14.** An unauthenticated client can force one hash per request; cost 14 is ~1.4s of CPU, cost 10 is ~60ms — a ~16× cut in DoS amplification on the single shared edge, still uncrackable for an `openssl rand` password. Caddy's `hash-password` is pinned at 14, so generate with `htpasswd -nbB -C 10`.
- **Adapt-time `{$DASH_PASSWORD_HASH}`, not runtime `{env.…}`.** `basic_auth` base64-wraps the account password; a runtime `{env.}` placeholder lands raw where base64 is expected and Caddy crash-loops on provision. The tradeoff: the hash is baked into `autosave.json`, so rotating the password needs a `caddy reload` + redeploy, not a bare container recreate.

And the matcher rule the collision taught: keep the dash `@cf` gate the **only** `remote_ip` matcher in the whole file. The `:2019` metrics listener stays plain (see its RESIDUAL note in the Caddyfile) precisely so a second `remote_ip` matcher can't silently drop one of them under the 2.10 adapter.

Verification ritual, now non-negotiable:

```bash
docker exec edge-caddy-1 grep '@cf' /etc/caddy/Caddyfile   # config the server actually holds
curl -sS -o /dev/null -w '%{http_code}\n' https://<ORIGIN_IP>/ -H 'Host: dash.sparboard.com'  # expect 404
```

## 6. How this compares to best practice

A mature platform reaches the same shape by different roads. Cloudflare Tunnel (`cloudflared`) is the textbook answer: the origin makes an *outbound* connection to Cloudflare and never exposes a public IP at all, so the "lock the origin to CF ranges" step becomes unnecessary — there's no reachable origin to lock. Or the dashboard would sit behind an identity-aware proxy (Cloudflare Access, Google IAP) doing SSO + device posture, not a single shared bcrypt password.

We deliberately cut those corners: the tunnel is more moving parts than one box wants, and one strong `basic_auth` password behind Cloudflare's WAF is proportionate for a single-operator platform. The IP-range lock is our stand-in for the tunnel's no-public-origin property — with the accepted debt that Cloudflare's ranges change and our list needs refreshing. Where we *match* best practice: WAF in front of the auth check, expensive crypto never reachable by anonymous traffic, and a private (tailnet) path kept independent of the public one so a public misconfiguration can never lock the operator out.

## 7. The underlying why (the transferable lesson)

Two lessons braid together here, and they're the payload.

First, **when the door leads to the master keys, no single lock is trusted.** The password could leak, the origin IP could be discovered, a WAF rule could be misconfigured — so each lock is chosen to cover the *others'* failure. That's [[layered-trust-defense-in-depth]]: you don't ask "is this lock strong?", you ask "what still holds after this lock fails?"

Second, and harder-won: **a fix you can't observe on the running artifact is not a fix — it's a hypothesis.** I had five correct changes and a false conclusion because I was inspecting the file I *sent*, not the file the server *ran*. The stale bind mount was invisible until I looked through the container's own eyes. Every "correct fix that mysteriously failed" is a signal to stop fixing and go verify the artifact under test ([[verify-the-artifact-under-test]]), and to distrust the symptom until you've traced it to its origin instead of patching where it surfaced ([[root-cause-over-patch]]). The red herrings — the matcher collision, the comment lexer — were real bugs, but chasing them while blind to the delivery layer is exactly how an afternoon disappears.

---
**Teaches:** [[layered-trust-defense-in-depth]] · [[verify-the-artifact-under-test]] · [[root-cause-over-patch]]
**Sources:** commit `5383d66`, commit `2e6d626`, commit `8f68070`, `stack/edge/Caddyfile:49-73`, `runbooks/gotchas/dashboard.md`
