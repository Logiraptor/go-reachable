package callgraph

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// Default thresholds for cha-pruned: interfaces where VTA prunes at least this
// fraction of CHA's invoke edges, and at least this many excess edges.
const (
	DefaultPruneRatioThreshold = 0.95
	DefaultMinExcessEdges      = 100
)

// InterfaceEdgeStats summarizes the CHA-only edges attributable to a single
// interface type. These are edges that CHA produces via interface dispatch
// that VTA prunes — i.e. likely false positives.
type InterfaceEdgeStats struct {
	Interface string `json:"interface"`
	Method    string `json:"method"`

	// CHAEdges is the total number of CHA edges from invoke sites on this
	// interface+method that are reachable from the entry point.
	CHAEdges int `json:"cha_edges"`

	// VTAEdges is the subset that VTA also retains.
	VTAEdges int `json:"vta_edges"`

	// CHAOnly = CHAEdges - VTAEdges: edges CHA has that VTA pruned.
	CHAOnly int `json:"cha_only"`

	// PruneRatio is CHAOnly / CHAEdges — what fraction of CHA invoke edges
	// VTA did not retain. High values mean CHA over-approximated heavily.
	PruneRatio float64 `json:"prune_ratio"`

	// ZScore is the z-score of CHAOnly relative to all interfaces.
	// High z-scores (>2) indicate statistical outliers — interfaces that
	// contribute disproportionately to CHA's false-positive noise.
	ZScore float64 `json:"z_score"`

	// Callees lists the callee function names for the CHA-only edges.
	Callees []string `json:"callees,omitempty"`
}

// CompareResult holds the full CHA-vs-VTA comparison.
type CompareResult struct {
	Entry string `json:"entry"`

	CHAReachable int `json:"cha_reachable"`
	VTAReachable int `json:"vta_reachable"`
	CHAOnlyFuncs int `json:"cha_only_funcs"`

	CHAEdgesTotal int `json:"cha_edges_total"`
	VTAEdgesTotal int `json:"vta_edges_total"`

	// InvokeEdges counts only edges originating from interface dispatch (invoke mode).
	CHAInvokeEdges int `json:"cha_invoke_edges"`
	VTAInvokeEdges int `json:"vta_invoke_edges"`

	// ByInterface is sorted by CHAOnly descending — the interfaces causing the
	// most false-positive edges appear first.
	ByInterface []InterfaceEdgeStats `json:"by_interface"`

	LoadTime     time.Duration `json:"load_time"`
	SSABuildTime time.Duration `json:"ssa_build_time"`
	CHABuildTime time.Duration `json:"cha_build_time"`
	VTABuildTime time.Duration `json:"vta_build_time"`
	TotalTime    time.Duration `json:"total_time"`
}

