#!/usr/bin/env bash
# deploy-edge.sh — apply the edge (Caddy) config and prove it. Idempotent.
#
# The edge has a split source of truth: the Caddyfile is the first-boot seed,
# but controld programs each app's route through Caddy's admin API, and a
# Caddyfile reload wipes those routes (Caddy runs --resume). So "edit the
# Caddyfile" is really a four-step ritual — make Caddy actually see the new
# file, validate it, reload, then redeploy every app to restore its route —
# followed by the question that used to be skipped: did it actually work?
# This script is that ritual as one self-verifying command.
#
# Run it on the box after syncing the repo. The sync method does not matter:
# the freshness guard below detects and heals a stale bind mount, which is the
# trap that silently serves an old Caddyfile after a plain rsync.
#   rsync -a --inplace <repo>/ root@<box>:/opt/qincloud/   # from your laptop
#   ssh root@<box> 'bash /opt/qincloud/scripts/deploy-edge.sh'
#
#   --dry-run   validate the on-disk Caddyfile and stop; touch nothing.
#
# Requires: docker, docker compose, curl, sha256sum. Run as root on the box.
set -Eeuo pipefail

log()  { printf '==> %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
trap 'die "failed at line $LINENO: $BASH_COMMAND"' ERR

readonly ROOT=/opt/qincloud
readonly EDGE_DIR="$ROOT/stack/edge"
readonly ENV_FILE="$ROOT/.env"
readonly CADDYFILE="$EDGE_DIR/Caddyfile"
readonly CADDY=edge-caddy-1
readonly CONTROLD=qincloud-controld
# A just-redeployed app (new container) may need a moment to become reachable
# — umami runs DB migrations on boot and 502s until they finish.
readonly VERIFY_TIMEOUT=90

DRY_RUN=false
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=true

compose() { docker compose --project-directory "$EDGE_DIR" --env-file "$ENV_FILE" "$@"; }

# --- preflight ---------------------------------------------------------------
[[ $EUID -eq 0 ]] || die "run as root on the box"
for cmd in docker curl sha256sum; do
  command -v "$cmd" >/dev/null || die "missing dependency: $cmd"
done
[[ -f "$CADDYFILE" ]] || die "no Caddyfile at $CADDYFILE — is the repo synced to $ROOT?"
docker inspect "$CADDY" >/dev/null 2>&1 || die "$CADDY is not running — bring stack/edge up first"
docker inspect "$CONTROLD" >/dev/null 2>&1 || die "$CONTROLD is not running — bring stack/controld up first"

# --- 1. validate the on-disk file, mount-independent, before any mutation -----
# Piped over stdin so this validates the TRUE on-disk Caddyfile regardless of
# whether the container's bind mount is stale (see the freshness guard below).
log "validating Caddyfile"
compose exec -T caddy caddy validate --config /dev/stdin --adapter caddyfile < "$CADDYFILE" >/dev/null 2>&1 \
  || die "Caddyfile is invalid — running config untouched"

if $DRY_RUN; then
  log "dry run: Caddyfile is valid; nothing applied"
  exit 0
fi

# --- 2. freshness guard: the container MUST serve the on-disk Caddyfile --------
# rsync writes a temp file and renames it, swapping the host inode while the
# container keeps the old one — the container then serves a STALE Caddyfile and
# every reload reads stale. Compare checksums; recreate to re-establish the
# mount if they differ (also picks up any stack/edge/compose.yml change).
host_sum=$(sha256sum "$CADDYFILE" | awk '{print $1}')
cont_sum=$(docker exec "$CADDY" sha256sum /etc/caddy/Caddyfile 2>/dev/null | awk '{print $1}') || cont_sum=""
if [[ "$host_sum" != "$cont_sum" ]]; then
  log "container Caddyfile is stale — recreating $CADDY to refresh the mount"
  compose up -d --force-recreate >/dev/null
  # small settle so the admin socket is back before reload
  sleep 2
fi

# --- 3. reload — this wipes controld-programmed app routes --------------------
log "reloading edge config"
compose exec -T caddy caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile \
  || die "reload failed — the previous config is still serving"

# --- 4. restore every app's route (the reload dropped them) ------------------
apps_raw=$(docker exec "$CONTROLD" controld list) || die "controld list failed"
mapfile -t rows < <(printf '%s\n' "$apps_raw" | tail -n +2)

declare -a hosts=()
for row in "${rows[@]}"; do
  [[ -n "${row// }" ]] || continue
  app=$(awk '{print $1}' <<<"$row")
  host=$(awk '{print $2}' <<<"$row")
  [[ -n "$app" ]] || continue
  log "restoring route: $app ($host)"
  docker exec "$CONTROLD" controld redeploy -app "$app" >/dev/null \
    || die "redeploy $app failed — its route is not restored"
  hosts+=("$host")
done
(( ${#hosts[@]} > 0 )) || log "no apps registered — nothing to route"

# --- 5. verify: caddy healthy + every app host responds (not 5xx / no-connect) -
log "verifying"
health=$(docker inspect -f '{{.State.Health.Status}}' "$CADDY" 2>/dev/null || echo unknown)
[[ "$health" == healthy || "$health" == starting ]] || die "caddy health is '$health'"

# verify_host prints the last observed HTTP code and returns non-zero if the
# host never became healthy within the timeout. 2xx/3xx/401/403/404 all mean
# "routed and responding" (401 is the dashboard's own auth challenge); only a
# 5xx or 000 (no connection / bad gateway) counts as broken or still booting.
verify_host() {
  local host="$1" code deadline=$(( SECONDS + VERIFY_TIMEOUT ))
  while :; do
    code=$(curl -sS -o /dev/null -w '%{http_code}' -m 10 "https://$host/" 2>/dev/null || echo 000)
    case "$code" in
      000 | 5??) : ;; # broken or still coming up — keep waiting
      *) printf '%s' "$code"; return 0 ;;
    esac
    (( SECONDS < deadline )) || { printf '%s' "$code"; return 1; }
    sleep 3
  done
}

failed=0
for host in "${hosts[@]}"; do
  if code=$(verify_host "$host"); then
    log "ok   https://$host ($code)"
  else
    warn "FAIL https://$host (last=$code after ${VERIFY_TIMEOUT}s)"
    failed=1
  fi
done
(( failed == 0 )) || die "one or more app hosts did not come back healthy — the edge is in a bad state, investigate now"

log "edge deployed and verified: ${#hosts[@]} app(s) routed"
