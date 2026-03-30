# Research: How VTA Produces False Negatives in Interface-Heavy Go Code

**Date:** 2026-03-29
**Status:** Complete
**Context:** Analysis of the VTA false negative observed in [Spike 003](../spikes/003-reachability-query.md) (PR #21112 vs RangeQueryHandler)

---

## Summary

VTA (Variable Type Analysis) missed the call edge from Loki's query middleware to `(*Codec).DecodeRequest` because the concrete `*Codec` type is stored into an interface-typed struct field during server initialization, but the field is read during request handling — a separate call tree. VTA's type propagation graph never connects the two because the initialization function falls outside the forward slice from the entry point.

This is not a bug in VTA. It is a fundamental limitation of how VTA scopes its analysis, and it affects any Go codebase that follows the idiomatic "construct at init time, use at request time" pattern with interface dispatch.

---

## What VTA Does

VTA resolves dynamic calls (interface dispatch and function values) by tracking which concrete types actually flow to each call site through SSA variables. It builds a **type propagation graph** — a directed graph separate from the call graph — where:

- **Nodes** represent program constructs that hold types: local variables, struct fields, function parameters, return values, globals, slice/map/channel elements.
- **Edges** represent type flow: assignments, parameter passing, return values, field reads/writes.
- **Initial labels**: each node starts labeled with its declared type. Concrete-type nodes (like `function{f: someFunc}`) carry their concrete type. Interface-typed nodes start empty — they receive types through propagation.

VTA computes SCCs (strongly connected components) on this graph and propagates types in reverse topological order. After propagation, each node is labeled with the conservative overapproximation of concrete types that can reach it at runtime.

For an interface call site like `codec.DecodeRequest(r)`, VTA looks at the `local` node for the `codec` variable and asks: "which concrete types propagated to this node?" It intersects those types with the initial CHA call graph's callees to produce the refined edge set.

**The critical rule**: if no concrete type propagates to an interface variable's node, VTA produces **zero call edges** from that call site. The call vanishes from the graph.

---

## The Loki Architecture That Triggers It

### Layer 1: Initialization (roundtrip.go)

```go
var codec base.Codec = DefaultCodec  // DefaultCodec is *queryrange.Codec{}
```

A concrete `*Codec` value is created and assigned to a variable typed as `base.Codec` (an interface). This is the only place where the concrete type enters the interface.

### Layer 2: Middleware Factory Functions

The `codec` variable is passed into ~10 different middleware factory functions:

```go
metricsTripperware, err := NewMetricTripperware(cfg, ..., codec, ...)
logFilterTripperware, err := NewLogFilterTripperware(cfg, ..., codec, ...)
```

Each factory accepts `codec base.Codec` (interface parameter) and stores it inside a struct field:

```go
type metricMiddleware struct {
    codec  base.Codec
    limits Limits
}
```

### Layer 3: Middleware Wrapping

These middleware structs implement `base.Middleware` (another interface). They get composed via `MergeMiddlewares`:

```go
func MergeMiddlewares(middleware ...Middleware) Middleware {
    return MiddlewareFunc(func(next Handler) Handler {
        for i := len(middleware) - 1; i >= 0; i-- {
            next = middleware[i].Wrap(next)
        }
        return next
    })
}
```

The `codec` is now buried inside a struct, inside a closure, inside a `MiddlewareFunc`, inside another `Handler`.

### Layer 4: The Actual Call

Deep inside one of these middleware handlers, the codec's `DecodeRequest` is called:

```go
func (m *someMiddleware) Do(ctx context.Context, req Request) (Response, error) {
    decoded, err := m.codec.DecodeRequest(ctx, httpReq, ...)
}
```

---

## Step-by-Step: How VTA Loses the Type

### Step 1: Concrete type enters the interface

```go
var codec base.Codec = DefaultCodec
```

In SSA, this is a `MakeInterface` instruction. VTA creates an edge:

```
Local(DefaultCodec) → Local(codec)
```

The concrete type `*queryrange.Codec` is the initial label on the `DefaultCodec` node. It flows to the `codec` local. So far so good.

### Step 2: Codec is passed as a function argument

```go
NewMetricTripperware(cfg, ..., codec, ...)
```

VTA creates edges between the argument and the parameter:

```
Local(codec) → Local(NewMetricTripperware.param_codec)
```

The type propagates. Still fine.

### Step 3: Codec is stored in a struct field

Inside `NewMetricTripperware`:

```go
m := &metricMiddleware{codec: codec}
```

This is a `FieldAddr` + `Store` in SSA. VTA creates bidirectional edges between the struct field and the value:

```
Local(codec_param) ↔ Field(metricMiddleware:codec)
```

The concrete type propagates to the field node. Still tracking.

### Step 4: The middleware struct is returned as an interface

```go
return m  // *metricMiddleware returned as base.Middleware interface
```

This is another `MakeInterface`. VTA tracks the type `*metricMiddleware` flowing into the `Middleware` interface. But it does **not** recursively track what's inside the struct's fields. The type propagation graph has no concept of "the `codec` field of whatever concrete type flows through this interface."

### Step 5: Middleware is composed via MergeMiddlewares

The middleware values flow through variadic parameters, into a slice, through a closure, and back out as a `MiddlewareFunc`. VTA tracks the `*metricMiddleware` type flowing through these containers, but the field contents are not part of the type propagation — they're structural, not type-flow.

### Step 6: Wrap is called on the middleware interface

```go
next = middleware[i].Wrap(next)
```

Interface dispatch on `Middleware.Wrap`. VTA resolves this correctly (it knows `*metricMiddleware` implements `Middleware`), so it creates the call edge to `(*metricMiddleware).Wrap`. Still fine at this level.

### Step 7: Inside Wrap, the handler closure captures the struct

```go
func (m *metricMiddleware) Wrap(next Handler) Handler {
    return HandlerFunc(func(ctx context.Context, req Request) (Response, error) {
        // m.codec.DecodeRequest(...)  ← the critical call
    })
}
```

The closure captures `m` (the receiver). VTA models this via `MakeClosure` edges. The receiver `m` is `*metricMiddleware` — a concrete type. VTA can track this.

### Step 8: The codec field is read from the struct — THIS IS WHERE IT BREAKS

Inside the closure:

```go
m.codec.DecodeRequest(ctx, httpReq)
```

In SSA, this becomes:

1. `t1 = &m.codec` (FieldAddr) — get pointer to the `codec` field
2. `t2 = *t1` (UnOp/MUL) — load the interface value
3. `t3 = invoke t2.DecodeRequest(...)` — interface dispatch

For step (1), VTA creates a `field` node `Field(metricMiddleware:codec)`. For step (2), it creates alias edges between the loaded value and the field. For step (3), it looks at the `local` node for `t2` and asks: **what concrete types reached this node?**

The type propagation from Step 3 (where `*Codec` was stored into the field) happened in `NewMetricTripperware` — a different function. VTA only creates interprocedural type flow edges for function parameters and return values, not for struct field mutations that happen in other functions.

The `Field(metricMiddleware:codec)` node is a shared, global node — all reads and writes to that field across the program contribute to it. But VTA only analyzes functions in the **forward slice** from the entry point.

### Step 9: The forward slice excludes the initialization function

The go-reachable VTA pipeline (matching govulncheck):

```go
initial := cha.CallGraph(prog)
fslice := forwardSlice(entryFunc, initial)
vtaCg := vta.CallGraph(fslice, initial)
```

The `forwardSlice` walks forward from `RangeQueryHandler` through the CHA graph to find reachable functions. VTA only analyzes those functions.

`NewMetricTripperware` (the function that stores `*Codec` into the struct field) is an initialization function called during server startup, not during request handling. `RangeQueryHandler` is called per-request. The call chains are:

```
main() → NewLokiCodec() → NewMetricTripperware()     ← initialization time
RangeQueryHandler() → middleware.Do() → m.codec.DecodeRequest()  ← request time
```

These are two separate call trees. The CHA forward slice from `RangeQueryHandler` follows the request-time path. It reaches `middleware.Do()` and the `DecodeRequest` call site. But it does not reach `NewMetricTripperware` because that's called from the server initialization path, not from the request handler.

### Step 10: The result — a phantom call site

VTA sees the call `m.codec.DecodeRequest(...)` but the `local` node for `m.codec` has no concrete types propagated to it. The `Field(metricMiddleware:codec)` node was never populated because the `Store` instruction that populates it lives in a function outside the forward slice.

With no types at the call site, VTA resolves it to zero callees. The edge from the middleware to `(*Codec).DecodeRequest` is pruned. `DecodeRequest` becomes unreachable. **False negative.**

---

## Why CHA Doesn't Have This Problem

CHA doesn't track type flow at all. When it sees:

```go
m.codec.DecodeRequest(ctx, httpReq)
```

It asks: "what concrete types in the entire program implement the `base.Codec` interface and have a `DecodeRequest` method?" The answer includes `*queryrange.Codec`. CHA adds the edge unconditionally. It doesn't care whether the concrete type actually flows to this particular variable — it only cares that it could.

This is why CHA finds 162,971 reachable functions (over-approximate) while VTA finds only 24,151 (under-approximate in this case). CHA's "imprecision" is exactly what makes it safe for reachability questions.

---

## The General Pattern

VTA loses type flow whenever:

1. A concrete type is **stored into an interface-typed struct field** during initialization
2. The struct is **later used** during request handling (a different call tree)
3. The **initialization function is outside the forward slice** from the entry point being analyzed

This pattern is extremely common in Go HTTP servers:

- **Dependency injection at startup**: store implementations into interface fields
- **Middleware wrapping**: compose handlers that capture interface-typed dependencies
- **Factory functions**: return structs with interface fields populated

VTA's false negatives are systematic, not random. They occur at every point where Go's idiomatic "construct at init time, use at request time" pattern meets interface dispatch.

---

## Implications for go-reachable

For a tool that needs to answer "might this change affect this path?":

- **CHA is the only safe default.** False positives (saying reachable when it isn't) trigger an unnecessary CI job. False negatives (saying NOT reachable when it IS) skip a needed CI job.
- **VTA should only be used as a secondary filter** when CHA produces too many false positives, and only with the understanding that it may miss real edges.
- **The forward-slice scoping is the root cause.** If VTA analyzed the entire program (not just the forward slice), it would see the `Store` instruction and propagate the type correctly. But whole-program VTA is prohibitively expensive on large codebases.

---

## References

- [VTA source: graph.go](https://go.googlesource.com/tools/+/refs/heads/master/go/callgraph/vta/graph.go) — type propagation graph construction
- [VTA source: vta.go](https://go.googlesource.com/tools/+/refs/heads/master/go/callgraph/vta/vta.go) — call graph construction from propagated types
- [VTA source: propagation.go](https://go.googlesource.com/tools/+/refs/heads/master/go/callgraph/vta/propagation.go) — SCC-based type propagation
- [govulncheck CHA→VTA→VTA pipeline](https://go.googlesource.com/vuln/+/v1.1.4/internal/vulncheck/utils.go)
- Sundaresan et al., "Practical Virtual Method Call Resolution for Java" — original VTA paper
