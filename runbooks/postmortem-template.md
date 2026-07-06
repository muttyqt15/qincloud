# Postmortem template (blameless)

> **Copy this file to `runbooks/postmortems/YYYY-MM-DD-<slug>.md` and fill it in.**
> A postmortem is written for the *system*, not the *person*. We assume everyone
> acted reasonably with the information they had; the artifact under review is the
> system that let a reasonable action cause harm. No names-as-blame — name people
> only to own follow-ups. If a line makes someone look stupid, rewrite it to
> describe the trap they fell into instead.

The point of this document is **root cause over patch**
([`../learnings/concepts/root-cause-over-patch.md`](../learnings/concepts/root-cause-over-patch.md)):
a postmortem that ends at "we restarted it" has failed. Keep asking *why was this
allowed to happen* until you reach the wrong state / bad assumption / missing
invariant, and let the action items kill the whole *class*, not the one symptom.

Drills (`drills/`) are the immutable narrative of *what happened*; gotchas
(`gotchas/`) are the living rules of *what to do about it*. A postmortem feeds
both: it links the drill/incident record and it lands a rule in the right gotcha
file in the **same** commit as the fix.

---

## Header

| Field | Value |
| --- | --- |
| **Incident** | short title |
| **Date** | YYYY-MM-DD (of the incident, not the writeup) |
| **Severity** | SEV1 (serving down / data at risk) · SEV2 (degraded / one app) · SEV3 (no user impact, near-miss / drill finding) |
| **Author(s)** | who wrote this |
| **Status** | draft / reviewed / action-items-tracked / closed |
| **Detected by** | which alert / dashboard / human, and how long after onset |
| **Evidence** | link the drill record, Alertmanager log excerpt, `git` SHA — the box is the record, not the laptop |

## Summary

Two or three sentences a future on-call can read in ten seconds: what broke, the
blast radius, how it was resolved. No jargon that isn't defined below.

## Impact

- **Who/what was affected** — which app(s), the dashboard, backups, the whole box.
- **Duration** — onset (UTC) → recovery (UTC) = wall-clock impact.
- **User-visible?** — 5xx served, or purely internal (e.g. a blinded metric).
- **Data** — any loss? State it against RPO. If a restore ran, the measured RTO.

## Timeline (UTC)

All times UTC — read them off the box (container uptimes, Prometheus state,
Alertmanager logs), never a laptop's ssh session that may have hung mid-poll.

| T (UTC) | Event |
| --- | --- |
| 00:00:00 | onset — the first thing that actually went wrong (often earlier than detection) |
| 00:00:00 | detection — which signal, and note the detection latency |
| 00:00:00 | mitigation started |
| 00:00:00 | recovery — service restored / alert resolved |

## Root cause (5 whys)

Trace the symptom back to its origin. Stop when the next "why" is an external
constant, not a decision you own.

1. **Symptom:** what surfaced.
2. **Why?** →
3. **Why?** →
4. **Why?** →
5. **Root cause:** the wrong state / silent assumption / missing invariant that
   made the whole class possible.

State it plainly: *what invariant was missing, and where should it have been
enforced* (a guard, a `${VAR:?}`, a type that makes the bad state
unrepresentable, a test that would have gone red).

## What went well

Name the controls that *worked* — they earned their keep and must survive future
cleanups. (e.g. the `for:` window suppressed a scrape-blip page; the pre-drill
backup verified before teardown; the fail-loud `${VAR:?}` blocked a bad `up`.)

## What went wrong / where we got lucky

Honest list of every rough edge, including the ones that didn't bite *this* time
but could have. "We got lucky that X" is a first-class entry — it's a latent
incident with the fuse already lit.

## Action items

Each item kills a *class*, has an owner and a tracking home, and is testable.
Prefer "make the bad state unrepresentable / fail loud at the cause" over "be
more careful". A postmortem is not closed until every P1 here is landed.

| # | Action (root-cause, not symptom) | Owner | Priority | Tracking / done-when |
| --- | --- | --- | --- | --- |
| 1 | e.g. add `pg_up == 0` alert so a DB outage pages directly, not only via the controld target | | P1 | rule in `qincloud.yml` + drill proving it fires |
| 2 | e.g. document the runtime-uid secret-install rule at the secret definition | | P2 | comment in `compose.yml` + gotcha entry |

