package diffsyms

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrDiffFilesNotInRepo is returned when every .go path in the diff is missing
// under repoRoot (usually a wrong -repo / working directory).
var ErrDiffFilesNotInRepo = errors.New("diff references Go files not found under repo root")

// Symbol represents a Go symbol that was changed by a diff.
type Symbol struct {
	Package  string // directory-relative package path (e.g., "pkg/engine")
	File     string // file path relative to repo root
	Name     string // function/method/type name
	Kind     string // "func", "method", "type", "var", "const", "init", "file"
	Receiver string // non-empty for methods (e.g., "*streamsView")
	Line     int    // line number of the symbol declaration
}

func (s Symbol) String() string {
	switch {
	case s.Receiver != "":
		return fmt.Sprintf("%s.(%s).%s", s.Package, s.Receiver, s.Name)
	case s.Name != "":
		return fmt.Sprintf("%s.%s", s.Package, s.Name)
	default:
		return fmt.Sprintf("%s (file-level)", s.File)
	}
}

// ChangedSymbols maps diff regions to the Go symbols they fall within.
// repoRoot is the absolute path to the repository root.
func ChangedSymbols(repoRoot string, regions []ChangedRegion) ([]Symbol, error) {
	byFile := groupByFile(regions)

	var symbols []Symbol
	var missingFiles []string
	for file, fileRegions := range byFile {
		absPath := filepath.Join(repoRoot, file)
		if _, err := os.Stat(absPath); err != nil {
			missingFiles = append(missingFiles, file)
			continue // file doesn't exist (deleted or renamed)
		}

		syms, err := symbolsForFile(repoRoot, file, absPath, fileRegions)
		if err != nil {
			return nil, fmt.Errorf("analyzing %s: %w", file, err)
		}
		symbols = append(symbols, syms...)
	}

	sort.Slice(symbols, func(i, j int) bool {
		if symbols[i].File != symbols[j].File {
			return symbols[i].File < symbols[j].File
		}
		return symbols[i].Line < symbols[j].Line
	})
	out := dedupSymbols(symbols)
	if len(out) == 0 && len(byFile) > 0 && len(missingFiles) == len(byFile) {
		sort.Strings(missingFiles)
		return nil, fmt.Errorf("%w: repo=%q missing=[%s]", ErrDiffFilesNotInRepo, repoRoot, strings.Join(missingFiles, ", "))
	}
	return out, nil
}

func symbolsForFile(repoRoot, relPath, absPath string, regions []ChangedRegion) ([]Symbol, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, absPath, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", relPath, err)
	}

	pkgDir := filepath.Dir(relPath)

	// Build an index of all top-level declarations with their line ranges.
	decls := indexDeclarations(fset, f, pkgDir, relPath)

	var symbols []Symbol
	for _, region := range regions {
		matched := matchRegionToDecls(region, decls)
		if len(matched) == 0 {
			// Change is outside any declaration (package clause, imports, file-level comments).
			symbols = append(symbols, Symbol{
				Package: pkgDir,
				File:    relPath,
				Kind:    "file",
				Line:    region.StartLine,
			})
		} else {
			symbols = append(symbols, matched...)
		}
	}
	return symbols, nil
}

type declRange struct {
	symbol    Symbol
	startLine int
	endLine   int
}

func indexDeclarations(fset *token.FileSet, f *ast.File, pkgDir, relPath string) []declRange {
	var decls []declRange

	for _, d := range f.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			sym := Symbol{
				Package: pkgDir,
				File:    relPath,
				Name:    decl.Name.Name,
				Kind:    "func",
				Line:    fset.Position(decl.Pos()).Line,
			}
			if decl.Recv != nil && len(decl.Recv.List) > 0 {
				sym.Kind = "method"
				sym.Receiver = receiverTypeName(decl.Recv.List[0].Type)
			}
			if decl.Name.Name == "init" {
				sym.Kind = "init"
			}
			decls = append(decls, declRange{
				symbol:    sym,
				startLine: fset.Position(decl.Pos()).Line,
				endLine:   fset.Position(decl.End()).Line,
			})

		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					decls = append(decls, declRange{
						symbol: Symbol{
							Package: pkgDir,
							File:    relPath,
							Name:    s.Name.Name,
							Kind:    "type",
							Line:    fset.Position(s.Pos()).Line,
						},
						startLine: fset.Position(s.Pos()).Line,
						endLine:   fset.Position(s.End()).Line,
					})
				case *ast.ValueSpec:
					kind := "var"
					if decl.Tok == token.CONST {
						kind = "const"
					}
					for _, name := range s.Names {
						decls = append(decls, declRange{
							symbol: Symbol{
								Package: pkgDir,
								File:    relPath,
								Name:    name.Name,
								Kind:    kind,
								Line:    fset.Position(name.Pos()).Line,
							},
							startLine: fset.Position(s.Pos()).Line,
							endLine:   fset.Position(s.End()).Line,
						})
					}
				}
			}
		}
	}
	return decls
}

func matchRegionToDecls(region ChangedRegion, decls []declRange) []Symbol {
	var matched []Symbol
	for _, d := range decls {
		if region.StartLine <= d.endLine && region.EndLine >= d.startLine {
			matched = append(matched, d.symbol)
		}
	}
	return matched
}

func receiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + receiverTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return receiverTypeName(t.X) // generic type, strip type params
	case *ast.IndexListExpr:
		return receiverTypeName(t.X)
	default:
		return fmt.Sprintf("%T", expr)
	}
}

func groupByFile(regions []ChangedRegion) map[string][]ChangedRegion {
	m := make(map[string][]ChangedRegion)
	for _, r := range regions {
		if !strings.HasSuffix(r.File, ".go") {
			continue
		}
		m[r.File] = append(m[r.File], r)
	}
	return m
}

func dedupSymbols(symbols []Symbol) []Symbol {
	seen := make(map[string]bool)
	var result []Symbol
	for _, s := range symbols {
		key := s.String()
		if !seen[key] {
			seen[key] = true
			result = append(result, s)
		}
	}
	return result
}
