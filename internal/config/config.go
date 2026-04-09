package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the .reachable.yaml / reachable.yaml schema (version 1).
type Config struct {
	Version int        `yaml:"version"`
	VTA     VTAConfig  `yaml:"vta"`
	Paths   []PathSpec `yaml:"paths"`
}

// VTAConfig names the main package used as the VTA analysis anchor (main.main).
type VTAConfig struct {
	Package string `yaml:"package"`
	Func    string `yaml:"func"`
}

// SymbolSpec identifies a single function or method as a reachability root.
type SymbolSpec struct {
	Package string `yaml:"package"`
	Recv    string `yaml:"recv"`
	Func    string `yaml:"func"`
}

// PathSpec is one named reachability query root. It may contain a single symbol
// (package/recv/func at top level) or multiple symbols via the symbols list.
type PathSpec struct {
	Name    string       `yaml:"name"`
	Package string       `yaml:"package"`
	Recv    string       `yaml:"recv"`
	Func    string       `yaml:"func"`
	Symbols []SymbolSpec `yaml:"symbols"`
}

// SymbolSpecs returns the resolved list of symbols for this path. If the
// top-level package/func fields are set, they are returned as a single-element
// list; otherwise the symbols list is returned directly.
func (p PathSpec) SymbolSpecs() []SymbolSpec {
	if p.Package != "" || p.Func != "" {
		return []SymbolSpec{{Package: p.Package, Recv: p.Recv, Func: p.Func}}
	}
	return p.Symbols
}

const configFileDot = ".reachable.yaml"
const configFilePlain = "reachable.yaml"

// Load reads and validates a YAML config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks required fields and schema version.
func (c *Config) Validate() error {
	if c.Version != 0 && c.Version != 1 {
		return fmt.Errorf("unsupported config version %d (supported: 1)", c.Version)
	}
	if len(c.Paths) == 0 {
		return fmt.Errorf("config: paths must be non-empty")
	}
	for i, p := range c.Paths {
		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("config: paths[%d].name is required", i)
		}
		hasInline := strings.TrimSpace(p.Package) != "" || strings.TrimSpace(p.Func) != ""
		hasSymbols := len(p.Symbols) > 0
		if hasInline && hasSymbols {
			return fmt.Errorf("config: paths[%d] (%s): use either package/func or symbols, not both", i, p.Name)
		}
		if !hasInline && !hasSymbols {
			return fmt.Errorf("config: paths[%d] (%s): package/func or symbols is required", i, p.Name)
		}
		if hasInline {
			if strings.TrimSpace(p.Package) == "" {
				return fmt.Errorf("config: paths[%d] (%s): package is required", i, p.Name)
			}
			if strings.TrimSpace(p.Func) == "" {
				return fmt.Errorf("config: paths[%d] (%s): func is required", i, p.Name)
			}
		}
		for j, s := range p.Symbols {
			if strings.TrimSpace(s.Package) == "" {
				return fmt.Errorf("config: paths[%d] (%s).symbols[%d]: package is required", i, p.Name, j)
			}
			if strings.TrimSpace(s.Func) == "" {
				return fmt.Errorf("config: paths[%d] (%s).symbols[%d]: func is required", i, p.Name, j)
			}
		}
	}
	if strings.TrimSpace(c.VTA.Package) == "" {
		return fmt.Errorf("config: vta.package is required")
	}
	if c.VTA.Func != "" && c.VTA.Func != "main" {
		return fmt.Errorf("config: vta.func must be main or omitted")
	}
	return nil
}

// MainFunc returns the configured main function name (default main).
func (c *Config) MainFunc() string {
	if c.VTA.Func != "" {
		return c.VTA.Func
	}
	return "main"
}

// FindConfig walks upward from repoRoot looking for .reachable.yaml then reachable.yaml.
func FindConfig(repoRoot string) (string, error) {
	dir, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", fmt.Errorf("resolving repo path: %w", err)
	}
	for {
		for _, name := range []string{configFileDot, configFilePlain} {
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no %s or %s found in %s or any parent directory", configFileDot, configFilePlain, repoRoot)
}
