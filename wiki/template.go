package wiki

import (
	"fmt"
	"strings"
	"time"
)

// agentsTemplate is the base schema every new wiki starts with — the conventions
// of the "LLM Wiki" pattern, written for the sub-agent that maintains the wiki.
// It is deterministic (the store never invokes a model); the WikiInit tool then
// has a sub-agent adapt it to the wiki's subject. %[1]s is the slug and %[2]s the
// description.
const agentsTemplate = `# Wiki: %[1]s

%[2]s

You are the maintainer of this wiki. It is a persistent, compounding knowledge
base: knowledge is compiled into the wiki once and kept current, not re-derived
from the raw sources on every question. You write and maintain every page; the
user curates sources and asks questions. This file is the schema — follow it, and
improve it when you learn what works for this subject.

## Layers

- ` + "`raw/`" + ` — the sources (articles, papers, notes, data). **Immutable**: read them,
  never modify, rename or delete them.
- ` + "`wiki/`" + ` — the pages. **Yours entirely**: you create, update and reorganise them.
- ` + "`AGENTS.md`" + ` — this schema.

## Pages

Plain markdown, one topic per page, linked with relative links. Use these kinds as
a starting point and adapt them to the subject:

- **Source summaries** — one page per ingested source: what it says, what is new,
  what it contradicts.
- **Entity pages** — people, products, systems, places.
- **Concept pages** — ideas, techniques, terms.
- **Analyses** — comparisons, syntheses, answers worth keeping.

Cross-reference generously. A page nobody links to is a page nobody finds.

## Two special files

- ` + "`wiki/index.md`" + ` — the catalogue: every page, one line each, grouped by kind,
  with a link and a one-line summary. Update it on every operation. When answering a
  question, read it first to find the relevant pages.
- ` + "`wiki/log.md`" + ` — an append-only chronological record. Add one entry per
  operation, always starting with ` + "`## [YYYY-MM-DD] op | title`" + ` (for example
  ` + "`## [2026-11-15] ingest | Article title`" + `), followed by a short note of what
  changed. Never edit or delete earlier entries.

## Operations

**Ingest.** Read each new source in ` + "`raw/`" + `, then write its summary page, update
the entity and concept pages it touches (one source may touch ten or more pages),
note where it contradicts earlier claims, update ` + "`index.md`" + `, and append to
` + "`log.md`" + `.

**Query.** Search ` + "`index.md`" + ` first, read the relevant pages, and answer from the
wiki with references to the pages you used. If the answer has lasting value — a
comparison, an analysis, a connection — file it as a new page, link it, and update
the index and the log. Say plainly when the wiki does not cover something.

**Lint.** Health-check the wiki: contradictions between pages, claims newer sources
have superseded, orphan pages with no inbound links, concepts mentioned but without
a page, missing cross-references, broken links, and gaps a search could fill. Fix
what you can, report what you cannot, and log the pass.

## Conventions

- Dates are ISO (` + "`YYYY-MM-DD`" + `). Keep page names lowercase with hyphens.
- Cite the source (a path under ` + "`raw/`" + ` or another page) for every non-obvious claim.
- Prefer updating an existing page to creating a near-duplicate.
- Finish every operation with a short report of what you changed.
`

// seedIndex and seedLog are the two special files a new wiki starts with.
const seedIndex = `# Index

The catalogue of this wiki: every page, one line each, grouped by kind.

## Sources

## Entities

## Concepts

## Analyses
`

func seedLog(slug string, now time.Time) string {
	return fmt.Sprintf("# Log\n\nChronological record of this wiki. Append-only.\n\n## [%s] init | %s\n\nWiki created.\n",
		now.Format("2006-01-02"), slug)
}

// baseAgentsMD renders the base schema for a wiki.
func baseAgentsMD(slug, description string) string {
	return fmt.Sprintf(agentsTemplate, slug, strings.TrimSpace(description))
}
