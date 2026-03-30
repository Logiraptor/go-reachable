package reachable

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Logiraptor/go-reachable/internal/callgraph"
	"github.com/Logiraptor/go-reachable/internal/diffsyms"
)

// Result describes whether a given entry point can reach any changed symbol.
type Result struct {
	Entry   callgraph.EntryPoint `json:"entry"`
	Touched bool                 `json:"touched"`
	Matches []Match              `json:"matches,omitempty"`
	Stats   callgraph.Stats      `json:"stats"`
}

// Match records a changed symbol that is reachable from the entry point.
type Match struct {
	Symbol   diffsyms.Symbol        `json:"symbol"`
	Function callgraph.ReachableFunc `json:"function"`
}

// Options configures the reachability analysis.
type Options struct {
	RepoDir   string
	Entry     callgraph.EntryPoint
	Algorithm callgraph.Algorithm
}

// VTAAnchor is JSON metadata for the main package used to build the call graph.
type VTAAnchor struct {
	Package string `json:"package"`
	Func    string `json:"func"`
}

// PathQuery pairs a stable name with a reachability entry point.
type PathQuery struct {
	Name  string
	Entry callgraph.EntryPoint
}

// MultiOptions configures multi-path analysis (one graph build, many walks).
type MultiOptions struct {
	RepoDir     string
	Algorithm   callgraph.Algorithm
	GraphEntry  callgraph.EntryPoint // VTA anchor: main package + main (or cha-pruned anchor)
	VTAMeta     VTAAnchor            // echoed in JSON (package + func name)
	PathQueries []PathQuery
}

// PathResult is reachability for one named path.
type PathResult struct {
	Name               string               `json:"name"`
	Entry              callgraph.EntryPoint `json:"entry"`
	Touched            bool                 `json:"touched"`
	Matches            []Match              `json:"matches,omitempty"`
	WalkTime           time.Duration        `json:"walk_time"`
	ReachableFunctions int                  `json:"reachable_functions"`
}

// MultiResult is the full multi-path output.
type MultiResult struct {
	VTA   VTAAnchor    `json:"vta"`
	Paths []PathResult `json:"paths"`
	Stats AggregateStats `json:"stats"`
}

// AggregateStats combines one build with summed walk time.
type AggregateStats struct {
	LoadTime       time.Duration `json:"load_time"`
	SSABuildTime   time.Duration `json:"ssa_build_time"`
	CGBuildTime    time.Duration `json:"cg_build_time"`
	WalkTime       time.Duration `json:"walk_time"` // sum of per-path walks
	TotalTime      time.Duration `json:"total_time"`
	PackagesLoaded int           `json:"packages_loaded"`
	TotalFunctions int           `json:"total_functions"`
	CGNodes        int           `json:"cg_nodes"`
	CGEdges        int           `json:"cg_edges"`
}

// Analyze runs the full pipeline: builds a call graph from the entry point,
// then checks whether any of the changed symbols appear in the reachable set.
func Analyze(opts Options, symbols []diffsyms.Symbol) (Result, error) {
	result := Result{Entry: opts.Entry}

	modulePath, err := readModulePath(opts.RepoDir)
	if err != nil {
		return result, fmt.Errorf("reading go.mod: %w", err)
	}

	reachable, stats, err := callgraph.Analyze(opts.RepoDir, opts.Entry, opts.Algorithm)
	if err != nil {
		return result, err
	}
	result.Stats = stats

	index := buildReachableIndex(reachable)
	matches, touched := matchChangedSymbols(modulePath, index, symbols)
	result.Matches = matches
	result.Touched = touched

	return result, nil
}

