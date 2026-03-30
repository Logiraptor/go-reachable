# Research: Static Analysis Landscape for Go Path Reachability

**Date:** 2026-03-29
**Status:** Complete
**Goal:** Understand available techniques, tools, and libraries for building a tool that determines which user-defined code paths are touched by a given git diff.

---

## Problem Statement

Given a Go codebase with user-defined "paths" (e.g., OTLP ingest path, Loki protobuf write path, HTTP API query path) and a git diff, determine through static analysis which paths are touched by the change.

The end goal is targeted CI automation:
- "This PR touches the write path → schedule a load test"
- "This PR touches the SQL layer → skip load test, run migration checks"

The tool should follow the UNIX philosophy and feel at home alongside `go fmt`, `golangci-lint`, etc.

---

## 1. The Algorithm

Three stages:

1. **Diff → Changed Symbols:** Parse the git diff, map changed lines to enclosing Go functions/methods/types.
2. **Call Graph Construction:** Build a call graph of the program starting from user-defined entry points (path roots).
3. **Reachability Query:** For each path's entry points, walk the call graph forward. If any changed symbol is reachable, that path is "touched."

This is essentially the inverse of what govulncheck does. govulncheck asks "are known vulnerable sinks reachable from entry points?" We ask "are changed symbols reachable from path entry points?"

---

## 2. Stage 1: Diff → Changed Symbols

### Diff Parsing

**`github.com/sourcegraph/go-diff`** (MIT, v0.6.0) parses unified diffs into structured Go types:

- `ParseMultiFileDiff([]byte)` → `[]*FileDiff`
- Each `FileDiff` contains `Hunks`, each hunk has `OrigStartLine`, `OrigLines`, `NewStartLine`, `NewLines`
- Gives us `{filename, line_start, line_end}` tuples for every changed region

Other options:
- **`github.com/Ataraxy-Labs/sem`** — multi-language semantic diff via tree-sitter, entity-level changes, JSON output. Supports 21 languages including Go.
- **`github.com/shinagawa-web/gosemdiff`** — Go-specific semantic diff using AST fingerprinting. Classifies changes as MOVE/RENAME/REFACTOR/LOGIC. Very early (Feb 2026, pre-v0.1.0).
- Raw `git diff -U0` parsing is trivial but `go-diff` handles edge cases.

### Line Range → AST Node Mapping

The Go toolchain provides everything needed to map "file.go lines 42-50" to "function Foo in package bar":

**Key types and functions:**

- **`go/token.FileSet`** — owns position information for all parsed files. `fset.Position(pos)` converts a `token.Pos` to `{Filename, Line, Column}`.
- **`go/token.File.LineStart(line)`** — converts a 1-based line number to a `token.Pos`. This is how we go from diff line numbers to AST positions.
- **`go/parser.ParseFile(fset, filename, src, mode)`** — parses a single Go file into an `*ast.File`.
- **`golang.org/x/tools/go/ast/astutil.PathEnclosingInterval(fset, file, start, end)`** — given a position range, returns the AST path from root to the deepest enclosing node. This is the key function.

**The recipe:**

```
For each hunk in the diff:
  1. Parse the file with go/parser
  2. Convert hunk line range to token.Pos via FileSet.LineStart()
  3. Call PathEnclosingInterval(start, end)
  4. Walk up the returned path until hitting *ast.FuncDecl or *ast.TypeSpec
  5. Record the changed symbol: package + function/type name
```

**Gotchas:**

- **`//line` directives** change debugging line mapping but `go/parser` positions still reflect physical file layout. Git line numbers match the file on disk, so this is fine for our use case.
- **Build tags / multiple files per package:** A diff in `foo_linux.go` only matters if that file is in the build configuration being analyzed. Need to handle `GOOS`/`GOARCH`/build constraints.
- **Overlapping edits:** A single hunk might span two functions. `PathEnclosingInterval` gives the innermost enclosing node for the interval as a whole. For wide intervals, sample multiple points or split by function boundaries.
- **Non-function changes:** Changes to package-level `var`, `const`, `type`, or `init()` need special handling. A changed type's method set might affect interface dispatch.
- **Generated code:** Parses fine syntactically but may produce noise if generators change boilerplate.

### Connecting AST to Type Information

To get fully qualified symbol names (needed for call graph matching), we need `go/types`:

- **`types.Info.Defs`** — maps `*ast.Ident` at definition sites to `types.Object`
- **`types.Info.Uses`** — maps `*ast.Ident` at use sites to `types.Object`
- **`types.Object.Pkg().Path()`** — gives the full package path
- **`types.Func`** — represents a function/method with full signature

For interface analysis:
- **`types.Implements(T, iface)`** — checks if concrete type T implements interface
- **`types.MethodSet(T)`** — returns all methods of type T (including promoted)

