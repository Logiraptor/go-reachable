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
