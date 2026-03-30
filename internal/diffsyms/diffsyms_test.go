package diffsyms_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Logiraptor/go-reachable/internal/diffsyms"
)

func TestChangedSymbols_AllGoFilesMissingFromRepo(t *testing.T) {
	wrongRoot := t.TempDir()
	diff := `diff --git a/pkg/missing.go b/pkg/missing.go
index aaa..bbb 100644
--- a/pkg/missing.go
+++ b/pkg/missing.go
@@ -1,3 +1,3 @@
 package p
-func A() {}
+func B() {}
`
	regions, err := diffsyms.ParseDiff([]byte(diff))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	if len(regions) == 0 {
		t.Fatal("expected at least one region")
	}
	_, err = diffsyms.ChangedSymbols(wrongRoot, regions)
	if err == nil {
		t.Fatal("expected error when diff paths are absent from repo root")
	}
	if !errors.Is(err, diffsyms.ErrDiffFilesNotInRepo) {
		t.Fatalf("expected ErrDiffFilesNotInRepo, got: %v", err)
	}
}

// lokiPR21173Diff is the diff from grafana/loki PR #21173:
// "feat(query-engine): Omit labels with empty values"
// 4 files changed: compat.go, compat_test.go, streams_view.go, streams_view_test.go
const lokiPR21173Diff = `diff --git a/pkg/engine/compat.go b/pkg/engine/compat.go
index 1ec9881cde4a9..5cca332a7fae8 100644
--- a/pkg/engine/compat.go
+++ b/pkg/engine/compat.go
@@ -115,7 +115,12 @@ func (b *streamsResultBuilder) CollectRecord(rec arrow.RecordBatch) {
 		case ident.ColumnType() == types.ColumnTypeLabel:
 			labelCol := col.(*array.String)
 			forEachNotNullRowColValue(numRows, labelCol, func(rowIdx int) {
-				b.rowBuilders[rowIdx].lbsBuilder.Set(shortName, labelCol.Value(rowIdx))
+				val := labelCol.Value(rowIdx)
+				if val == "" {
+					// We also drop empty labels from stream labels to match classic Loki engine behavior.
+					return
+				}
+				b.rowBuilders[rowIdx].lbsBuilder.Set(shortName, val)
 			})
 
 		// One of the metadata columns
diff --git a/pkg/engine/compat_test.go b/pkg/engine/compat_test.go
index 7a1006b090182..2f7636f75e451 100644
--- a/pkg/engine/compat_test.go
+++ b/pkg/engine/compat_test.go
@@ -473,6 +473,68 @@ func TestStreamsResultBuilder(t *testing.T) {
 		}
 		require.Equal(t, expected, streams)
 	})
+	t.Run("labels with empty values are dropped from stream labels", func(t *testing.T) {
+		colTs := semconv.ColumnIdentTimestamp
+		colMsg := semconv.ColumnIdentMessage
+		colEnv := semconv.NewIdentifier("env", types.ColumnTypeLabel, types.Loki.String)
+		colRegion := semconv.NewIdentifier("region", types.ColumnTypeLabel, types.Loki.String)
+
+		schema := arrow.NewSchema(
+			[]arrow.Field{
+				semconv.FieldFromIdent(colTs, false),
+				semconv.FieldFromIdent(colMsg, false),
+				semconv.FieldFromIdent(colEnv, true),
+				semconv.FieldFromIdent(colRegion, true),
+			},
+			nil,
+		)
+		rows := arrowtest.Rows{
+			{
+				colTs.FQN():     time.Unix(0, 1620000000000000001).UTC(),
+				colMsg.FQN():    "log line 1",
+				colEnv.FQN():    "prod",
+				colRegion.FQN(): "us-west",
+			},
+			{
+				colTs.FQN():     time.Unix(0, 1620000000000000002).UTC(),
+				colMsg.FQN():    "log line 2",
+				colEnv.FQN():    "prod",
+				colRegion.FQN(): "",
+			},
+			{
+				colTs.FQN():     time.Unix(0, 1620000000000000003).UTC(),
+				colMsg.FQN():    "log line 3",
+				colEnv.FQN():    "",
+				colRegion.FQN(): "us-east",
+			},
+		}
+
+		record := rows.Record(memory.DefaultAllocator, schema)
+
+		pipeline := executor.NewBufferedPipeline(record)
+		defer pipeline.Close()
+
+		builder := newStreamsResultBuilder(logproto.FORWARD, false)
+		err := collectResult(context.Background(), pipeline, builder)
+
+		require.NoError(t, err)
+		require.Equal(t, 3, builder.Len())
+
+		md, _ := metadata.NewContext(t.Context())
+		result := builder.Build(stats.Result{}, md)
+		streams := result.Data.(logqlmodel.Streams)
+
+		require.Equal(t, 3, len(streams), "should have 3 unique streams (empty label values dropped)")
+
+		streamLabels := make([]string, len(streams))
+		for i, s := range streams {
+			streamLabels[i] = s.Labels
+		}
+		require.Contains(t, streamLabels, labels.FromStrings("env", "prod", "region", "us-west").String())
+		require.Contains(t, streamLabels, labels.FromStrings("env", "prod").String())
+		require.Contains(t, streamLabels, labels.FromStrings("region", "us-east").String())
+	})
+
 	t.Run("categorize labels does not consider metadata or parsed keys when building output streams", func(t *testing.T) {
 		colTs := semconv.ColumnIdentTimestamp
 		colMsg := semconv.ColumnIdentMessage
diff --git a/pkg/engine/internal/executor/streams_view.go b/pkg/engine/internal/executor/streams_view.go
index 68dc2672ad8c8..ebfaeeb568a25 100644
--- a/pkg/engine/internal/executor/streams_view.go
+++ b/pkg/engine/internal/executor/streams_view.go
@@ -180,6 +180,11 @@ func (v *streamsView) Labels(ctx context.Context, id int64) ([]labels.Label, err
 			panic(fmt.Sprintf("unexpected column type %T for labels", colValues))
 		}
 
+		// Drop labels with empty values to match classic Loki engine behavior.
+		if label.Value == "" {
+			continue
+		}
+
 		lbs = append(lbs, label)
 	}
 
diff --git a/pkg/engine/internal/executor/streams_view_test.go b/pkg/engine/internal/executor/streams_view_test.go
index 3d318a281a475..07ed5a09a946e 100644
--- a/pkg/engine/internal/executor/streams_view_test.go
+++ b/pkg/engine/internal/executor/streams_view_test.go
@@ -141,6 +141,33 @@ func Test_streamsView(t *testing.T) {
 		require.Equal(t, expect, actual, "expected all streams to be returned with the proper labels")
 	})
 
+	t.Run("labels with empty values are dropped", func(t *testing.T) {
+		emptyStreams := []labels.Labels{
+			labels.FromStrings("app", "loki", "env", "prod", "region", "us-west"),
+			labels.FromStrings("app", "loki", "env", ""),
+			labels.FromStrings("app", "", "env", "prod"),
+		}
+
+		emptySec := buildStreamsSection(t, emptyStreams)
+
+		view := newStreamsView(emptySec, &streamsViewOptions{
+			BatchSize: 1,
+		})
+		require.NoError(t, view.Open(t.Context()))
+
+		lbs1, err := view.Labels(t.Context(), 1)
+		require.NoError(t, err)
+		require.Equal(t, labels.FromStrings("app", "loki", "env", "prod", "region", "us-west"), labels.New(lbs1...))
+
+		lbs2, err := view.Labels(t.Context(), 2)
+		require.NoError(t, err)
+		require.Equal(t, labels.FromStrings("app", "loki"), labels.New(lbs2...), "empty env value should be dropped")
+
+		lbs3, err := view.Labels(t.Context(), 3)
+		require.NoError(t, err)
+		require.Equal(t, labels.FromStrings("env", "prod"), labels.New(lbs3...), "empty app value should be dropped")
+	})
+
 	t.Run("labels before open returns error", func(t *testing.T) {
 		view := newStreamsView(sec, &streamsViewOptions{BatchSize: 1})
 		lbs, err := view.Labels(t.Context(), 1)
`

