---
title: Layered Trust & Defense in Depth
slug: layered-trust-defense-in-depth
type: concept
status: stable
difficulty: 3
tags: [qincloud, principle, security, networking]
created: 2026-07-06
updated: 2026-07-06
related: ["[[x1-public-dashboard-cloudflare]]", "[[m0-host-baseline]]", "[[m6-first-app-umami]]", "[[m5-dashboard]]", "[[make-invalid-states-unrepresentable]]", "[[adversarial-review]]"]
sources: ["stack/edge/Caddyfile", "stack/data/compose.yml", "scripts/close-public-ssh.sh", "runbooks/gotchas/dashboard.md", "runbooks/gotchas/host.md"]
---

# Layered Trust & Defense in Depth

> **The principle in one line:** reachability is not authorization — stack several independent controls so that any one of them failing does not open the door.

## What it means (plain English)

A bank vault isn't protected by one thing. There's the locked building, the guard, the vault door, the time-lock, the camera. No single layer is trusted to be perfect; the point is that an attacker has to defeat *all* of them at once, and the layers are *independent* — picking the front-door lock tells you nothing about cracking the time-lock.

The opposite — and the trap — is assuming "they can't get here, so they must be allowed." That's confusing *reachability* (can a request physically arrive?) with *authorization* (is this request permitted?). The instant your one wall has a gap, there's nothing behind it.

## Why it matters

Single-layer security is a coin flip against your worst day. Networks get misconfigured, a firewall rule gets dropped, a DNS record flips from private to public, a VPN goes down. If your only defense was "this port isn't reachable," every one of those turns into a full breach. Layers turn a single failure into a *near-miss*: the outer wall fell, but the inner one held.

## Where it showed up in QinCloud

- **[[x1-public-dashboard-cloudflare]] — the root-equivalent door.** The dashboard controls `docker.sock` (effectively root on the box), and we made it *public*. One control would have been reckless. So: (1) it's behind HTTP basic auth; (2) it's fronted by Cloudflare, which hides the origin IP and absorbs floods with a WAF; and (3) — crucially — Caddy *locks the origin to Cloudflare's IP ranges*, so even someone who learns the real server IP and skips Cloudflare gets a `404` before the password prompt (`Caddyfile`). Three independent layers; the direct-to-origin lock is what closes the denial-of-service the [[adversarial-review]] found. And the tailnet path (below) stays as a separate, un-public door.

- **[[m0-host-baseline]] — admin planes on the tailnet, not the internet.** Grafana, Prometheus, and the raw dashboard bind only to the Tailscale IP. SSH went further: `scripts/close-public-ssh.sh` removes the public `:22` allowance entirely so sshd answers on the tailnet interface only. Note the layering isn't just "firewall": the real invariant is *only Caddy publishes public ports*, because Docker's published ports bypass the firewall (`host.md`) — so the guard is architectural, not just a rule.

- **[[m6-first-app-umami]] — network isolation on top of passwords.** A tenant app reaches Postgres over `tenant_db_net`, but *not* Redis or the control plane. Even though each database role already has its own password (one layer), the network simply doesn't carry a path to the things a tenant shouldn't touch (a second, independent layer). Verified from inside the container, not assumed. [[m5-dashboard]] adds the same spirit for CSRF: requiring the `HX-Request` header, which a cross-origin page cannot forge.

## How to apply it

For anything sensitive, name at least two *independent* controls and check they don't share a failure mode. Ask the killer question: **"if this one layer failed open, what stops the attacker?"** If the answer is "nothing," add a layer. Prefer controls at different levels — network reachability, identity/auth, and application logic — so a single misconfiguration can't defeat them together. Verify each layer *actually* blocks (curl the origin directly; resolve DNS from inside the tenant container), because an unverified layer is a story, not a defense.

## Signs you're violating it

- "It's only reachable from X, so we don't need auth on it."
- The security of a root-equivalent surface rests on exactly one thing.
- Two "layers" that both fail if the same firewall rule or DNS record is wrong (not independent).
- You've never tested what happens when a layer is bypassed.

---
**Related:** [[make-invalid-states-unrepresentable]] · [[adversarial-review]] · [[m0-host-baseline]]
