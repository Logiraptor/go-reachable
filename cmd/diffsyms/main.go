package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/Logiraptor/go-reachable/internal/diffsyms"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "diffsyms: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		repoDir    string
		prNumber   string
		baseBranch string
		outputFmt  string
	)

	flag.StringVar(&repoDir, "repo", ".", "path to the Go repository root")
	flag.StringVar(&prNumber, "pr", "", "GitHub PR number (fetches diff via gh CLI)")
	flag.StringVar(&baseBranch, "base", "", "base branch/commit to diff against (uses git diff <base>...HEAD)")
	flag.StringVar(&outputFmt, "format", "text", "output format: text, json")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `diffsyms — map a git diff to changed Go symbols

Usage:
  diffsyms -repo ~/workspace/loki -pr 21173
  diffsyms -repo ~/workspace/loki -base main
  git diff main...HEAD | diffsyms -repo ~/workspace/loki
  cat pr.diff | diffsyms -repo ~/workspace/loki

Flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	repoDir, err := filepath.Abs(repoDir)
	if err != nil {
		return fmt.Errorf("resolving repo path: %w", err)
	}

	diffBytes, err := getDiff(repoDir, prNumber, baseBranch)
	if err != nil {
		return err
	}

	regions, err := diffsyms.ParseDiff(diffBytes)
	if err != nil {
		return fmt.Errorf("parsing diff: %w", err)
	}

	symbols, err := diffsyms.ChangedSymbols(repoDir, regions)
	if err != nil {
		return fmt.Errorf("mapping symbols: %w", err)
	}

	return printSymbols(symbols, outputFmt)
}

func getDiff(repoDir, prNumber, baseBranch string) ([]byte, error) {
	switch {
	case prNumber != "":
		return getDiffFromPR(repoDir, prNumber)
	case baseBranch != "":
		return getDiffFromGit(repoDir, baseBranch)
	default:
		return getDiffFromStdin()
	}
}

func getDiffFromPR(repoDir, prNumber string) ([]byte, error) {
	if _, err := strconv.Atoi(prNumber); err != nil {
		return nil, fmt.Errorf("invalid PR number: %q", prNumber)
	}
	cmd := exec.Command("gh", "pr", "diff", prNumber)
	cmd.Dir = repoDir
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("fetching PR diff: %w", err)
	}
	return out, nil
}

func getDiffFromGit(repoDir, baseBranch string) ([]byte, error) {
	cmd := exec.Command("git", "diff", baseBranch+"...HEAD")
	cmd.Dir = repoDir
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running git diff: %w", err)
	}
	return out, nil
}

func getDiffFromStdin() ([]byte, error) {
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		return nil, fmt.Errorf("no input: use -pr, -base, or pipe a diff to stdin")
	}
	return io.ReadAll(os.Stdin)
}

type symbolOutput struct {
	Package  string `json:"package"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Receiver string `json:"receiver,omitempty"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	FQN      string `json:"fqn"`
}

func printSymbols(symbols []diffsyms.Symbol, format string) error {
	if len(symbols) == 0 {
		fmt.Fprintln(os.Stderr, "no Go symbols changed")
		return nil
	}

	switch format {
	case "json":
		out := make([]symbolOutput, len(symbols))
		for i, s := range symbols {
			out[i] = symbolOutput{
				Package:  s.Package,
				Name:     s.Name,
				Kind:     s.Kind,
				Receiver: s.Receiver,
				File:     s.File,
				Line:     s.Line,
				FQN:      s.String(),
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)

	default:
		var maxKind int
		for _, s := range symbols {
			if len(s.Kind) > maxKind {
				maxKind = len(s.Kind)
			}
		}
		for _, s := range symbols {
			loc := fmt.Sprintf("%s:%d", s.File, s.Line)
			fmt.Printf("%-*s  %-60s  %s\n", maxKind, s.Kind, s.String(), loc)
		}
		return nil
	}
}

