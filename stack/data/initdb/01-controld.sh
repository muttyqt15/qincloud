#!/bin/sh
# 01-controld.sh — create the dedicated controld role + database (control-plane
# state). A dedicated role, not the cluster superuser: controld only ever needs
# its own database, and once M6 tenant databases share this cluster the control
# plane's credentials must not be able to reach them. CONTROLD_DSN uses it:
#   postgresql://controld:<CONTROLD_DB_PASSWORD>@qincloud-postgres:5432/controld?sslmode=disable
#
# A shell script rather than .sql because the role password comes from the
# environment (CONTROLD_DB_PASSWORD, set in /opt/qincloud/.env and passed in by
# stack/data/compose.yml) — initdb .sql files cannot read env vars.
#
# docker-entrypoint-initdb.d scripts run ONLY when the pgdata volume is fresh
# (first boot of an empty volume) — they do NOT run on an already-initialized
# box. For an existing volume, create both once by hand:
#   docker exec qincloud-postgres psql -U "$POSTGRES_USER" \
#     -c "CREATE ROLE controld LOGIN PASSWORD '<CONTROLD_DB_PASSWORD>'" \
#     -c "CREATE DATABASE controld OWNER controld"
#
# DR note: deploy IDs (and thus qc-<app>-<id> container names) come from an
# identity column, and a dump restore rewinds its sequence to the dump's value.
# If app containers created after that backup still exist on the box, remove
# the qc-* containers after restoring or new deploys collide on names.
set -eu

# :'pw' is psql variable quoting — the password is passed as a psql variable,
# never interpolated by the shell or the SQL text, so any character is safe.
psql -v ON_ERROR_STOP=1 -v pw="$CONTROLD_DB_PASSWORD" \
	--username "$POSTGRES_USER" --dbname postgres <<'EOSQL'
CREATE ROLE controld LOGIN PASSWORD :'pw';
CREATE DATABASE controld OWNER controld;
EOSQL
