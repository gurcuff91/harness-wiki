> **Historical.** Written on 2026-11-15 when this feature was going to live INSIDE harness (as v0.98.0: Wiki* tools, a TUI icon, `/api/wiki` on the harness server). It was moved out into this project to keep harness small; the behaviour described — the store, the lock, the all-or-nothing ingest, queries without a lock — is what harness-wiki implements, but the package layout, the tools and the harness-specific sections no longer apply. Kept for the reasoning behind the decisions.

# LLM Wiki — implementation plan

Design: [`2026-11-15-llm-wiki-design.md`](2026-11-15-llm-wiki-design.md) (approved).
Target: **v0.98.0**. Six stages, **one local commit each**, no push and no release
without the owner's approval.

Every stage ends green on: `gofmt -l .` (empty), `go vet ./...`, `git diff --check`,
`GOOS=windows go build ./... && GOOS=windows go vet ./agent/wiki/...`, and its own
tests with `-race -count=2` (the new packages must not leak process-global state — see
AGENTS.md). The full suite runs at stage 6 and again before the release, with an
isolated HOME.

## Hooks into the existing code (verified against the tree)

| Piece | Where | Pattern copied from |
|---|---|---|
| Option + field | `agent.AgentOptions` (`agent/agent.go`) next to `Memory` | `Memory`/`sharedMemory`, `ownsMemory` |
| Ownership and close | `Agent.Close()` (`agent.go:607`) | the `memStore` block |
| Tool registration | `buildSessionTools` (`agent.go`), after the Memo* block | `if a.memStore != nil { … isToolAllowed … }` |
| Maintainer executor | next to the `Subagent` executor (`agent.go` ~l.1420) | `New(AgentOptions{…})` + `Subscribe` + `awaitSubagentResult` |
| Timeout translation | `isTimeout` (`agent/tools/timeout.go`) | `Subagent` |
| Background pattern | `runSubagentBackground` (`agent/tools/subagent.go`) | context.Background(), result temp file |
| Facade option | `harness.go` `AgentWithMemory` | one-line option + `TestAgentWithMemorySetsTheStore` |
| CLI default | `internal/cli/agent.go` `defaultMemory()` | untyped nil on failure |
| HTTP | `server/server.go` router + `handleListMemories` | `s.agent.Memory()`, 503 when absent |
| Tool-block list | `buildFetchSummarizer` `DisallowedTools` and `subagentDisallowedTools` | add the six `Wiki*` names |

Two facts that shaped the plan: (1) `Subagent`'s executor uses `loader.Copy()`, which
**keeps the parent's cwd**, so the maintainer needs its **own** `FileResourceLoader`
rooted at the wiki folder; (2) `FileResourceLoader.findAgentsMD` walks **up** 3 levels,
so a wiki without its own `AGENTS.md` would inherit a parent's — harmless because
`Init` always writes one.

---

## Stage 1 — `agent/wiki`: slug, errors, lock, manifest, store

**Files (new):** `agent/wiki/{wiki.go,slug.go,errors.go,lock.go,fs.go,template.go}` +
tests. No import of `agent/`, `server`, `internal/providers` — verified by a test that
fails if `go list -deps` of the package contains any of them.

1. **`errors.go`** — `ErrBusy{Op string; Since time.Time}`, `ErrNotFound`,
   `ErrInvalidSlug`, `ErrExists`, `ConflictError{Paths []string}`, each with `Error()`
   and `errors.Is`/`As` support.
2. **`slug.go`** — `ValidSlug(string) error` (regex
   `^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$` + the reserved Windows names, which are
   checked with and without extension for path segments) and
   `ValidRelPath(name string) (clean string, err error)` for `files[].name`:
   split on `/` and `\`; reject absolute, drive letters, UNC, empty/`.`/`..` segments,
   `< > : " | ? *`, control chars, trailing dot/space, reserved names even with an
   extension, segment > 255, whole name > 200; return the `/`-joined clean path.
