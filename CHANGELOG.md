# Changelog

All notable changes to this project will be documented in this file.

## [0.1.0] - 2026-11-15

### Initial release

LLM-maintained wikis over HTTP, ported out of harness (where it was developed as v0.98.0 and then
moved here to keep harness small), on top of the harness SDK.

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
- Verified with unit tests per package, an end-to-end run of the real binary (two processes sharing
  one directory) and mutation tests; builds and vets for linux, darwin and windows.
