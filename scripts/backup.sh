#!/usr/bin/env bash
# backup.sh — nightly QinCloud backup. pg_dump -Fc of every database in the
# running postgres container + a redis RDB snapshot, gzipped with timestamped
# names, uploaded to Cloudflare R2 via rclone (configured purely from env),
# each upload verified non-empty, remote pruned to the newest 14 per prefix.
# Exits non-zero on ANY failure — a metric/alert hooks that later.
#
# Normally run by systemd (scripts/systemd/qincloud-backup.timer):
#   systemctl start qincloud-backup.service
# Manual run on the box:
#   set -a; . /opt/qincloud/.env; set +a; /opt/qincloud/scripts/backup.sh
#
# Requires: docker, rclone, gzip, flock.
# Env: R2_ACCOUNT_ID, R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY, R2_BUCKET,
#      POSTGRES_USER.
set -Eeuo pipefail

log() { printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
die() { printf '[%s] ERROR: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; exit 1; }
trap 'die "failed at line $LINENO: $BASH_COMMAND"' ERR

# --- preflight ---------------------------------------------------------------
: "${R2_ACCOUNT_ID:?R2_ACCOUNT_ID is not set}"
: "${R2_ACCESS_KEY_ID:?R2_ACCESS_KEY_ID is not set}"
: "${R2_SECRET_ACCESS_KEY:?R2_SECRET_ACCESS_KEY is not set}"
: "${R2_BUCKET:?R2_BUCKET is not set}"
: "${POSTGRES_USER:?POSTGRES_USER is not set}"

for cmd in docker rclone gzip flock; do
  command -v "$cmd" >/dev/null || die "missing dependency: $cmd"
done

# container names are fixed in stack/data/compose.yml
readonly PG_CONTAINER=qincloud-postgres
readonly REDIS_CONTAINER=qincloud-redis
readonly KEEP=14

# gate on the compose healthchecks, not just Running: a Persistent= catch-up run
# right after boot hits a postgres that is up but still replaying WAL
for c in "$PG_CONTAINER" "$REDIS_CONTAINER"; do
  health_deadline=$(( SECONDS + 120 ))
  until [[ "$(docker inspect -f '{{.State.Health.Status}}' "$c" 2>/dev/null)" == "healthy" ]]; do
    (( SECONDS < health_deadline )) || die "container $c not healthy within 120s"
    sleep 5
  done
done

# rclone remote "r2" built entirely from env — no config file on disk
export RCLONE_CONFIG_R2_TYPE=s3
export RCLONE_CONFIG_R2_PROVIDER=Cloudflare
export RCLONE_CONFIG_R2_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID"
export RCLONE_CONFIG_R2_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY"
export RCLONE_CONFIG_R2_ENDPOINT="https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com"
# scoped R2 tokens usually lack CreateBucket; skip the probe
export RCLONE_CONFIG_R2_NO_CHECK_BUCKET=true
readonly REMOTE="r2:${R2_BUCKET}"

# single-instance guard — a slow run must not overlap the next one
exec 9>/run/lock/qincloud-backup.lock || die "cannot open lock file"
flock -n 9 || die "another backup run holds the lock"

WORKDIR=$(mktemp -d) || die "mktemp failed"
trap 'rm -rf -- "$WORKDIR"' EXIT

readonly TS="$(date -u +%Y%m%dT%H%M%SZ)"

# --- helpers -----------------------------------------------------------------
# upload_verified <local-file> <remote-prefix> — copy, then re-list the object
# and require a positive size; an empty offsite backup is a silent disaster.
upload_verified() {
  local -r file="$1" prefix="$2"
  local -r base="$(basename -- "$file")"
  [[ -s "$file" ]] || die "refusing to upload empty file: $file"
  rclone copyto "$file" "$REMOTE/$prefix/$base"
  local size
  size=$(rclone lsl "$REMOTE/$prefix/$base" 2>/dev/null | awk '{print $1; exit}') || size=""
  [[ "$size" =~ ^[0-9]+$ && "$size" -gt 0 ]] \
    || die "upload verification failed for $prefix/$base (size='$size')"
  log "uploaded $prefix/$base ($size bytes)"
}

# prune_remote <remote-prefix> — keep the newest $KEEP objects. Filenames embed
# a UTC timestamp so a lexical sort is a chronological sort.
prune_remote() {
  local -r prefix="$1"
  local listing
  listing=$(rclone lsf --files-only "$REMOTE/$prefix" | sort)
  local -a files=()
  [[ -n "$listing" ]] && mapfile -t files <<< "$listing"
  local -r n=${#files[@]}
  (( n > KEEP )) || return 0
  local f
  for f in "${files[@]:0:n-KEEP}"; do
    rclone deletefile "$REMOTE/$prefix/$f"
    log "pruned $prefix/$f"
  done
}

# --- postgres: pg_dump -Fc every database ------------------------------------
db_list=$(docker exec "$PG_CONTAINER" psql -U "$POSTGRES_USER" -d postgres -At -c \
  "SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY datname") \
  || die "listing databases failed"
[[ -n "$db_list" ]] || die "no databases found in $PG_CONTAINER"
mapfile -t databases <<< "$db_list"

for db in "${databases[@]}"; do
  # filenames and remote paths are built from datname — reject anything exotic
  [[ "$db" =~ ^[A-Za-z0-9_-]+$ ]] || die "unsupported database name: '$db'"
  dump="$WORKDIR/pg_${db}_${TS}.dump"
  # no -t: keep docker exec stdout binary-safe for the custom-format dump
  docker exec "$PG_CONTAINER" pg_dump -U "$POSTGRES_USER" -Fc --no-password "$db" > "$dump"
  [[ -s "$dump" ]] || die "pg_dump produced an empty dump for $db"
  gzip "$dump"
  upload_verified "$dump.gz" "postgres/$db"
  prune_remote "postgres/$db"
done
log "postgres: backed up ${#databases[@]} database(s): ${databases[*]}"

# cluster globals (roles, passwords, memberships, ALTER ROLE settings) — the
# per-DB dumps alone cannot rebuild app users on a fresh box
globals="$WORKDIR/pg_globals_${TS}.sql"
docker exec "$PG_CONTAINER" pg_dumpall -U "$POSTGRES_USER" --globals-only > "$globals"
[[ -s "$globals" ]] || die "pg_dumpall --globals-only produced an empty file"
gzip "$globals"
upload_verified "$globals.gz" "postgres/_globals"
prune_remote "postgres/_globals"

# --- redis: BGSAVE + copy dump.rdb — cheap insurance on top of AOF ------------
lastsave_before=$(docker exec "$REDIS_CONTAINER" redis-cli LASTSAVE)
[[ "$lastsave_before" =~ ^[0-9]+$ ]] || die "unexpected LASTSAVE reply: '$lastsave_before'"
docker exec "$REDIS_CONTAINER" redis-cli BGSAVE >/dev/null

# LASTSAVE advancing is the only reliable BGSAVE-completed signal
deadline=$(( SECONDS + 120 ))
while :; do
  lastsave_now=$(docker exec "$REDIS_CONTAINER" redis-cli LASTSAVE)
  [[ "$lastsave_now" =~ ^[0-9]+$ ]] || die "unexpected LASTSAVE reply: '$lastsave_now'"
  (( lastsave_now > lastsave_before )) && break
  (( SECONDS < deadline )) || die "redis BGSAVE did not complete within 120s"
  sleep 2
done

rdb="$WORKDIR/redis_${TS}.rdb"
docker cp -q "$REDIS_CONTAINER:/data/dump.rdb" "$rdb"
[[ -s "$rdb" ]] || die "copied redis dump.rdb is empty"
gzip "$rdb"
upload_verified "$rdb.gz" "redis"
prune_remote "redis"

log "backup complete: ${#databases[@]} postgres dump(s) + 1 redis snapshot @ $TS"
