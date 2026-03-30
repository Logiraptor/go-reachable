# go-reachable

Static reachability checks for Go: given a **unified diff** (for example from a PR) and a **handler or function** you care about, decide whether any **changed function or method** is reachable from that entry point via the call graph.

Typical use: “Does this PR touch code on the query path, the write path, or neither?”

## Requirements

- **Go** 1.25+
- **RAM**: analyzing large modules (for example Grafana Loki) often needs **6–8+ GB** heap during the call-graph phase
- A **clone of the repo** at a commit where changed files still exist (paths in the diff must resolve to the tree you analyze)
- **Diff input**: pipe a unified diff on stdin, or pass a path to a `.diff` file. The tool does not run `git` or `gh` for you

## Install

From this repository:

```bash
go build -o reachable ./cmd/reachable
```

Put `reachable` on your `PATH`, or invoke it by full path.

## Loki example (verified)

These commands were run against a current clone of [`grafana/loki`](https://github.com/grafana/loki) and [PR #21112](https://github.com/grafana/loki/pull/21112) (“allow disabling cache for `query_range` requests”). That PR changes `(*Codec).DecodeRequest` in `pkg/querier/queryrange` — on the **range query** path, not the **push** path.

Use **`-algo cha`** here. Class-based hierarchy analysis follows interface dispatch conservatively, which matches this scenario. **`-algo vta`** (the CLI default) can **miss** edges across middleware and interfaces on the query path and report a false “not reachable” for this kind of change.

### 1. Clone Loki

```bash
git clone https://github.com/grafana/loki.git
cd loki
export LOKI="$(pwd)"
```

### 2. Fetch the PR diff

With [GitHub CLI](https://cli.github.com/):

```bash
gh pr diff 21112 --repo grafana/loki > /tmp/loki-21112.diff
```

Without `gh`, download the patch from the PR page and save it as a file.

### 3. Range query handler — should be **reachable**

`RangeQueryHandler` implements `/loki/api/v1/query_range`. The codec change belongs on this path, so you should see **`REACHABLE`** and exit code **0**:

```bash
reachable -repo "$LOKI" -algo cha check \
  -pkg github.com/grafana/loki/v3/pkg/querier \
  -recv "*QuerierAPI" \
  -func RangeQueryHandler \
  /tmp/loki-21112.diff
```

Example stderr/stdout shape:

```text
stage 1: diff → symbols
  2 changed regions → 2 symbols (2 functions/methods)

stage 2–3: call graph from github.com/grafana/loki/v3/pkg/querier.(*QuerierAPI).RangeQueryHandler [cha]
  ...
REACHABLE — 1 changed symbol(s) reachable from ...
  method   pkg/querier/queryrange.(Codec).DecodeRequest             depth=5
```

You can pipe instead of a file:

```bash
gh pr diff 21112 --repo grafana/loki | reachable -repo "$LOKI" -algo cha check \
  -pkg github.com/grafana/loki/v3/pkg/querier \
  -recv "*QuerierAPI" \
  -func RangeQueryHandler
```

### 4. Push handler — should be **not reachable**

`PushHandler` is the `/loki/api/v1/push` entry. The same diff should **not** reach that handler; expect **`NOT REACHABLE`** and exit code **2**:

```bash
reachable -repo "$LOKI" -algo cha check \
  -pkg github.com/grafana/loki/v3/pkg/distributor \
  -recv "*Distributor" \
  -func PushHandler \
  /tmp/loki-21112.diff
```

### Exit codes (`check`)

| Code | Meaning |
|------|--------|
| 0 | At least one changed function/method is reachable from the entry (or no Go functions changed and nothing to check) |
| 1 | Error |
| 2 | **Not reachable** (text mode) or not touched (JSON mode for `check`) |

## Troubleshooting

### `2 changed regions → 0 symbols` (or exit: `diff references Go files not found under repo root`)

`-repo` must be the **root of the same Go module** whose paths appear in the diff (the directory that contains `go.mod`). If you pipe a Loki PR diff but pass `-repo .` from **this** repo (`go-reachable`), paths like `pkg/querier/queryrange/codec.go` do not exist there, so nothing maps to symbols.

Fix: clone Loki, `cd` into it (or `export LOKI=…`), and run `reachable -repo "$LOKI" …`.

### Empty diff from `gh pr diff`

If `gh pr diff …` prints nothing, fix authentication (`gh auth login`), network, or the PR number. An empty pipe yields no regions; the tool cannot analyze anything.

## Multi-path config (`.reachable.yaml`)

The `paths` subcommand reads `.reachable.yaml` (or `reachable.yaml`) and checks several named entry points in one run. It requires **`-algo vta`** or **`-algo cha-pruned`** (not plain `cha`).

`vta.package` is the import path of the binary’s `main` package (VTA anchor). Under `paths`, each entry names a reachability root: `package`, `func`, and optional `recv` for methods (the same idea as `-pkg`, `-func`, and `-recv` on `check`). Optional `vta.func` must be `main` if set; omit it to use the default.

Example (aligned with the Loki `check` examples above):

```yaml
version: 1
vta:
  package: github.com/grafana/loki/v3/cmd/loki
paths:
  - name: range-query
    package: github.com/grafana/loki/v3/pkg/querier
    recv: "*QuerierAPI"
    func: RangeQueryHandler
  - name: push
    package: github.com/grafana/loki/v3/pkg/distributor
    recv: "*Distributor"
    func: PushHandler
```

```bash
reachable -repo "$LOKI" -algo vta paths < /tmp/loki-21112.diff
```

## Other commands in this repo

| Command | Role |
|---------|------|
| `callgraph` | Inspect reachable functions from one entry (debugging / exploration) |
| `cgdiff` | Compare call graphs or algorithms |
| `diffsyms` | Map a diff to changed symbols only (no reachability) |

## Design notes

More background, benchmarks, and algorithm trade-offs (CHA vs VTA) live under [`docs/`](docs/).