`go/types` alone does not enumerate all implementers of an interface across the program — that requires a workspace-wide scan or call graph analysis.

---

## 3. Stage 2: Call Graph Construction

### Package Loading

**`golang.org/x/tools/go/packages`** is the standard way to load Go packages with full type information under modules.

**Load modes and their cost:**

| Mode | What it gives | Cost |
|------|--------------|------|
| `NeedName` | Package name, path | Cheap (metadata only) |
| `NeedFiles` / `NeedCompiledGoFiles` | File lists | Cheap |
| `NeedImports` | Direct import graph | Cheap |
| `NeedDeps` | Recursive dependency loading | **Expensive** — can explode to large graphs |
| `NeedSyntax` | Parsed ASTs | Grows with source size |
| `NeedTypes` + `NeedTypesInfo` | Type checking | Expensive, requires full dependency chain |

For call graph construction, we need `NeedSyntax + NeedTypes + NeedTypesInfo + NeedDeps` — the full load. This is the main cost driver.

**Performance strategy:** Use narrow patterns. Load only changed packages + their transitive importers, not the entire module. `NeedDeps` is the usual cost multiplier.

**Gotcha:** Interactions between `NeedSyntax` and `NeedTypes`/`NeedTypesInfo` have had bugs where fields stay empty unless combined correctly. Follow pkg.go.dev examples and prefer compound modes like `LoadAllSyntax`.

### SSA Construction

**`golang.org/x/tools/go/ssa`** builds a Static Single Assignment intermediate representation. Required for all call graph algorithms except trivial static-call-only analysis.

```go
prog := ssa.NewProgram(fset, ssa.InstantiateGenerics)
// Create packages from go/packages output...
prog.Build()
```

**What SSA adds beyond go/types:**
- Explicit call sites (`ssa.Call`, `ssa.Go`, `ssa.Defer`)
- Materialized closures and free variables
- Control flow graph per function (basic blocks, phi nodes)
- Foundation for VTA's type-flow analysis

**Tradeoffs:**
- Build time is O(code size) with large constants
- Memory: retains IR for every function — much larger than go/types alone
- Not designed for incremental reuse across edits
- `ssa.InstantiateGenerics` flag is important for generics-heavy code

### Call Graph Algorithms

Four algorithms available in `golang.org/x/tools/go/callgraph`:

| Algorithm | Package | How it works | Precision | Speed | Requirements |
|-----------|---------|-------------|-----------|-------|-------------|
| **Static** | `callgraph/static` | Only resolves static calls | Lowest — misses all dynamic dispatch | Fastest | Parsed program |
| **CHA** | `callgraph/cha` | Class Hierarchy Analysis: for interface/func-value calls, considers all types that could satisfy the static type | Coarse for interfaces | Fast, linear-ish | SSA program |
| **RTA** | `callgraph/rta` | Rapid Type Analysis: starts from root set, grows reachable methods as new types are discovered | Better than CHA (prunes dead types) | Moderate | Whole program (needs main/test entry) |
| **VTA** | `callgraph/vta` | Variable Type Analysis: propagates types along SSA-based type-flow graph | Between RTA and pointer analysis | Heavier than RTA | SSA program; experimental API |
| **Pointer** | `go/pointer` | Andersen-style points-to analysis | Most precise | Slow, high memory | Whole program |

**All of these are over-approximate (sound for "may call")** — suitable for "affected paths might include..." which is exactly what we want. Under-approximate "must/definite" analysis needs different techniques.

### What govulncheck Actually Uses

Confirmed from `golang.org/x/vuln v1.1.4` source (`internal/vulncheck/utils.go`):

```go
// callGraph builds a call graph of prog based on VTA analysis.
func callGraph(ctx context.Context, prog *ssa.Program, entries []*ssa.Function) (*callgraph.Graph, error) {
    entrySlice := make(map[*ssa.Function]bool)
    for _, e := range entries {
        entrySlice[e] = true
    }

    // Step 1: CHA as the initial conservative graph
    initial := cha.CallGraph(prog)

    // Step 2: Forward slice from entries, first VTA pass
    fslice := forwardSlice(entrySlice, initial)
    vtaCg := vta.CallGraph(fslice, initial)

    // Step 3: Second VTA pass refining the first
    fslice = forwardSlice(entrySlice, vtaCg)
    cg := vta.CallGraph(fslice, vtaCg)

    cg.DeleteSyntheticNodes()
    return cg, nil
}
```

This is a **CHA → forward slice → VTA → forward slice → VTA** pipeline:
1. CHA builds the initial conservative call graph (fast, over-approximate)
2. Forward slice prunes to only functions reachable from entry points
3. VTA refines the call graph with type-flow analysis (reduces false edges from interface dispatch)
4. Repeat steps 2-3 for further refinement

