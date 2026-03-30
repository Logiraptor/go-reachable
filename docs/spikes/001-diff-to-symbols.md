# Spike 001: Diff → Changed Symbols

**Date:** 2026-03-29
**Status:** Complete
**Validates:** Research §2 (Diff → Changed Symbols) from [001-static-analysis-landscape.md](../research/001-static-analysis-landscape.md)

---

## Goal

Parse a real GitHub PR diff and map changed lines to the enclosing Go functions, methods, and types. Validate accuracy against a known PR.

## Test Subject

**Loki PR #21173** — [feat(query-engine): Omit labels with empty values](https://github.com/grafana/loki/pull/21173)

A clean feature PR: 4 files, 2 production functions changed, 2 test functions changed. Touches the query engine's label handling in both `pkg/engine` and `pkg/engine/internal/executor`.

## What We Built

Two-stage pipeline in `internal/diffsyms`:

1. **`ParseDiff([]byte) → []ChangedRegion`** — Uses `sourcegraph/go-diff` to parse unified diffs into `{file, startLine, endLine}` tuples. One region per hunk, using "new side" line numbers.

2. **`ChangedSymbols(repoRoot, regions) → []Symbol`** — For each `.go` file in the diff, parses the AST with `go/parser`, indexes all top-level declarations with their line ranges, then matches diff regions to enclosing declarations.

Plus a CLI (`cmd/diffsyms`) that accepts input three ways: `-pr N` (via `gh`), `-base branch` (via `git diff`), or stdin pipe.

## Results Against Loki PR #21173

```
method  pkg/engine.(*streamsResultBuilder).CollectRecord              pkg/engine/compat.go:65
func    pkg/engine.TestStreamsResultBuilder                           pkg/engine/compat_test.go:26
method  pkg/engine/internal/executor.(*streamsView).Labels            pkg/engine/internal/executor/streams_view.go:133
func    pkg/engine/internal/executor.Test_streamsView                 pkg/engine/internal/executor/streams_view_test.go:15
```

**4/4 symbols correctly identified.** Each changed hunk maps to exactly the right enclosing function/method:

| Diff hunk | Expected symbol | Found? |
|-----------|----------------|--------|
| `compat.go` lines 115–126 (empty label check) | `(*streamsResultBuilder).CollectRecord` | Yes |
| `compat_test.go` lines 473–540 (new test case) | `TestStreamsResultBuilder` | Yes |
| `streams_view.go` lines 180–190 (empty label check) | `(*streamsView).Labels` | Yes |
| `streams_view_test.go` lines 141–173 (new test case) | `Test_streamsView` | Yes |

Also validated against **Loki PR #21155** (Goldfish correlation ID, 11 files, ~60 symbols) — all symbols resolved correctly including methods with pointer receivers, test functions, types, constants, and file-level changes.

## What Worked Well

1. **`sourcegraph/go-diff` is solid.** Handles all the edge cases in unified diff format. The `b/` prefix stripping is the only gotcha. v0.7.0, MIT, no issues.

2. **`go/parser` + manual declaration indexing beats `astutil.PathEnclosingInterval`.** The research doc recommended `PathEnclosingInterval`, but in practice, building a flat index of all `*ast.FuncDecl` / `*ast.GenDecl` declarations and doing line-range overlap is simpler and handles the "hunk spans two functions" case naturally (returns both). `PathEnclosingInterval` gives only the innermost enclosing node for the interval as a whole, which would miss one function when a hunk spans a boundary.

3. **No type information needed for Stage 1.** `go/parser` alone (no `go/types`, no `packages.Load`) is sufficient to map lines to symbol names. This keeps Stage 1 fast — parsing a single file takes <1ms. The full-qualified package path comes from the file's directory relative to the repo root, which matches Go module conventions.

4. **Receiver type extraction from AST is straightforward.** `*ast.FuncDecl.Recv` gives the receiver, and walking `*ast.StarExpr` / `*ast.Ident` / `*ast.IndexExpr` handles pointer receivers and generics.

## Gotchas Discovered

1. **Hunk line numbers are "new side" numbers.** The diff contains both original and new line numbers. We must use the new side (`NewStartLine`, `NewLines`) because that's what matches the files on disk after the PR. The research doc's recipe is correct here.

2. **go-diff prefixes filenames with `a/` and `b/`.** Need to strip these. `OrigName` gets `a/`, `NewName` gets `b/`.

3. **Pure deletion hunks have `NewLines == 0`.** These need to be skipped (the file may not exist, or the lines don't exist in the new version). For our use case this is correct — if code was deleted, the symbol may no longer exist.

4. **File-level changes (imports, package clause) don't fall inside any declaration.** We emit a `"file"` kind symbol for these. This is important for Stage 3 — a changed import could mean a new dependency was added, which might be relevant for path analysis.

5. **Test functions use Go's naming conventions.** `Test_streamsView` (with underscore) is a valid test function name. The tool handles this correctly because it reads the actual AST, not pattern-matching on names.

## What We Didn't Need

- **`go/types`** — Not needed for symbol identification. Will be needed in Stage 2 for call graph construction.
- **`golang.org/x/tools/go/packages`** — Not needed. `go/parser.ParseFile` is sufficient for single-file AST analysis.
- **`astutil.PathEnclosingInterval`** — Replaced with simpler line-range overlap against a declaration index.
- **`gosemdiff` or `sem`** — The standard go-diff + go/parser approach works well. Semantic diff tools might help filter noise (e.g., MOVE vs LOGIC changes) but aren't needed for the core pipeline.

## Performance

- Parsing the diff: <1ms
- Parsing + indexing one Go file: <1ms
- Total for PR #21173 (4 files): <5ms
- Total for PR #21155 (11 Go files): <10ms

Stage 1 will not be a bottleneck. The research doc's concern about performance is entirely about Stage 2 (SSA construction, call graph).

## Open Questions for Stage 2

1. **Package path qualification:** We currently use the directory path relative to repo root (e.g., `pkg/engine`). For call graph matching, we'll need the full Go import path (e.g., `github.com/grafana/loki/v3/pkg/engine`). This requires reading `go.mod` to get the module path prefix.

2. **Nested function literals:** The diff in `compat.go` actually changes code inside an anonymous function passed to `forEachNotNullRowColValue`. Our tool correctly maps this to the enclosing `CollectRecord` method (the outermost `*ast.FuncDecl`). For call graph analysis, we might want to also track the inner closure, since SSA represents closures as separate `*ssa.Function` objects.

3. **Generated code filtering:** Not encountered in this PR, but Loki has protobuf-generated files. Should add `// Code generated` detection before Stage 2.

## CLI Usage

```bash
# By PR number (requires gh CLI, run from repo dir)
diffsyms -repo ~/workspace/loki -pr 21173

# By base branch
diffsyms -repo ~/workspace/loki -base main

# From stdin
gh pr diff 21173 | diffsyms -repo ~/workspace/loki

# JSON output for scripting
diffsyms -repo ~/workspace/loki -pr 21173 -format json
```
