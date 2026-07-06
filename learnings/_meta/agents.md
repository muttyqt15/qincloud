---
title: Agent Guide
slug: agents
type: meta
status: stable
tags: [qincloud, meta]
created: 2026-07-06
updated: 2026-07-06
---

# Agent Guide

How an AI agent should **read** and **extend** this `learnings/` vault. Read
[[conventions]] first — it defines the folder layout, frontmatter, and linking
rules this guide assumes. This vault is two things: **milestone notes** (the
story of one build) and **concept notes** (the atomic, reusable principles those
builds keep hitting). Everything below is about moving between the two.

## 1. How to RETRIEVE

**Filter by frontmatter before reading bodies.** Every note carries structured
frontmatter — use it as an index, not decoration:

- `type` — `milestone` for a specific build, `concept` for a reusable principle,
  `index` for the README map, `meta` for this folder. Answering "how do we do X
  in general?" → start in `concepts/`. Answering "what happened when we built
  X?" → start in `milestones/`.
- `tags` — domain filter (`#infra`, `#security`, `#observability`,
  `#databases`, `#networking`, `#reliability`, `#devops`). Narrow to the domain
  before grepping bodies.
- `difficulty` — 1 (any reader) … 5 (deep systems). Match it to how deep the
  question needs to go; don't pull a difficulty-5 note to answer a framing
  question.

**Follow the wikilinks; treat concept notes as the reusable core.** A milestone
tells one story, but its `**Teaches:** [[concept]]` footer points to the
transferable lesson. When a question is general, hop from the milestone to the
concept and answer from there — the concept is written to transfer to any
project, the milestone is the evidence. Concepts list their instances under
"Where it showed up in QinCloud," so you can fan back out to every milestone that
hit a principle.

**Trust `sources` for provenance, and verify against it.** The `sources` field
(commits, `path/to/file`, runbook records) is where a claim came from. When a
claim is load-bearing for what you're about to do, open the source and confirm
it still holds — the vault is a pointer to ground truth, not a replacement for
it. Do not restate a note's claim as current fact without checking its source
when it matters.

## 2. How to EXTEND

When a new milestone lands, extend the vault in this order:

1. **Add the milestone note** from `_meta/templates/milestone.md`. Copy the
   template, fill the ramp (§1 plain English → §7 the transferable lesson), set
   frontmatter (`type: milestone`, `milestone: M?`, `difficulty`, `tags`,
   `sources`). Filename is a kebab slug: `m9-<short-name>.md`.

2. **Extract any new reusable lesson into a concept note.** If the milestone
   teaches a principle that isn't already a concept, create one from
   `_meta/templates/concept.md` — **atomic (one idea per note)**, kebab slug,
   `type: concept`, `#principle` tag. If the lesson is already a concept, don't
   duplicate it — link to the existing one.

3. **Backlink bidirectionally.** The milestone's `**Teaches:**` footer links to
   the concept; the concept's "Where it showed up in QinCloud" section links
   back to the milestone. Both directions, every time — a one-way link is a bug.

4. **Update the README map.** Add the new milestone (and any new concept) to
   `README.md` so the map stays complete. A note that isn't reachable from the
   map is lost.

## 3. Invariants (never violate)

- **Kebab-case slugs** for every filename, matching the frontmatter `slug`.
  Wikilinks reference the slug (`[[m4-controld-deploy-engine]]`).
- **Two folders deep, max.** `milestones/`, `concepts/`, `_meta/` (+ templates).
  No deeper nesting.
- **One idea per concept note.** Over ~700 words or 3+ distinct H2s → split it.
- **The plain-English → systems ramp.** Every substantive note opens so an
  intelligent non-engineer can follow (with an analogy), then descends to
  commands, configs, and `file:line`. Never start at the systems level.
- **Never invent a slug that has no file.** Every `[[wikilink]]` must resolve to
  an existing note. If you need to reference something not yet written, create
  the note (at least a stub) or don't link it — a dangling link is a broken map.

---
**See also:** [[conventions]] · `_meta/templates/`