// Compare builds both CHA and VTA call graphs from the same SSA program,
// walks reachable edges from the entry point, and reports which interface
// dispatch sites produce the most CHA-only edges (likely false positives).
func Compare(dir string, entry EntryPoint) (CompareResult, error) {
	var res CompareResult
	res.Entry = entry.String()
	totalStart := time.Now()

	loadStart := time.Now()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps |
			packages.NeedTypes | packages.NeedTypesInfo |
			packages.NeedSyntax,
		Dir: dir,
	}
	pkgs, err := packages.Load(cfg, entry.Package, "./...")
	if err != nil {
		return res, fmt.Errorf("loading packages: %w", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		return res, fmt.Errorf("package loading had errors (see above)")
	}
	res.LoadTime = time.Since(loadStart)

	allPkgs := allPackages(pkgs)

	ssaStart := time.Now()
	prog, ssaPkgs := ssautil.AllPackages(allPkgs, ssa.InstantiateGenerics)
	prog.Build()
	res.SSABuildTime = time.Since(ssaStart)

	entryFunc, err := findEntry(prog, ssaPkgs, entry)
	if err != nil {
		return res, err
	}

	// Build CHA.
	chaStart := time.Now()
	chaCG := cha.CallGraph(prog)
	res.CHABuildTime = time.Since(chaStart)

	// Build VTA (govulncheck-style two-pass; forward slice from mains when present).
	vtaStart := time.Now()
	fslice := vtaAnalysisSlice(prog, chaCG, entryFunc)
	vtaCG := vta.CallGraph(fslice, chaCG)
	fslice = vtaAnalysisSlice(prog, vtaCG, entryFunc)
	vtaCG = vta.CallGraph(fslice, vtaCG)
	vtaCG.DeleteSyntheticNodes()
	res.VTABuildTime = time.Since(vtaStart)

	// Collect reachable edges from each graph.
	chaEdges := reachableEdges(chaCG, entryFunc)
	vtaEdges := reachableEdges(vtaCG, entryFunc)

	chaReachable := reachableFuncs(chaCG, entryFunc)
	vtaReachable := reachableFuncs(vtaCG, entryFunc)

	res.CHAReachable = len(chaReachable)
	res.VTAReachable = len(vtaReachable)

	chaOnly := 0
	for f := range chaReachable {
		if !vtaReachable[f] {
			chaOnly++
		}
	}
	res.CHAOnlyFuncs = chaOnly

	res.CHAEdgesTotal = len(chaEdges)
	res.VTAEdgesTotal = len(vtaEdges)

	// Build a set of VTA edges for fast lookup.
	type edgeKey struct {
		caller *ssa.Function
		callee *ssa.Function
	}
	vtaSet := make(map[edgeKey]bool, len(vtaEdges))
	for _, e := range vtaEdges {
		vtaSet[edgeKey{e.Caller.Func, e.Callee.Func}] = true
	}

	// Walk CHA invoke edges and classify by interface.
	type ifaceMethod struct {
		iface  string
		method string
	}
	type bucket struct {
		chaCount int
		vtaCount int
		callees  map[string]bool // CHA-only callee names
	}
	buckets := make(map[ifaceMethod]*bucket)

	for _, e := range chaEdges {
		if e.Site == nil {
			continue
		}
		cc := e.Site.Common()
		if !cc.IsInvoke() {
			continue
		}

		res.CHAInvokeEdges++

		ifaceType := cc.Value.Type().String()
		methodName := cc.Method.Name()
		key := ifaceMethod{ifaceType, methodName}

		b, ok := buckets[key]
		if !ok {
			b = &bucket{callees: make(map[string]bool)}
			buckets[key] = b
		}
		b.chaCount++

		ek := edgeKey{e.Caller.Func, e.Callee.Func}
		if vtaSet[ek] {
			b.vtaCount++
		} else {
			b.callees[e.Callee.Func.String()] = true
		}
	}

	// Count VTA invoke edges.
	for _, e := range vtaEdges {
		if e.Site == nil {
			continue
		}
		if e.Site.Common().IsInvoke() {
			res.VTAInvokeEdges++
		}
	}

	// Convert buckets to sorted slice.
	for key, b := range buckets {
		chaOnly := b.chaCount - b.vtaCount
		if chaOnly <= 0 {
			continue
		}
		callees := make([]string, 0, len(b.callees))
		for c := range b.callees {
			callees = append(callees, c)
		}
		sort.Strings(callees)

		res.ByInterface = append(res.ByInterface, InterfaceEdgeStats{
			Interface: key.iface,
			Method:    key.method,
			CHAEdges:  b.chaCount,
			VTAEdges:  b.vtaCount,
			CHAOnly:   chaOnly,
			Callees:   callees,
		})
	}
	sort.Slice(res.ByInterface, func(i, j int) bool {
		return res.ByInterface[i].CHAOnly > res.ByInterface[j].CHAOnly
	})

	computePruneRatios(res.ByInterface)
	computeZScores(res.ByInterface)

	res.TotalTime = time.Since(totalStart)
	return res, nil
}

// computePruneRatios sets PruneRatio on each stat (CHAOnly / CHAEdges).
func computePruneRatios(stats []InterfaceEdgeStats) {
	for i := range stats {
		if stats[i].CHAEdges > 0 {
			stats[i].PruneRatio = float64(stats[i].CHAOnly) / float64(stats[i].CHAEdges)
		}
	}
}

