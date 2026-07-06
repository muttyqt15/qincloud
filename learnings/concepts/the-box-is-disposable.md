---
title: The Box Is Disposable
slug: the-box-is-disposable
type: concept
status: stable
difficulty: 2
tags: [qincloud, principle, reliability, infra]
created: 2026-07-06
updated: 2026-07-06
related: ["[[m8-disaster-recovery]]", "[[m0-host-baseline]]", "[[m2-data-and-backups]]", "[[idempotent-self-verifying-operations]]", "[[single-source-of-truth]]"]
sources: ["README.md", "runbooks/drills/2026-07-06-m8-box-rebuild-drill.md", "scripts/bootstrap.sh", "scripts/backup.sh"]
---

# The Box Is Disposable

> **The principle in one line:** treat every server as replaceable — config lives in git, state lives in volumes and offsite backups, secrets live in a manager — so any machine can be rebuilt from zero.

## What it means (plain English)

There are two ways to own a server. One is a **pet**: it has a name, you've hand-tuned it over months, nobody remembers everything that was done to it, and if it dies you're in deep trouble. The other is **cattle**: it's interchangeable, everything about it is written down somewhere reproducible, and if one falls over you build a fresh one from the recipe and move on.

"The box is disposable" is the discipline of keeping your server as cattle. Nothing important lives *only* in the running machine's head. The machine is just the current place the recipe happens to be executing.

## Why it matters

A pet server is a slow-motion emergency. Every hand-edit that isn't written down is knowledge that dies with the disk. When it finally fails — and it will — you're reconstructing from memory under pressure. Worse, you can never *test* your recovery, because you're too scared to touch the one machine that works. Disposability flips this: because rebuilding is routine, you can rehearse it, and a dead box becomes a 12-minute chore instead of a catastrophe.

## Where it showed up in QinCloud

- **[[m8-disaster-recovery]] — the drill that proved it.** We didn't just *claim* the box was disposable — we destroyed it on purpose: every container, volume, network, image, and `/opt/qincloud` deleted, then rebuilt from `bootstrap.sh` + `git clone` + R2 restore. Serving again in **~12 minutes** (`runbooks/drills/…-m8-box-rebuild-drill.md`). That rehearsal is the only real proof the principle holds; see [[idempotent-self-verifying-operations]].

- **The three homes for the three kinds of state.** This is the mechanism (`README.md`): **config in git** (every compose file, Caddyfile, script — the repo is the recipe), **state in volumes + offsite** (Postgres/Redis data in named volumes, dumped nightly to R2 by [[m2-data-and-backups]]), and **secrets in a manager** (the `.env` is rebuilt from your password manager, never committed). Rebuilding is just: run the recipe, restore the state, re-inject the secrets.

- **[[m0-host-baseline]] — the recipe is idempotent.** `bootstrap.sh` can be re-run cleanly, which is *why* a fresh box converges to the same baseline every time. Reproducibility is what makes the box throwaway.

- **The drift lesson.** The drill surfaced a real trap: the git recipe had moved *ahead* of the running box (a new DB role existed only in the repo), so the rebuild became an unplanned migration. Disposability assumes the recipe and the box agree — see [[single-source-of-truth]]. Reconcile the running box whenever the repo changes, or the "rebuild" discovers surprises at the worst moment.

## How to apply it

Ask of your server: **"if this disk died right now, what would I lose that isn't written down somewhere reproducible?"** Whatever the honest answer names — a hand-edited config, an undocumented package, a secret only in the running env — is a pet-shaped liability. Move it to its proper home: config to git, state to a backed-up volume, secret to a manager. Then *prove* it by rebuilding into a throwaway target (or, once brave, the real one). Never hand-edit the running system; edit the recipe and re-apply it.

## Signs you're violating it

- You're afraid to reboot or rebuild the server.
- "Rebuilding it would take days and I'm not sure I remember everything."
- Config changes are made by SSHing in and editing files in place.
- You've never actually restored a backup or rebuilt from scratch.

---
**Related:** [[idempotent-self-verifying-operations]] · [[m2-data-and-backups]] · [[m0-host-baseline]]