// AnalyzePaths builds one call graph from GraphEntry, then runs a forward slice
// from each path query. Multi-path mode with CHA is not supported (load graph
// would omit packages).
func AnalyzePaths(opts MultiOptions, symbols []diffsyms.Symbol) (MultiResult, error) {
	var out MultiResult
	out.VTA = opts.VTAMeta

	if len(opts.PathQueries) == 0 {
		return out, fmt.Errorf("no path queries")
	}
	if len(opts.PathQueries) > 1 && opts.Algorithm == callgraph.AlgoCHA {
		return out, fmt.Errorf("multi-path analysis requires algorithm vta or cha-pruned, not cha")
	}

	modulePath, err := readModulePath(opts.RepoDir)
	if err != nil {
		return out, fmt.Errorf("reading go.mod: %w", err)
	}

	totalStart := time.Now()

	a, buildStats, err := callgraph.BuildGraph(opts.RepoDir, opts.GraphEntry, opts.Algorithm)
	if err != nil {
		return out, err
	}

	out.Stats = AggregateStats{
		LoadTime:       buildStats.LoadTime,
		SSABuildTime:   buildStats.SSABuildTime,
		CGBuildTime:    buildStats.CGBuildTime,
		PackagesLoaded: buildStats.PackagesLoaded,
		TotalFunctions: buildStats.TotalFunctions,
		CGNodes:        buildStats.CGNodes,
		CGEdges:        buildStats.CGEdges,
	}

	var sumWalk time.Duration
	out.Paths = make([]PathResult, 0, len(opts.PathQueries))

	for _, q := range opts.PathQueries {
		reachableFuncs, walkTime, err := callgraph.ReachableFrom(a, q.Entry)
		if err != nil {
			return out, fmt.Errorf("path %q: %w", q.Name, err)
		}
		sumWalk += walkTime

		index := buildReachableIndex(reachableFuncs)
		matches, touched := matchChangedSymbols(modulePath, index, symbols)

		out.Paths = append(out.Paths, PathResult{
			Name:               q.Name,
			Entry:              q.Entry,
			Touched:            touched,
			Matches:            matches,
			WalkTime:           walkTime,
			ReachableFunctions: len(reachableFuncs),
		})
	}

	out.Stats.WalkTime = sumWalk
	out.Stats.TotalTime = time.Since(totalStart)

	return out, nil
}

func matchChangedSymbols(modulePath string, index map[string]callgraph.ReachableFunc, symbols []diffsyms.Symbol) ([]Match, bool) {
	var matches []Match
	for _, sym := range symbols {
		if sym.Kind == "file" || sym.Kind == "type" || sym.Kind == "var" || sym.Kind == "const" {
			continue
		}

		fqPkg := qualifyPackage(modulePath, sym.Package)

		key := matchKey(fqPkg, sym.Receiver, sym.Name)
		if rf, ok := index[key]; ok {
			matches = append(matches, Match{Symbol: sym, Function: rf})
			continue
		}

		if sym.Receiver != "" && !strings.HasPrefix(sym.Receiver, "*") {
			ptrKey := matchKey(fqPkg, "*"+sym.Receiver, sym.Name)
			if rf, ok := index[ptrKey]; ok {
				matches = append(matches, Match{Symbol: sym, Function: rf})
			}
		}
	}
	return matches, len(matches) > 0
}

// matchKey builds a lookup key for a function/method in the reachable index.
func matchKey(pkg, receiver, name string) string {
	if receiver != "" {
		recv := strings.TrimPrefix(receiver, "*")
		return pkg + ".(*" + recv + ")." + name
	}
	return pkg + "." + name
}

// buildReachableIndex creates a map from "pkg.FuncName" or "pkg.(*Type).Method"
// to the ReachableFunc, for O(1) lookups.
func buildReachableIndex(funcs []callgraph.ReachableFunc) map[string]callgraph.ReachableFunc {
	idx := make(map[string]callgraph.ReachableFunc, len(funcs))
	for _, f := range funcs {
		key := f.Package + "." + f.Name
		idx[key] = f
	}
	return idx
}

// qualifyPackage converts a directory-relative package path to a full Go import
// path using the module path from go.mod.
//
// Example: modulePath="github.com/grafana/loki/v3", relPkg="pkg/querier"
// → "github.com/grafana/loki/v3/pkg/querier"
func qualifyPackage(modulePath, relPkg string) string {
	if relPkg == "." || relPkg == "" {
		return modulePath
	}
	return modulePath + "/" + relPkg
}

// readModulePath extracts the module path from go.mod in the given directory.
func readModulePath(dir string) (string, error) {
	f, err := os.Open(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")), nil
		}
	}
	return "", fmt.Errorf("no module directive found in %s/go.mod", dir)
}
