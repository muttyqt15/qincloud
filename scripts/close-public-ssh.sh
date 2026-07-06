#!/usr/bin/env bash
# close-public-ssh.sh — final hardening step, run AFTER `tailscale up`
# (rebuild-from-zero step 10): remove the public ssh allowance so sshd is
# reachable over the tailnet interface only. Idempotent.
#
# RECOVERY if tailscale ever breaks: the VPS provider's web console →
#   ufw limit 22/tcp        # temporarily reopen public ssh
# then fix tailscale and run this script again.
set -Eeuo pipefail

log() { printf '==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# Refuse to lock the door unless the other door actually exists.
ip link show tailscale0 >/dev/null 2>&1 || die "no tailscale0 interface — run 'tailscale up' first"
tailscale status >/dev/null 2>&1 || die "tailscale not running or not authenticated"

log "ufw: admit ssh on tailscale0 only"
ufw allow in on tailscale0 to any port 22 proto tcp >/dev/null
ufw delete limit 22/tcp >/dev/null 2>&1 || true # absent rule = already done

log "resulting ssh rules:"
ufw status verbose | grep -E "^22|22/tcp" || die "no ssh rule left at all — check ufw status"
