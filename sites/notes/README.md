# sites/notes/ — the learnings vault as a website

This folder turns the [`../../learnings/`](../../learnings/) Obsidian vault into
the static site served at **notes.sparboard.com**. It contains no prose of its
own — it's the *build recipe*, not the content. The content is the Markdown in
`learnings/`; this folder compiles it.

## What it is (and why it's meta)

The site is itself **a QinCloud app, deployed by controld** — QinCloud hosts the
story of how QinCloud was built. That's not a gimmick: it's the tightest
possible proof that the deploy pipeline works, because publishing these very
notes exercises image-build → push → `controld` deploy → Caddy route → live TLS
every time they change.

## The pieces

| File | Role |
| --- | --- |
| `quartz.config.ts` | **Quartz** config. Quartz is a static-site generator built for Obsidian vaults — it understands `[[wikilinks]]`, backlinks, and the graph view, turning the vault into a linked website. This is why the notes' liberal `[[slug]]` linking survives into the published site. |
| `Dockerfile` | Multi-stage build: Quartz compiles the vault to static HTML, then it's copied into an nginx image. |
| `nginx.conf` | The tiny web server config that serves the built static files inside the container. |

## Mental model: content and presentation are separate

Hold this split and the folder makes sense:

- **`learnings/`** = the source of truth (Markdown, frontmatter, wikilinks).
  Authors write there and nowhere else.
- **`sites/notes/`** = the transform (Quartz → static HTML → nginx container).
  You touch this only to change *how* the vault is presented, never *what* it
  says.

Editing a note never means touching this folder. Editing this folder means
changing the site's look or build, not its content.

## How it's published

```sh
scripts/deploy-notes.sh    # build image → push → controld deploy/redeploy → verify live
```

That script is self-verifying (see [idempotent, self-verifying operations](../../learnings/concepts/idempotent-self-verifying-operations.md)): it
doesn't succeed until the new site actually answers over TLS. Because the vault
changed, run it after any `learnings/` edit — including the M7/M9 milestone
notes.

## How it interacts

- **Reads** [`../../learnings/`](../../learnings/) at build time.
- **Deployed by** [`../../controld/`](../../controld/) like any tenant app.
- **Routed by** [`../../stack/edge/`](../../stack/edge/) (Caddy) with auto-TLS at
  notes.sparboard.com.
- Built/pushed by [`../../scripts/deploy-notes.sh`](../../scripts/).

## SRE concept: eat your own dog food

The documentation site runs on the platform it documents. Every publish is a
live integration test of the deploy path — the surest way to know the pipeline
works is that the thing describing the pipeline was shipped by it.