func TestParseDiff_LokiPR21173(t *testing.T) {
	regions, err := diffsyms.ParseDiff([]byte(lokiPR21173Diff))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}

	// PR touches 4 files, each with 1 hunk = 4 regions
	if len(regions) != 4 {
		t.Fatalf("expected 4 regions, got %d", len(regions))
	}

	// Verify each region
	tests := []struct {
		file      string
		startLine int
		endLine   int
	}{
		{"pkg/engine/compat.go", 115, 126},
		{"pkg/engine/compat_test.go", 473, 540},
		{"pkg/engine/internal/executor/streams_view.go", 180, 190},
		{"pkg/engine/internal/executor/streams_view_test.go", 141, 173},
	}

	for i, tt := range tests {
		r := regions[i]
		if r.File != tt.file {
			t.Errorf("region[%d].File = %q, want %q", i, r.File, tt.file)
		}
		if r.StartLine != tt.startLine {
			t.Errorf("region[%d].StartLine = %d, want %d", i, r.StartLine, tt.startLine)
		}
		if r.EndLine != tt.endLine {
			t.Errorf("region[%d].EndLine = %d, want %d", i, r.EndLine, tt.endLine)
		}
	}
}

// TestNewSideLineNumbers_RenameFunction proves that when a function is renamed,
// the tool detects the NEW name (since it uses new-side line numbers against the
// new-side file). The OLD name is not reported — it no longer exists.
// This is correct behavior: we want "what symbols are affected in the new code?"
func TestNewSideLineNumbers_RenameFunction(t *testing.T) {
	// Set up a temp "repo" with a Go file that has the NEW function name.
	// The diff renames OldFunc → NewFunc.
	repoRoot := t.TempDir()
	mkFile(t, filepath.Join(repoRoot, "pkg", "example", "rename.go"), `package example

func Helper() int {
	return 1
}

func NewFunc() string {
	return "new"
}

func Unrelated() bool {
	return true
}
`)

	// Diff: rename OldFunc → NewFunc (the function signature line changes).
	// The hunk's new-side lines cover the NewFunc declaration.
	diff := `diff --git a/pkg/example/rename.go b/pkg/example/rename.go
index aaa..bbb 100644
--- a/pkg/example/rename.go
+++ b/pkg/example/rename.go
@@ -4,7 +4,7 @@ func Helper() int {
 	return 1
 }
 
-func OldFunc() string {
+func NewFunc() string {
 	return "new"
 }
 
`
	regions, err := diffsyms.ParseDiff([]byte(diff))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}

	symbols, err := diffsyms.ChangedSymbols(repoRoot, regions)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}

	// Should find NewFunc (the renamed function) — it exists in the new file
	// and the hunk's new-side lines overlap its declaration.
	found := symbolNames(symbols)
	if !found["NewFunc"] {
		t.Errorf("expected NewFunc to be detected, got symbols: %v", symbols)
	}
	// OldFunc should NOT appear — it doesn't exist in the new file.
	if found["OldFunc"] {
		t.Errorf("OldFunc should not appear (it no longer exists), got symbols: %v", symbols)
	}
	// Unrelated should NOT appear.
	if found["Unrelated"] {
		t.Errorf("Unrelated should not appear, got symbols: %v", symbols)
	}
}