## Lessons → learnings

Link the durable principle(s) this reinforced, and land or update the rule:

- Concept(s): [`../learnings/concepts/...`](../learnings/concepts/)  — which principle this is another instance of.
- Gotcha updated: [`gotchas/...`](gotchas/) — the *what to do about it* rule, edited in the same commit as the fix.
- Drill written/updated: [`drills/...`](drills/) — the evidence, if a drill reproduced or verified the fix.

---

## Worked example (stub) — M3 Alertmanager permission-denied page

*Filled from [`drills/2026-07-06-m3-pager-drill.md`](drills/2026-07-06-m3-pager-drill.md).
This is a SEV3 near-miss caught during a drill, shown to demonstrate the shape.*

| Field | Value |
| --- | --- |
| **Incident** | Alertmanager could not read its Discord webhook — first page would have been dropped |
| **Date** | 2026-07-06 |
| **Severity** | SEV3 (caught in the M3 pager drill, before any real incident relied on it) |
| **Detected by** | the drill itself — a synthetic test alert, not a real outage |
| **Evidence** | [`drills/2026-07-06-m3-pager-drill.md`](drills/2026-07-06-m3-pager-drill.md); Alertmanager log `read webhook_url_file: … permission denied` |

**Summary.** Alertmanager started clean, parsed its config, and reported healthy —
but the very first notify failed with `permission denied` on
`/run/secrets/discord_webhook`. Had this been a real outage, the page would have
been silently dropped. Fixed by owning the secret to the container's runtime uid.

**Impact.** Zero user impact (drill). Blast radius *would* have been: every page
dropped until someone noticed the silence — the worst failure mode, because a
broken pager looks identical to a quiet, healthy one.

**Timeline (UTC).**

| T | Event |
| --- | --- |
| 10:01:46 | first synthetic alert fails to notify: `open /run/secrets/discord_webhook: permission denied` |
| ~10:05 | root cause found; `chown 65534:65534 && chmod 400` on the host file |
| 10:10:17 | synthetic alert #2 dispatched cleanly |

**Root cause (5 whys).**
1. The page didn't send. **Why?** Alertmanager got `permission denied` reading the webhook file.
2. **Why?** The file was `600 root:root`; Alertmanager runs as `nobody` (uid 65534).
3. **Why?** Compose file-secrets are bind mounts that keep *host* permissions; nothing translated host ownership to the container's runtime user.
4. **Why?** The config *loaded* fine, so a green healthcheck stood in for "it works" — the last hop (an actual notify) was never exercised at install time.
5. **Root cause:** a container-user vs host-file-ownership mismatch that fails at **use** time, not load time — and no invariant forced us to check the image's runtime uid before installing its secret.

**What went well.** The drill manufactured a real dispatch instead of trusting the
green healthcheck — which is the *only* reason this was found before it mattered.

**What went wrong / lucky.** We got lucky that a drill, not a real outage, was the
first thing to ever exercise the notify path.

**Action items.**

| # | Action | Owner | Priority | Done-when |
| --- | --- | --- | --- | --- |
| 1 | Document `install -o 65534 -g 65534 -m 400 …` at the secret definition | (ops) | P1 | comment landed in `stack/observability/compose.yml` ✅ |
| 2 | Generalize the rule: check an image's runtime uid before installing its secret | (ops) | P2 | entry in [`gotchas/alerting.md`](gotchas/alerting.md) ✅ |

**Lessons → learnings.** Another instance of
[`../learnings/concepts/observe-what-matters.md`](../learnings/concepts/observe-what-matters.md)
("a loaded config is not a working pipeline") and
[`../learnings/concepts/verify-the-artifact-under-test.md`](../learnings/concepts/verify-the-artifact-under-test.md).
Rule lives in [`gotchas/alerting.md`](gotchas/alerting.md); evidence in
[`drills/2026-07-06-m3-pager-drill.md`](drills/2026-07-06-m3-pager-drill.md).

*(Alternate worked stub worth writing up when it recurs: the x1 stale-mount
afternoon — a bind mount silently serving stale content after a redeploy.)*
