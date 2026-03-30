# Spike 003: Reachability Query — Does This Diff Touch This Path?

**Date:** 2026-03-29
**Status:** Complete
**Validates:** Research §4 (Reachability Query) from [001-static-analysis-landscape.md](../research/001-static-analysis-landscape.md)
**Depends on:** [001-diff-to-symbols.md](001-diff-to-symbols.md), [002-call-graph-from-entry-point.md](002-call-graph-from-entry-point.md)

---

## Goal

Wire together the full pipeline: diff → changed symbols → call graph → reachability check. Given a real Loki API endpoint and a real PR, answer "does this change touch code reachable from this entry point?" Validate with true positives and true negatives. End with a working CLI tool.

## Test Subjects

**Entry point:** `(*QuerierAPI).RangeQueryHandler` in `github.com/grafana/loki/v3/pkg/querier` — the handler behind Loki's `/loki/api/v1/query_range` endpoint. Handles range log/metric queries: parses the request, plans the query, dispatches to the query engine, and returns results.

**Negative control:** `(*Distributor).PushHandler` in `github.com/grafana/loki/v3/pkg/distributor` — the handler behind `/loki/api/v1/push`. Completely separate write path.

**PR #21112** — [feat(loki): allow disabling cache for query_range requests](https://github.com/grafana/loki/pull/21112)
- 2 files changed: `pkg/querier/queryrange/codec.go`, `pkg/querier/queryrange/codec_test.go`
- Changes `(Codec).DecodeRequest` to propagate the `Cache-Control: no-cache` header for query_range requests
- Should be reachable from RangeQueryHandler, not from PushHandler

**PR #21173** — [feat(query-engine): Omit labels with empty values](https://github.com/grafana/loki/pull/21173)
- 4 files changed across `pkg/engine` and `pkg/engine/internal/executor`
- Changes `(*streamsResultBuilder).CollectRecord` and `(*streamsView).Labels`
- Should be reachable from RangeQueryHandler (query engine is in the query path)

## What We Built

Library (`internal/reachable`) and CLI (`cmd/reachable`) implementing the full three-stage pipeline:

1. **Diff → Symbols** (from spike 001): Parse the PR diff, map changed lines to enclosing Go functions/methods.
2. **Call Graph Construction** (from spike 002): Load the entry point package, build SSA, construct call graph (CHA or VTA).
3. **Reachability Query** (new): Build an index of all reachable functions from the entry point, then check whether any changed symbol appears in the index.

### The Symbol Matching Problem

The key new challenge in this spike: bridging the naming gap between Stage 1 and Stage 2.

- **Stage 1** produces directory-relative package paths: `pkg/querier/queryrange`
- **Stage 2** produces full Go import paths: `github.com/grafana/loki/v3/pkg/querier/queryrange`

Solution: read the `module` directive from `go.mod` and prepend it to the relative path. `module github.com/grafana/loki/v3` + `pkg/querier/queryrange` → `github.com/grafana/loki/v3/pkg/querier/queryrange`.

For function matching, the call graph nodes use SSA's naming convention:
- Plain functions: `PackagePath.FuncName`
- Methods: `PackagePath.(*Type).MethodName` (always pointer receiver form in SSA)

We normalize both sides to this format and do O(1) hash lookups.

## Results

### PR #21112 vs RangeQueryHandler (CHA) — True Positive

```
stage 1: diff → symbols
  2 changed regions → 2 symbols (2 functions/methods)
  method   pkg/querier/queryrange.(Codec).DecodeRequest
  func     pkg/querier/queryrange.Test_codec_DecodeRequest_cacheHeader

stage 2: call graph from (*QuerierAPI).RangeQueryHandler [cha]
  load:      3.2s
  ssa build: 1.4s
  cg build:  6.8s
  walk:      539ms
  reachable: 162,971 functions across 2,149 packages

stage 3: reachability check (13.5s total)
REACHABLE — 1 changed symbol(s) reachable from (*QuerierAPI).RangeQueryHandler

  method   pkg/querier/queryrange.(Codec).DecodeRequest             depth=5
```

**Correct.** `DecodeRequest` is reachable at BFS depth 5. The test function is correctly excluded (not in the production call graph). The call chain is: `RangeQueryHandler` → query middleware → codec → `DecodeRequest`.

### PR #21112 vs PushHandler (CHA) — True Negative

```
stage 1: diff → symbols (same as above)

stage 2: call graph from (*Distributor).PushHandler [cha]
  load:      2.3s
  ssa build: 1.1s
  cg build:  4.7s
  walk:      322ms
  reachable: 131,376 functions across 1,958 packages

stage 3: reachability check (9.8s total)
NOT REACHABLE — no changed symbols reachable from (*Distributor).PushHandler
```

