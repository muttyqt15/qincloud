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

## `compose down` needs the same env vars as `up`

`${VAR:?}` interpolation runs on every compose command — a missing variable
blocks even teardown. Dummy-set the var or `docker rm -f` the containers
directly when dismantling a stack whose env contract has moved on.

## Compose interpolates `$` inside .env values

A bcrypt hash (`$2a$14$…`) stored raw in `.env` gets mangled: compose treats
`$2a` etc. as variable references and substitutes blanks — the consumer
receives a corrupted value and nothing fails loud (compose only warns).
Escape every `$` as `$$` when the value must contain dollars
(`DASH_PASSWORD_HASH` is stored escaped). Class: any secret with `$` in it.

## docker.io pulls are flaky from this VPS

TLS handshake timeouts against registry-1.docker.io come and go. Any
procedure that pulls many images (rebuild, new stack) needs a retry loop;
consider a registry mirror if it worsens.

## SSH is tailnet-only (closed 2026-07-06)

Public `:22` is closed; ufw admits ssh on `tailscale0` only
(`scripts/close-public-ssh.sh`, rebuild step 10). Connect via
`ssh root@100.125.12.20` — the public IP no longer answers on 22. If
tailscale breaks: provider web console → `ufw limit 22/tcp` to reopen
temporarily, fix tailscale, re-run the script. A FRESH box necessarily
starts with public 22 (bootstrap.sh keeps the `limit` rule) until
tailscale is authenticated — closing it is deliberately the last step.
