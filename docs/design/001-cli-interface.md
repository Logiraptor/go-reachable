# Design: `go-reachable` CLI Interface

**Date:** 2026-03-30  
**Status:** Draft — interface definition (implementation follows)  
**Depends on:** Spikes 001–003, [research/001-static-analysis-landscape.md](../research/001-static-analysis-landscape.md)

---

## Goals

- One small **config file** that names *product paths* (OTLP ingest, query API, etc.) and maps each name to a Go entry symbol (package + function, optional method receiver).
- **Diff-only input:** The tool reads a **unified diff** (bytes only). It does **not** invoke `git`, `gh`, or any other VCS — callers produce the diff however they like (`git diff`, patches from CI, etc.) and pass it in. That keeps the interface small and aligned with the Unix philosophy (one job: diff + config → which paths are touched).
- **Analysis defaults:** **VTA** for call graph construction, with the **VTA analysis root** at `main` (see [VTA root vs path entries](#vta-root-vs-path-entries)).

---

## Config file

### Location and discovery

| Mechanism | Behavior |
|-----------|----------|
| `-config PATH` | Use the given file. |
| (default) | Search upward from `-repo` for `.reachable.yaml`, then `reachable.yaml`. If none found, error unless using legacy single-entry flags (if retained). |

### Format

YAML, UTF-8. Suggested dependency: `gopkg.in/yaml.v3` (per research doc).

```yaml
# Optional. Bump when the schema changes.
version: 1

# VTA is sliced/refined from this root so init wiring and startup code are in scope.
# Defaults are inferred when omitted (see below).
vta:
  package: github.com/example/app/cmd/server   # import path of the main package
  func: main                                   # must be "main" for a command

# Named paths: each entry is a reachability *query* root (handler, RPC method, etc.).
paths:
  - name: otlp-ingest
    package: github.com/example/app/pkg/ingest
    recv: "*Ingest"
    func: HandleOTLP

  - name: query-http
    package: github.com/example/app/pkg/querier
    recv: "*API"
    func: RangeQueryHandler

  - name: migrations
    package: github.com/example/app/pkg/storage
    func: RunMigrations
```

### Field reference

| Field | Required | Description |
|-------|----------|-------------|
| `version` | No | Integer schema version; reject unknown major versions. |
| `vta.package` | Recommended | Import path of the **main** package whose `main` function anchors VTA. |
| `vta.func` | No | Must be `main` when set; default `main`. |
| `paths` | Yes (for multi-path mode) | Non-empty list of named paths. |
| `paths[].name` | Yes | Stable identifier for CI (ASCII, suitable for labels and JSON keys). |
| `paths[].package` | Yes | Full import path (same semantics as `-pkg` today). |
| `paths[].func` | Yes | Function or method name. |
| `paths[].recv` | No | Receiver type string, e.g. `*Handler` (same semantics as `-recv` today). |

### Defaults for `vta`

If `vta` is omitted:

1. **Single-path configs** (future optional shortcut): could infer from the only `paths` entry — *not* used for VTA root; VTA root must still be `main`.
2. **Recommended default:** require `vta.package` once `paths` has more than one entry, **or** require an explicit `vta` block whenever using VTA (clearest for CI).

**Practical default:** If only one `main` exists under `./cmd/...`, auto-detect is tempting but fragile in monorepos. **MVP rule:** `vta.package` is **required** when using the config file; optional follow-up is auto-discovery via `go list -f '{{.ImportPath}}' ./cmd/...` heuristics.

---

## VTA root vs path entries

- **VTA root (`vta.package` + `main`):** Used to build/refine the VTA call graph so dynamic dispatch and startup behavior match a real binary (aligned with govulncheck-style CHA → slice → VTA). This is **one** `main.main` (or the configured main package).
- **Path entries (`paths`):** Each is a separate **forward reachability query**: “Is any changed symbol reachable from this symbol?” against the same graph (or a graph consistent with the VTA settings).

So: **VTA entry = `main`** (in `vta.package`); **named entries** = product paths you care about for CI gating.

---

## Command-line interface

### Name

Binary: `reachable` (existing) or `go-reachable` if published as a module install; this doc uses **`reachable`** for examples.

### Synopsis

```text
reachable [global flags] <subcommand> [subcommand flags]
```

**MVP subcommands:**

| Subcommand | Purpose |
|------------|---------|
| `paths` | Read config, parse diff, print which **named paths** are touched (default multi-path workflow). |
| `check` | *(Optional legacy)* Single entry via flags only — can remain for scripts; or deprecate in favor of a one-line config. |

If you prefer a flat CLI without subcommands, `reachable` with `-config` and no subcommand can default to `paths` behavior.

### Global flags

| Flag | Default | Description |
|------|---------|-------------|
| `-repo` | `.` | Repository root (module root or workspace root for `go list`). |
| `-config` | *(search)* | Path to YAML config. |
| `-format` | `text` | `text` \| `json` — machine-readable for CI. |
| `-algo` | `vta` | **Default `vta`.** Other values (`cha`, `cha-pruned`) optional for experiments; production CI should use `vta` per this design. |

### `paths` subcommand arguments

| Argument | Description |
|----------|-------------|
| `[diff-file]` | Path to a file containing a **unified diff**. If omitted, read the diff from **stdin**. Use `-` explicitly for stdin if the CLI reserves a different meaning for “missing file” in your implementation. |

No `-pkg`/`-func` on `paths` — those come from config.

### Diff input (file or stdin)

- **Contract:** Input must be a **unified diff** (same format `go-diff` / Stage 1 spikes expect). Paths inside the diff are interpreted relative to `-repo` when resolving files on disk.
- **No VCS in-process:** The binary does not run `git`, `gh`, or similar. Anything about branches, PRs, or merges is **outside** this tool.
- **TTY rule:** If no `diff-file` is given and stdin is a **terminal**, print usage and exit with an error (nothing to read). Redirect or pipe a file instead.

---

## Output

### Text (default)

One line per path, stable ordering (e.g. config order or sorted by `name`):

```text
touched: otlp-ingest, query-http
not touched: migrations
```

Or tabular:

```text
PATH            TOUCHED
otlp-ingest     yes
query-http      yes
migrations      no
```

**Recommendation:** Emit **only touched** paths on stdout for minimal CI parsing, and **verbose** table via `-v` / `-format text -verbose`. Exact choice is an implementation detail; document the final format in `-help`.

### JSON

```json
{
  "vta": { "package": "github.com/example/app/cmd/server", "func": "main" },
  "paths": [
    { "name": "otlp-ingest", "touched": true, "matches": [ /* … */ ] },
    { "name": "query-http", "touched": true, "matches": [ /* … */ ] },
    { "name": "migrations", "touched": false }
  ],
  "stats": { /* aggregate or per-path timings — TBD */ }
}
```

Reuse existing `reachable.Result`-shaped objects per path where possible.

---

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | Success; analysis completed. **At least one** path is touched *or* there were no changed Go symbols to analyze (same “vacuous success” policy as today — document clearly). |
| `1` | Error (config, I/O, analysis failure). |
| `2` | Success; **no** named path is touched by the diff (optional: only if `-exit-code-untouched` or CI mode; otherwise always `0` when analysis succeeds). |

**CI recommendation:** Support an explicit flag:

- `-fail-if-none-touched` → exit `2` when every path is untouched (for “must touch one of these” workflows), **or**
- Default exit `0` on success and rely on JSON for gating.

Pick one default and document it; many teams want `0` = “tool ran” and parse JSON for routing.

---

## Implementation notes (non-normative)

- **Performance:** Multi-path mode may run one SSA/VTA build and **multiple forward walks** from each `paths[]` entry (cheaper than N full analyses). Validate with benchmarks on representative repos.
- **Verify:** Future `reachable verify` should load config and ensure each `paths[]` and `vta` symbol resolves (see research doc risks).
- **Build tags:** Same as spikes: respect `GOOS`/`GOARCH` when loading packages.

---

## Examples

```bash
# Diff in a file (caller produced it however they want)
reachable -repo . -config .reachable.yaml paths /tmp/pr.diff

# Pipe a diff (e.g. git is only in the shell, not inside reachable)
git diff main...HEAD | reachable -repo . -config .reachable.yaml paths

# Redirect
reachable -repo . -config .reachable.yaml paths < /tmp/pr.diff

# JSON for automation
git diff main...HEAD | reachable -repo . -config .reachable.yaml paths -format json
```

---

## Summary

| Topic | Decision |
|-------|----------|
| Config | YAML: `vta` + `paths[]` with `name`, `package`, `func`, optional `recv` |
| Algorithm | **VTA** default; `vta.package` + `main` as VTA root |
| Diff | **File path and/or stdin only** — unified diff bytes; **no** git/gh/VCS inside the tool |
| Primary UX | `reachable paths` (or equivalent) listing **which path names** are touched |

This document is the contract for the MVP CLI; flags and JSON field names may gain small renames during implementation, but the **split between VTA root (`main`) and named path entries** should stay stable.
