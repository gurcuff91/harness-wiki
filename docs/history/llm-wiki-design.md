> **Historical.** Written on 2026-11-15 when this feature was going to live INSIDE harness (as v0.98.0: Wiki* tools, a TUI icon, `/api/wiki` on the harness server). It was moved out into this project to keep harness small; the behaviour described — the store, the lock, the all-or-nothing ingest, queries without a lock — is what harness-wiki implements, but the package layout, the tools and the harness-specific sections no longer apply. Kept for the reasoning behind the decisions.

# LLM Wiki — design

**Status:** approved by the owner (2026-11-15) · **Breaking:** no (additive) · **Target:** v0.98.0

Source of the idea: Karpathy's "LLM Wiki" gist
(<https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f>). Instead of
RAG (rediscovering knowledge on every question), the LLM **incrementally builds
and maintains a persistent wiki** — interlinked markdown between the user and
the raw sources — so knowledge is compiled once and kept current. Three layers
(raw sources, the wiki, the schema = an `AGENTS.md`), three operations
(ingest, query, lint), two special files (`index.md`, `log.md`).

## Goal

Give Harness a native, multi-wiki version of that pattern: a **wiki store** that
manages several independent wikis, and tools (plus a small HTTP API) to create,
list, delete, ingest into, query and lint them. Each wiki is maintained by an
ephemeral sub-agent that runs *inside the wiki's folder*, so it reads that
wiki's own `AGENTS.md` and behaves as its maintainer.

## Decisions

1. **Scope:** a native Harness feature; one `WikiStore` manages many wikis.
2. **Modes:** `WikiIngest` and `WikiLint` are **always background**, no `timeout`.
   `WikiQuery` is **synchronous only**, with a long default `timeout`.
   `WikiInit` and `WikiDelete` are synchronous with a short `timeout`.
3. **Permissions:** Ingest, Query and Lint all **write** (no `save`/`fix`
   parameters — the maintainer owns the wiki). The sub-agent keeps
   Write/Edit/Bash but cannot launch sub-agents, manage schedules, recurse into
   `Wiki*`, write memory or call colleagues.
4. **Init:** the store writes a deterministic base template; the *tool* then runs
   a sub-agent that adapts the `AGENTS.md` to the wiki's description.
5. **Layering:** the store never invokes a model. AI lives only in the tools.
6. **Concurrency:** ingest, lint, init and delete take a per-wiki lock `<slug>/.lock`
   (own implementation, heartbeat, Windows-compatible; `internal/filelock` is
   untouched). A wiki busy with one of them fails **another** at once with "wiki is busy"; **queries are not subject to the lock** (see the Query row below).
7. **Slug:** strict and portable.
8. **Sources:** paths, globs (including `**`), whole directories, and inline
   `name` + `content`. Everything ends up as files under `raw/`.
9. **Clashes in `raw/`:** all-or-nothing; identical files count as already ingested.
10. **Interrupted operation:** a `.pending` marker for Ingest/Lint. It only warns,
    it never blocks.
11. **`WikiList`:** slug, description and a live-computed status.
12. **Manifest:** every wiki carries its own `wiki.json`, the only source of truth.
    There is **no** `index.json`.
