# Host baseline gotchas (Ubuntu 24.04)

Evidence: [drills/2026-07-06-m0-baseline-verification.md](../drills/2026-07-06-m0-baseline-verification.md)

## Docker-published ports bypass UFW

Docker writes its own iptables chains ahead of UFW's. The firewall is **not**
the guard for services — the invariant "only Caddy publishes public ports"
is. Admin UIs bind `${TS_IP:?}` (fail-loud: an empty TS_IP would silently
bind 0.0.0.0), databases publish nothing.

## noble is journald-only — no /var/log/auth.log

fail2ban's default sshd jail fails to start. `jail.local` pins
`backend = systemd` and bootstrap installs `python3-systemd`.

## Minimal images assume nothing

The provider's Ubuntu image ships without `curl`. `bootstrap.sh` installs its
own dependencies first; any new script should declare and check its deps
(`command -v`) rather than assume.

## Residual risk (open)

Public `:22` is still open (rate-limited, key-only). Once comfortable with
Tailscale reliability, move sshd to tailnet-only — deliberately, since a
tailscale outage then requires the provider console.