// TestNewSideLineNumbers_PureDeletionInsideFunction proves that a pure-deletion
// hunk (NewLines == 0) is skipped, which means deleting lines from inside a
// function WITHOUT any context lines would be missed.
//
// In practice this only happens with -U0 (zero context) diffs. Standard unified
// diffs always include 3 context lines, so NewLines > 0 even for pure deletions.
// But this test documents the theoretical gap.
func TestNewSideLineNumbers_PureDeletionInsideFunction(t *testing.T) {
	// A -U0 diff that deletes a line inside a function.
	// NewStartLine=7, NewLines=0 → pure deletion, skipped by ParseDiff.
	diff := `diff --git a/pkg/example/del.go b/pkg/example/del.go
index aaa..bbb 100644
--- a/pkg/example/del.go
+++ b/pkg/example/del.go
@@ -7,1 +7,0 @@ func Foo() {
-	fmt.Println("deleted line")
`
	regions, err := diffsyms.ParseDiff([]byte(diff))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}

	// Pure deletion hunk: NewLines == 0, so ParseDiff skips it.
	// This means the change to Foo is NOT detected.
	if len(regions) != 0 {
		t.Fatalf("expected 0 regions for pure-deletion hunk, got %d: %v", len(regions), regions)
	}

	// With standard context (3 lines), the same deletion DOES get detected:
	diffWithContext := `diff --git a/pkg/example/del.go b/pkg/example/del.go
index aaa..bbb 100644
--- a/pkg/example/del.go
+++ b/pkg/example/del.go
@@ -4,7 +4,6 @@ package example
 
 func Foo() {
 	fmt.Println("keep")
-	fmt.Println("deleted line")
 	fmt.Println("also keep")
 }
 
`
	repoRoot := t.TempDir()
	mkFile(t, filepath.Join(repoRoot, "pkg", "example", "del.go"), `package example

import "fmt"

func Foo() {
	fmt.Println("keep")
	fmt.Println("also keep")
}
`)

	regions, err = diffsyms.ParseDiff([]byte(diffWithContext))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	if len(regions) != 1 {
		t.Fatalf("expected 1 region with context, got %d", len(regions))
	}

	symbols, err := diffsyms.ChangedSymbols(repoRoot, regions)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	found := symbolNames(symbols)
	if !found["Foo"] {
		t.Errorf("expected Foo to be detected with context lines, got: %v", symbols)
	}
}

