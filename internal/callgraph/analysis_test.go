package callgraph

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildGraphReachableFromDistinctRoots(t *testing.T) {
	if testing.Short() {
		t.Skip("SSA/VTA build is slow")
	}
	dir := filepath.Join("..", "..", "testdata", "minimal")
	mainEntry := EntryPoint{
		Package: "example.com/minimal/cmd/app",
		Func:    "main",
	}
	a, _, err := BuildGraph(dir, mainEntry, AlgoVTA)
	if err != nil {
		t.Fatal(err)
	}

	fEntry := EntryPoint{Package: "example.com/minimal/pkg", Func: "F"}
	gEntry := EntryPoint{Package: "example.com/minimal/pkg", Func: "G"}

	rf, _, err := ReachableFrom(a, fEntry)
	if err != nil {
		t.Fatal(err)
	}
	rg, _, err := ReachableFrom(a, gEntry)
	if err != nil {
		t.Fatal(err)
	}

	hasName := func(funcs []ReachableFunc, want string) bool {
		for _, f := range funcs {
			if strings.Contains(f.Name, want) {
				return true
			}
		}
		return false
	}
	if !hasName(rf, "F") {
		t.Fatalf("reachable from F should include F, got %#v", rf)
	}
	if hasName(rf, "G") {
		t.Fatalf("reachable from F should not include G, got %#v", rf)
	}
	if !hasName(rg, "G") {
		t.Fatalf("reachable from G should include G, got %#v", rg)
	}
}
