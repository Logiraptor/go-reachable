package reachable

import (
	"testing"

	"github.com/Logiraptor/go-reachable/internal/callgraph"
	"github.com/Logiraptor/go-reachable/internal/diffsyms"
)

func TestMatchChangedSymbols(t *testing.T) {
	idx := map[string]callgraph.ReachableFunc{
		"example.com/m/pkg.(*T).M": {Package: "example.com/m/pkg", Name: "(*T).M", Depth: 1},
		"example.com/m/pkg.Foo":    {Package: "example.com/m/pkg", Name: "Foo", Depth: 0},
	}
	symbols := []diffsyms.Symbol{
		{Kind: "method", Package: "pkg", Receiver: "T", Name: "M"},
		{Kind: "func", Package: "pkg", Name: "Bar"},
	}
	matches, touched := matchChangedSymbols("example.com/m", idx, symbols)
	if !touched || len(matches) != 1 {
		t.Fatalf("matches=%v touched=%v", matches, touched)
	}
}

func TestAnalyzePathsRejectsMultiCHA(t *testing.T) {
	_, err := AnalyzePaths(MultiOptions{
		Algorithm: callgraph.AlgoCHA,
		PathQueries: []PathQuery{
			{Name: "a", Entry: callgraph.EntryPoint{Package: "p", Func: "f"}},
			{Name: "b", Entry: callgraph.EntryPoint{Package: "p", Func: "g"}},
		},
	}, nil)
	if err == nil {
		t.Fatal("expected error for multi-path CHA")
	}
}
