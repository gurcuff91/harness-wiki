package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gurcuff91/harness-wiki/service"
	"github.com/gurcuff91/harness-wiki/wiki"
)

// tool is one MCP tool: its public definition and the function that runs it. Every tool is a thin
// adapter over a service method — it parses and checks its arguments, calls the service, and
// renders the result as text for the model.
type tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Execute     func(ctx context.Context, args json.RawMessage) (string, error)
}

// buildTools returns the six wiki tools over a service.
func buildTools(svc *service.Service) []tool {
	return []tool{
		{
			Name: "WikiList",
			Description: "List every wiki with its slug, description and live status. " +
				"Status is ready (free to use), busy (an ingest, a lint or a delete is working on it right now — op and since say what and since when) " +
				"or incomplete (an earlier ingest or lint was cut short; the next one verifies the wiki's coherence first). " +
				"WikiIngest and WikiLint run in the background, so this is how you follow them: call it again later and look for busy → ready.",
			Schema:  schema(nil),
			Execute: func(ctx context.Context, _ json.RawMessage) (string, error) { return runList(svc) },
		},
		{
			Name: "WikiCreate",
			Description: "Create a wiki: a persistent, interlinked markdown knowledge base that a model builds and keeps current from the sources you ingest into it. " +
				"The description says what the wiki is about; a maintainer then adapts the wiki's rules (its AGENTS.md) to that subject, which can take up to a couple of minutes. " +
				"The wiki exists and is usable even if that adaptation fails — the reply says whether it worked.",
			Schema: schema(map[string]prop{
				"slug":        str("Lowercase name of the wiki: letters, digits and hyphens (e.g. \"rust-notes\"). It is the id every other tool takes."),
				"description": str("What the wiki is about — its subject and scope. The maintainer shapes the wiki's structure from this."),
				"model":       modelProp,
			}, "slug", "description"),
			Execute: func(ctx context.Context, args json.RawMessage) (string, error) { return runCreate(ctx, svc, args) },
		},
		{
			Name: "WikiDelete",
			Description: "Permanently delete a wiki and everything in it — its sources, its pages, its history. This cannot be undone. " +
				"Refused while an ingest, a lint or another delete is working on the wiki.",
			Schema:  schema(map[string]prop{"slug": slugProp}, "slug"),
			Execute: func(ctx context.Context, args json.RawMessage) (string, error) { return runDelete(svc, args) },
		},
		{
			Name: "WikiIngest",
			Description: "Add sources to a wiki and have its maintainer weave them into the wiki's pages. " +
				"Give 'sources' (file paths, globs with ** or directories ON THE SERVER's filesystem) and/or 'files' (inline name + content, written to the wiki's raw/ folder; the name may contain \"/\" for subfolders). " +
				"The sources are copied in immediately and the reply lists them; the maintainer then works IN THE BACKGROUND, so this returns at once — call WikiList to see busy turn back to ready. " +
				"It is all-or-nothing: a file that already exists with different content, a bad name or a missing source refuses the whole ingest and changes nothing. " +
				"A source whose content was already ingested is reported as already ingested and not repeated. Refused while the wiki is busy.",
			Schema: schema(map[string]prop{
				"slug":    slugProp,
				"sources": arr("Paths on the server's filesystem: files, globs (** crosses directories) or directories (copied whole).", prop{Type: "string"}),
				"files": arr("Inline sources, written as files under the wiki's raw/ folder.", prop{
					Type: "object",
					Properties: map[string]prop{
						"name":    str("File name inside raw/, e.g. \"notes/2025-01-meeting.md\"."),
						"content": str("The file's text."),
					},
					Required: []string{"name", "content"},
				}),
				"guidance": str("Optional steer for the maintainer on how to treat these sources (what matters, what to skip, where it fits)."),
				"model":    modelProp,
			}, "slug"),
			Execute: func(ctx context.Context, args json.RawMessage) (string, error) { return runIngest(svc, args) },
		},
		{
			Name: "WikiQuery",
			Description: "Ask a wiki a question and get an answer grounded in its pages. It waits for the answer, which can take from several seconds to a few minutes. " +
				"It takes no lock: any number of queries can run at once, even while an ingest or a lint is working on the wiki (then it may see pages still being updated). " +
				"If the wiki is incomplete (an earlier job was cut short) the answer says so.",
			Schema: schema(map[string]prop{
				"slug":     slugProp,
				"question": str("The question, self-contained."),
				"timeout":  {Type: "integer", Description: "Seconds to wait for the answer (default 300, at most 1800). Past it the call fails and any partial answer is discarded."},
				"model":    modelProp,
			}, "slug", "question"),
			Execute: func(ctx context.Context, args json.RawMessage) (string, error) { return runQuery(ctx, svc, args) },
		},
		{
			Name: "WikiLint",
			Description: "Health-check a wiki and repair it: broken links, orphan pages, contradictions, stale claims, gaps. " +
				"It runs IN THE BACKGROUND and returns at once — call WikiList to see busy turn back to ready. Refused while the wiki is busy.",
			Schema: schema(map[string]prop{
				"slug":     slugProp,
				"guidance": str("Optional steer: what to look at first or to leave alone."),
				"model":    modelProp,
			}, "slug"),
			Execute: func(ctx context.Context, args json.RawMessage) (string, error) { return runLint(svc, args) },
		},
	}
}

// ── schema ──────────────────────────────────────────────────────────────────

// prop is one JSON-schema property.
type prop struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	// Items is the element schema of an array.
	Items *prop `json:"items,omitempty"`
	// Properties and Required describe an object.
	Properties map[string]prop `json:"properties,omitempty"`
	Required   []string        `json:"required,omitempty"`
}

