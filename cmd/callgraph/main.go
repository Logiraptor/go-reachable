package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Logiraptor/go-reachable/internal/callgraph"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "callgraph: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dir       string
		pkg       string
		receiver  string
		funcName  string
		algo      string
		outputFmt string
		maxDepth  int
		topN      int
	)

	flag.StringVar(&dir, "dir", ".", "path to the Go module root")
	flag.StringVar(&pkg, "pkg", "", "full import path of the entry point package (required)")
	flag.StringVar(&receiver, "recv", "", "receiver type name for methods (e.g., \"*Distributor\")")
	flag.StringVar(&funcName, "func", "", "function or method name (required)")
	flag.StringVar(&algo, "algo", "cha", "call graph algorithm: cha, vta, cha-pruned")
	flag.StringVar(&outputFmt, "format", "text", "output format: text, json, stats")
	flag.IntVar(&maxDepth, "depth", -1, "max BFS depth to display (-1 = unlimited)")
	flag.IntVar(&topN, "top", 0, "show only the top N packages by reachable function count (0 = show all)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `callgraph — build a call graph from an entry point and walk reachable functions

Usage:
  callgraph -dir ~/workspace/loki -pkg github.com/grafana/loki/v3/pkg/distributor -recv "*Distributor" -func PushHandler
  callgraph -dir ~/workspace/loki -pkg github.com/grafana/loki/v3/pkg/distributor -recv "*Distributor" -func PushHandler -algo vta
  callgraph -dir . -pkg ./cmd/myapp -func main

Flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	if pkg == "" || funcName == "" {
		flag.Usage()
		return fmt.Errorf("-pkg and -func are required")
	}

	dir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolving dir: %w", err)
	}

	entry := callgraph.EntryPoint{
		Package:  pkg,
		Receiver: receiver,
		Func:     funcName,
	}

	algorithm := callgraph.AlgoCHA
	switch strings.ToLower(algo) {
	case "vta":
		algorithm = callgraph.AlgoVTA
	case "cha-pruned":
		algorithm = callgraph.AlgoCHAPruned
	}

	fmt.Fprintf(os.Stderr, "entry:     %s\n", entry)
	fmt.Fprintf(os.Stderr, "algorithm: %s\n", algorithm)
	fmt.Fprintf(os.Stderr, "loading...\n")

	reachable, stats, err := callgraph.Analyze(dir, entry, algorithm)
	if err != nil {
		return err
	}

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	switch outputFmt {
	case "json":
		return printJSON(reachable, stats, maxDepth)
	case "stats":
		return printStats(stats, memStats)
	default:
		printSummary(reachable, stats, memStats, maxDepth, topN)
		return nil
	}
}

func printStats(stats callgraph.Stats, mem runtime.MemStats) error {
	fmt.Printf("Load:       %v\n", stats.LoadTime)
	fmt.Printf("SSA build:  %v\n", stats.SSABuildTime)
	fmt.Printf("CG build:   %v\n", stats.CGBuildTime)
	fmt.Printf("Walk:       %v\n", stats.WalkTime)
	fmt.Printf("Total:      %v\n", stats.TotalTime)
	fmt.Printf("Packages:   %d\n", stats.PackagesLoaded)
	fmt.Printf("Functions:  %d (total SSA)\n", stats.TotalFunctions)
	fmt.Printf("CG nodes:   %d\n", stats.CGNodes)
	fmt.Printf("CG edges:   %d\n", stats.CGEdges)
	fmt.Printf("Reachable:  %d\n", stats.Reachable)
	fmt.Printf("Heap alloc: %d MB\n", mem.HeapAlloc/1024/1024)
	fmt.Printf("Sys mem:    %d MB\n", mem.Sys/1024/1024)
	return nil
}

func printSummary(reachable []callgraph.ReachableFunc, stats callgraph.Stats, mem runtime.MemStats, maxDepth, topN int) {
	fmt.Fprintf(os.Stderr, "\n--- stats ---\n")
	fmt.Fprintf(os.Stderr, "load:       %v\n", stats.LoadTime)
	fmt.Fprintf(os.Stderr, "ssa build:  %v\n", stats.SSABuildTime)
	fmt.Fprintf(os.Stderr, "cg build:   %v\n", stats.CGBuildTime)
	fmt.Fprintf(os.Stderr, "walk:       %v\n", stats.WalkTime)
	fmt.Fprintf(os.Stderr, "total:      %v\n", stats.TotalTime)
	fmt.Fprintf(os.Stderr, "packages:   %d\n", stats.PackagesLoaded)
	fmt.Fprintf(os.Stderr, "functions:  %d (total SSA)\n", stats.TotalFunctions)
	fmt.Fprintf(os.Stderr, "cg nodes:   %d\n", stats.CGNodes)
	fmt.Fprintf(os.Stderr, "cg edges:   %d\n", stats.CGEdges)
	fmt.Fprintf(os.Stderr, "reachable:  %d\n", stats.Reachable)
	fmt.Fprintf(os.Stderr, "heap alloc: %d MB\n", mem.HeapAlloc/1024/1024)
	fmt.Fprintf(os.Stderr, "sys mem:    %d MB\n", mem.Sys/1024/1024)
	fmt.Fprintf(os.Stderr, "---\n\n")

	// Aggregate by package.
	pkgCounts := make(map[string]int)
	maxBFSDepth := 0
	for _, f := range reachable {
		if maxDepth >= 0 && f.Depth > maxDepth {
			continue
		}
		pkgCounts[f.Package]++
		if f.Depth > maxBFSDepth {
			maxBFSDepth = f.Depth
		}
	}

	type pkgStat struct {
		pkg   string
		count int
	}
	sorted := make([]pkgStat, 0, len(pkgCounts))
	for p, c := range pkgCounts {
		sorted = append(sorted, pkgStat{p, c})
	}
	// Sort by count descending.
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].count > sorted[i].count {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	limit := len(sorted)
	if topN > 0 && topN < limit {
		limit = topN
	}

	fmt.Printf("Packages reachable from entry point (%d packages, %d functions, max depth %d):\n\n", len(pkgCounts), stats.Reachable, maxBFSDepth)
	for i := 0; i < limit; i++ {
		fmt.Printf("  %4d  %s\n", sorted[i].count, sorted[i].pkg)
	}
	if topN > 0 && topN < len(sorted) {
		fmt.Printf("  ... and %d more packages\n", len(sorted)-topN)
	}
}

func printJSON(reachable []callgraph.ReachableFunc, stats callgraph.Stats, maxDepth int) error {
	filtered := reachable
	if maxDepth >= 0 {
		filtered = nil
		for _, f := range reachable {
			if f.Depth <= maxDepth {
				filtered = append(filtered, f)
			}
		}
	}

	out := struct {
		Stats     callgraph.Stats          `json:"stats"`
		Functions []callgraph.ReachableFunc `json:"functions"`
	}{
		Stats:     stats,
		Functions: filtered,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
