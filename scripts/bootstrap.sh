#!/usr/bin/env bash
# bootstrap.sh — idempotent host baseline for the QinCloud box (Ubuntu 24.04).
# Safe to rerun. Run as root: bash bootstrap.sh
#
# Does: sshd key-only auth, UFW (deny-in, allow 22/80/443), fail2ban,
# unattended-upgrades, Docker Engine + compose plugin, app_net/data_net
# bridge networks, Tailscale install.
set -Eeuo pipefail
export DEBIAN_FRONTEND=noninteractive LC_ALL=C.UTF-8

log() { printf '==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
trap 'die "failed at line $LINENO: $BASH_COMMAND"' ERR

# Fixed subnet for admin_net so Caddy's :2019 metrics block can allow only
# this range (see stack/edge/Caddyfile). Kept in a Docker-private range that
# does not clash with the default bridge pools.
readonly ADMIN_NET_SUBNET=10.201.7.0/24

[[ $EUID -eq 0 ]] || die "must run as root"
. /etc/os-release
[[ $ID == ubuntu ]] || die "expected Ubuntu, got $ID"

log "base packages"
apt-get update -qq
# rsync: deploy path (laptop → box); unzip: rclone's installer needs it
apt-get install -y -qq curl ca-certificates ufw fail2ban python3-systemd \
  unattended-upgrades rsync unzip >/dev/null

log "rclone (upstream — backup.sh/restore-drill.sh → R2)"
# noble's apt rclone is 1.60 (2022), which predates R2 quirk handling and
# 501s (NotImplemented) on the first attempt of every upload — install
# current upstream instead
if ! command -v rclone >/dev/null || rclone version | grep -q "^rclone v1.60"; then
  curl -fsSL https://rclone.org/install.sh | bash >/dev/null 2>&1 || die "rclone install failed"
fi

log "sshd: key-only auth"
install -d -m 0755 /etc/ssh/sshd_config.d
cat > /etc/ssh/sshd_config.d/99-qincloud.conf <<'EOF'
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
EOF
sshd -t
systemctl reload ssh

# Public 22 stays open here by necessity: a fresh box has no authenticated
# tailscale yet. scripts/close-public-ssh.sh (rebuild step 10) closes it
# once tailscale is up.
log "ufw: default deny incoming, allow 22/80/443"
ufw default deny incoming >/dev/null
ufw default allow outgoing >/dev/null
ufw limit 22/tcp >/dev/null # rate-limits ssh brute force
ufw allow 80/tcp >/dev/null
ufw allow 443/tcp >/dev/null
ufw --force enable >/dev/null
# NOTE: docker-published ports bypass UFW via iptables. The invariant is
# "only Caddy publishes ports" — everything else stays on bridge networks.

log "fail2ban: sshd jail (systemd backend — noble has no auth.log by default)"
cat > /etc/fail2ban/jail.local <<'EOF'
[sshd]
enabled  = true
backend  = systemd
maxretry = 5
bantime  = 1h
EOF
systemctl enable --now fail2ban >/dev/null 2>&1
systemctl restart fail2ban

log "unattended-upgrades"
cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF

log "docker engine + compose plugin"
if ! command -v docker >/dev/null; then
  curl -fsSL https://get.docker.com | sh
fi
systemctl enable --now docker >/dev/null 2>&1

# tenant_db_net carries ONLY postgres + the apps that request a database
# (AppSpec.UseDB) — redis, exporters, and controld are never on it.
log "docker bridge networks: app_net, data_net, tenant_db_net"
for net in app_net data_net tenant_db_net; do
  docker network inspect "$net" >/dev/null 2>&1 || docker network create "$net" >/dev/null
done

# admin_net carries ONLY caddy + controld + prometheus (dashboard proxy and
# metrics scrape) — tenant apps are never on it. Fixed subnet so Caddy's
# :2019 metrics listener can allow only this range (the listener binds every
# interface Caddy is on, and Caddy is also on app_net to proxy tenant apps).
if ! docker network inspect admin_net >/dev/null 2>&1; then
  docker network create --subnet "$ADMIN_NET_SUBNET" admin_net >/dev/null
fi

log "tailscale"
if ! command -v tailscale >/dev/null; then
  curl -fsSL https://tailscale.com/install.sh | sh
fi
if ! tailscale status >/dev/null 2>&1; then
  log "ACTION NEEDED: run 'tailscale up' and open the printed auth URL"
fi

log "bootstrap complete"