// TestNewSideLineNumbers_FunctionDeletedEntirely proves that when an entire
// function is deleted, it's correctly skipped (the function no longer exists
// in the new code, so there's nothing to analyze).
func TestNewSideLineNumbers_FunctionDeletedEntirely(t *testing.T) {
	repoRoot := t.TempDir()
	mkFile(t, filepath.Join(repoRoot, "pkg", "example", "gone.go"), `package example

func StillHere() {}
`)

	// Diff deletes RemovedFunc entirely. The new-side context lines only
	// cover StillHere and surrounding whitespace.
	diff := `diff --git a/pkg/example/gone.go b/pkg/example/gone.go
index aaa..bbb 100644
--- a/pkg/example/gone.go
+++ b/pkg/example/gone.go
@@ -1,7 +1,3 @@
 package example
 
-func RemovedFunc() {
-	return
-}
-
 func StillHere() {}
`
	regions, err := diffsyms.ParseDiff([]byte(diff))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}

	symbols, err := diffsyms.ChangedSymbols(repoRoot, regions)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}

	found := symbolNames(symbols)
	// RemovedFunc doesn't exist in the new file, so it can't be detected.
	if found["RemovedFunc"] {
		t.Errorf("RemovedFunc should not appear (deleted), got: %v", symbols)
	}
	// StillHere IS in the new-side range (the context lines shift it up).
	// Whether it appears depends on whether the hunk range overlaps its declaration.
	// New-side: lines 1-3 → "package example\n\nfunc StillHere() {}"
	// StillHere is on line 3, so it DOES overlap.
	if !found["StillHere"] {
		t.Errorf("expected StillHere to be detected (context lines overlap), got: %v", symbols)
	}
}

// TestNewSideLineNumbers_RenameAndModifyBody proves the tool handles the case
// where a function is renamed AND its body is modified in the same hunk.
// Note: context lines in the hunk cause neighboring functions (Alpha, Gamma)
// to also be detected — this is a known false-positive from hunk-level granularity,
// not a false-negative gap.
func TestNewSideLineNumbers_RenameAndModifyBody(t *testing.T) {
	repoRoot := t.TempDir()
	mkFile(t, filepath.Join(repoRoot, "pkg", "example", "both.go"), `package example

func Alpha() int {
	return 1
}

func BetaRenamed() string {
	return "modified"
}

func Gamma() bool {
	return true
}
`)

	diff := `diff --git a/pkg/example/both.go b/pkg/example/both.go
index aaa..bbb 100644
--- a/pkg/example/both.go
+++ b/pkg/example/both.go
@@ -4,8 +4,8 @@ func Alpha() int {
 	return 1
 }
 
-func Beta() string {
-	return "original"
+func BetaRenamed() string {
+	return "modified"
 }
 
 func Gamma() bool {
`
	regions, err := diffsyms.ParseDiff([]byte(diff))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}

	symbols, err := diffsyms.ChangedSymbols(repoRoot, regions)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}

	found := symbolNames(symbols)
	if !found["BetaRenamed"] {
		t.Errorf("expected BetaRenamed, got: %v", symbols)
	}
	if found["Beta"] {
		t.Errorf("Beta should not appear (renamed away), got: %v", symbols)
	}
	// Alpha and Gamma appear as false positives because the hunk's context
	// lines (3 lines by default) overlap their declarations. This is the
	// known trade-off of hunk-level granularity — over-reporting, not under-reporting.
	// A line-level diff parser could eliminate these, but hunk-level is safe
	// because it errs on the side of inclusion.
}

