# harness-wiki

An HTTP server for **LLM-maintained wikis**, after [Karpathy's "LLM Wiki"](https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f):
instead of re-deriving knowledge from your documents on every question (RAG), a model
**incrementally builds and maintains a persistent wiki** — interlinked markdown between you and
your sources — so knowledge is compiled once and kept current. harness-wiki manages **several**
wikis, each with its own schema, and is built on the [harness](https://github.com/gurcuff91/harness)
SDK, so any provider harness supports works.

```
harness-wiki serve [--host 127.0.0.1] [--port 8787] [--model provider/model]
```

## How it works

Each wiki is a folder, `./harness-wiki/<slug>/`, **in the directory you start the server in**:

```
harness-wiki/<slug>/
  wiki.json      the wiki's manifest (slug, description) — the only source of truth
  AGENTS.md      the wiki's schema: how its pages are organised and what to do on each operation
  raw/           your sources, immutable
  wiki/          the pages — index.md and log.md are kept by the maintainer
```

A **maintainer** — a harness sub-agent whose working directory is the wiki's folder — reads that
wiki's own `AGENTS.md` and maintains the wiki by its rules. Three operations:

| Operation | What it does | Mode |
|---|---|---|
| **ingest** | copies your sources into `raw/`, then the maintainer integrates them: a summary page per source, updated entity and concept pages, the index and the log | background (`202`) |
| **query** | the maintainer reads the index and the relevant pages and answers with references; a valuable answer is filed back as a new page | synchronous |
| **lint** | health-check and repair: contradictions, stale claims, orphan pages, missing links | background (`202`) |

## Install

Download the archive for your platform from the
[releases page](https://github.com/gurcuff91/harness-wiki/releases) (macOS, Linux and Windows;
`SHA256SUMS.txt` is alongside), or build it:

```bash
make install        # builds and copies to ~/go/bin/harness-wiki
```

## Providers

harness-wiki uses harness's own provider setup, unchanged. On a machine **with** harness it reads
`~/.harness` as harness does. On a machine **without** it, the same environment variables harness
uses activate a provider:

| Provider | Variable |
|---|---|
| Anthropic | `ANTHROPIC_API_KEY` |
| OpenAI | `OPENAI_API_KEY` |
| MiniMax | `MINIMAX_API_KEY` |
| Ollama Cloud | `OLLAMA_CLOUD_API_KEY` |
| OpenCode Go | `OPENCODE_GO_API_KEY` |
| Ollama (local) | `OLLAMA_URL` |

`claude-oauth` and `codex-oauth` are browser logins stored in `~/.harness/credentials.json` and have
no environment variable, so they are only available where harness is installed and logged in.

`--model provider/model` picks the model; without it the first model any active provider offers is
used. Any request can override it with a `"model"` field.

## The API

| | |
|---|---|
| `GET /api/wiki` | every wiki with its live status |
| `POST /api/wiki` | create a wiki → `201` |
| `DELETE /api/wiki/{slug}` | delete a wiki |
| `POST /api/wiki/{slug}/ingest` | ingest sources → `202` |
| `POST /api/wiki/{slug}/lint` | health-check and repair → `202` |
| `POST /api/wiki/{slug}/query` | ask a question |
| `GET /api/docs/openapi.json` | the OpenAPI document |
| `GET /api/health` | liveness |

```bash
curl -X POST localhost:8787/api/wiki -d '{"slug":"chrysler-sgw","description":"Research on the Chrysler SGW"}'

curl -X POST localhost:8787/api/wiki/chrysler-sgw/ingest -d '{
  "sources": ["/data/notes", "/data/papers/**/*.md"],
  "files":   [{"name": "ctx/data.md", "content": "..."}]
}'

curl localhost:8787/api/wiki                      # busy while it works, ready when it finishes
curl -X POST localhost:8787/api/wiki/chrysler-sgw/query -d '{"question":"how does the gateway authenticate?"}'
```

**Sources** are paths on the server — a file, a glob (`**` matches across directories), a whole
directory (copied with the directory included) — and/or inline `{name, content}` files, where `name`
may be a path (`ctx/data.md` becomes `raw/ctx/data.md`). The copy is **all or nothing**: a file that
already exists with different content refuses the whole ingest (`409`, listing the clashes), while an
identical one counts as already ingested. Symbolic links found inside a directory are skipped and
reported, never followed.

**Status** is computed on every call, never stored: `ready`; `busy` (an ingest, lint, create or
delete is running — `op` and `since` say which and since when); or `incomplete` (an ingest or lint was
interrupted, so the wiki may be half updated — run a lint, which is told to verify it first). A
background job has no result report: follow it with `GET /api/wiki`.

**Concurrency.** Ingest, lint, create and delete are one at a time per wiki, across processes (a lock
file with a heartbeat; another operation is refused at once with `409` and what is running). **Queries
take no lock**: any number run at once, even while an ingest is working on the wiki.

Errors are `{"error": {"message": "...", "details": {...}}}` with `400` invalid slug/body/source,
`404` no such wiki, `409` busy / already exists / clashes in `raw/`, `413` body over 64 MB, `504` a
query timed out.

## Security

**The API has no authentication.** By default it listens on `127.0.0.1` only. `--host 0.0.0.0`
publishes it, and the server then prints a warning: anyone who can reach the port can read and change
the wikis, make the server copy **any file it can read** into a wiki (sources are paths on the
server), and spend the model's tokens. Put it behind something that authenticates before exposing it.

The slug is `[a-z0-9-]` (1–64); inline file names are validated segment by segment (no `..`, absolute
paths, drive letters, Windows device names, forbidden characters) and re-checked to stay inside `raw/`.

## Go client

```go
import "github.com/gurcuff91/harness-wiki/client"

c := client.New("localhost:8787")
c.WikiCreate("sgw", "Chrysler SGW research")
c.WikiIngest("sgw", client.WikiSources{Files: []client.WikiFile{{Name: "a.md", Content: "..."}}})
answer, err := c.WikiQuery("sgw", "how does it authenticate?", 2*time.Minute)
if client.IsWikiBusy(err) { /* another operation holds the wiki */ }
```

## Layout

| Package | |
|---|---|
| `wiki` | the store: folders, the lock, the all-or-nothing ingest. Pure persistence — never calls a model, and a test enforces that it imports nothing from harness or this module |
| `maintainer` | the AI side: a harness sub-agent rooted at the wiki's folder |
| `service` | the rules of each operation, combining the two |
| `api` | HTTP, and nothing but HTTP |
| `client` | the typed Go client (no harness dependency) |