var (
	slugProp  = str("The wiki's slug, as WikiList shows it.")
	modelProp = str("Optional model for this operation as provider/model (e.g. anthropic/claude-sonnet-4-5); the server's default when omitted.")
)

func str(desc string) prop { return prop{Type: "string", Description: desc} }

func arr(desc string, items prop) prop { return prop{Type: "array", Description: desc, Items: &items} }

// schema builds an object input schema.
func schema(props map[string]prop, required ...string) json.RawMessage {
	if props == nil {
		props = map[string]prop{}
	}
	obj := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		obj["required"] = required
	}
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err) // a static value: it cannot fail
	}
	return b
}

// ── arguments ───────────────────────────────────────────────────────────────

// parse decodes a tool's arguments into v. Unknown fields are ignored: a model that adds one is
// not worth failing a call over, and the required ones are checked by the tool.
func parse(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return invalid("arguments: %v", err)
	}
	return nil
}

// need returns an error naming the first blank required field.
func need(fields ...[2]string) error {
	for _, f := range fields {
		if strings.TrimSpace(f[1]) == "" {
			return invalid("%q is required", f[0])
		}
	}
	return nil
}

// ── tools ───────────────────────────────────────────────────────────────────

func runList(svc *service.Service) (string, error) {
	list, err := svc.List()
	if err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func runCreate(ctx context.Context, svc *service.Service, args json.RawMessage) (string, error) {
	var a struct {
		Slug        string `json:"slug"`
		Description string `json:"description"`
		Model       string `json:"model"`
	}
	if err := parse(args, &a); err != nil {
		return "", err
	}
	if err := need([2]string{"slug", a.Slug}, [2]string{"description", a.Description}); err != nil {
		return "", err
	}
	c, err := svc.Create(ctx, a.Slug, a.Description, a.Model)
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Wiki %q created and ready.", c.Info.Slug)
	if c.Adapted {
		return msg + " Its rules were adapted to the description.", nil
	}
	return msg + " Its rules could NOT be adapted to the description (" + c.AdaptedError + "), so it uses the base rules; it is fully usable.", nil
}

func runDelete(svc *service.Service, args json.RawMessage) (string, error) {
	var a struct {
		Slug string `json:"slug"`
	}
	if err := parse(args, &a); err != nil {
		return "", err
	}
	if err := need([2]string{"slug", a.Slug}); err != nil {
		return "", err
	}
	if err := svc.Delete(a.Slug); err != nil {
		return "", err
	}
	return fmt.Sprintf("Wiki %q deleted.", a.Slug), nil
}

func runIngest(svc *service.Service, args json.RawMessage) (string, error) {
	var a struct {
		Slug     string      `json:"slug"`
		Sources  []string    `json:"sources"`
		Files    []wiki.File `json:"files"`
		Guidance string      `json:"guidance"`
		Model    string      `json:"model"`
	}
	if err := parse(args, &a); err != nil {
		return "", err
	}
	if err := need([2]string{"slug", a.Slug}); err != nil {
		return "", err
	}
	acc, err := svc.Ingest(a.Slug, wiki.Sources{Paths: a.Sources, Files: a.Files}, a.Guidance, a.Model)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Ingest started on wiki %q; the maintainer is working in the background.\n", a.Slug)
	listLine(&b, "ingested", acc.Ingest.Ingested)
	listLine(&b, "already ingested (not repeated)", acc.Ingest.AlreadyIngested)
	listLine(&b, "skipped", acc.Ingest.Skipped)
	if acc.Interrupted {
		b.WriteString("An earlier job on this wiki had been cut short; the maintainer verifies its coherence first.\n")
	}
	b.WriteString(statusLine(svc, a.Slug))
	return b.String(), nil
}

func runLint(svc *service.Service, args json.RawMessage) (string, error) {
	var a struct {
		Slug     string `json:"slug"`
		Guidance string `json:"guidance"`
		Model    string `json:"model"`
	}
	if err := parse(args, &a); err != nil {
		return "", err
	}
	if err := need([2]string{"slug", a.Slug}); err != nil {
		return "", err
	}
	acc, err := svc.Lint(a.Slug, a.Guidance, a.Model)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Lint started on wiki %q; the maintainer is working in the background.\n", a.Slug)
	if acc.Interrupted {
		b.WriteString("An earlier job on this wiki had been cut short; the maintainer verifies its coherence first.\n")
	}
	b.WriteString(statusLine(svc, a.Slug))
	return b.String(), nil
}

func runQuery(ctx context.Context, svc *service.Service, args json.RawMessage) (string, error) {
	var a struct {
		Slug     string  `json:"slug"`
		Question string  `json:"question"`
		Timeout  float64 `json:"timeout"`
		Model    string  `json:"model"`
	}
	if err := parse(args, &a); err != nil {
		return "", err
	}
	if err := need([2]string{"slug", a.Slug}, [2]string{"question", a.Question}); err != nil {
		return "", err
	}
	// The service caps the timeout at service.MaxTimeout (30 min) — the cap the REST API has too.
	return svc.Query(ctx, a.Slug, a.Question, timeoutOf(a.Timeout), a.Model)
}

// ── rendering ───────────────────────────────────────────────────────────────

// listLine writes "label: a, b, c" when there is something to say.
func listLine(b *strings.Builder, label string, items []string) {
	if len(items) > 0 {
		fmt.Fprintf(b, "%s: %s\n", label, strings.Join(items, ", "))
	}
}

// statusLine is the wiki's status right after a background job was started: "busy" at once, so
// the model knows to follow it through WikiList.
func statusLine(svc *service.Service, slug string) string {
	info, err := svc.Store.Get(slug)
	if err != nil {
		return "Call WikiList to follow it.\n"
	}
	return fmt.Sprintf("Status: %s (%s). Call WikiList to see it turn back to ready.\n", info.Status, info.Op)
}
