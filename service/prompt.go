package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/gurcuff91/harness-wiki/maintainer"
)

// interruptedNote opens the prompt of any operation on a wiki whose earlier ingest or lint did not
// finish cleanly (a pending marker with no live lock): the wiki may be half updated, so the
// maintainer verifies its coherence before doing anything else.
const interruptedNote = "NOTE: the previous %s on this wiki (started %s) did not finish cleanly — it was interrupted, so the wiki may be half updated. Before anything else, verify its coherence: check that index.md and log.md agree with the pages that exist, that every source in raw/ has a summary page, and repair what you find. Then do the work below.\n\n"

// PromptArgs are what a prompt is built from. Only the fields an operation uses are read.
type PromptArgs struct {
	Subject         string // init: the wiki's description
	Guidance        string // ingest, lint: free text steering this run; never changes the wiki's rules
	Question        string // query
	Ingested        []string
	AlreadyIngested []string
	Interrupted     bool
	PrevOp          string
	PrevSince       time.Time
}

// Prompt builds the maintainer's task for one operation.
func Prompt(op maintainer.Op, a PromptArgs) string {
	var b strings.Builder
	if a.Interrupted {
		fmt.Fprintf(&b, interruptedNote, a.PrevOp, a.PrevSince.Format("2006-01-02 15:04 MST"))
	}
	switch op {
	case maintainer.OpInit:
		fmt.Fprintf(&b, "This wiki was just created. Its AGENTS.md is a generic base schema. Adapt it to the wiki's subject, which is:\n\n%s\n\nEdit ONLY AGENTS.md: choose the kinds of page that make sense for this subject, the tags or categories to use, the naming conventions and anything domain-specific the maintainer should always do — keep the structure (raw/, wiki/, index.md, log.md, the three operations) and keep it concise. Do not create pages and do not touch raw/ or wiki/. Finish with one line saying what you changed.", a.Subject)
	case maintainer.OpIngest:
		b.WriteString("Ingest the new sources into this wiki. They are already in raw/ (paths relative to raw/):\n")
		for _, p := range a.Ingested {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
		if len(a.AlreadyIngested) > 0 {
			b.WriteString("These were already in raw/ with identical content and are NOT new — do not re-ingest them unless they have no summary page yet:\n")
			for _, p := range a.AlreadyIngested {
				fmt.Fprintf(&b, "  - %s\n", p)
			}
		}
		b.WriteString("\nFollow the Ingest workflow in AGENTS.md: read each new source, write its summary page, update the entity and concept pages it touches, note contradictions with earlier claims, update wiki/index.md and append an entry to wiki/log.md. Never modify raw/.")
		if strings.TrimSpace(a.Guidance) != "" {
			fmt.Fprintf(&b, "\n\nGuidance for this ingest: %s", a.Guidance)
		}
		b.WriteString("\n\nFinish with a short report of what you changed.")
	case maintainer.OpQuery:
		fmt.Fprintf(&b, "Answer this question from the wiki:\n\n%s\n\nFollow the Query workflow in AGENTS.md: read wiki/index.md first to find the relevant pages, read them, and answer from the wiki with references to the pages you used. Say plainly what the wiki does not cover. If the answer has lasting value, file it as a new page, link it, and update wiki/index.md and wiki/log.md. Never modify raw/.", a.Question)
	case maintainer.OpLint:
		b.WriteString("Health-check this wiki and repair it. Follow the Lint workflow in AGENTS.md: look for contradictions between pages, stale claims, orphan pages, concepts without a page, missing cross-references and broken links; fix what you can, report what you cannot, and append an entry to wiki/log.md. Never modify raw/.")
		if strings.TrimSpace(a.Guidance) != "" {
			fmt.Fprintf(&b, "\n\nGuidance for this lint: %s", a.Guidance)
		}
		b.WriteString("\n\nFinish with a short report of what you found and fixed.")
	}
	return b.String()
}