3. **`wiki.go`** — the port:
   ```go
   type Store interface {
       Init(slug, description string) (Info, error)
       List() ([]Info, error)            // status computed live
       Get(slug string) (Info, error)
       Delete(slug string) error         // takes the lock itself
       Dir(slug string) (string, error)  // absolute folder of an existing wiki
       Begin(slug, op string) (Op, error)// takes the lock; Ingest/Lint also write .pending
       AddSources(slug string, in Sources) (IngestResult, error) // stage 2
       Close() error
   }
   type Op interface { Pending() *PendingInfo; Done(ok bool); Context() context.Context }
   type Info struct { Slug, Description string; CreatedAt time.Time; Status Status; Op string; Since time.Time }
   ```
   `Op.Done(true)` removes `.pending` and releases the lock; `Done(false)` releases the
   lock and leaves `.pending`. `Op.Context()` is cancelled when the heartbeat sees the
   token was lost.
4. **`lock.go`** — own lock, never holds the file open: create with
   `O_CREATE|O_EXCL|O_WRONLY`, write `{op, started_at, token}`, **close at once**.
   Heartbeat goroutine `os.Chtimes` every `heartbeatEvery` (5 s) and re-reads the token
   each beat (lost ⇒ cancel the op's context). A lock whose mtime is older than
   `staleAfter` (30 s) is reclaimed by `os.Rename` to a unique name, then removed, then
   a fresh create — one winner among racing processes. `release` verifies the token and
   retries removal a few times with short sleeps (Windows). Both durations are package
   variables so tests can shrink them.
5. **`fs.go`** — `FSStore` (`OpenFS(root string)`; `""` = `~/.harness/wiki`, **no disk
   access until the first `Init`**):
   - `Init`: validate; refuse `ErrExists` if the folder has a manifest, and a distinct
     error if it exists without one; `os.Mkdir` the slug dir (non-recursive, so an
     existing folder fails); `raw/`, `wiki/`; `AGENTS.md` from `template.go`;
     `wiki/index.md` and `wiki/log.md` seeds; **`wiki.json` last** (write to a temp name
     then rename); on any failure remove what this call created.
   - `List`: `os.ReadDir(root)`; skip non-dirs, symlinks (`Lstat`), and folders without
     a readable `wiki.json`; identity = folder name; status per design (busy wins over
     incomplete; `Since` from the lock/pending files).
   - `Delete`: `Begin`-style lock; verify direct child of root, has manifest, not a
     symlink; remove `wiki.json` first, then `RemoveAll`.
6. **`template.go`** — the base `AGENTS.md` (the gist's conventions: layers, ingest /
   query / lint workflows, `index.md` catalogue format, `log.md` prefix
   `## [YYYY-MM-DD] op | title`), parameterised by slug and description, plus the two
   seed files.

**Tests:** slug table (valid; `..`, `a/b`, `A`, 65 chars, `con`, `aux`, `com1`, `-a`,
`a-`, empty); `ValidRelPath` table (all the rejections above + `ctx/data.md` ok);
`Init` creates the layout, writes the manifest last (a failure injected after `AGENTS.md`
leaves **no** manifest and the folder is removed), never overwrites, lazy root;
`List` ignores no-manifest / corrupt-manifest / symlinked / plain-file entries, orders
stably, shows `ready`/`busy`/`incomplete`; `Delete` removes the manifest first, refuses
a busy wiki, refuses a symlink and a non-wiki folder; lock: exclusive, heartbeat keeps
it alive past `staleAfter`, expiry is reclaimed, two goroutines racing a reclaim yield
one winner, a stolen token cancels the context, release with the wrong token is a no-op;
the no-forbidden-imports test. Symlink tests `t.Skip` when `os.Symlink` fails.

**Commit:** `wiki: store, slug, lock and manifest (no AI)`.

## Stage 2 — ingest: sources and the all-or-nothing copy

**Files:** `agent/wiki/{ingest.go,glob.go}` + tests.

1. **`glob.go`** — `matchGlob(pattern, path string) bool` with `*`, `?`, `[...]` per
   segment and `**` spanning segments; no dependency (not `filepath.Glob`).
2. **`ingest.go`** —
   ```go
   type Sources struct { Paths []string; Files []File }   // File{Name, Content string}
   type IngestResult struct { Ingested, AlreadyIngested, Skipped []string } // relative to raw/, "/"-separated
   ```
   `AddSources` (called by the tool **while holding the lock**):
   - **Plan (no writes):** expand each `Paths` entry — a plain file → `raw/<base>`; a
     glob → each matching file → `raw/<base>`; a directory → the whole tree under
     `raw/<dirname>/…`. Inside a directory symlinks are **skipped** and listed in
     `Skipped` as `<path>: symlink`; an explicit path that is itself a symlink is
     resolved. `Files` go through `ValidRelPath` and land at `raw/<name>`. Reject a
     source that contains, or lies inside, the wiki folder. Enforce `maxFiles = 2000`,
     `maxBytes = 200 << 20` (package constants). Detect destination collisions inside the
     plan, **case-insensitively**.
   - **Clashes:** for each destination that exists, identical = same size and SHA-256
     (⇒ `AlreadyIngested`), different ⇒ collected; any different ⇒ `*ConflictError` with
     the full list and **nothing is written**.
   - `Lstat` each existing segment of every destination under `raw/` — a symlink ⇒ error.
   - **Copy:** per file, write a temp name in the destination folder then `os.Rename`
     (never overwrite); `MkdirAll` the parents; track created files/dirs; on any failure
     remove what this call created.
3. `Sources` with neither `Paths` nor `Files` ⇒ a clear error.

**Tests:** single file, glob (`*.md`, `docs/**/*.md`, no match ⇒ error), whole directory
(directory name preserved, nested), `Files` with `ctx/data.md` (creates `raw/ctx/`),
identical re-ingest = `AlreadyIngested` and no change, a differing file = `ConflictError`
listing every clash and writing nothing (including files that did not clash), case
collision (`A.md`/`a.md`) is a clash, symlink inside a directory skipped and reported, a
source containing the wiki rejected, limits (2001 files; size via a lowered constant),
a failure injected mid-copy leaves `raw/` as it was, and `ValidRelPath` escapes
(`../x`, `/etc/passwd`, `C:\x`, `a/../../b`) never leave `raw/`.

**Commit:** `wiki: ingest — paths, globs, directories and inline files, all-or-nothing`.

## Stage 3 — `agent/tools/wiki.go`: the six tools

**Files:** `agent/tools/wiki.go`, `names.go` (six constants), tests with a fake store and
a fake executor. The tools package must not import `agent/wiki`; it gets small interfaces
(same trick as `MemoryStore`):

```go
type WikiStore interface { /* the methods the tools need, with tool-local DTOs */ }
type WikiExecutor func(ctx context.Context, req WikiRun) (string, error)
type WikiRun struct { Slug, Op, Prompt string; MaxIterations int; Pending bool }
```
`agent/agent.go` adapts `wiki.Store` to `WikiStore` (stage 4), exactly as
`memory.NewToolAdapter` does.

1. **Schemas and validation** per the design table; `requireFields`; whitespace-only
   rejected; `timeout` clamped (Init 120 s default, Query 300 s default, both capped).
2. **`WikiInit`** — `Init` then executor `adapt` with the timeout; an adaptation failure
   returns success text "created with the base template, adaptation pending" (the wiki
   exists), not an error.
3. **`WikiList`**, **`WikiDelete`** — direct.
4. **`WikiQuery`** — sync: `Begin` → executor under `context.WithTimeout` → `Done(true)`;
   a timeout returns the standard "Timeout after …" message and `Done(true)` (Query never
   leaves `.pending`, and `Begin("query")` does not write one).
5. **`WikiIngest`** — `Begin("ingest")` (⇒ `ErrBusy` immediately) → `AddSources`; on error
   `Done(false)` is **not** used: a clash/validation error releases the lock and removes
   the `.pending` it just wrote (add `Op.Abort()` for "nothing happened"); on success the
   tool launches the goroutine and returns the ingested list + the result file path. The
   goroutine runs with `context.Background()` **derived from the op's context** (so
   a lost lock cancels it), `recover()`s a panic, writes `Error: …` to the result file,
   and calls `Done(err == nil)`.
