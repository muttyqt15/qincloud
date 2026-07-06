---
title: M0 — Host Baseline & Hardening
slug: m0-host-baseline
type: milestone
milestone: M0
status: stable
difficulty: 3
tags: [qincloud, security, networking, infra, devops]
created: 2026-07-06
updated: 2026-07-06
related: ["[[layered-trust-defense-in-depth]]", "[[fail-loud-at-boundaries]]", "[[idempotent-self-verifying-operations]]", "[[the-box-is-disposable]]", "[[root-cause-over-patch]]", "[[make-invalid-states-unrepresentable]]"]
sources: ["scripts/bootstrap.sh", "scripts/close-public-ssh.sh", "runbooks/drills/2026-07-06-m0-baseline-verification.md", "runbooks/gotchas/host.md", "commit 51c5df4"]
---

# M0 — Host Baseline & Hardening

> **In one sentence:** take a raw, internet-exposed rented server and, with one rerunnable script, turn it into a locked, self-defending base — firewalled, brute-force-banned, key-only login, its internal networks pre-drawn — before a single real service lands on it.

*This note ramps from plain English (§1–4) to the systems level (§5+). Read as far down as you want to go.*

## 1. What we were building — and why it matters (plain English)

A fresh VPS (Virtual Private Server — a computer you rent in a data center) arrives like a house with the front door wide open and the address already printed in the phone book. Within minutes of it going online, automated bots on the internet are trying the handle: guessing passwords on the login port, scanning for anything that answers.

Everything QinCloud does later — the web front door, the databases, the dashboards — sits *on this one box*. If the base is soft, nothing built on top is safe. So M0 is the foundation pour: before any furniture, we fit a real lock (login by cryptographic key only, never a password), a security guard who bans anyone who keeps rattling the door (fail2ban), a wall around the property that turns away all uninvited visitors (a firewall), and we chalk out the interior rooms — the private lanes that different services will later use to talk to each other without shouting across the public street.

The whole point: **you should be able to lose the box entirely and rebuild this exact state from scratch by running one script.** The house is disposable; the blueprint is what matters.

## 2. The plan (initial approach)

`scripts/bootstrap.sh` is that blueprint — one idempotent script (safe to run again and again; a second run changes nothing) that does, top to bottom:

- **Key-only SSH.** Turn off password login entirely so no amount of guessing gets in.
- **A firewall (UFW).** Default-deny everything coming in; only allow the three ports a web platform needs — 22 (SSH), 80 (HTTP), 443 (HTTPS).
- **fail2ban.** Watch the login log; ban any IP that fails five times.
- **Auto security updates** so the box patches itself.
- **Docker** plus a set of named private networks (`app_net`, `data_net`, `tenant_db_net`, `admin_net`) — the interior lanes.
- **Tailscale** — a private mesh network (VPN) that later becomes the *admin plane*, the staff-only entrance.

The going-in assumption was the tidy one every tutorial teaches: **the firewall is the guard.** Deny everything, open three ports, done.

## 3. Where it deviated

Two surprises, both caught in the verification drill (`runbooks/drills/2026-07-06-m0-baseline-verification.md`, `bootstrap.sh` @ `51c5df4`):

1. **fail2ban wouldn't start.** Its default SSH jail reads `/var/log/auth.log` — and Ubuntu 24.04 ("noble") ships journald-only, with no such file. The guard we hired had nowhere to look.

2. **The firewall does not actually guard the services.** This is the big one. Docker publishes container ports by writing its *own* iptables rules ahead of UFW's chain. A container that publishes `5432:5432` is reachable from the internet **even though UFW never allowed 5432.** The wall we were relying on has a Docker-shaped hole in it, and it's silent — `ufw status` still proudly shows "deny incoming" while Postgres answers the world.

So the tidy mental model — "firewall = guard" — was wrong for this box. UFW protects the *host's own* listeners; it does not protect anything Docker publishes.

## 4. The fix — and how I found it

Both fixes were found the same way: **actually test the artifact, don't trust that it configured.** The drill ran `fail2ban-client status sshd`, scanned the box's open ports with `ss -tlnp`, and — critically — ran an external `nc` port scan *from the laptop* to see the box the way the internet sees it.

- **fail2ban:** root cause is the missing log, not a fail2ban bug. Fix at the cause — `jail.local` pins `backend = systemd` so it reads journald directly, and bootstrap installs `python3-systemd` (`bootstrap.sh:61-70`). The jail now matches on `_SYSTEMD_UNIT=sshd.service`.

- **The Docker/UFW hole:** you cannot make UFW cover Docker without a fight, so the guard is moved to a *design invariant* instead of the firewall: **only Caddy (the edge proxy) publishes ports.** Everything else — Postgres, Redis, exporters, controld — stays on internal bridge networks with **zero `ports:` entries**, reachable only by name from other containers. The firewall becomes a second layer, not the only one. The external scan confirmed it: only 22 answered; 2019/5432/6379/9090/3000 were all filtered.

