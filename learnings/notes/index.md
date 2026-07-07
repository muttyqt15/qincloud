---
title: Notes
slug: notes
type: index
status: stable
tags: [qincloud, notes]
created: 2026-07-07
updated: 2026-07-07
---

# Notes

The free-form corner of the vault. The [milestones](../README.md) are the
curated, ramps-from-plain-English build stories; **this** is where shorter,
less formal things live — a running observation, a "today I learned," a rough
idea not yet worth a full write-up.

Nothing here has to follow the milestone template. Drop in a Markdown file, give
it a `title` (and, if you care about ordering, a `created` / `updated` date),
and it appears on its own — in the **Recently updated** list, the Explorer
sidebar, search, and the graph — the next time the site is published. No index
table to edit by hand.

## How to add one

1. Create `learnings/notes/<slug>.md` (or anywhere under `learnings/`).
2. Write it. Frontmatter is optional; a `title` and a date make it read and
   sort nicely.
3. Publish: `scripts/deploy-notes.sh` from the repo root — it builds, rolls the
   site, and verifies it's live.

That's the whole loop. The curated milestone/concept tables on the home page
stay hand-picked on purpose; everything else the site surfaces for you.
