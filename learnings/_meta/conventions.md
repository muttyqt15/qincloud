---
title: Vault Conventions
slug: conventions
type: meta
status: stable
tags: [qincloud, meta]
created: 2026-07-06
updated: 2026-07-06
---

# Vault Conventions

How this `learnings/` vault is structured, so it stays consistent as it grows
and so both humans and agents can navigate and extend it. This is an Obsidian
vault — open the `learnings/` folder as a vault and the `[[wikilinks]]` become
a navigable graph.

## The idea in one line

Every milestone we built teaches something that outlives it. **Milestone notes**
tell the story of one build (approach → deviation → fix → best-practice → why);
**concept notes** are the atomic, reusable principles those stories keep hitting.
Milestones link *into* concepts, so the concepts compound as more milestones land.

## Folder layout (max two levels deep)

```
learnings/
├── README.md            # the map (MOC): start here
├── milestones/          # one note per build — the narrative + the teaching
├── concepts/            # atomic, reusable principles — the compounding core
└── _meta/               # conventions, templates, agent guide
```

## Filenames & titles

- **Filenames are kebab-case slugs**: `m4-controld-deploy-engine.md`,
  `single-source-of-truth.md`. This keeps wikilinks clean (`[[single-source-of-truth]]`)
  and URL-safe. The slug is the identity; the frontmatter `title` is the display name.
- One idea per note. A concept note over ~700 words or with 3+ distinct H2s
  probably wants splitting.

## Frontmatter (every note)

```yaml
---
title: Human Readable Title
slug: kebab-case-matches-filename
type: milestone | concept | index | meta
milestone: M4            # milestone notes only
status: stable
difficulty: 1            # 1 = any reader … 5 = deep systems
tags: [qincloud, security, networking]
created: 2026-07-06
updated: 2026-07-06
related: ["[[slug-a]]", "[[slug-b]]"]
sources: ["commit 336da45", "runbooks/gotchas/caddy.md", "controld/internal/deploy/deploy.go"]
---
```

`sources` is provenance: where the claims come from (commits, code paths,
drill records) so a reader — or an agent — can verify rather than trust.

## Linking rules

- Link **liberally** with `[[slug]]`. A concept named in prose should be a link.
- Links are **bidirectional in spirit**: if a milestone teaches a concept, the
  concept note lists that milestone under "Where it showed up".
- Navigation flows through links, not folders. Any note is one `[[link]]` from
  anywhere.

## Tags

- `#qincloud` on everything.
- Domain: `#infra` `#security` `#observability` `#databases` `#networking`
  `#golang` `#reliability` `#devops`.
- `#principle` on concept notes.

## The teaching contract (why this vault is different)

Each note **ramps**: it opens in plain English an intelligent non-engineer can
follow (with an analogy), then descends to the systems level a practitioner can
operate from — commands, configs, `file:line`. Nobody is left out at the top;
nobody is starved at the bottom. See the templates in `_meta/templates/`.
