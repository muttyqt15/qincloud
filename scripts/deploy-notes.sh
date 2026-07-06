#!/usr/bin/env bash
# deploy-notes.sh — build, push, roll, and PROVE the notes site. Idempotent.
#
# The notes site is the Quartz static build of the learnings/ vault
# (sites/notes/Dockerfile), served by nginx, published as the controld app
# "notes" at https://notes.sparboard.com. A deploy is a four-step ritual —
# build the image for the RIGHT architecture, push it to ghcr, tell controld on
# the box to roll the app to that exact tag, then the question the naive path
# skips: is the new site actually serving? This script is that ritual as one
# self-verifying command, modelled on deploy-edge.sh.
#
# WHERE IT RUNS: on the OPERATOR's machine (your laptop), from the REPO ROOT —
# not on the box. It needs the repo (build context = repo root, so the image
# can COPY both learnings/ and sites/notes/), docker buildx to build+push, and
# tailnet ssh to the box to drive controld. It does NOT touch box files.
#
#   ./scripts/deploy-notes.sh                 # tag v-<UTC timestamp> + :latest
#   ./scripts/deploy-notes.sh --tag v7        # pin an explicit tag
#
# THE ARCHITECTURE TRAP (learnings/milestones/x3-*, and hard-won): the box is
# amd64. A Mac builds arm64 by default, and an arm64 image exec-format-crashes
# the instant nginx starts — the container never serves and the deploy "half
# works". So the build is ALWAYS `--platform linux/amd64 --push`; never a plain
# `docker build` (which bakes the host arch) and never `--load` (single-arch to
# the local daemon, no push). The deploy pins the immutable :<tag>, not :latest,
# so the container rolls to the exact bytes we just built and can be verified.
#
# ONE MANUAL PREREQUISITE: the ghcr package must already exist and be PUBLIC
# (so the box can pull it without registry creds), and `gh` must be logged in
# with a token that can write:packages — this script does the `docker login
# ghcr.io` for you from `gh auth token`. If you have never pushed this package,
# push once and flip it to Public in the GitHub package settings first.
#
# Requires (operator machine): docker (with buildx), gh, ssh, curl.
set -Eeuo pipefail

log()  { printf '==> %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
trap 'die "failed at line $LINENO: $BASH_COMMAND"' ERR

# App identity — fixed by how notes is built and published.
readonly APP=notes
readonly HOST=notes.sparboard.com
readonly PORT=80                       # nginx EXPOSE 80 (sites/notes/nginx.conf)
readonly IMAGE_REPO=ghcr.io/muttyqt15/qcloud-notes
readonly DOCKERFILE=sites/notes/Dockerfile
readonly CONTROLD=qincloud-controld    # container name on the box
# Box is tailnet-only (runbooks/gotchas/host.md): public :22 is closed.
# Override BOX / SSH_KEY in the environment if your tailnet identity differs.
readonly BOX="${QIN_BOX:-root@100.125.12.20}"
readonly SSH_KEY="${QIN_SSH_KEY:-$HOME/.ssh/qin-vps}"
# A freshly-rolled container needs a moment to answer through Caddy over the
# public internet; poll rather than assume.
readonly VERIFY_TIMEOUT=120

ssh_box() { ssh -i "$SSH_KEY" -o BatchMode=yes "$BOX" "$@"; }

# --- args --------------------------------------------------------------------
TAG=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag) TAG="${2:-}"; shift 2 || die "--tag needs a value" ;;
    --tag=*) TAG="${1#--tag=}"; shift ;;
    -h | --help) sed -n '2,33p' "$0"; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done
[[ -n "$TAG" ]] || TAG="v-$(date -u +%Y%m%dT%H%M%SZ)"
[[ "$TAG" =~ ^[A-Za-z0-9._-]+$ ]] || die "invalid tag: '$TAG'"
readonly TAG
readonly PINNED="$IMAGE_REPO:$TAG"
readonly LATEST="$IMAGE_REPO:latest"

# --- preflight ---------------------------------------------------------------
for cmd in docker gh ssh curl; do
  command -v "$cmd" >/dev/null || die "missing dependency: $cmd"
done
docker buildx version >/dev/null 2>&1 || die "docker buildx is not available — needed for --platform linux/amd64 --push"
# Run from the repo root: the build context is "." and the image COPYs both
# learnings/ and sites/notes/. Anchor on the two paths that must both exist.
[[ -f "$DOCKERFILE" ]] || die "no $DOCKERFILE here — run from the repo root"
[[ -d learnings ]] || die "no learnings/ here — run from the repo root (the vault is the site content)"
gh auth status >/dev/null 2>&1 || die "gh is not logged in — run 'gh auth login' (token needs write:packages)"

