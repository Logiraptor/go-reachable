package diffsyms

import (
	"fmt"

	godiff "github.com/sourcegraph/go-diff/diff"
)

// ChangedRegion represents a contiguous range of changed lines in a file.
type ChangedRegion struct {
	File      string
	StartLine int // 1-based, inclusive
	EndLine   int // 1-based, inclusive
}

// ParseDiff extracts changed line regions from a unified diff.
// It returns one ChangedRegion per hunk, using the "new file" side line numbers
// (what the code looks like after the change).
func ParseDiff(diffBytes []byte) ([]ChangedRegion, error) {
	fileDiffs, err := godiff.ParseMultiFileDiff(diffBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing diff: %w", err)
	}

	var regions []ChangedRegion
	for _, fd := range fileDiffs {
		filename := fd.NewName
		if filename == "" || filename == "/dev/null" {
			continue // deleted file
		}
		// go-diff prefixes with "b/"
		filename = stripDiffPrefix(filename)

		for _, hunk := range fd.Hunks {
			start, end := hunkNewLines(hunk)
			if start == 0 {
				continue // pure deletion hunk
			}
			regions = append(regions, ChangedRegion{
				File:      filename,
				StartLine: int(start),
				EndLine:   int(end),
			})
		}
	}
	return regions, nil
}

// hunkNewLines returns the 1-based start and end line numbers of the added/modified
// lines in a hunk (the "new" side). For pure deletions, returns (0, 0).
func hunkNewLines(h *godiff.Hunk) (start, end int32) {
	if h.NewLines == 0 {
		return 0, 0
	}
	return h.NewStartLine, h.NewStartLine + h.NewLines - 1
}

func stripDiffPrefix(name string) string {
	if len(name) > 2 && (name[:2] == "a/" || name[:2] == "b/") {
		return name[2:]
	}
	return name
}