// TestNewSideLineNumbers_PureDeletionZeroContext is the actual gap:
// a -U0 diff that deletes lines inside a function produces NewLines=0,
// which ParseDiff skips. The function IS modified but we don't detect it.
func TestNewSideLineNumbers_PureDeletionZeroContext(t *testing.T) {
	repoRoot := t.TempDir()
	mkFile(t, filepath.Join(repoRoot, "pkg", "example", "zctx.go"), `package example

import "fmt"

func Modified() {
	fmt.Println("keep")
	fmt.Println("also keep")
}
`)

	// -U0 diff: delete one line from inside Modified(), no context lines.
	// @@ -7,1 +6,0 @@ means: old side has 1 line at 7, new side has 0 lines at 6.
	diff := `diff --git a/pkg/example/zctx.go b/pkg/example/zctx.go
index aaa..bbb 100644
--- a/pkg/example/zctx.go
+++ b/pkg/example/zctx.go
@@ -7,1 +6,0 @@ func Modified() {
-	fmt.Println("deleted")
`
	regions, err := diffsyms.ParseDiff([]byte(diff))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}

	// GAP: NewLines=0 → region is skipped → Modified() is NOT detected.
	// This is a false negative, but only possible with -U0 diffs.
	if len(regions) != 0 {
		t.Logf("regions: %v (if non-zero, the gap is fixed!)", regions)
	}

	// With standard context (-U3, the default), the deletion IS detected:
	diffU3 := `diff --git a/pkg/example/zctx.go b/pkg/example/zctx.go
index aaa..bbb 100644
--- a/pkg/example/zctx.go
+++ b/pkg/example/zctx.go
@@ -4,6 +4,5 @@ import "fmt"
 
 func Modified() {
 	fmt.Println("keep")
-	fmt.Println("deleted")
 	fmt.Println("also keep")
 }
`
	regions, err = diffsyms.ParseDiff([]byte(diffU3))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	if len(regions) != 1 {
		t.Fatalf("expected 1 region with -U3, got %d", len(regions))
	}

	symbols, err := diffsyms.ChangedSymbols(repoRoot, regions)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	found := symbolNames(symbols)
	if !found["Modified"] {
		t.Errorf("expected Modified with -U3 context, got: %v", symbols)
	}
}

func mkFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symbolNames(symbols []diffsyms.Symbol) map[string]bool {
	m := make(map[string]bool)
	for _, s := range symbols {
		m[s.Name] = true
	}
	return m
}

func TestChangedSymbols_LokiPR21173(t *testing.T) {
	lokiRoot := filepath.Join(os.Getenv("HOME"), "workspace", "loki")
	if _, err := os.Stat(lokiRoot); err != nil {
		t.Skipf("loki repo not found at %s: %v", lokiRoot, err)
	}

	// We need the files from the PR's target branch. The diff uses "new" side
	// line numbers, so we need the files as they exist after the PR merged.
	// Since loki is checked out at HEAD (which includes this PR), this works.
	regions, err := diffsyms.ParseDiff([]byte(lokiPR21173Diff))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}

	symbols, err := diffsyms.ChangedSymbols(lokiRoot, regions)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}

	if len(symbols) == 0 {
		t.Fatal("expected at least one changed symbol")
	}

	// Print all symbols for manual validation
	t.Logf("Found %d changed symbols:", len(symbols))
	for _, s := range symbols {
		t.Logf("  [%s] %s (line %d)", s.Kind, s, s.Line)
	}

	// Validate expected symbols are present.
	// The PR changes:
	// 1. (*streamsResultBuilder).CollectRecord in pkg/engine/compat.go
	// 2. TestStreamsResultBuilder in pkg/engine/compat_test.go
	// 3. (*streamsView).Labels in pkg/engine/internal/executor/streams_view.go
	// 4. Test_streamsView in pkg/engine/internal/executor/streams_view_test.go
	expected := map[string]bool{
		"pkg/engine.(*streamsResultBuilder).CollectRecord":                     false,
		"pkg/engine.TestStreamsResultBuilder":                                  false,
		"pkg/engine/internal/executor.(*streamsView).Labels":                  false,
		"pkg/engine/internal/executor.Test_streamsView":                       false,
	}

	for _, s := range symbols {
		key := s.String()
		if _, ok := expected[key]; ok {
			expected[key] = true
		}
	}

	for key, found := range expected {
		if !found {
			t.Errorf("expected symbol not found: %s", key)
		}
	}
}