# --- 1. authenticate to ghcr from the gh token (no separate PAT to manage) ---
log "logging in to ghcr.io"
gh_user=$(gh api user --jq .login) || die "could not read gh user — is gh authed?"
gh auth token | docker login ghcr.io -u "$gh_user" --password-stdin >/dev/null \
  || die "docker login ghcr.io failed — check the gh token has write:packages"

# --- 2. build for the box's arch and push (both the pinned tag and :latest) ---
# --platform linux/amd64 is load-bearing (see header). --push, not --load: the
# box pulls from ghcr, so the bytes must land in the registry, not the laptop.
log "building + pushing $PINNED (linux/amd64)"
docker buildx build \
  --platform linux/amd64 \
  --file "$DOCKERFILE" \
  --tag "$PINNED" \
  --tag "$LATEST" \
  --push \
  . \
  || die "buildx build/push failed — image not published, box untouched"

# --- 3. roll the app on the box to the exact tag we just pushed ---------------
# `deploy` (not `redeploy`): redeploy re-runs the STORED spec, which still pins
# the OLD image — it would never move the app forward. deploy with the new
# -image is the correct roll-forward, and notes carries no -env/-db, so there is
# no stored secret to clobber (the caution in runbooks/gotchas/deploys.md is
# specifically about re-typing env; notes has none). controld keeps the old
# container serving until the new one is ready AND routed, so a bad build cannot
# take the site down.
log "rolling $APP on $BOX to $TAG"
ssh_box "docker exec $CONTROLD controld deploy -app $APP -image $PINNED -port $PORT -host $HOST" \
  || die "controld deploy failed on the box — old container still serving"

# --- 4a. verify the box accepted our exact tag (not a stale spec) ------------
# controld list columns: APP HOST IMAGE CONTAINER UPDATED. Prove the recorded
# image is the tag we pushed — this closes the "did the deploy roll forward?"
# gap before we even touch HTTP.
log "verifying controld recorded $TAG"
listed=$(ssh_box "docker exec $CONTROLD controld list") || die "controld list failed on the box"
recorded=$(awk -v app="$APP" '$1==app {print $3}' <<<"$listed")
[[ "$recorded" == "$PINNED" ]] \
  || die "controld shows $APP image '$recorded', expected '$PINNED' — the roll did not take"

# --- 4b. verify the live site: 200 homepage AND the graph asset present -------
# Two grounded checks against the real published site (both confirmed to exist
# on the running site, not invented):
#   - GET / is 200 and the HTML carries the Quartz graph-view DOM
#     (`graph-container`), so the page rendered with its graph, and
#   - /static/contentIndex.json — the JSON the graph view fetches to draw its
#     nodes/edges — is 200, application/json, and non-empty.
# A freshly-rolled container can 502/000 through Caddy for a beat, so poll the
# homepage to 200 first, then assert the graph pieces with teeth.
log "verifying https://$HOST is live with its graph"
deadline=$(( SECONDS + VERIFY_TIMEOUT ))
home=""
while :; do
  code=$(curl -sS -o /dev/null -w '%{http_code}' -m 10 "https://$HOST/" 2>/dev/null || echo 000)
  if [[ "$code" == 200 ]]; then
    home=$(curl -sS -m 10 "https://$HOST/" 2>/dev/null || echo "")
    [[ "$home" == *graph-container* ]] && break
    warn "homepage is 200 but has no graph-container yet — waiting"
  fi
  (( SECONDS < deadline )) || die "https://$HOST/ never served a 200 with its graph within ${VERIFY_TIMEOUT}s (last=$code)"
  sleep 3
done

# graph data source — the asset the graph view actually reads. The trailing
# \n in -w matters: `read` returns non-zero on an input with no final newline
# (even though it read the values), and under errexit that would fail a
# perfectly good deploy at the verification step.
read -r gcode gtype gbytes < <(
  curl -sS -o /dev/null -w '%{http_code} %{content_type} %{size_download}\n' -m 15 \
    "https://$HOST/static/contentIndex.json" 2>/dev/null || echo "000 - 0"
)
[[ "$gcode" == 200 ]]                 || die "graph asset /static/contentIndex.json returned $gcode"
[[ "$gtype" == application/json* ]]   || die "graph asset content-type is '$gtype', expected application/json"
[[ "$gbytes" =~ ^[0-9]+$ && "$gbytes" -gt 0 ]] \
  || die "graph asset /static/contentIndex.json is empty (bytes='$gbytes')"

log "notes deployed and verified: $PINNED live at https://$HOST/ (graph asset $gbytes bytes)"