// computeZScores sets the ZScore field on each InterfaceEdgeStats based on
// the distribution of CHAOnly values across all interfaces.
func computeZScores(stats []InterfaceEdgeStats) {
	if len(stats) == 0 {
		return
	}

	var sum float64
	for i := range stats {
		sum += float64(stats[i].CHAOnly)
	}
	mean := sum / float64(len(stats))

	var sqDiffSum float64
	for i := range stats {
		d := float64(stats[i].CHAOnly) - mean
		sqDiffSum += d * d
	}
	stddev := math.Sqrt(sqDiffSum / float64(len(stats)))

	for i := range stats {
		if stddev > 0 {
			stats[i].ZScore = (float64(stats[i].CHAOnly) - mean) / stddev
		}
	}
}

// Outliers returns the interface+method pairs whose CHAOnly edge count has a
// z-score >= threshold. These are the worst offenders for CHA false positives.
func Outliers(stats []InterfaceEdgeStats, threshold float64) []InterfaceEdgeStats {
	var out []InterfaceEdgeStats
	for _, s := range stats {
		if s.ZScore >= threshold {
			out = append(out, s)
		}
	}
	return out
}

// PruneTargets returns a set of "interfaceType\x00methodName" keys for
// interfaces where CHA is noisy enough that cha-pruned should replace CHA
// edges with VTA's: PruneRatio >= ratioThreshold and CHAOnly >= minExcess.
func PruneTargets(stats []InterfaceEdgeStats, ratioThreshold float64, minExcess int) map[string]bool {
	keys := make(map[string]bool)
	for _, s := range stats {
		if s.CHAOnly >= minExcess && s.PruneRatio >= ratioThreshold {
			keys[s.Interface+"\x00"+s.Method] = true
		}
	}
	return keys
}

// reachableEdges returns all edges in the subgraph reachable from root via BFS.
func reachableEdges(cg *callgraph.Graph, root *ssa.Function) []*callgraph.Edge {
	rootNode := cg.Nodes[root]
	if rootNode == nil {
		return nil
	}

	visited := make(map[*callgraph.Node]bool)
	queue := []*callgraph.Node{rootNode}
	visited[rootNode] = true

	var edges []*callgraph.Edge
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		for _, e := range cur.Out {
			edges = append(edges, e)
			if !visited[e.Callee] {
				visited[e.Callee] = true
				queue = append(queue, e.Callee)
			}
		}
	}
	return edges
}

// reachableFuncs returns the set of functions reachable from root via BFS.
func reachableFuncs(cg *callgraph.Graph, root *ssa.Function) map[*ssa.Function]bool {
	rootNode := cg.Nodes[root]
	if rootNode == nil {
		return nil
	}

	visited := make(map[*ssa.Function]bool)
	queue := []*callgraph.Node{rootNode}
	visited[root] = true

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range cur.Out {
			if !visited[e.Callee.Func] {
				visited[e.Callee.Func] = true
				queue = append(queue, e.Callee)
			}
		}
	}
	return visited
}

// shortIfaceName trims the package path to just the last element for display.
func shortIfaceName(full string) string {
	if idx := strings.LastIndex(full, "/"); idx >= 0 {
		return full[idx+1:]
	}
	return full
}

// isPruneTarget reports whether s would be pruned by cha-pruned at the given thresholds.
func isPruneTarget(s InterfaceEdgeStats, ratioThreshold float64, minExcess int) bool {
	return s.CHAOnly >= minExcess && s.PruneRatio >= ratioThreshold
}

