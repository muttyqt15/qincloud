import { PageLayout, SharedLayout } from "./quartz/cfg"
import * as Component from "./quartz/components"

// QinCloud Learnings — layout override.
//
// This file REPLACES Quartz's default quartz.layout.ts (the Dockerfile COPYs it
// over the cloned default). It is a faithful copy of that default with one
// addition: a self-updating "Recently updated" list in the right sidebar. The
// list sorts by each note's modified date (configuration.defaultDateType =
// "modified", sourced frontmatter → git → filesystem), so it refreshes on every
// build with zero manual curation — drop a Markdown file into learnings/ and it
// appears here, in the Explorer, in search, and in the graph on the next deploy.

// Reused across every content page (incl. the homepage). Sorting is the
// component default (byDateAndAlphabetical over the configured date type).
const recentlyUpdated = Component.RecentNotes({
  title: "Recently updated",
  limit: 8,
  showTags: false,
})

export const sharedPageComponents: SharedLayout = {
  head: Component.Head(),
  header: [],
  afterBody: [],
  // Drop Quartz's upstream GitHub/Discord footer links — this is a portfolio site.
  footer: Component.Footer({ links: {} }),
}

export const defaultContentPageLayout: PageLayout = {
  beforeBody: [
    Component.ConditionalRender({
      component: Component.Breadcrumbs(),
      condition: (page) => page.fileData.slug !== "index",
    }),
    Component.ArticleTitle(),
    Component.ContentMeta(),
    Component.TagList(),
  ],
  left: [
    Component.PageTitle(),
    Component.MobileOnly(Component.Spacer()),
    Component.Flex({
      components: [
        { Component: Component.Search(), grow: true },
        { Component: Component.Darkmode() },
        { Component: Component.ReaderMode() },
      ],
    }),
    Component.Explorer(),
  ],
  right: [
    recentlyUpdated,
    Component.Graph(),
    Component.DesktopOnly(Component.TableOfContents()),
    Component.Backlinks(),
  ],
}

export const defaultListPageLayout: PageLayout = {
  beforeBody: [Component.Breadcrumbs(), Component.ArticleTitle(), Component.ContentMeta()],
  left: [
    Component.PageTitle(),
    Component.MobileOnly(Component.Spacer()),
    Component.Flex({
      components: [
        { Component: Component.Search(), grow: true },
        { Component: Component.Darkmode() },
      ],
    }),
    Component.Explorer(),
  ],
  right: [recentlyUpdated],
}
