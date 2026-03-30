package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Logiraptor/go-reachable/internal/callgraph"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "cgdiff: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dir                 string
		pkg                 string
		receiver            string
		funcName            string
		outputFmt           string
		topN                int
		verbose             bool
		pruneRatioThreshold float64
		minExcess           int
	)

	flag.StringVar(&dir, "dir", ".", "path to the Go module root")
	flag.StringVar(&pkg, "pkg", "", "full import path of the entry point package (required)")
	flag.StringVar(&receiver, "recv", "", "receiver type name for methods (e.g., \"*Distributor\")")
	flag.StringVar(&funcName, "func", "", "function or method name (required)")
	flag.StringVar(&outputFmt, "format", "text", "output format: text, json")
	flag.IntVar(&topN, "top", 20, "show top N interfaces by excess edges (0 = all)")
	flag.BoolVar(&verbose, "v", false, "show individual callee functions for each interface")
	flag.Float64Var(&pruneRatioThreshold, "prune-ratio", callgraph.DefaultPruneRatioThreshold,
		"mark cha-pruned targets when CHAOnly/CHAEdges >= this (0–1)")
	flag.IntVar(&minExcess, "min-excess", callgraph.DefaultMinExcessEdges,
		"mark cha-pruned targets only when CHA-only edge count >= this")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `cgdiff — compare CHA vs VTA call graphs and show false positives by interface

Shows which Go interfaces cause the most CHA over-approximation (false positive
edges) compared to VTA. Helps identify where CHA's "every implementor" strategy
produces the most noise.

Usage:
  cgdiff -dir ~/workspace/loki -pkg github.com/grafana/loki/v3/pkg/distributor -recv "*Distributor" -func PushHandler
  cgdiff -dir ~/workspace/loki -pkg github.com/grafana/loki/v3/pkg/distributor -recv "*Distributor" -func PushHandler -v
  cgdiff -dir ~/workspace/loki -pkg github.com/grafana/loki/v3/pkg/distributor -recv "*Distributor" -func PushHandler -format json

Flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	if pkg == "" || funcName == "" {
		flag.Usage()
		return fmt.Errorf("-pkg and -func are required")
	}

	dir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolving dir: %w", err)
	}

	entry := callgraph.EntryPoint{
		Package:  pkg,
		Receiver: receiver,
		Func:     funcName,
	}

	fmt.Fprintf(os.Stderr, "entry: %s\n", entry)
	fmt.Fprintf(os.Stderr, "building CHA and VTA call graphs...\n")

	res, err := callgraph.Compare(dir, entry)
	if err != nil {
		return err
	}

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	fmt.Fprintf(os.Stderr, "heap: %d MB | sys: %d MB\n",
		memStats.HeapAlloc/1024/1024, memStats.Sys/1024/1024)

	switch strings.ToLower(outputFmt) {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	default:
		fmt.Print(callgraph.FormatCompareText(res, topN, pruneRatioThreshold, minExcess))
		if verbose {
			fmt.Print(callgraph.FormatCompareCallees(res, topN))
		}
		return nil
	}
}
