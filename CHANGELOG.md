# Changelog

All notable changes to this project will be documented in this file.

## [0.2.0] - 2026-11-16

### Added — `POST /mcp`: the wikis as MCP tools

A harness (or any MCP client) can now use the wikis directly, by pointing at one URL:

```json
"mcp": { "wiki": { "url": "http://127.0.0.1:8787/mcp", "timeout": 600000 } }
```

- **Six tools** — `WikiList`, `WikiCreate`, `WikiDelete`, `WikiIngest`, `WikiQuery`, `WikiLint` — each a
  thin adapter over the same service the REST API uses, so every rule is shared: ingest and lint run in
  the background and are followed with `WikiList`, queries take no lock, a busy wiki is refused, the
  ingest is all-or-nothing. A failed call is a tool result with `isError`, which the model can read and
  recover from, not a JSON-RPC error.
- **MCP Streamable HTTP, stateless.** One JSON-RPC message (or a batch) per `POST`, answered with one
  JSON reply; a notification gets `202` and no body; `GET`/`DELETE` are `405`. No session id and no SSE.
  Methods: `initialize` (protocol `2024-11-05`, the revision harness's client speaks), `ping`,
  `tools/list`, `tools/call`. A query stops when the client hangs up.
- **Set `timeout` on the client.** harness's MCP client applies the server's `timeout` (default 5 s) to
  every call, so a slow `WikiQuery`/`WikiCreate` is cut off unless it is raised — the README says so
  prominently. It is a client setting; the server cannot work around it.
- `/mcp` has the REST API's trust model: no authentication. It is documented in the OpenAPI document
  and the README.
- Tested against the real client: the suite drives the server with harness's own `mcp` package
  (`NewHTTPTransport` / `Client`), plus raw-protocol, concurrency (five queries at once during an
  ingest) and shutdown tests; 51 mutations of the new code, each killed.

## [0.1.1] - 2026-11-16

### Changed — the API's documentation endpoints now match harness's

0.1.0 served only the OpenAPI document, and probed liveness through a `GET /api/health` that harness
does not have. They now follow harness's API:

- **`GET /api/docs`** serves the API reference page — the same Scalar UI harness serves, rendering this
  server's own document. (It loads the Scalar bundle from a CDN, as harness's page does.)
- **`GET /api/docs/openapi.json`** now names the server it is served from: its `servers` entry is the
  address the request arrived on, so it is right behind any `--host`/`--port`, `--host 0.0.0.0`
  included. The document is titled "harness-wiki API" and tagged `wiki` / `server`.
- **`GET /api/server`** replaces `GET /api/health`. It has harness's shape — `name`, `version`, `url`,
  `cwd`, `pid`, `started_at` — and is the liveness probe, as it is in harness: it touches no wiki and no
  model. **`/api/health` is gone** (it answers `404`); anything that probed it should probe
  `/api/server`.
- Tests pin the three routes, the page's content, the filled-in version and address, and that the
  endpoint reports the real process; each fails when the behaviour is removed.

## [0.1.0] - 2026-11-15

### Initial release

LLM-maintained wikis over HTTP, built on the [harness](https://github.com/gurcuff91/harness) SDK. It
started as a feature of harness itself and was moved into its own project to keep harness small; the
original design and plan are kept in `docs/history/`.

- **Several wikis** under `./harness-wiki/<slug>/` in the directory the server starts in, each with its
  own manifest, schema (`AGENTS.md`), immutable `raw/` and `wiki/` pages.
- **Ingest, query, lint**, run by a harness sub-agent whose working directory is the wiki's folder, so
  it follows that wiki's own schema. Ingest and lint run in the background (`202`); query is
  synchronous and **takes no lock**, so any number run at once, even during an ingest.
- **All-or-nothing ingest** from paths, globs (`**`), whole directories and inline files; symlinks in a
  directory are skipped, names are validated and confined to `raw/`, clashes refuse the whole ingest.
- **Live status** (`ready` / `busy` / `incomplete`), computed on every call. One ingest, lint, create or
  delete at a time per wiki across processes, through a lock file with a heartbeat (Windows-safe: the
  file is never held open); a busy wiki refuses another at once with what is running. An interrupted
  job leaves a marker and the next operation's maintainer is told to verify the wiki first.
- `harness-wiki serve [--host] [--port] [--model]` — defaults `127.0.0.1:8787` and the first model any
  active provider offers; `--host 0.0.0.0` publishes it (with a warning: there is no authentication).
- Providers work as in harness (`~/.harness`, or on a machine without it harness's own environment
  variables); a request can carry its own `"model"`.
- A typed Go client, an OpenAPI document at `/api/docs/openapi.json`, and a graceful shutdown that
  cancels running maintainers and releases every lock.
- Prebuilt binaries for macOS (amd64, arm64), Linux (amd64, arm64) and Windows (amd64), published with a
  `SHA256SUMS.txt` by the release workflow; all are static (`CGO_ENABLED=0`).
- Verified with unit tests per package, an end-to-end run of the real binary (two processes sharing
  one directory) and mutation tests; builds and vets for linux, darwin and windows.
