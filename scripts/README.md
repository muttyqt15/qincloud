# scripts/ — the operator's hands

The small set of shell scripts that stand the box up, back it up, and change it
safely. If [`../stack/`](../stack/) is *what runs*, this folder is *how you
operate it* — the verbs. Every script is written to be **idempotent**
(safe to re-run) and **fail-loud** (`set -Eeuo pipefail`, guard every input).

## The scripts

| Script | What it does | When |
| --- | --- | --- |
| `bootstrap.sh` | Turns a raw Ubuntu box into a hardened base: UFW, fail2ban, key-only sshd, Docker, the shared bridges (`app_net`/`data_net`/…), unattended-upgrades. | First boot; re-run anytime (idempotent). |
| `deploy-edge.sh` | Applies a Caddy edge change and **proves it**: validate → heal the bind mount → reload → restore controld's routes → confirm each site 200s. | Any Caddyfile change. |
| `deploy-notes.sh` | Builds the notes site image from [`../learnings/`](../learnings/), pushes it, and rolls it through controld — self-verifying. | After editing the learnings vault. |
| `backup.sh` | Nightly `pg_dump` of every database → R2, plus the Redis `users.acl`; writes a freshness metric for the `BackupStale` alert. | Run by the systemd timer; also manual. |
| `restore-drill.sh` | Rehearses a restore **into a throwaway container** — never the real cluster. Proves backups are restorable without risking prod. | Periodically, and after backup changes. |
| `close-public-ssh.sh` | Flips sshd to tailnet-only. Refuses to run unless Tailscale is up (so you can't lock yourself out). | Once, after the tailnet is trusted. |
| `systemd/` | The `qincloud-backup.service` + `.timer` units that schedule `backup.sh`. | Installed during rebuild. |

## Mental model: idempotent + self-verifying

Read any script and you'll see the same two habits, because they're the whole
philosophy of operating a disposable box:

1. **Idempotent** — running it twice is the same as running it once. `bootstrap.sh`
   re-run on a live box changes nothing; a deploy script re-run just re-proves
   the current state. This is what makes "rebuild from zero" mechanical rather
   than terrifying.
2. **Self-verifying** — the script doesn't exit success until it has *checked*
   the thing it did (site returns 200, image digest matches, backup landed in
   R2 and re-downloads clean). A deploy that "finished" but didn't verify is a
   hope. See [idempotent, self-verifying operations](../learnings/concepts/idempotent-self-verifying-operations.md).

The two are why the box is safe to lose: the scripts *are* the reproducible
blueprint, and they prove their own success.

## The one that's deliberately manual

`restore-drill.sh` rehearses into a throwaway container **on purpose** — the
*real* restore in DR is a human `psql` + `pg_restore`, because an automated
restore pointed at the live data directory is exactly the tool that turns a
small incident into a total one. The line between "rehearse freely" and "the
real thing is deliberately hand-driven" is a safety design, not an omission.

## How it interacts

- `bootstrap.sh` creates the bridges every [`../stack/`](../stack/) project joins.
- `backup.sh` reads [`../stack/data/`](../stack/data/) (pg_dump, Redis acl) and
  feeds [`../stack/observability/`](../stack/observability/) (the freshness
  metric) → R2.
- `deploy-edge.sh` / `deploy-notes.sh` drive [`../stack/edge/`](../stack/edge/)
  and controld.

## SRE concepts here

- **The box is disposable / cattle not pets** — never hand-edit the running
  system; change it by re-running a script. [The box is disposable](../learnings/concepts/the-box-is-disposable.md).
- **Gate the irreversible** — `close-public-ssh.sh` refuses without tailnet;
  the real restore is manual. Dangerous verbs check their preconditions first.

Before editing host or backup behaviour, read the matching gotcha:
[`../runbooks/gotchas/host.md`](../runbooks/gotchas/host.md),
[`../runbooks/gotchas/backups-r2.md`](../runbooks/gotchas/backups-r2.md).