This is the most battle-tested approach in the Go ecosystem for production reachability analysis. govulncheck runs this in CI across the entire Go ecosystem.

**Performance notes from govulncheck issues:**
- Source-level scanning can be slow on very large repos (users reported 30-36+ minutes in some cases — issues #54940, #68307)
- Binary mode is much faster but trades precision
- Pathological stack enumeration (many runtime stacks) caused hangs, later mitigated (#60708)

### Recommendation for go-reachable

**Default: CHA only.** Fast, conservative, good enough for "might touch this path" decisions.

**Optional: CHA + VTA (govulncheck-style).** For when CHA produces too many false positives in interface-heavy code. Offer via `--algorithm=vta` flag.

**Skip: Pointer analysis.** Too slow for CI on large repos. Reserve for offline analysis if ever needed.

**Skip: RTA.** CHA is simpler and nearly as fast; VTA is strictly more precise. RTA occupies an awkward middle ground for our use case.

---

## 4. Stage 3: Reachability Query

### The Call Graph Data Structure

`callgraph.Graph` contains:
- `Nodes map[*ssa.Function]*callgraph.Node` — one node per function
- Each `Node` has `In []*Edge` (callers) and `Out []*Edge` (callees)
- Each `Edge` has `Caller`, `Callee`, and `Site` (the `ssa.CallInstruction`)

### Forward Reachability

Simple BFS/DFS from path entry point nodes through `Out` edges. If any node in the reachable set corresponds to a changed symbol, the path is touched.

govulncheck does exactly this (forward slice from entries, backward slice from sinks, then intersects). For our use case, forward-only is sufficient since we just need "is changed symbol reachable?"

### Matching Changed Symbols to Call Graph Nodes

Changed symbols from Stage 1 are identified by `{package_path, function_name}` (or `{package_path, type_name, method_name}` for methods).

Call graph nodes are `*ssa.Function` which provide:
- `f.Package().Pkg.Path()` — package path
- `f.Name()` — function name
- `f.Signature.Recv()` — receiver type for methods

Match by comparing these fields. Handle generics by stripping type parameters (govulncheck does this with `strings.Cut(f.Name(), "[")`).

---

## 5. Existing Tools & Prior Art

### Directly Relevant

| Tool | What it does | Relevance | Status |
|------|-------------|-----------|--------|
| **govulncheck** (`golang.org/x/vuln`) | Entry points → reachable vulnerable symbols via SSA + CHA + VTA | Closest architectural match. Inverted direction from our use case. Internal APIs not importable but pattern is clear. | Production, v1.0+ |
| **gosemdiff** | AST-level semantic diff for Go. Classifies changes as MOVE/RENAME/REFACTOR/LOGIC via AST fingerprinting. | Could complement our diff→symbol mapping to filter noise. | Very early (Feb 2026) |
| **sem** (Ataraxy-Labs) | Multi-language semantic diff via tree-sitter. Entity-level changes, JSON output. | Alternative to go-diff + AST for diff→symbol mapping. | Active |
| **Symflower test-runner** | Package-level test impact analysis. | Coarser than function-level but validates the market. | Production |

### Related but Different

| Tool | What it does | Why not sufficient |
|------|-------------|-------------------|
| **gopls** | Call hierarchy via LSP (incoming/outgoing calls) | Editor-oriented, not batch CI. No programmatic API for full-graph reachability. |
| **guru** (`x/tools/cmd/guru`) | Had callers/callees/pointsto modes | **Deprecated** → gopls. |
| **`cmd/callgraph`** (`x/tools`) | CLI over callgraph algorithms | Dumps entire call graph, no diff integration or path concept. |
| **Bazel `rdeps`** | Build-graph level impact analysis | Requires Bazel. Google/Uber use this (Uber's "Changed Targets Calculation" with Merkle trees). |
| **Meta's predictive test selection** | ML + history to predict which tests to run | Not static analysis; requires historical data infrastructure. |

### Academic/Industry Context

- **Change Impact Analysis (CIA)** is a well-studied field. Classic approaches use dependency graphs, historical co-change, and static slicing.
- **Program slicing** (forward/backward) is the theoretical foundation. Forward slice from a changed line ≈ "what could this affect?" Our tool does forward reachability from entry points, checking intersection with changes.
- **Go test caching** (`go test` without `-count=1`) reuses results when inputs are unchanged. This is cache-driven, not diff-aware — it doesn't know which functions changed, only whether any file in the test's input set changed.

---

## 6. Key Libraries & Dependencies

```
# Core analysis pipeline
golang.org/x/tools/go/packages        # Package loading with modules support
golang.org/x/tools/go/ssa             # SSA intermediate representation
golang.org/x/tools/go/ssa/ssautil     # SSA construction utilities
golang.org/x/tools/go/callgraph       # Call graph data structure
golang.org/x/tools/go/callgraph/cha   # CHA algorithm (default)
golang.org/x/tools/go/callgraph/vta   # VTA algorithm (optional, higher precision)

# Diff → symbol mapping
golang.org/x/tools/go/ast/astutil     # PathEnclosingInterval
github.com/sourcegraph/go-diff        # Unified diff parsing

# Config & output
gopkg.in/yaml.v3                      # Config file parsing
```

All of these are well-maintained, widely used in the Go ecosystem, and have stable APIs (except VTA which is marked experimental but is used by govulncheck in production).

---

## 7. Risks & Open Questions

| Risk | Severity | Mitigation |
|------|----------|------------|
| **SSA build time on large repos** | High | Scope to changed packages + importers; offer package-level-only fast mode; cache call graphs between runs |
| **Interface dispatch over-approximation (CHA)** | Medium | CHA is conservative (safe for "might touch"); VTA available for precision when needed |
| **Reflection, `go:linkname`, CGo** | Low | Inherently invisible to static analysis; document as known limitation. Rare in application-level path code. |
| **Config drift (stale entry points)** | Medium | `go-reachable verify` subcommand to validate all entry points resolve to real symbols |
| **Generated code noise** | Low | Parse normally; optionally exclude files matching `*_gen.go` or containing `// Code generated` |
| **Generics** | Low | `ssa.InstantiateGenerics` handles this; VTA has generics-aware refinements |
| **Multiple build configurations** | Medium | Default to current GOOS/GOARCH; allow override via flags. May need multiple analysis passes for cross-platform code. |

### Open Design Questions

1. **Granularity of "changed":** Should we track function-level changes only, or also type changes (which affect interface dispatch), constant changes, etc.?
2. **Transitive type changes:** If a struct field type changes, should all methods on that struct be considered "changed"?
3. **Test code:** Should test functions be valid path entry points? Useful for "which test suites are affected" but different from production paths.
4. **Incremental analysis:** Can we cache and incrementally update the call graph, or is full rebuild on each PR acceptable?
5. **Monorepo support:** Should paths be able to span multiple Go modules?

---

## 8. Suggested Next Steps

1. **Spike: diff → symbols** — Parse a real Loki PR diff, map to changed functions using go-diff + go/ast. Validate accuracy.
2. **Spike: call graph from entry point** — Load a subset of Loki, build CHA call graph from one known entry point, measure time/memory.
3. **Spike: reachability query** — Combine the above: does entry point X reach changed function Y?
4. **Define CLI interface** — Flags, config format, output modes, exit codes.
5. **Build MVP** — Wire the pipeline together.

---

## References

- [govulncheck v1.0 announcement](https://go.dev/blog/govulncheck)
- [govulncheck tutorial](https://go.dev/doc/tutorial/govulncheck)
- [govulncheck source (vulncheck/utils.go)](https://go.googlesource.com/vuln/+/v1.1.4/internal/vulncheck/utils.go) — contains the CHA→VTA→VTA pipeline
- [govulncheck source (vulncheck/source.go)](https://go.googlesource.com/vuln/+/v1.1.4/internal/vulncheck/source.go) — entry point detection and call graph slicing
- [go/callgraph package](https://pkg.go.dev/golang.org/x/tools/go/callgraph)
- [CHA algorithm](https://pkg.go.dev/golang.org/x/tools/go/callgraph/cha)
- [VTA algorithm](https://pkg.go.dev/golang.org/x/tools/go/callgraph/vta)
- [RTA algorithm](https://pkg.go.dev/golang.org/x/tools/go/callgraph/rta)
- [go/packages](https://pkg.go.dev/golang.org/x/tools/go/packages)
- [go/ssa](https://pkg.go.dev/golang.org/x/tools/go/ssa)
- [astutil.PathEnclosingInterval](https://pkg.go.dev/golang.org/x/tools/go/ast/astutil#PathEnclosingInterval)
- [sourcegraph/go-diff](https://github.com/sourcegraph/go-diff)
- [ssautil.Reachable proposal (#69291)](https://github.com/golang/go/issues/69291)
- [govulncheck perf issues (#54940, #68307)](https://github.com/golang/go/issues/54940)
- [gopls navigation features](https://go.dev/gopls/features/navigation)
- [Uber: How we halved Go monorepo CI build time](https://www.uber.com/en-IN/blog/how-we-halved-go-monorepo-ci-build-time/)
- [Meta: Predictive test selection](https://engineering.fb.com/2019/08/22/developer-tools/predictive-test-selection/)
- [Symflower test impact analysis](https://symflower.com/en/company/blog/2024/test-impact-analysis)
