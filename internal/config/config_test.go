package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".reachable.yaml")
	content := `
version: 1
vta:
  package: example.com/app/cmd/server
paths:
  - name: api
    package: example.com/app/pkg/api
    func: Handle
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.VTA.Package != "example.com/app/cmd/server" {
		t.Fatalf("vta.package: got %q", c.VTA.Package)
	}
	if len(c.Paths) != 1 || c.Paths[0].Name != "api" {
		t.Fatalf("paths: %#v", c.Paths)
	}
}

func TestLoadMultiSymbol(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".reachable.yaml")
	content := `
version: 1
vta:
  package: example.com/app/cmd/server
paths:
  - name: api
    symbols:
      - package: example.com/app/pkg/api
        recv: "*Handler"
        func: handleCreate
      - package: example.com/app/pkg/api
        recv: "*Handler"
        func: handleList
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Paths) != 1 {
		t.Fatalf("paths count: got %d want 1", len(c.Paths))
	}
	syms := c.Paths[0].SymbolSpecs()
	if len(syms) != 2 {
		t.Fatalf("symbol count: got %d want 2", len(syms))
	}
	if syms[0].Func != "handleCreate" || syms[1].Func != "handleList" {
		t.Fatalf("symbols: %+v", syms)
	}
}

func TestValidateMixedInlineAndSymbols(t *testing.T) {
	c := &Config{
		VTA:   VTAConfig{Package: "x"},
		Paths: []PathSpec{{Name: "a", Package: "p", Func: "f", Symbols: []SymbolSpec{{Package: "p", Func: "g"}}}},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for mixed inline and symbols")
	}
}

func TestValidateSymbolsMissingFunc(t *testing.T) {
	c := &Config{
		VTA:   VTAConfig{Package: "x"},
		Paths: []PathSpec{{Name: "a", Symbols: []SymbolSpec{{Package: "p"}}}},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for symbol missing func")
	}
}

func TestSymbolSpecsInline(t *testing.T) {
	p := PathSpec{Name: "a", Package: "p", Recv: "*T", Func: "f"}
	syms := p.SymbolSpecs()
	if len(syms) != 1 {
		t.Fatalf("got %d symbols, want 1", len(syms))
	}
	if syms[0].Package != "p" || syms[0].Recv != "*T" || syms[0].Func != "f" {
		t.Fatalf("unexpected: %+v", syms[0])
	}
}

func TestValidateMissingVTAPackage(t *testing.T) {
	c := &Config{
		Paths: []PathSpec{{Name: "a", Package: "p", Func: "f"}},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for missing vta.package")
	}
}

func TestValidateBadVTAFunc(t *testing.T) {
	c := &Config{
		VTA: VTAConfig{Package: "x", Func: "notmain"},
		Paths: []PathSpec{{Name: "a", Package: "p", Func: "f"}},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for vta.func != main")
	}
}

func TestFindConfig(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, ".reachable.yaml")
	if err := os.WriteFile(cfgPath, []byte("version: 1\nvta:\n  package: x\npaths:\n  - name: p\n    package: y\n    func: z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	found, err := FindConfig(sub)
	if err != nil {
		t.Fatal(err)
	}
	if found != cfgPath {
		t.Fatalf("FindConfig: got %q want %q", found, cfgPath)
	}
}
