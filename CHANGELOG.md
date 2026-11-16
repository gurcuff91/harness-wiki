# Changelog

All notable changes to this project will be documented in this file.

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