The deeper move: a symptom ("Postgres is exposed") would tempt a patch ("add a UFW deny rule for 5432"). But UFW can't see Docker's chain, so that patch would *look* right and do nothing. The real fix makes the exposed state unable to exist — if nothing publishes the port, there's nothing to firewall.

## 5. Going deep (systems level)

**The four bridge networks** (`bootstrap.sh:84-97`) are trust boundaries drawn in the network layer:

- `app_net` — Caddy ↔ tenant apps.
- `data_net` — apps ↔ Redis and exporters.
- `tenant_db_net` — carries **only** Postgres and the apps that declared `AppSpec.UseDB`. Redis, exporters, controld are never on it.
- `admin_net` — **only** Caddy + controld + Prometheus, on a *fixed* subnet `10.201.7.0/24`. It's fixed (not auto-assigned) because Caddy's `:2019` metrics listener binds every interface Caddy is on, so the Caddyfile allowlists exactly this range to keep the admin metrics off tenant-reachable `app_net`. That subnet constant is a single source of truth shared between `bootstrap.sh` and the Caddyfile.

**Admin UIs bind `${TS_IP:?}`** — the `:?` means compose *aborts loudly* if `TS_IP` is empty, because an empty bind silently falls back to `0.0.0.0` (all interfaces = public). See [[fail-loud-at-boundaries]].

**Closing public SSH** is deliberately the *last* step, not part of bootstrap. A fresh box has no authenticated Tailscale yet, so `bootstrap.sh:48-57` keeps `ufw limit 22/tcp` (public, but rate-limited + key-only). Once `tailscale up` succeeds, `scripts/close-public-ssh.sh` runs (rebuild step 10):

```bash
ip link show tailscale0 >/dev/null 2>&1 || die "no tailscale0 interface — run 'tailscale up' first"
tailscale status      >/dev/null 2>&1 || die "tailscale not running or not authenticated"
ufw allow in on tailscale0 to any port 22 proto tcp
ufw delete limit 22/tcp
```

It **refuses to lock the public door until it has verified the private door exists** — checking `tailscale0` and an authenticated `tailscale status` first. Otherwise you'd lock yourself out of your own box. Recovery is documented inline: the VPS provider's web console → `ufw limit 22/tcp` to reopen, fix Tailscale, rerun. SSH is now `ssh root@100.125.12.20` over the tailnet; the public IP no longer answers on 22 (`runbooks/gotchas/host.md`).

**Other operational gotchas** captured from this milestone: the provider image ships without `curl` (bootstrap installs its own deps and every script checks `command -v` rather than assume); compose interpolates `$` inside `.env` values, so a bcrypt hash must be stored with every `$` escaped as `$$` or it's silently corrupted; `docker.io` pulls are flaky from this VPS and want a retry loop.

## 6. How this compares to best practice

The individual pieces are textbook CIS-benchmark hardening: key-only SSH, default-deny firewall, fail2ban, unattended-upgrades. A mature team would go further — a dedicated non-root deploy user (we still SSH as `root` with `prohibit-password`), SSH port-knocking or bastion hosts, and the whole baseline expressed in Ansible/Terraform with drift detection rather than a bash script.

The deliberate corners: **one box, one bash script, no config-management daemon.** For a solo SRE-portfolio platform that's the right altitude — the script *is* the desired-state declaration, and its idempotency plus the drill give most of what drift detection would. The one place we spend real rigor is the Docker/UFW gap, because getting that wrong is a silent data breach, not a cosmetic miss. When this grows past one box or one operator, the bash script becomes the thing to revisit first.

## 7. The underlying why (the transferable lesson)

**Your security guarantee lives at the layer that actually enforces it — not at the layer you assumed.** "I have a firewall" felt like safety; the real enforcement point was "Docker doesn't publish the port." A control you *believe* is protecting you while it quietly isn't is worse than no control, because it stops you looking. You only find these gaps by testing the artifact from the outside — the external `nc` scan, not `ufw status` — and by stacking layers so no single wrong assumption is fatal.

---
**Teaches:** [[layered-trust-defense-in-depth]] · [[fail-loud-at-boundaries]] · [[idempotent-self-verifying-operations]] · [[the-box-is-disposable]] · [[root-cause-over-patch]] · [[make-invalid-states-unrepresentable]]
**Sources:** `scripts/bootstrap.sh`, `scripts/close-public-ssh.sh`, `runbooks/drills/2026-07-06-m0-baseline-verification.md`, `runbooks/gotchas/host.md`, commit `51c5df4`
