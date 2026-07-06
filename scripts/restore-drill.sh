#!/usr/bin/env bash
# restore-drill.sh — DR rehearsal. Fetches the latest pg dump from R2, restores
# it into a THROWAWAY postgres container on data_net, runs a sanity query, and
# prints the elapsed seconds (measured RTO). The throwaway container is always
# removed, even on failure. This script never touches the real qincloud-postgres
# service or its pgdata volume — it only reads from R2.
#
# Run on the box:
#   set -a; . /opt/qincloud/.env; set +a; /opt/qincloud/scripts/restore-drill.sh [database]
# With no argument it drills the most recently uploaded dump of any database.
#
# Requires: docker, rclone, gzip, openssl.
# Env: R2_ACCOUNT_ID, R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY, R2_BUCKET.
set -Eeuo pipefail

log() { printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
die() { printf '[%s] ERROR: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; exit 1; }
trap 'die "failed at line $LINENO: $BASH_COMMAND"' ERR

# --- preflight ---------------------------------------------------------------
: "${R2_ACCOUNT_ID:?R2_ACCOUNT_ID is not set}"
: "${R2_ACCESS_KEY_ID:?R2_ACCESS_KEY_ID is not set}"
: "${R2_SECRET_ACCESS_KEY:?R2_SECRET_ACCESS_KEY is not set}"
: "${R2_BUCKET:?R2_BUCKET is not set}"

for cmd in docker rclone gzip openssl; do
  command -v "$cmd" >/dev/null || die "missing dependency: $cmd"
done

# same rclone env-only remote as backup.sh — kept inline so each ops script
# stays self-contained (no sourcing dependency at 3am)
export RCLONE_CONFIG_R2_TYPE=s3
export RCLONE_CONFIG_R2_PROVIDER=Cloudflare
export RCLONE_CONFIG_R2_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID"
export RCLONE_CONFIG_R2_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY"
export RCLONE_CONFIG_R2_ENDPOINT="https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com"
export RCLONE_CONFIG_R2_NO_CHECK_BUCKET=true
readonly REMOTE="r2:${R2_BUCKET}"

# same pinned image as stack/data/compose.yml so the drill is representative
readonly DRILL_IMAGE="postgres:16.9-alpine"
readonly DRILL_CONTAINER="qincloud-restore-drill-$$"

# --- pick the dump to drill --------------------------------------------------
requested_db="${1:-}"
if [[ -n "$requested_db" ]]; then
  [[ "$requested_db" =~ ^[A-Za-z0-9_-]+$ ]] || die "unsupported database name: '$requested_db'"
  latest=$(rclone lsf --files-only "$REMOTE/postgres/$requested_db" | sort | tail -n 1)
  [[ -n "$latest" ]] || die "no dumps found for database '$requested_db'"
  remote_file="postgres/$requested_db/$latest"
else
  # newest by modtime across all databases; format is "<modtime>;<path>".
  # _globals holds pg_dumpall SQL, not a pg_restore-able -Fc dump — skip it
  newest_line=$(rclone lsf -R --files-only --format "tp" --exclude "_globals/**" "$REMOTE/postgres" | sort | tail -n 1)
  [[ -n "$newest_line" ]] || die "no dumps found under postgres/ in bucket $R2_BUCKET"
  remote_file="postgres/${newest_line#*;}"
fi
log "drilling dump: $remote_file"

# --- drill: fetch → throwaway restore → sanity, all timed as the RTO ---------
start_epoch=$(date +%s)

WORKDIR=$(mktemp -d) || die "mktemp failed"
# guaranteed cleanup: the drill container and workdir go away no matter what
trap 'docker rm -f -v "$DRILL_CONTAINER" >/dev/null 2>&1 || true; rm -rf -- "$WORKDIR"' EXIT

rclone copyto "$REMOTE/$remote_file" "$WORKDIR/dump.gz"
[[ -s "$WORKDIR/dump.gz" ]] || die "fetched dump is empty: $remote_file"
gzip -d "$WORKDIR/dump.gz"

# throwaway superuser + random password: nothing shared with the real service
drill_pw=$(openssl rand -hex 16)
docker run -d --name "$DRILL_CONTAINER" --network data_net \
  -e POSTGRES_USER=drill \
  -e POSTGRES_PASSWORD="$drill_pw" \
  -e POSTGRES_DB=drill \
  --memory 512m \
  "$DRILL_IMAGE" >/dev/null

# -h 127.0.0.1 (TCP): the image's init-phase temp server only listens on the
# unix socket, so a socket check would report ready too early
deadline=$(( SECONDS + 90 ))
until docker exec "$DRILL_CONTAINER" pg_isready -h 127.0.0.1 -U drill -d drill >/dev/null 2>&1; do
  (( SECONDS < deadline )) || die "throwaway postgres not ready within 90s"
  sleep 2
done

docker exec "$DRILL_CONTAINER" createdb -U drill restore_target
# --no-owner/--no-privileges: prod roles don't exist in the throwaway
docker exec -i "$DRILL_CONTAINER" pg_restore -U drill -d restore_target \
  --no-owner --no-privileges < "$WORKDIR/dump"

table_count=$(docker exec "$DRILL_CONTAINER" psql -U drill -d restore_target -At -c \
  "SELECT count(*) FROM information_schema.tables
   WHERE table_schema NOT IN ('pg_catalog','information_schema')")
[[ "$table_count" =~ ^[0-9]+$ ]] || die "sanity query failed (got '$table_count')"
if (( table_count == 0 )); then
  log "WARNING: restore succeeded but contains 0 user tables — verify the source backup"
fi

elapsed=$(( $(date +%s) - start_epoch ))
log "drill OK: $remote_file → $table_count user table(s) restored"
log "measured RTO (fetch → restore → sanity): ${elapsed}s"
printf 'RTO_SECONDS=%d\n' "$elapsed"