6. **`WikiLint`** — same without sources.
7. **Error translation** — `ErrBusy` ⇒ `wiki "x" is busy (ingest, 4m) — try again later, or check WikiList`;
   `ErrNotFound`, `ErrInvalidSlug` (states the valid shape), `*ConflictError` (lists paths).
8. **Prompts** — `buildWikiPrompt(op, guidance, ingested, already, pending)`; pending
   prepends the "previous operation was interrupted — verify coherence first" line.

**Tests:** schema/validation table; each tool against fakes; the busy error text;
background returns at once and the lock is held until the goroutine ends; `.pending`
present on a failed/panicking run and absent on success; a clash leaves no `.pending` and
no lock; Query timeout; the prompt includes `guidance` and the pending line; Init
adaptation failure is a success message.

**Commit:** `wiki: the six Wiki* tools over an injected store and executor`.

## Stage 4 — `agent/agent.go`: option, executor, lifecycle

1. **`AgentOptions.Wiki wiki.Store`** (nil = off) + `a.wikiStore`, `ownsWiki`; a
   `isNilStore`-style guard (typed nil). `Agent.Wiki()` accessor for the server.
2. **Register** the tools in `buildSessionTools` after the Memo* block:
   `if a.wikiStore != nil { … isToolAllowed(...) }`, each tool behind its own name.
