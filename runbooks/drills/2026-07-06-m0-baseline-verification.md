# Drill: M0 host baseline verification

**Date:** 2026-07-06 · **Applied:** `scripts/bootstrap.sh` @ `51c5df4` · **Box:** Ubuntu 24.04.4, 4 vCPU / 7.7GB

**Goal:** prove the hardened baseline holds before any service lands on the box.

## Checks

| Check | Expectation | Result |
| --- | --- | --- |
| `ufw status verbose` | active; default deny incoming; 22 LIMIT, 80/443 ALLOW only | ✅ exactly that (v4+v6) |
| `fail2ban-client status sshd` | jail running | ✅ journald matches `_SYSTEMD_UNIT=sshd.service` |
| `sshd -T` | password + kbd-interactive off, root key-only | ✅ `passwordauthentication no` |
| `docker network ls` | `app_net`, `data_net` bridges exist | ✅ |
| `ss -tlnp` on box | sshd is the only listener | ✅ only `:22` |
| external `nc` scan from laptop | only 22 open; 2019/5432/6379/9090/3000 filtered | ✅ 80/443 closed (nothing listening yet — expected until M1) |
| rerun `bootstrap.sh` | idempotent, no errors, no duplicate rules | ✅ second run clean |

## Found during setup

1. **Box image is ultra-minimal** — no `curl` preinstalled. bootstrap.sh installs it first; don't assume tooling on fresh boxes.
2. **Ubuntu 24.04 has no `/var/log/auth.log` by default** (journald only, no rsyslog). fail2ban's default sshd jail would fail to start → jail.local pins `backend = systemd` and bootstrap installs `python3-systemd`.
3. **Docker-published ports bypass UFW** (docker writes its own iptables chains). The firewall is *not* the guard for services — the invariant "only Caddy publishes ports" is. Verified: postgres will get zero `ports:` entries in M2.

## Residual risk

- Tailscale installed but not yet authenticated (`tailscale up` pending operator action) — admin plane lands with M3 bindings.
- 22 is internet-exposed (rate-limited, key-only). Once Tailscale is up, consider moving sshd to tailnet-only and dropping the public 22 allow.
