package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Logiraptor/go-reachable/internal/callgraph"
	"github.com/Logiraptor/go-reachable/internal/config"
	"github.com/Logiraptor/go-reachable/internal/diffsyms"
	"github.com/Logiraptor/go-reachable/internal/reachable"
)

func main() {
	code := 0
	if err := run(&code); err != nil {
		fmt.Fprintf(os.Stderr, "reachable: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(exitCode *int) error {
	root := flag.NewFlagSet("reachable", flag.ContinueOnError)
	root.SetOutput(os.Stderr)

	repoDir := root.String("repo", ".", "path to the Go repository root (module or workspace root)")
	configPath := root.String("config", "", "path to YAML config (default: search for .reachable.yaml or reachable.yaml)")
	outputFmt := root.String("format", "text", "output format: text, json")
	algoStr := root.String("algo", "vta", "call graph algorithm: vta (default), cha, cha-pruned")
	failIfNone := root.Bool("fail-if-none-touched", false, "exit with code 2 when every named path is untouched (paths subcommand)")
	verbose := root.Bool("v", false, "verbose: for paths, print a PATH/TOUCHED table; for check, extra stderr detail")

	root.Usage = func() {
		fmt.Fprintf(os.Stderr, `reachable — diff + config → which product paths are touched (or legacy single-entry check)

Usage:
  reachable [global flags] paths [diff-file]
  reachable [global flags] check [diff-file]

Global flags:
`)
		root.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Examples:
  reachable -repo . paths /tmp/pr.diff
  git diff main...HEAD | reachable -repo . -config .reachable.yaml paths
  reachable -repo . -config .reachable.yaml paths < /tmp/pr.diff
  git diff main | reachable -repo . check -pkg example.com/m -func Main

Subcommands:
  paths   Read config, parse unified diff, print which named paths are touched (default multi-path workflow).
  check   Single entry point via -pkg/-func/-recv; diff from file argument or stdin.

The tool does not run git or gh; pass unified diff bytes via file or pipe.
`)
	}

	if err := root.Parse(os.Args[1:]); err != nil {
		return err
	}

	args := root.Args()
	var sub string
	var rest []string
	switch {
	case len(args) == 0:
		root.Usage()
		return fmt.Errorf("subcommand required: paths or check (you may omit the word \"paths\" and pass only a diff file)")
	case args[0] == "paths" || args[0] == "check":
		sub = args[0]
		rest = args[1:]
	default:
		sub = "paths"
		rest = args
	}

	repoAbs, err := filepath.Abs(*repoDir)
	if err != nil {
		return fmt.Errorf("resolving repo path: %w", err)
	}

	algorithm, err := parseAlgo(*algoStr)
	if err != nil {
		return err
	}

	switch sub {
	case "paths":
		return runPaths(repoAbs, *configPath, *outputFmt, algorithm, *failIfNone, *verbose, rest, exitCode)
	case "check":
		return runCheck(repoAbs, *outputFmt, algorithm, *verbose, rest, exitCode)
	default:
		return fmt.Errorf("unknown subcommand %q", sub)
	}
}

func parseAlgo(s string) (callgraph.Algorithm, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "vta":
		return callgraph.AlgoVTA, nil
	case "cha":
		return callgraph.AlgoCHA, nil
	case "cha-pruned":
		return callgraph.AlgoCHAPruned, nil
	default:
		return "", fmt.Errorf("unknown algorithm %q (vta, cha, cha-pruned)", s)
	}
}

func runPaths(repoDir, configPath, outputFmt string, algorithm callgraph.Algorithm, failIfNone, verbose bool, rest []string, exitCode *int) error {
	cfgPath := configPath
	if cfgPath == "" {
		var err error
		cfgPath, err = config.FindConfig(repoDir)
		if err != nil {
			return fmt.Errorf("config: %w (use -config or add .reachable.yaml)", err)
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	diffBytes, err := readUnifiedDiff(rest)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "stage 1: diff → symbols\n")
	regions, err := diffsyms.ParseDiff(diffBytes)
	if err != nil {
		return fmt.Errorf("parsing diff: %w", err)
	}

	symbols, err := diffsyms.ChangedSymbols(repoDir, regions)
	if err != nil {
		return fmt.Errorf("mapping symbols: %w", err)
	}

	funcSymbols := filterFuncSymbols(symbols)
	fmt.Fprintf(os.Stderr, "  %d changed regions → %d symbols (%d functions/methods)\n",
		len(regions), len(symbols), len(funcSymbols))

	if len(funcSymbols) == 0 {
		fmt.Fprintf(os.Stderr, "\nno changed functions/methods — nothing to check\n")
		if outputFmt == "json" {
			pathsOut := make([]map[string]any, 0, len(cfg.Paths))
			for _, p := range cfg.Paths {
				pathsOut = append(pathsOut, map[string]any{
					"name":    p.Name,
					"touched": false,
				})
			}
			out := map[string]any{
				"vta": map[string]string{
					"package": cfg.VTA.Package,
					"func":    cfg.MainFunc(),
				},
				"paths": pathsOut,
				"stats": map[string]any{"skipped": true},
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		}
		return nil
	}

	mainName := cfg.MainFunc()
	graphEntry := callgraph.EntryPoint{
		Package: cfg.VTA.Package,
		Func:    mainName,
	}

	queries := make([]reachable.PathQuery, 0, len(cfg.Paths))
	for _, p := range cfg.Paths {
		queries = append(queries, reachable.PathQuery{
			Name: p.Name,
			Entry: callgraph.EntryPoint{
				Package:  p.Package,
				Receiver: p.Recv,
				Func:     p.Func,
			},
		})
	}

	opts := reachable.MultiOptions{
		RepoDir:     repoDir,
		Algorithm:   algorithm,
		GraphEntry:  graphEntry,
		VTAMeta:     reachable.VTAAnchor{Package: cfg.VTA.Package, Func: mainName},
		PathQueries: queries,
	}

	fmt.Fprintf(os.Stderr, "\nstage 2–3: call graph [%s] + reachability per path\n", algorithm)
	totalStart := time.Now()

	multi, err := reachable.AnalyzePaths(opts, funcSymbols)
	if err != nil {
		return err
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	fmt.Fprintf(os.Stderr, "  load:      %v\n", multi.Stats.LoadTime)
	fmt.Fprintf(os.Stderr, "  ssa build: %v\n", multi.Stats.SSABuildTime)
	fmt.Fprintf(os.Stderr, "  cg build:  %v\n", multi.Stats.CGBuildTime)
	fmt.Fprintf(os.Stderr, "  walk sum:  %v\n", multi.Stats.WalkTime)
	fmt.Fprintf(os.Stderr, "  total:     %v\n", multi.Stats.TotalTime)
	fmt.Fprintf(os.Stderr, "  heap:      %d MB\n", mem.HeapAlloc/1024/1024)
	fmt.Fprintf(os.Stderr, "\nreachability check (%v)\n", time.Since(totalStart))

	switch outputFmt {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(multi); err != nil {
			return err
		}
	default:
		printPathsText(multi, verbose)
	}

	anyTouched := false
	for _, p := range multi.Paths {
		if p.Touched {
			anyTouched = true
			break
		}
	}
	if failIfNone && !anyTouched {
		*exitCode = 2
	}
	return nil
}

func runCheck(repoDir, outputFmt string, algorithm callgraph.Algorithm, verbose bool, rest []string, exitCode *int) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pkg := fs.String("pkg", "", "full import path of the entry point package (required)")
	recv := fs.String("recv", "", "receiver type for methods (e.g. \"*QuerierAPI\")")
	funcName := fs.String("func", "", "function or method name (required)")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: reachable [global flags] check [flags] [diff-file]\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *pkg == "" || *funcName == "" {
		fs.Usage()
		return fmt.Errorf("-pkg and -func are required")
	}

	diffBytes, err := readUnifiedDiff(fs.Args())
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "stage 1: diff → symbols\n")
	regions, err := diffsyms.ParseDiff(diffBytes)
	if err != nil {
		return fmt.Errorf("parsing diff: %w", err)
	}

	symbols, err := diffsyms.ChangedSymbols(repoDir, regions)
	if err != nil {
		return fmt.Errorf("mapping symbols: %w", err)
	}

	funcSymbols := filterFuncSymbols(symbols)
	fmt.Fprintf(os.Stderr, "  %d changed regions → %d symbols (%d functions/methods)\n",
		len(regions), len(symbols), len(funcSymbols))

	if len(funcSymbols) == 0 {
		fmt.Fprintf(os.Stderr, "\nno changed functions/methods — nothing to check\n")
		return nil
	}

	entry := callgraph.EntryPoint{
		Package:  *pkg,
		Receiver: *recv,
		Func:     *funcName,
	}

	fmt.Fprintf(os.Stderr, "\nstage 2–3: call graph from %s [%s]\n", entry, algorithm)
	if verbose {
		fmt.Fprintf(os.Stderr, "  loading packages...\n")
	}

	totalStart := time.Now()
	result, err := reachable.Analyze(reachable.Options{
		RepoDir:   repoDir,
		Entry:     entry,
		Algorithm: algorithm,
	}, funcSymbols)
	if err != nil {
		return err
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	fmt.Fprintf(os.Stderr, "  load:      %v\n", result.Stats.LoadTime)
	fmt.Fprintf(os.Stderr, "  ssa build: %v\n", result.Stats.SSABuildTime)
	fmt.Fprintf(os.Stderr, "  cg build:  %v\n", result.Stats.CGBuildTime)
	fmt.Fprintf(os.Stderr, "  walk:      %v\n", result.Stats.WalkTime)
	fmt.Fprintf(os.Stderr, "  reachable: %d functions\n", result.Stats.Reachable)
	fmt.Fprintf(os.Stderr, "  heap:      %d MB\n", mem.HeapAlloc/1024/1024)
	fmt.Fprintf(os.Stderr, "\nreachability check (%v)\n", time.Since(totalStart))

	switch outputFmt {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			return err
		}
		if !result.Touched {
			*exitCode = 2
		}
		return nil
	default:
		return printCheckText(result, entry, exitCode)
	}
}

func printPathsText(m reachable.MultiResult, verbose bool) {
	if verbose {
		fmt.Printf("%-32s %s\n", "PATH", "TOUCHED")
		for _, p := range m.Paths {
			touched := "no"
			if p.Touched {
				touched = "yes"
			}
			fmt.Printf("%-32s %s\n", p.Name, touched)
		}
		return
	}
	var touched, untouched []string
	for _, p := range m.Paths {
		if p.Touched {
			touched = append(touched, p.Name)
		} else {
			untouched = append(untouched, p.Name)
		}
	}
	if len(touched) > 0 {
		fmt.Printf("touched: %s\n", strings.Join(touched, ", "))
	}
	if len(untouched) > 0 {
		fmt.Printf("not touched: %s\n", strings.Join(untouched, ", "))
	}
}

func printCheckText(result reachable.Result, entry callgraph.EntryPoint, exitCode *int) error {
	if result.Touched {
		fmt.Printf("REACHABLE — %d changed symbol(s) reachable from %s\n\n", len(result.Matches), entry)
		for _, m := range result.Matches {
			fmt.Printf("  %-8s %-55s  depth=%d\n", m.Symbol.Kind, m.Symbol, m.Function.Depth)
		}
		fmt.Println()
		return nil
	}
	fmt.Printf("NOT REACHABLE — no changed symbols reachable from %s\n", entry)
	*exitCode = 2
	return nil
}

func filterFuncSymbols(symbols []diffsyms.Symbol) []diffsyms.Symbol {
	var out []diffsyms.Symbol
	for _, s := range symbols {
		if s.Kind == "func" || s.Kind == "method" || s.Kind == "init" {
			out = append(out, s)
		}
	}
	return out
}

func readUnifiedDiff(rest []string) ([]byte, error) {
	switch len(rest) {
	case 0:
		return readStdinDiff()
	case 1:
		if rest[0] == "-" {
			return readStdinDiff()
		}
		return os.ReadFile(rest[0])
	default:
		return nil, fmt.Errorf("expected at most one diff file argument, got %d", len(rest))
	}
}

func readStdinDiff() ([]byte, error) {
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		return nil, fmt.Errorf("no diff input: specify a diff file or pipe a unified diff to stdin")
	}
	return io.ReadAll(os.Stdin)
}