3. **Maintainer executor** (`buildWikiExecutor`): `New(AgentOptions{…})` with
   `SystemPrompt` = the common maintainer prompt, `MaxIterations` per op (100/100/40/15),
   `Store: store.NewInMemoryStore()`, **`ResourceLoader: resources.NewFileResourceLoader(dir)`**
   (not `loader.Copy()`), `DisallowedTools` = `Subagent`, `Schedule*`, `Wiki*`,
   `MemoWrite`, `MemoDelete`, `ColleagueAsk`, `ColleagueList`; session `cwd` = `dir` (from
   `Store.Dir`); model = the parent session's current model (via `sessRef`, as `Subagent`).
   Reuse the subscribe/await logic; fold the maintainer's cost into the parent
   (`addDelegatedCost`), as `Subagent` does.
4. **Block recursion elsewhere:** add the six `Wiki*` names to `subagentDisallowedTools`
   and to `buildFetchSummarizer`'s list.
5. **`Agent.Close`:** cancel in-flight wiki operations and wait up to a few seconds for
   their locks to be released (a tracked `sync.WaitGroup` + a cancellable base context),
   then close the store when owned.
6. **System prompt:** a short `## Wiki` block when the store is present (what the tools
   are, that Ingest/Lint run in the background and `WikiList` shows the status), gated
   like `## Memory`.

**Tests:** option sets the store; no store ⇒ no tools and no prompt block; the maintainer
runs with `cwd` = the wiki and its system prompt contains that wiki's own `AGENTS.md`
(fake provider records the request); the blocked tools are absent from its registry; a
sub-agent of the main agent has no `Wiki*`; `Close` cancels a running background op and
returns with the lock released; the main agent's cost includes the maintainer's.

**Commit:** `wiki: AgentOptions.Wiki, the maintainer sub-agent and its lifecycle`.

## Stage 5 — facade, CLI, HTTP API, end to end

1. **`harness.go`:** `AgentWithWiki(s wiki.Store) AgentOption` + `TestAgentWithWikiSetsTheStore`.
2. **`internal/cli/agent.go`:** `defaultWiki() wiki.Store` (`wiki.OpenFS("")`, **untyped nil
   on failure**, **no setting**) wired into the three builders that already pass
   `Memory: defaultMemory()` — so CLI, TUI, server and every transport get it. Test: the
   untyped-nil rule (like `agent_test.go:76`) and that nothing is created on disk until
   `Init`.