**Correct.** The query_range codec is not in PushHandler's call graph. Even CHA's over-approximate 131K-function reachable set doesn't include it — the distributor package doesn't import `pkg/querier/queryrange`.

### PR #21112 vs RangeQueryHandler (VTA) — False Negative

```
stage 2: call graph from (*QuerierAPI).RangeQueryHandler [vta]
  load:      3.3s
  ssa build: 3.6s
  cg build:  30.7s
  walk:      32ms
  reachable: 24,151 functions

stage 3: reachability check (42.4s total)
NOT REACHABLE
```

**Incorrect — false negative.** VTA prunes the edge from the query middleware to `Codec.DecodeRequest`. The codec is invoked through an interface dispatch (`queryrangebase.Codec` interface) inside HTTP middleware that VTA's type-flow analysis can't trace. The concrete `Codec` type is wired in at initialization time and flows through several layers of middleware wrapping before reaching the call site.

This is a significant finding: **VTA can produce false negatives for interface-heavy middleware chains.** CHA is the safer default for "does this change affect this path?" questions where false negatives are worse than false positives.

### PR #21173 vs RangeQueryHandler (CHA) — True Positive (Multiple Matches)

```
stage 1: diff → symbols
  4 changed regions → 4 symbols (4 functions/methods)
  method   pkg/engine.(*streamsResultBuilder).CollectRecord
  func     pkg/engine.TestStreamsResultBuilder
  method   pkg/engine/internal/executor.(*streamsView).Labels
  func     pkg/engine/internal/executor.Test_streamsView

stage 2: call graph from (*QuerierAPI).RangeQueryHandler [cha]
  reachable: 162,970 functions

stage 3: reachability check (23.3s total)
REACHABLE — 2 changed symbol(s) reachable from (*QuerierAPI).RangeQueryHandler

  method   pkg/engine.(*streamsResultBuilder).CollectRecord         depth=5
  method   pkg/engine/internal/executor.(*streamsView).Labels       depth=8
```

**Correct.** Both production functions are reachable. `CollectRecord` at depth 5 (closer to the query API), `Labels` at depth 8 (deeper in the executor). Test functions correctly excluded.

## Summary Table

| PR | Entry Point | Algorithm | Expected | Actual | Time |
|----|------------|-----------|----------|--------|------|
| #21112 | RangeQueryHandler | CHA | REACHABLE | REACHABLE (1 match, depth=5) | 13.5s |
| #21112 | PushHandler | CHA | NOT REACHABLE | NOT REACHABLE | 9.8s |
| #21112 | RangeQueryHandler | VTA | REACHABLE | NOT REACHABLE (**false negative**) | 42.4s |
| #21173 | RangeQueryHandler | CHA | REACHABLE | REACHABLE (2 matches, depth=5,8) | 23.3s |

## Key Findings

### 1. The full pipeline works end-to-end

The three-stage architecture from the research doc is validated. Diff parsing (<5ms) + symbol mapping (<5ms) + call graph construction (~10s) + reachability check (O(1) per symbol) produces correct results on real Loki PRs.

### 2. CHA is the right default; VTA has false negatives

CHA's over-approximation is a feature for our use case. When asking "might this change affect this path?", false positives (saying it's reachable when it isn't) are acceptable — they just trigger an unnecessary CI job. False negatives (saying it's NOT reachable when it IS) are dangerous — they skip a needed CI job.

VTA's false negative on PR #21112 is caused by interface dispatch through middleware chains. This is extremely common in Go HTTP servers (middleware wrapping, handler interfaces, codec interfaces). VTA should only be used as a secondary filter when CHA produces too many false positives, never as the primary check.

### 3. Symbol matching via module path + function name works cleanly

The `go.mod` module path prefix approach bridges Stage 1 (directory-relative) and Stage 2 (full import path) naming without needing `go/types` or `packages.Load` in Stage 1. This keeps Stage 1 fast and independent.

SSA normalizes all methods to pointer receiver form `(*T).Method`, so we normalize the AST-extracted receiver the same way. No ambiguity.

### 4. Test functions are naturally excluded

Test files are not imported by production code, so test functions don't appear in the call graph rooted at a production entry point. This means the tool automatically answers "does this change affect the production path?" without needing to filter test symbols explicitly.

### 5. The exit code convention enables CI scripting

- Exit 0: reachable (or no changed Go symbols) → run the CI job
- Exit 1: error → fail the pipeline
- Exit 2: not reachable → skip the CI job

