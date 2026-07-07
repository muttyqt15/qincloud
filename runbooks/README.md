# runbooks/ — the operational record

Everything about *operating* QinCloud that isn't code or config: how things are
done, what has broken, what was learned, and the rules that came out of it. If
the rest of the repo is the system, this folder is the system's **memory**.

## The four kinds of document (and how they relate)

| Folder / file | What it is | Mutability |
| --- | --- | --- |
| `drills/` | The **immutable narrative of what happened** — dated records of drills and incidents, with times read off the box. | Append-only; never rewritten. |
| `gotchas/` | The **living rules of what to do about it** — one file per subsystem, the "don't repeat this" knowledge. | Edited in the same commit as the fix. |
| `postmortem-template.md` | The **blameless** structure for turning an incident into a fixed *class* of bug. | Copied per incident. |
| `apps/`, `data-services.md` | **How-to** operator guides — onboard an app, provision/migrate/rotate a database. | Kept current with the system. |

The relationship is the important part: **a drill or incident is the evidence;
a gotcha is the rule it produces; a postmortem is the bridge between them.** When
something breaks, the fix, the gotcha update, and (if drilled) the drill record
all land together — so the knowledge is never separated from the change that
earned it.

## Mental model: root cause over patch

Every document here is built around one refusal — to stop at the symptom. A
postmortem that ends at "we restarted it" has failed; it must keep asking *why
was this allowed to happen* until it reaches the missing invariant, and its
action items must kill the whole class ("make the bad state unrepresentable /
fail loud at the cause"), not the one instance. See
[root-cause over patch](../learnings/concepts/root-cause-over-patch.md). That's why the gotchas read as *rules* ("the aclfile
replaces the whole user table") rather than *logs* ("redis broke on the 7th").

## What's in here

- **`drills/`** — `failure-catalogue.md` (every failure mode + which ones page
  today, honestly), plus dated records: M0 baseline verification, M2 restore,
  M3 pager, M8 box-rebuild, the AppDown pager drill. The `failure-catalogue.md`
  is the index; start there.
- **`gotchas/`** — one file per domain: `caddy`, `deploys`, `data-services`,
  `backups-r2`, `alerting`, `dashboard`, `host`, `dev-process`. Its own
  [`gotchas/README.md`](gotchas/README.md) explains the one-file-per-domain rule.
- **`postmortem-template.md`** — blameless, 5-whys, with a worked example filled
  from the real M3 near-miss.
- **`apps/`** — per-app runbooks (e.g. Umami).
- **`data-services.md`** — the operator guide for the shared Postgres/Redis:
  provision, migrate, rotate, deprovision, DR.

## How it interacts

- Drills reference the alerts in [`../stack/observability/`](../stack/observability/)
  and the metrics they fire on.
- Operator guides drive [`../controld/`](../controld/) (`provision`, `deploy`)
  and [`../scripts/`](../scripts/).
- Every gotcha is the *what to do* companion to a [`../learnings/`](../learnings/)
  concept's *why*.

## The rule for adding to this folder

- **Drilled or broke something?** Write the dated record in `drills/` (times off
  the box), land the rule in the right `gotchas/` file, and if it was an
  incident, a postmortem — **all in the same commit as the fix.**
- **Never** rewrite history in `drills/`; correct forward with a new record.

## SRE concept: the operational memory outlives the operator

A one-person platform's biggest risk is that the *reasoning* lives only in the
operator's head. This folder is the deliberate externalisation of that: the box
can be rebuilt from [`../scripts/`](../scripts/), and the *judgment* to run it can
be rebuilt from here.