3. **HTTP (`server/wiki.go` + routes in `server.go`):**
   - `GET /api/wiki` → `200` list; `POST /api/wiki` `{slug, description}` → `201`;
     `DELETE /api/wiki/{slug}` → `200`; `POST …/ingest` → `202`; `POST …/lint` → `202`;
     `POST …/query` `{question, timeout}` → `200`.
   - Handlers call the store/executor through the agent (`Agent.Wiki()` + an exported
     `Agent.WikiRun(...)` that wraps the executor with the model = the server's default),
     never duplicate logic. `503` when no store; status mapping per the design
     (`400` invalid slug/body, `404`, `409` busy/exists/clash with `details`).
   - Query `timeout` capped server-side (constant), default 300 s.
   - **OpenAPI** entries in `server/server_docs.go` (single quotes inside the raw string),
     including the `WikiInfo` schema and the status/`409` details.
   - `client/`: typed methods (`WikiList/WikiCreate/WikiDelete/WikiIngest/WikiLint/WikiQuery`)
     and `client.IsWikiBusy`, so SDK consumers don't hand-roll HTTP.
4. **End to end (real binary):** a fake OpenAI-compatible model; `Init` → `Ingest` with a
   directory + an inline file (background) → `GET /api/wiki` shows `busy` → completes →
   `ready`, files exist under `raw/` and the maintainer wrote pages and updated
   `index.md`/`log.md` → `Query` answers with a citation → `Lint`; a second `harness serve`
   process sharing the HOME sees `busy` and gets `409`; kill -9 mid-Ingest ⇒ `busy` then
   `incomplete`. Scripted in `/tmp` first, then the durable parts as Go tests.

**Tests:** every endpoint incl. the code mapping, `503` without a store, `409` details,
the `202` body, the query timeout cap, `-count=2`; the client methods against the real
server; the facade test; the CLI default.

**Commit:** `wiki: facade, CLI default, HTTP API and client`.

## Stage 6 — mutants, docs, version, final validation

1. **Mutants with a control run** (script like `/tmp/mut*.py`; uncompilable mutants are
   rewritten, not counted): slug accepts `..`; manifest written before the rest; `Delete`
   follows symlinks; confinement `Rel` check removed; `**` does not descend; an identical
   clash treated as a conflict; lock does not check its token; reclaim not atomic;
   `Query` writes `.pending`; `busy` does not win over `incomplete`; symlinks followed in a
   directory; case collision ignored; `Init` writes the manifest then fails to clean up;
   `Close` does not wait. **Every survivor is a test gap to close before moving on.**
2. **Docs:** `CHANGELOG.md` 0.98.0; `AGENTS.md` — the persistent-state rule gains
   `~/.harness/wiki/` (approved by the owner), the package tree gains `agent/wiki` and the
   tools/HTTP sections, the "Adding a New Tool" notes mention the `Wiki*` family;
   `Makefile` `VERSION=v0.98.0`.
3. **Validation:** `gofmt`, `go vet`, `git diff --check`, `GOOS=windows go build ./...`,
   the full suite `-race -count=1` with an isolated HOME (expect 30 `ok`: the 29 plus
   `agent/wiki`), and `-count=2` over `./agent/wiki/... ./agent/tools/... ./server/...`.
4. `make install`, commit, report to the owner and **wait for approval** before
   `make release && git push origin main && make release-push`, CI watch, memory entry and
   the English report for the jade-kaiban dev.

## Risks and how the plan answers them

| Risk | Answer |
|---|---|
| A lock stolen from a live but slow holder | heartbeat + token check each beat + cancel on loss (stage 1) |
| Windows cannot delete an open file | the lock file is created and closed at once; release retries (stage 1) |
| `**` glob missing in the stdlib | own matcher with tests (stage 2) |
| An ingest copying secrets through a symlink | symlinks inside directories are skipped and reported (stage 2) |
| A wiki left `busy` after a normal shutdown | `Agent.Close` cancels and waits (stage 4) |
| The maintainer reading the wrong `AGENTS.md` | its own loader rooted at the wiki, tested by inspecting the request (stage 4) |
| Global test state leaking (providers, ports) | `t.Cleanup` everywhere, `-count=2` on every stage |
| Feature size creeping | the design's "not included" list is a scope fence: no result reports, events, CLI command or search engine in this release |
