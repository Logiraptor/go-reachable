# Spike 002: Call Graph from Entry Point

**Date:** 2026-03-29
**Status:** Complete
**Validates:** Research §3 (Call Graph Construction) and §4 (Reachability Query) from [001-static-analysis-landscape.md](../research/001-static-analysis-landscape.md)

---

## Goal

Load a real Go package (Loki's distributor), build a call graph from a known API endpoint handler, walk forward to enumerate all reachable functions, and measure time/memory. Compare CHA vs VTA algorithms.

## Test Subject

**Loki `/loki/api/v1/push` endpoint** — handled by `(*Distributor).PushHandler` in `github.com/grafana/loki/v3/pkg/distributor`. This is Loki's primary write path: receives log pushes over HTTP, validates, distributes to ingesters. A real production endpoint with deep call chains through protobuf parsing, tenant validation, ring-based distribution, and storage backends.

Also tested: `(*Querier).TailHandler` in `github.com/grafana/loki/v3/pkg/querier/tail` — the `/loki/api/v1/tail` WebSocket endpoint.

## What We Built

Library (`internal/callgraph`) and CLI (`cmd/callgraph`) implementing:

1. **Package loading** via `golang.org/x/tools/go/packages` — loads the entry point package with full type info, then collects all transitive dependencies for SSA.
2. **SSA construction** via `golang.org/x/tools/go/ssa` + `ssautil.AllPackages` — builds the SSA IR with `InstantiateGenerics` for generics support.
3. **Call graph construction** — CHA (default) or the govulncheck-style CHA→VTA→VTA pipeline.
4. **Forward BFS walk** from the entry point node through call graph edges, recording each reachable function with its BFS depth.

## Results

### PushHandler (`/loki/api/v1/push`)

| Metric | CHA | VTA |
|--------|-----|-----|
| **Load time** | 2.2s | 2.3s |
| **SSA build** | 1.0s | 0.9s |
| **CG build** | 5.1s | 16.8s |
| **Walk** | 0.5s | 47ms |
| **Total** | 9.1s | 21.4s |
| **Packages loaded** | 1,958 | 1,958 |
| **Total SSA functions** | 234,989 | 234,987 |
| **CG nodes** | 235,151 | 38,476 |
| **CG edges** | 18,723,434 | 319,108 |
| **Reachable functions** | 131,375 | 19,618 |
| **Reachable packages** | 1,780 | 728 |
| **Max BFS depth** | 35 | 40 |
| **Heap alloc** | 6.6 GB | 7.7 GB |
| **Sys memory** | 7.8 GB | 9.1 GB |

### TailHandler (`/loki/api/v1/tail`)

| Metric | CHA | VTA |
|--------|-----|-----|
| **Total** | 9.6s | 21.9s |
| **CG nodes** | 235,825 | 38,236 |
| **CG edges** | 18,901,492 | 322,363 |
| **Reachable functions** | 131,890 | 19,701 |
| **Heap alloc** | 5.6 GB | 8.5 GB |

### go-reachable itself (small repo baseline)

| Metric | CHA |
|--------|-----|
| **Total** | 385ms |
| **Packages** | 72 |
| **SSA functions** | 8,694 |
| **Reachable** | 4,326 |
| **Heap alloc** | 257 MB |

## Key Findings

### 1. CHA is fast but extremely over-approximate

CHA considers every concrete type that could satisfy an interface method call, regardless of whether that type is ever constructed in the reachable code. For Loki, this means PushHandler "reaches" 131K functions across 1,780 packages — including SQLite, AWS S3, Cassandra, Kafka, and every other storage backend, even though PushHandler only uses one.

The top CHA packages (modernc.org/sqlite, aws-sdk-go-v2/s3, franz-go/kmsg) are clearly false positives for a push handler analysis. CHA is useful as a conservative upper bound but too noisy for "which paths does this change touch?" decisions.

### 2. VTA dramatically prunes the graph

VTA reduces reachable functions by **85%** (131K → 19.6K) and packages by **59%** (1,780 → 728). The CG edge count drops from 18.7M to 319K — a **98.3% reduction**. This is because VTA tracks actual type flow through SSA variables, pruning interface dispatch edges where the concrete type never flows to that call site.

The tradeoff: CG build time increases from 5s to 17s (3.4x), and peak memory rises from 6.6 GB to 7.7 GB. The walk itself is much faster (47ms vs 475ms) because the graph is smaller.

### 3. The govulncheck CHA→VTA→VTA pipeline works as documented

The two-pass VTA refinement from the research doc (CHA → forward slice → VTA → forward slice → VTA) works correctly. The forward slice between passes is critical — it prunes the function set before each VTA pass, keeping VTA's type-flow analysis tractable.

### 4. Package loading dominates small-repo time; CG build dominates large-repo time

For go-reachable (72 packages): load=253ms, SSA=88ms, CG=26ms — loading is the bottleneck.
For Loki (1,958 packages): load=2.2s, SSA=1.0s, CG=5.1s (CHA) / 16.8s (VTA) — CG build dominates.

### 5. Method lookup via `types.MethodSet` works cleanly

Finding an SSA function for a method like `(*Distributor).PushHandler` requires looking up the type in the SSA package, constructing a pointer type, getting its method set, and calling `prog.MethodValue()`. This is more reliable than string-matching against `ssautil.AllFunctions()`.

### 6. Both endpoints converge to similar reachable sets

PushHandler and TailHandler reach nearly identical function counts (131K vs 131.9K for CHA, 19.6K vs 19.7K for VTA). This suggests CHA's over-approximation saturates the graph — most of Loki's dependency tree becomes reachable through interface dispatch. VTA shows more differentiation but still converges because both endpoints share core infrastructure (logging, HTTP, gRPC, protobuf).

This is expected for a monolith. The real value of path reachability will come from **intersecting** the reachable set with changed symbols (Stage 3), not from the reachable set size alone.

## What Worked Well

1. **`golang.org/x/tools/go/packages`** handles Loki's complex module setup (v3 suffix, 1,958 transitive deps) without issues. The `NeedDeps` mode correctly resolves the full dependency graph.

2. **`ssautil.AllPackages`** is the right way to build SSA from `packages.Load` output. It handles the `*packages.Package` → `*ssa.Package` mapping and builds all dependencies.

3. **`ssa.InstantiateGenerics`** is essential. Without it, generic functions aren't monomorphized and call edges through generics are lost.

4. **`callgraph/cha` and `callgraph/vta`** have stable, simple APIs. CHA takes a `*ssa.Program`, VTA takes `map[*ssa.Function]bool` + initial graph. Both return `*callgraph.Graph` with the same node/edge structure.

5. **BFS with depth tracking** gives useful structural information. The max depth (35-40 for Loki) indicates how deep the call chains go, which helps calibrate expectations for path analysis.

## Gotchas Discovered

1. **`ssautil.AllFunctions` returns `map[*ssa.Function]bool`, not a slice.** The VTA API also takes `map[*ssa.Function]bool`. This is consistent but different from what you might expect.

2. **`packages.Load` loads the entry package + deps, not the whole module.** If you load `pkg/distributor`, you get distributor and everything it imports, but not `pkg/querier` unless distributor imports it. This is correct for our use case (we only need the entry point's reachable set) but means the "total SSA functions" count varies by entry point.

3. **Memory is significant.** 6-9 GB for Loki analysis. This is fine for CI (which typically has 8-16 GB) but rules out running on developer laptops for the largest repos without scoping. The research doc's suggestion to "scope to changed packages + importers" is important for production use.

4. **CHA builds the full program call graph, not a rooted one.** Unlike VTA (which takes a function set), CHA always analyzes the entire program. The forward BFS walk then extracts the rooted subgraph. This means CHA's build time doesn't benefit from a narrow entry point — it's always O(program size).

5. **The 29K "empty package" functions in CHA** are SSA-internal synthetic functions (init, wrappers, thunks) with no package. These inflate the count but are harmless.

## Performance Assessment

| Scenario | CHA | VTA | Verdict |
|----------|-----|-----|---------|
| **Small repo** (72 pkgs) | 385ms | N/A | Either algorithm is fine |
| **Large repo** (2K pkgs) | ~9s | ~22s | CHA acceptable for CI; VTA if precision needed |
| **Memory** | 6-7 GB | 8-9 GB | Both need CI-class memory |

For the go-reachable MVP: **default to CHA** for speed, offer `--algo=vta` for precision. The 85% reduction in false positives from VTA is significant, but 9s CHA is fast enough for CI and the reachability *intersection* with changed symbols (Stage 3) will further reduce noise.

## Open Questions for Stage 3

1. **Symbol matching granularity:** The call graph nodes are `*ssa.Function` with full package paths. Stage 1's symbols use directory-relative paths. We need to bridge these — either by reading `go.mod` to construct full import paths in Stage 1, or by stripping module prefixes in Stage 2.

2. **Closure handling:** SSA represents closures as separate `*ssa.Function` objects (e.g., `PushHandler$1`). If a diff changes code inside a closure, Stage 1 maps it to the enclosing `FuncDecl`. We need to match the enclosing function OR enumerate its closures in the call graph.

3. **Init functions:** Package `init()` functions are called implicitly. If a diff changes an `init()`, it potentially affects everything that imports that package. The call graph includes init edges, but we may want special handling.

4. **Scoping for performance:** Loading all 1,958 packages for every analysis is wasteful. For Stage 3, we should scope: load only the packages that contain changed symbols + their transitive importers. This requires a two-phase approach: quick `NeedImports`-only load to build the import graph, then full load of the relevant subset.

5. **Caching:** The SSA build (1s) and CG build (5-17s) are the expensive parts. Can we cache the call graph between CI runs and incrementally update it? The `callgraph.Graph` structure isn't serializable out of the box, but we could serialize the adjacency list with function identifiers.

## CLI Usage

```bash
# CHA analysis (default, ~9s for Loki)
callgraph -dir ~/workspace/loki \
  -pkg github.com/grafana/loki/v3/pkg/distributor \
  -recv "*Distributor" \
  -func PushHandler

# VTA analysis (more precise, ~22s)
callgraph -dir ~/workspace/loki \
  -pkg github.com/grafana/loki/v3/pkg/distributor \
  -recv "*Distributor" \
  -func PushHandler \
  -algo vta

# Stats only
callgraph -dir ~/workspace/loki \
  -pkg github.com/grafana/loki/v3/pkg/distributor \
  -recv "*Distributor" \
  -func PushHandler \
  -format stats

# JSON output (for scripting)
callgraph -dir ~/workspace/loki \
  -pkg github.com/grafana/loki/v3/pkg/distributor \
  -recv "*Distributor" \
  -func PushHandler \
  -format json -depth 3

# Top 20 packages by function count
callgraph -dir ~/workspace/loki \
  -pkg github.com/grafana/loki/v3/pkg/distributor \
  -recv "*Distributor" \
  -func PushHandler \
  -top 20

# Plain function (no receiver)
callgraph -dir ~/workspace/loki \
  -pkg github.com/grafana/loki/v3/pkg/distributor \
  -func NewDistributor

# Works on any Go module
callgraph -dir . -pkg ./internal/diffsyms -func ChangedSymbols
```