13. **Activation:** a store's presence enables the feature (like `agent/memory`).
    The harness binary has **no setting** for it: CLI, TUI, server and transports
    always use `AgentWithWiki` with the filesystem store at `~/.harness/wiki/`,
    created lazily (nothing on disk until the first `WikiInit`). Only SDK users can
    pass another store or path. The new on-disk state `~/.harness/wiki/` is
    **approved** (AGENTS.md's persistent-state rule is updated accordingly).
14. **HTTP API:** list, create, delete, ingest, lint, query. A background job is
    followed through `GET /api/wiki` and its `status` (no result report, no
    events — kept deliberately small).

## 1. Architecture and layers

```
agent/wiki/                 PUBLIC (SDK) — no AI, never imports agent/, server or providers
  store.go                  Store (port): Init, List, Get, Delete, AddSources, operation control
  fs.go                     FSStore: the filesystem implementation
  slug.go                   slug + relative-path validation (portable, anti-traversal)
  lock.go                   own lock with heartbeat
  ingest.go                 source resolution: path, glob (**), directory, name+content
  template.go               the base AGENTS.md template

agent/tools/wiki.go         the six tools; AI enters through an injected executor
agent/agent.go              AgentOptions.Wiki; the executor (sub-agent with cwd = the wiki)
harness.go                  AgentWithWiki(store)
server/                     the HTTP endpoints (section 6)
internal/cli                always-on, fixed path
```

`agent/wiki` follows `agent/memory`'s shape: a port with a default
implementation, injected per agent, owned and closed by the agent. The tools take
the store and a `WikiExecutor` function (like `SubagentExecutor`); they never
build agents. `agent.go` builds the executor: a sub-agent whose
`ResourceLoader` is a **new** `FileResourceLoader` rooted at the wiki folder
(`loader.Copy()` would keep the parent's cwd, which is *not* what we want).

## 2. On-disk layout and operation lifecycle

```
~/.harness/wiki/<slug>/
  wiki.json        manifest {slug, description, created_at} — written LAST by Init
  AGENTS.md        the schema; the maintainer reads it because the wiki is its cwd
  raw/             immutable sources
  wiki/
    index.md       catalogue (seeded by Init, maintained by the sub-agent)
    log.md         chronological, entries prefixed `## [YYYY-MM-DD] op | title`
  .lock            exists only while an operation runs
  .pending         exists only if an Ingest/Lint did not finish cleanly
```

**`.lock`:** JSON `{op, started_at, token}`, created with `O_CREATE|O_EXCL` and
closed immediately (never held open — Windows cannot delete or replace an open
file). The owner renews its mtime with `os.Chtimes` every 5 s; a lock not renewed
for 30 s is abandoned and is reclaimed by an atomic rename-away before a new one
is created. Only the holder of the token releases it.

**`.pending`:** JSON `{op, started_at}`. Only Ingest and Lint write it. It never
blocks anything.

| Operation | Steps |
|---|---|
| **Init** (sync) | validate slug; create the folders; write `AGENTS.md` from the template; seed `index.md` and `log.md`; write `wiki.json` last. Then the tool runs the adapting sub-agent. If that fails the wiki already exists with the base template. |
| **Delete** (sync) | take the lock; delete `wiki.json` first (it vanishes from `WikiList`), then the rest. |
| **Ingest** (background) | take the lock, write `.pending`; copy sources to `raw/` (all-or-nothing). A clash returns the error at once, releases the lock and leaves no `.pending`. Otherwise launch the sub-agent with cwd = the wiki and return the ingested files. On success the goroutine removes `.pending` and releases the lock; on failure or cancellation it releases the lock and leaves `.pending`. |
| **Lint** (background) | like Ingest without the source copy. |
| **Query** (sync) | **takes no lock** (amended 2026-11-15, owner's decision): open a read-only `View`, run the sub-agent with the `timeout`. Any number of queries run at once, even during an ingest or a lint; what a query writes relates to its own question. It never makes a wiki busy, never leaves a `.pending`, and is still told when an earlier ingest/lint was interrupted. |

When a `.pending` exists at the start of an operation the prompt tells the
maintainer: the previous operation was interrupted — verify coherence first.

**`WikiList`** walks the folders and reads each `wiki.json` (no manifest, or an
unreadable one, means the folder is ignored). Status is computed live — never
stored: `busy` if a lock with a fresh heartbeat exists, `incomplete` if a
`.pending` exists and no live lock does, `ready` otherwise; `op` and `since` are
included when they apply. `busy` wins over `incomplete` (a normal Ingest has both
files). If a process dies mid-Ingest the wiki shows `busy` for ~30 s and then
becomes `incomplete` by itself.

**Identity** is the folder name: if a copied wiki was renamed, the folder wins
over the slug inside `wiki.json`.

## 3. Tools and maintainer

| Tool | Parameters | Mode | Returns |
|---|---|---|---|
| `WikiInit` | `slug`, `description`, `timeout` | sync, default 120 s | confirmation; if the AI adaptation fails: "created with the base template, adaptation pending" |
| `WikiList` | — | sync | per wiki: `slug`, `description`, `status`, plus `op`/`since` when applicable |
| `WikiDelete` | `slug` | sync | confirmation; "wiki is busy" if an operation is running |
| `WikiIngest` | `slug`, `sources[]` (path, glob or directory), `files[]` (`{name, content}`), optional `guidance` | **background only** | files ingested (paths relative to `raw/`), already-ingested ones, and the result file path |
| `WikiQuery` | `slug`, `question`, `timeout` | **sync only**, default 300 s | the maintainer's answer |
| `WikiLint` | `slug`, optional `guidance` | **background only** | the result file path |

`guidance` is free text steering the maintainer for that run; it does not change
the wiki's rules. Ingest needs at least one source in `sources` or `files`.

**The maintainer** is an ephemeral sub-agent with `cwd` = the wiki folder.
Blocked tools: `Subagent`, `Schedule*`, `Wiki*` (no recursion), `MemoWrite`,
`MemoDelete`, `ColleagueAsk`, `ColleagueList`. It keeps `Bash`, `Read`, `Write`,
`Edit`, `Fetch` and `WebSearch`. Iteration budgets: Ingest and Lint 100, Query 40,
Init 15.

**Common system prompt:** you are the maintainer of the wiki in this directory;
read `AGENTS.md` first and its conventions win; `raw/` is immutable (read, never
modify); `wiki/` is yours, and you keep `index.md` and `log.md` up to date on every
operation; close with a short report of what you changed.

**Per-operation prompts:** *Ingest* — the new and already-ingested files plus
`guidance`; read each source, summarise it, update entity and concept pages, the
index and the log. *Query* — search `index.md` first, read the relevant pages,
answer with citations, and file the answer as a new page when it has lasting
value. *Lint* — look for contradictions, stale claims, orphan pages, concepts
without a page and broken references, and fix what you can. *Init* — adapt
`AGENTS.md` to the description (page types, tags, domain conventions) and touch
nothing else.

**Errors the model receives:** `wiki is busy` (with the operation and elapsed time,
and "try again later"), `wiki not found`, `invalid slug` (with the valid shape),
`conflicts in raw/` (the differing files that already exist; nothing is copied).

## 4. Errors and security

**Everything that comes from outside is validated before the disk is touched.**

*Slug:* `^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`, minus the reserved Windows names
(`con prn aux nul com1–com9 lpt1–lpt9`). No dots or slashes, so `../x` is
impossible by construction.

*`name` of `files[]`* — the most obvious escape hatch — validated segment by
segment, splitting on `/` and `\`. Rejected: absolute paths, drive letters and UNC
paths; empty segments, `.` and `..`; the characters `< > : " | ? *` and control
characters; segments ending in a dot or space; reserved Windows names **also with an
extension** (`con.txt`); segments over 255 characters, or a relative name over 200
characters in total (proposed, to keep margin under Windows' classic 260). A second
barrier checks with `filepath.Rel` that the destination stays inside `raw/`. Reports
always use `/`.

*Case collisions:* `A.md` and `a.md` are the same file on macOS and Windows; two
destinations in one ingest differing only in case are a clash.

*Filesystem sources:* a glob selects files only, through our own matcher (the
standard library has no `**`); a directory is copied whole, directory included.
**Symlinks:** a path the caller gives explicitly is resolved and copied; inside a
directory symlinks are **not followed** — they are skipped and reported as
`skipped: symlink`, so a link to `~/.ssh` cannot slip into a wiki. A source that
contains the wiki's own folder, or lies inside it, is rejected. Limits: more than
2000 files or 200 MB in one ingest is rejected (constants, easy to change), so a
mistaken `/` or `~` cannot fill the disk.

*All-or-nothing copy:* (1) plan without writing — resolve every source, validate
every destination, detect clashes; an existing file is identical if size and SHA-256
match (= already ingested), and a differing one fails the whole ingest with the full
list; (2) copy each file to a temp name in its folder and rename it into place,
never overwriting; (3) on a mid-way failure (full disk) remove what this call
created, so `raw/` is as it was. Before writing, `Lstat` confirms no segment of the
destination inside `raw/` is a symlink (covers a wiki copied from another machine).

*Store safety:* `Init` never overwrites (existing folder with a manifest →
`wiki already exists`; without one → asks to review or delete it by hand) and
cleans up what it created if it fails half way. `Delete` only removes a real wiki:
the path must sit directly under the store root, contain `wiki.json` and not be a
symlink. Symlinked folders in the store root are ignored by `WikiList`.

*The lock notices when it was lost:* the heartbeat compares the token on every
beat, covering a process suspended over 30 s (Ctrl+Z) whose lock another process
reclaimed; on waking it sees the token is no longer its own, cancels the operation
and leaves `.pending`. Reclaiming an expired lock renames it to a unique name first,
so two processes racing for it have exactly one winner. Releasing the lock retries a
few times (Windows may hold the file briefly); if it cannot, the heartbeat stops and
the lock expires by itself.

*Background failures:* the goroutine recovers a `panic`, releases the lock and
leaves `.pending`; the result file says `Error: …`. `Agent.Close` cancels running
wiki operations and waits a few seconds for them to release their lock, so a
normal shutdown does not leave a wiki `busy` for 30 s. A user `Stop` does not
affect a background Ingest/Lint, as with `Subagent`.

*Typed errors* in `agent/wiki`: `ErrBusy{Op, Since}`, `ErrNotFound`,
`ErrInvalidSlug`, `ErrExists`, `ConflictError{Paths}`; the tools turn them into
actionable messages, the API into status codes.

*Windows:* no `_windows.go`. Verification is `GOOS=windows go build ./...` and
`go vet` on every validation; tests use `t.TempDir` and assume nothing Unix-only;
symlink tests skip when `os.Symlink` fails (it needs privileges on Windows).

*Out of scope on purpose:* restricting which paths the model may read for ingest —
it can already read any path with `Read` and `Bash`, so a restriction here would be
theatre.

## 5. Tests, validation, documentation, version

**Version:** a new feature, **v0.98.0**, additive. The only cost for people who do
not use it: every request carries six more tool definitions.

**Tests by layer.**
- `agent/wiki` (no AI): slug valid/invalid (traversal, reserved names, case); `Init`
  (creates everything, manifest last, no overwrite, cleans up on failure); `List`
  (ignores no-manifest, unreadable and symlinked folders; `ready`/`busy`/`incomplete`);
  `Delete` (manifest first, refuses `busy`, refuses fake paths); the lock (`O_EXCL`,
  heartbeat, expiry, atomic reclaim between two processes, token loss); ingest (path,
  `**` glob, whole directory, `name`+`content`, all-or-nothing clash, identical =
  already ingested, symlinks skipped, a source containing the wiki, limits, case
  collision, cleanup on a mid-way failure).
- `agent/tools`: the six tools with a fake executor — parameter validation, typed
  error translation, background returns the path and releases the lock when done,
  `.pending` left on failure and removed on success, a recovered `panic`.
- `agent`: the real executor with a fake provider — the sub-agent runs with cwd in
  the wiki and receives its `AGENTS.md`; the blocked tools are absent; `Agent.Close`
  cancels and waits; without a store there are no tools and no prompt block.
- Server: every endpoint (section 6), including the status-code mapping.
- End to end: the real binary against a fake model — `Init` → `Ingest` (background) →
  `List` shows `busy` → finishes → `ready` → `Query` reads and cites → `Lint`; and a
  second process seeing `busy`.
- Root: `TestAgentWithWikiSetsTheStore` next to the other options' tests.

**Mutants with a control run.** The ones that matter most: the slug accepts `..`; the
manifest is written before the rest; delete follows symlinks; the confinement check is
skipped; `**` does not descend; an identical clash is treated as a conflict; the lock
does not check its token; reclaiming an expired lock is not atomic; `Query` leaves
`.pending`; `busy` does not win over `incomplete`.

**Validation:** `gofmt`, `go vet`, `git diff --check`, the whole suite with `-race` and
an isolated HOME, **plus `-count=2`** of the new packages (so no global state leaks),
**plus `GOOS=windows go build ./...` and `go vet`**.

**Documentation:** `CHANGELOG.md`; `AGENTS.md` (the persistent-state rule, the package
tree, the tool and API sections); `Makefile` version; the OpenAPI document
(`server/server_docs.go`) for the new endpoints.

**Order of work** — stages, each its own local commit: (1) `agent/wiki` slug, lock,
store and manifest; (2) ingest; (3) `agent/tools/wiki.go`; (4) `agent/agent.go` and
`AgentOptions.Wiki`; (5) `harness.go`, `internal/cli`, the HTTP API and the end to end
test; (6) mutants, docs, version. No push and no release without the owner's approval.

**Not included, on purpose:** search beyond `index.md` (a `qmd`-style engine — the gist
says the index is enough up to a few hundred pages); a CLI command (`harness wiki …`),
which can be added later without changing anything here; a store-path setting in the
binary (the owner's decision); a result report or event stream for background jobs.

## 6. HTTP API

| Endpoint | Equals | Mode | Success |
|---|---|---|---|
| `GET /api/wiki` | `WikiList` | sync | `200` — the list |
| `POST /api/wiki` | `WikiInit` | sync | `201` |
| `DELETE /api/wiki/{slug}` | `WikiDelete` | sync | `200` |
| `POST /api/wiki/{slug}/ingest` | `WikiIngest` | background | `202` |
| `POST /api/wiki/{slug}/lint` | `WikiLint` | background | `202` |
| `POST /api/wiki/{slug}/query` | `WikiQuery` | sync, with `timeout` | `200` — the answer |

`POST /api/wiki` (not in the first list, added by the owner) is what lets an
API-only client create a wiki at all; its body is `{slug, description}`.

**Bodies.** Create: `{slug, description}`. Ingest: `{sources[], files[{name, content}],
guidance}`. Lint: `{guidance}`. Query: `{question, timeout}` — `timeout` is in seconds
and bounded by a server-side maximum. Delete: no body.

**Errors** use the standard `{"error": {"message", "details"}}` shape: `400` invalid
slug or body; `404` no such wiki; `409` the wiki is busy (the operation and elapsed
time in `details`), already exists, or has clashes in `raw/` (the list in `details`);
`503` the wiki feature is not enabled on this agent (as memory is).

**Background jobs are followed with `GET /api/wiki`** and the wiki's `status`: `busy`
while it works, `ready` when it finished, `incomplete` if it was cut short. There is no
result report and no event stream. A `202` returns the ingested files and the status,
not a result file: a result path in the server's temp directory is meaningless to a
remote client.

**Decisions this surfaced, flagged for review:**
- *Paths in `sources[]` are paths on the **server**.* The API runs with the server's
  filesystem permissions, so a caller can make it copy any file the server can read into
  a wiki. The server has no authentication today and the tools already allow the same
  through `Read` and `Bash`, so this adds no new exposure, but it means the API is only
  as private as the port it listens on.
- *`query` is long-lived.* The server sets no write timeout, so a long Query holds the
  connection open until it answers or hits its `timeout`; the client must allow for it.
- *Wiki endpoints are server-level, not session-level:* no session is needed, and the
  maintainer sub-agent runs on the server's default model, the same model a new
  colleague conversation uses.