// FormatCompareText produces a human-readable summary of the comparison.
// pruneRatioThreshold and minExcess control which rows are marked as cha-pruned targets (*).
func FormatCompareText(res CompareResult, topN int, pruneRatioThreshold float64, minExcess int) string {
	var b strings.Builder

	fmt.Fprintf(&b, "CHA vs VTA comparison for %s\n", res.Entry)
	fmt.Fprintf(&b, "══════════════════════════════════════════════════════\n\n")

	fmt.Fprintf(&b, "  Reachable functions:  CHA %-8d  VTA %-8d  (CHA-only: %d)\n",
		res.CHAReachable, res.VTAReachable, res.CHAOnlyFuncs)
	fmt.Fprintf(&b, "  Total edges:          CHA %-8d  VTA %-8d\n",
		res.CHAEdgesTotal, res.VTAEdgesTotal)
	fmt.Fprintf(&b, "  Invoke edges:         CHA %-8d  VTA %-8d\n",
		res.CHAInvokeEdges, res.VTAInvokeEdges)
	fmt.Fprintf(&b, "\n")

	prunedEdges := res.CHAInvokeEdges - res.VTAInvokeEdges
	if res.CHAInvokeEdges > 0 {
		pct := float64(prunedEdges) / float64(res.CHAInvokeEdges) * 100
		fmt.Fprintf(&b, "  VTA pruned %d of %d invoke edges (%.1f%%)\n\n",
			prunedEdges, res.CHAInvokeEdges, pct)
	}

	fmt.Fprintf(&b, "  Timing: load %v | ssa %v | cha %v | vta %v | total %v\n\n",
		res.LoadTime.Round(time.Millisecond),
		res.SSABuildTime.Round(time.Millisecond),
		res.CHABuildTime.Round(time.Millisecond),
		res.VTABuildTime.Round(time.Millisecond),
		res.TotalTime.Round(time.Millisecond))

	if len(res.ByInterface) == 0 {
		fmt.Fprintf(&b, "  No interface-dispatch differences found.\n")
		return b.String()
	}

	pruneSet := PruneTargets(res.ByInterface, pruneRatioThreshold, minExcess)
	fmt.Fprintf(&b, "False positives by interface (CHA-only invoke edges)\n")
	fmt.Fprintf(&b, "──────────────────────────────────────────────────────\n")
	if len(pruneSet) > 0 {
		fmt.Fprintf(&b, "  * = cha-pruned target (prune ratio >= %.0f%% and excess >= %d) — %d interfaces\n\n",
			pruneRatioThreshold*100, minExcess, len(pruneSet))
	}
	fmt.Fprintf(&b, "  %-50s  %6s  %6s  %6s  %6s  %7s\n", "INTERFACE.Method", "CHA", "VTA", "EXCESS", "PRUNE%", "Z-SCORE")
	fmt.Fprintf(&b, "  %-50s  %6s  %6s  %6s  %6s  %7s\n",
		strings.Repeat("─", 50),
		strings.Repeat("─", 6),
		strings.Repeat("─", 6),
		strings.Repeat("─", 6),
		strings.Repeat("─", 6),
		strings.Repeat("─", 7))

	limit := len(res.ByInterface)
	if topN > 0 && topN < limit {
		limit = topN
	}

	totalExcess := 0
	for i := 0; i < limit; i++ {
		s := res.ByInterface[i]
		label := fmt.Sprintf("%s.%s", shortIfaceName(s.Interface), s.Method)
		if len(label) > 50 {
			label = label[:47] + "..."
		}
		marker := " "
		if isPruneTarget(s, pruneRatioThreshold, minExcess) {
			marker = "*"
		}
		fmt.Fprintf(&b, "%s %-50s  %6d  %6d  %6d  %5.1f%%  %7.1f\n",
			marker, label, s.CHAEdges, s.VTAEdges, s.CHAOnly, s.PruneRatio*100, s.ZScore)
		totalExcess += s.CHAOnly
	}

	if topN > 0 && topN < len(res.ByInterface) {
		remaining := 0
		for i := topN; i < len(res.ByInterface); i++ {
			remaining += res.ByInterface[i].CHAOnly
		}
		fmt.Fprintf(&b, "  ... and %d more interfaces (%d excess edges)\n",
			len(res.ByInterface)-topN, remaining)
		totalExcess += remaining
	}

	fmt.Fprintf(&b, "\n  Total CHA-only invoke edges: %d\n", totalExcess)

	return b.String()
}

// FormatCompareCallees produces a detailed view showing the actual callee
// functions for each interface's CHA-only edges.
func FormatCompareCallees(res CompareResult, topN int) string {
	var b strings.Builder

	limit := len(res.ByInterface)
	if topN > 0 && topN < limit {
		limit = topN
	}

	for i := 0; i < limit; i++ {
		s := res.ByInterface[i]
		fmt.Fprintf(&b, "\n%s.%s  (%d CHA-only edges)\n",
			shortIfaceName(s.Interface), s.Method, s.CHAOnly)
		for _, c := range s.Callees {
			// Trim the full SSA name to something readable.
			short := c
			if idx := strings.LastIndex(c, "/"); idx >= 0 {
				short = c[idx+1:]
			}
			fmt.Fprintf(&b, "    %s\n", short)
		}
	}

	return b.String()
}

