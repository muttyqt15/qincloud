# App surface: Notes web editor (edit the vault from the browser)

Edit and publish the [learnings vault](../../learnings/) from
`https://dash.sparboard.com/edit` (or the tailnet dashboard at `TS_IP:8600/edit`)
instead of a laptop + `deploy-notes.sh`. It is a surface of **controld**, not a
separate app: the dashboard mounts `/edit` when — and only when — controld is
given a git token.

## What happens on Save

One linear path (`controld/internal/authoring`):

```
SafeRel (confine to learnings/**.md) → git pull --ff-only → write file
  → git add+commit+push (to the mirror) → build notes image → push ghcr → controld deploy
```

- **git is the source of truth.** The box holds a reconstructable clone in the
  `notes_repo` volume (controld re-clones from the mirror on first boot). A save
  commits and pushes there *before* anything is rebuilt, so the live site can
  never show a change the mirror doesn't have.
- **Publishing reuses the deploy state machine.** The rebuild is a normal
  `controld deploy` of the `notes` app, so it inherits the one rule — a failed
  build never takes the live site down — and the outcome shows on the `notes`
  app in the dashboard (and its deploy history). Expect ~a minute.
- The in-browser preview is an approximation (`# boom` → a big heading as you
  type). The authoritative render is Quartz on publish.

## Enabling it (one-time)

The editor is **opt-in by secret**. Without the token the `/edit` routes don't
exist and controld runs exactly as before.

1. **Create a scoped token.** A GitHub **fine-grained PAT** on
   `muttyqt15/qincloud` with **Contents: Read and write** (git push) and
   **Packages: Read and write** (ghcr push). A classic PAT with `repo` +
   `write:packages` also works but is broader. This is the one thing that can
   push to the repo and the image — treat it like a deploy key.
2. **Install it on the box** (tailnet-only, `/opt/qincloud/secrets/` is
   gitignored and 0600):
   ```sh
   install -m 600 /dev/null /opt/qincloud/secrets/gh_token
   printf '%s' '<the-token>' > /opt/qincloud/secrets/gh_token
   ```
3. **Recreate controld** so it mounts the secret and the `notes_repo` volume:
   ```sh
   docker compose --project-directory /opt/qincloud/stack/controld \
     --env-file /opt/qincloud/.env up -d --build
   ```
   On boot controld logs `notes editor enabled at /edit` and clones the mirror
   into `/workspace` (first time only).

To **disable**: empty the file (`: > /opt/qincloud/secrets/gh_token`) and
recreate controld — an empty token is treated as "off".

## Security model

- **Writes are confined to `learnings/**.md`.** `SafeRel` rejects any path that
  is absolute, climbs out (`../`), or isn't Markdown — the editor cannot touch
  controld's code, the compose files, or anything else in the repo.
- **Auth is the dashboard's auth**: tailnet-only on `TS_IP:8600`, and Caddy
  `basic_auth` on `dash.sparboard.com`. `/edit`'s POST also requires the
  `HX-Request` header (the dashboard's tokenless CSRF gate).
- **The token never hits argv.** git reads it from the process env via an inline
  credential helper; ghcr auth is base64-JSON in the push options. It is not
  persisted in `.git/config`.

## Gaps / notes

- **Publish failures after the ~1s handoff are shown on the `notes` app in the
  dashboard** (its deploy goes `failed` with the reason) and logged box-side —
  not echoed back into the editor flash. Watch the notes app after a save.
- **Concurrency with `deploy-notes.sh`:** both push to the mirror and both can
  roll `notes`. Single-operator use is fine (the clone pulls before writing); a
  push rejected for divergence surfaces as a save error — pull/retry.
- **Rollback** is `controld redeploy -app notes` off the last-good spec, or
  deploy an older `ghcr.io/muttyqt15/qcloud-notes:v-…` tag; every publish leaves
  an immutable tag in ghcr.
