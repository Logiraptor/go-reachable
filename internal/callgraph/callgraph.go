package callgraph

import (
	"fmt"
	"go/types"
	"strings"
	"time"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// Stats captures timing and size metrics from the analysis.
type Stats struct {
	LoadTime     time.Duration `json:"load_time"`
	SSABuildTime time.Duration `json:"ssa_build_time"`
	CGBuildTime  time.Duration `json:"cg_build_time"`
	WalkTime     time.Duration `json:"walk_time"`
	TotalTime    time.Duration `json:"total_time"`

	PackagesLoaded int `json:"packages_loaded"`
	TotalFunctions int `json:"total_functions"`
	CGNodes        int `json:"cg_nodes"`
	CGEdges        int `json:"cg_edges"`
	Reachable      int `json:"reachable"`
}

// ReachableFunc describes a function reachable from the entry point.
type ReachableFunc struct {
	Package  string `json:"package"`
	Name     string `json:"name"`
	Position string `json:"position,omitempty"`
	Depth    int    `json:"depth"`
}

// Algorithm selects the call graph construction strategy.
type Algorithm string

const (
	AlgoCHA       Algorithm = "cha"
	AlgoVTA       Algorithm = "vta"
	AlgoCHAPruned Algorithm = "cha-pruned"
)

// EntryPoint identifies a function to start the reachability walk from.
type EntryPoint struct {
	Package  string // full import path
	Receiver string // receiver type name, e.g. "*Distributor" (empty for plain functions)
	Func     string // function/method name
}

func (e EntryPoint) String() string {
	if e.Receiver != "" {
		return fmt.Sprintf("%s.(%s).%s", e.Package, e.Receiver, e.Func)
	}
	return fmt.Sprintf("%s.%s", e.Package, e.Func)
}

// Analysis holds a built SSA program and call graph for repeated reachability queries.
type Analysis struct {
	prog    *ssa.Program
	ssaPkgs []*ssa.Package
	cg      *callgraph.Graph
}

// BuildGraph loads packages, builds SSA, and constructs the call graph using
// graphEntry as the anchor (for VTA slicing and cha-pruned). Caller runs
// ReachableFrom with the same or different entry points without rebuilding.
func BuildGraph(dir string, graphEntry EntryPoint, algo Algorithm) (*Analysis, Stats, error) {
	var stats Stats
	loadStart := time.Now()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps |
			packages.NeedTypes | packages.NeedTypesInfo |
			packages.NeedSyntax,
		Dir: dir,
	}
	loadPatterns := []string{graphEntry.Package}
	if algo == AlgoVTA || algo == AlgoCHAPruned {
		// VTA's forward slice must include startup (e.g. middleware wiring) so
		// types stored during init propagate to request-time interface calls.
		// That requires main packages in the SSA program; load the whole module.
		loadPatterns = []string{graphEntry.Package, "./..."}
	}
	pkgs, err := packages.Load(cfg, loadPatterns...)
	if err != nil {
		return nil, stats, fmt.Errorf("loading packages: %w", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		return nil, stats, fmt.Errorf("package loading had errors (see above)")
	}
	stats.LoadTime = time.Since(loadStart)

	allPkgs := allPackages(pkgs)
	stats.PackagesLoaded = len(allPkgs)

	ssaStart := time.Now()
	prog, ssaPkgs := ssautil.AllPackages(allPkgs, ssa.InstantiateGenerics)
	prog.Build()
	stats.SSABuildTime = time.Since(ssaStart)

	stats.TotalFunctions = len(ssautil.AllFunctions(prog))

	entryFunc, err := findEntry(prog, ssaPkgs, graphEntry)
	if err != nil {
		return nil, stats, err
	}

	cgStart := time.Now()
	var cg *callgraph.Graph
	switch algo {
	case AlgoVTA:
		initial := cha.CallGraph(prog)
		fslice := vtaAnalysisSlice(prog, initial, entryFunc)
		vtaCg := vta.CallGraph(fslice, initial)
		fslice = vtaAnalysisSlice(prog, vtaCg, entryFunc)
		cg = vta.CallGraph(fslice, vtaCg)
		cg.DeleteSyntheticNodes()
	case AlgoCHAPruned:
		cg = buildPrunedCHA(prog, entryFunc, DefaultPruneRatioThreshold, DefaultMinExcessEdges)
	default:
		cg = cha.CallGraph(prog)
	}
	stats.CGBuildTime = time.Since(cgStart)

	stats.CGNodes = len(cg.Nodes)
	for _, n := range cg.Nodes {
		stats.CGEdges += len(n.Out)
	}

	return &Analysis{prog: prog, ssaPkgs: ssaPkgs, cg: cg}, stats, nil
}

// ReachableFrom runs a forward BFS from entry against a graph built with BuildGraph.
func ReachableFrom(a *Analysis, entry EntryPoint) ([]ReachableFunc, time.Duration, error) {
	if a == nil {
		return nil, 0, fmt.Errorf("nil analysis")
	}
	entryFunc, err := findEntry(a.prog, a.ssaPkgs, entry)
	if err != nil {
		return nil, 0, err
	}
	walkStart := time.Now()
	reachable := bfsFrom(a.cg, entryFunc)
	return reachable, time.Since(walkStart), nil
}

// Analyze loads packages, builds a call graph, and walks forward from the
// entry point to find all reachable functions.
func Analyze(dir string, entry EntryPoint, algo Algorithm) ([]ReachableFunc, Stats, error) {
	totalStart := time.Now()
	a, stats, err := BuildGraph(dir, entry, algo)
	if err != nil {
		return nil, stats, err
	}
	reachable, walkTime, err := ReachableFrom(a, entry)
	if err != nil {
		return nil, stats, err
	}
	stats.WalkTime = walkTime
	stats.Reachable = len(reachable)
	stats.TotalTime = time.Since(totalStart)
	return reachable, stats, nil
}

// buildPrunedCHA constructs a hybrid call graph: CHA everywhere, except for
// interface dispatch sites where CHA over-approximates heavily (prune ratio
// >= ratioThreshold and excess edges >= minExcess). At those sites, VTA's
// refined edges replace CHA's.
//
// This preserves CHA's safety (no false negatives) for interfaces where VTA
// agrees with most of CHA's edges, while surgically removing noise from
// universal interfaces like io.Reader, fmt.Stringer, error.Error, etc.
func buildPrunedCHA(prog *ssa.Program, entryFunc *ssa.Function, ratioThreshold float64, minExcess int) *callgraph.Graph {
	chaCG := cha.CallGraph(prog)

	// Build VTA for comparison (use main roots so init paths participate).
	fslice := vtaAnalysisSlice(prog, chaCG, entryFunc)
	vtaCG := vta.CallGraph(fslice, chaCG)
	fslice = vtaAnalysisSlice(prog, vtaCG, entryFunc)
	vtaCG = vta.CallGraph(fslice, vtaCG)
	vtaCG.DeleteSyntheticNodes()

	// Collect reachable invoke-edge stats to find outliers.
	chaEdges := reachableEdges(chaCG, entryFunc)
	vtaEdges := reachableEdges(vtaCG, entryFunc)

	type edgeKey struct {
		caller *ssa.Function
		callee *ssa.Function
	}
	vtaSet := make(map[edgeKey]bool, len(vtaEdges))
	for _, e := range vtaEdges {
		vtaSet[edgeKey{e.Caller.Func, e.Callee.Func}] = true
	}

	type ifaceMethod struct {
		iface  string
		method string
	}
	type bucket struct {
		chaCount int
		vtaCount int
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
		key := ifaceMethod{cc.Value.Type().String(), cc.Method.Name()}
		b, ok := buckets[key]
		if !ok {
			b = &bucket{}
			buckets[key] = b
		}
		b.chaCount++
		if vtaSet[edgeKey{e.Caller.Func, e.Callee.Func}] {
			b.vtaCount++
		}
	}

	stats := make([]InterfaceEdgeStats, 0, len(buckets))
	for key, b := range buckets {
		chaOnly := b.chaCount - b.vtaCount
		if chaOnly <= 0 {
			continue
		}
		stats = append(stats, InterfaceEdgeStats{
			Interface: key.iface,
			Method:    key.method,
			CHAEdges:  b.chaCount,
			VTAEdges:  b.vtaCount,
			CHAOnly:   chaOnly,
		})
	}
	computePruneRatios(stats)
	pruneSet := PruneTargets(stats, ratioThreshold, minExcess)

	// Now mutate the CHA graph: for nodes reachable from entry, remove
	// CHA invoke edges that target outlier interfaces and replace with VTA's.
	//
	// We build a map of VTA edges keyed by (caller, site-position, callee)
	// for the outlier interfaces, then walk CHA and swap.
	type siteEdgeKey struct {
		callerFunc *ssa.Function
		ifaceKey   string
	}
	vtaByCallSite := make(map[siteEdgeKey]map[*ssa.Function]bool)
	for _, e := range vtaEdges {
		if e.Site == nil {
			continue
		}
		cc := e.Site.Common()
		if !cc.IsInvoke() {
			continue
		}
		ik := cc.Value.Type().String() + "\x00" + cc.Method.Name()
		if !pruneSet[ik] {
			continue
		}
		sk := siteEdgeKey{e.Caller.Func, ik}
		if vtaByCallSite[sk] == nil {
			vtaByCallSite[sk] = make(map[*ssa.Function]bool)
		}
		vtaByCallSite[sk][e.Callee.Func] = true
	}

	// Walk reachable CHA nodes and prune outlier invoke edges.
	reachNodes := reachableFuncs(chaCG, entryFunc)
	for fn := range reachNodes {
		node := chaCG.Nodes[fn]
		if node == nil {
			continue
		}

		var kept []*callgraph.Edge
		for _, e := range node.Out {
			if e.Site == nil {
				kept = append(kept, e)
				continue
			}
			cc := e.Site.Common()
			if !cc.IsInvoke() {
				kept = append(kept, e)
				continue
			}
			ik := cc.Value.Type().String() + "\x00" + cc.Method.Name()
			if !pruneSet[ik] {
				kept = append(kept, e)
				continue
			}
			// This is an outlier interface edge. Only keep it if VTA also has it.
			sk := siteEdgeKey{fn, ik}
			if vtaByCallSite[sk] != nil && vtaByCallSite[sk][e.Callee.Func] {
				kept = append(kept, e)
			}
			// Otherwise drop it — CHA false positive from an outlier interface.
		}
		node.Out = kept
	}

	return chaCG
}

func findEntry(prog *ssa.Program, ssaPkgs []*ssa.Package, entry EntryPoint) (*ssa.Function, error) {
	for _, pkg := range ssaPkgs {
		if pkg == nil {
			continue
		}
		if pkg.Pkg.Path() != entry.Package {
			continue
		}

		if entry.Receiver == "" {
			if f := pkg.Func(entry.Func); f != nil {
				return f, nil
			}
			return nil, fmt.Errorf("function %s not found in package %s", entry.Func, entry.Package)
		}

		recv := strings.TrimPrefix(entry.Receiver, "*")
		t := pkg.Type(recv)
		if t == nil {
			return nil, fmt.Errorf("type %s not found in package %s", recv, entry.Package)
		}

		// Pointer method set includes both pointer and value receiver methods.
		lookupType := types.NewPointer(t.Type())
		mset := prog.MethodSets.MethodSet(lookupType)
		for i := 0; i < mset.Len(); i++ {
			sel := mset.At(i)
			if sel.Obj().Name() == entry.Func {
				if f := prog.MethodValue(sel); f != nil {
					return f, nil
				}
			}
		}

		return nil, fmt.Errorf("method (*%s).%s not found in package %s", recv, entry.Func, entry.Package)
	}
	return nil, fmt.Errorf("package %s not found among loaded packages", entry.Package)
}

func forwardSlice(root *ssa.Function, cg *callgraph.Graph) map[*ssa.Function]bool {
	node := cg.Nodes[root]
	if node == nil {
		return map[*ssa.Function]bool{root: true}
	}

	seen := make(map[*ssa.Function]bool)
	var walk func(*callgraph.Node)
	walk = func(n *callgraph.Node) {
		if seen[n.Func] {
			return
		}
		seen[n.Func] = true
		for _, e := range n.Out {
			walk(e.Callee)
		}
	}
	walk(node)
	return seen
}

// mainFuncRoots returns every package-main function in prog (executable entrypoints).
func mainFuncRoots(prog *ssa.Program) []*ssa.Function {
	var mains []*ssa.Function
	for fn := range ssautil.AllFunctions(prog) {
		if fn == nil || fn.Pkg == nil || fn.Pkg.Pkg == nil {
			continue
		}
		if fn.Pkg.Pkg.Name() != "main" {
			continue
		}
		if fn.Name() != "main" || fn.Signature.Recv() != nil {
			continue
		}
		mains = append(mains, fn)
	}
	return mains
}

// vtaAnalysisSlice is the function set passed to vta.CallGraph as the forward
// slice. We union the CHA forward slice from each package main so VTA analyzes
// startup as well as request paths; if there is no main in the loaded program
// (library-only), we use the user's entry point.
func vtaAnalysisSlice(prog *ssa.Program, cg *callgraph.Graph, entryFunc *ssa.Function) map[*ssa.Function]bool {
	mains := mainFuncRoots(prog)
	if len(mains) == 0 {
		return forwardSlice(entryFunc, cg)
	}
	union := make(map[*ssa.Function]bool)
	for _, m := range mains {
		for fn := range forwardSlice(m, cg) {
			union[fn] = true
		}
	}
	return union
}

func bfsFrom(cg *callgraph.Graph, root *ssa.Function) []ReachableFunc {
	rootNode := cg.Nodes[root]
	if rootNode == nil {
		return nil
	}

	type item struct {
		node  *callgraph.Node
		depth int
	}

	visited := make(map[*callgraph.Node]bool)
	queue := []item{{rootNode, 0}}
	visited[rootNode] = true

	var result []ReachableFunc

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		f := cur.node.Func
		rf := ReachableFunc{
			Name:  funcName(f),
			Depth: cur.depth,
		}
		if f.Package() != nil {
			rf.Package = f.Package().Pkg.Path()
		}
		if pos := f.Pos(); pos.IsValid() {
			rf.Position = f.Prog.Fset.Position(pos).String()
		}
		result = append(result, rf)

		for _, edge := range cur.node.Out {
			if !visited[edge.Callee] {
				visited[edge.Callee] = true
				queue = append(queue, item{edge.Callee, cur.depth + 1})
			}
		}
	}
	return result
}

func funcName(f *ssa.Function) string {
	if recv := f.Signature.Recv(); recv != nil {
		recvType := recv.Type().String()
		if idx := strings.LastIndex(recvType, "/"); idx >= 0 {
			if dotIdx := strings.Index(recvType[idx:], "."); dotIdx >= 0 {
				recvType = recvType[idx+dotIdx+1:]
			}
		}
		recvType = strings.TrimPrefix(recvType, "*")
		return fmt.Sprintf("(*%s).%s", recvType, f.Name())
	}
	return f.Name()
}

func allPackages(roots []*packages.Package) []*packages.Package {
	seen := make(map[string]*packages.Package)
	var visit func(*packages.Package)
	visit = func(p *packages.Package) {
		if _, ok := seen[p.ID]; ok {
			return
		}
		seen[p.ID] = p
		for _, imp := range p.Imports {
			visit(imp)
		}
	}
	for _, r := range roots {
		visit(r)
	}
	result := make([]*packages.Package, 0, len(seen))
	for _, p := range seen {
		result = append(result, p)
	}
	return result
}