```yaml
# Example GitHub Actions usage
- name: Check if query path is affected
  id: check
  continue-on-error: true
  run: |
    reachable -repo . \
      -pkg github.com/grafana/loki/v3/pkg/querier \
      -recv "*QuerierAPI" -func RangeQueryHandler \
      -base ${{ github.event.pull_request.base.sha }}

- name: Run query load test
  if: steps.check.outcome == 'success'
  run: make load-test-query
```

### 6. Performance is CI-acceptable

| Metric | CHA | VTA |
|--------|-----|-----|
| Total time | 10-23s | 42s |
| Memory | 5.7-6.9 GB | 8.6 GB |

CHA at 10-23s is well within CI budget (most CI jobs take minutes). The variance comes from package loading (which depends on how many transitive deps the entry point pulls in). VTA at 42s is still usable but the false negative risk makes it unsuitable as default.

## Gotchas Discovered

### 1. Non-function symbols need special handling

The current pipeline only matches functions and methods. Changes to types, constants, variables, and file-level code (imports) are skipped. This is correct for direct reachability but misses indirect effects:
- A changed struct field type could affect all methods on that struct
- A changed constant used in a reachable function affects that function's behavior
- A changed `init()` function affects everything that imports its package

For the MVP, skipping non-function symbols is pragmatic. The tool still catches the most important case (changed function bodies). Type/const changes can be added later.

### 2. Closure bodies map to the enclosing function

If a diff changes code inside an anonymous function (closure), Stage 1 maps it to the enclosing `*ast.FuncDecl`. The call graph has separate `*ssa.Function` nodes for closures (e.g., `DecodeRequest$1`). Since we match against the enclosing function name, this works — if `DecodeRequest` is reachable, its closures are too. But if only a closure is reachable (not the parent), we'd miss it. This hasn't been a problem in practice.

### 3. VTA's false negatives are systematic, not random

VTA loses edges at interface dispatch boundaries where the concrete type flows through multiple layers of abstraction (middleware wrapping, dependency injection, factory functions). This is architectural — Go HTTP servers are built on interface dispatch. CHA handles this correctly because it considers all concrete types that satisfy the interface.

### 4. Package loading time varies by entry point

Loading from `pkg/querier` (2,149 packages) takes longer than from `pkg/distributor` (1,958 packages) because the querier imports more of the codebase. The SSA build and CG build scale accordingly. For a multi-entry-point config, we'd want to load once and query multiple entry points.

## Open Questions for MVP

1. **Multi-entry-point config:** The real use case has multiple paths (write, query, compactor, etc.) each with their own entry point. We need a config file format and the ability to check all paths in one run, sharing the expensive package loading step.

2. **Non-function symbol handling:** Should a changed type trigger "reachable" if any method on that type is reachable? Should a changed constant trigger "reachable" if the function using it is reachable? Both seem useful but require cross-referencing the AST with the call graph.

3. **Incremental analysis:** Loading 2K packages and building SSA takes 4-5s. Can we cache the call graph between CI runs? The `callgraph.Graph` isn't serializable, but we could serialize the adjacency list as `{pkg.FuncName → [pkg.FuncName]}` and rebuild only changed packages.

4. **Package-level fast mode:** For very large repos, we could skip SSA entirely and just check whether the changed package is in the entry point's import graph. This would be O(seconds) instead of O(tens of seconds) but much less precise.

5. **Multiple module support:** Monorepos with multiple `go.mod` files (like Loki's `tools/` directory) need per-module analysis. The current single-`go.mod` approach works for the main module.

## CLI Usage

```bash
# Check if a PR touches the query path
reachable -repo ~/workspace/loki \
  -pkg github.com/grafana/loki/v3/pkg/querier \
  -recv "*QuerierAPI" -func RangeQueryHandler \
  -pr 21112

# Check if a branch touches the write path
reachable -repo ~/workspace/loki \
  -pkg github.com/grafana/loki/v3/pkg/distributor \
  -recv "*Distributor" -func PushHandler \
  -base main

# Pipe a diff from stdin
git diff main...feature | reachable -repo ~/workspace/loki \
  -pkg github.com/grafana/loki/v3/pkg/querier \
  -recv "*QuerierAPI" -func RangeQueryHandler

# JSON output for scripting
reachable -repo ~/workspace/loki \
  -pkg github.com/grafana/loki/v3/pkg/querier \
  -recv "*QuerierAPI" -func RangeQueryHandler \
  -pr 21173 -format json

# Works on any Go repo
reachable -repo ~/workspace/my-service \
  -pkg github.com/myorg/my-service/internal/api \
  -recv "*Server" -func HandleRequest \
  -base main

# Use VTA for higher precision (with false negative risk)
reachable -repo ~/workspace/loki \
  -pkg github.com/grafana/loki/v3/pkg/querier \
  -recv "*QuerierAPI" -func RangeQueryHandler \
  -pr 21173 -algo vta
```
