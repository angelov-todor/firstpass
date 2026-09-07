package ghpr

import (
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/runner"
)

func TestPRDiffReturnsTheDiff(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "pr diff", Result: runner.Result{Stdout: []byte("--- a/x\n+++ b/x\n+ line\n")}},
	}}
	diff, truncated, err := New(f, "gh").PRDiff(t.Context(), fbRef)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("a short diff is not truncated")
	}
	if !strings.Contains(diff, "+ line") {
		t.Errorf("diff = %q", diff)
	}
}

// A cut diff must be reported as cut, and cut on a line boundary.
//
// Both halves matter. A reviewer that believes it saw the whole change reads
// the absence of a rename as evidence the rename was not needed, which is the
// cross-repo finding this exists to produce, inverted. And a diff ending
// mid-hunk reads as a malformed hunk rather than a truncated file, leaving the
// reviewer reasoning about a change that appears to stop in the middle of a
// line.
func TestPRDiffTruncatesOnALineBoundaryAndSaysSo(t *testing.T) {
	huge := strings.Repeat("+ a line of diff that is long enough to matter\n", 2000)
	if len(huge) <= MaxDiffBytes {
		t.Fatalf("fixture is not large enough to trigger truncation: %d bytes", len(huge))
	}
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "pr diff", Result: runner.Result{Stdout: []byte(huge)}},
	}}
	diff, truncated, err := New(f, "gh").PRDiff(t.Context(), fbRef)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Error("an over-cap diff must report itself truncated")
	}
	if len(diff) > MaxDiffBytes {
		t.Errorf("diff is %d bytes, cap is %d", len(diff), MaxDiffBytes)
	}
	// Every line in the fixture is identical, so "ends on a boundary" means the
	// text after the final newline is either empty or one whole line.
	const line = "+ a line of diff that is long enough to matter"
	if tail := diff[strings.LastIndexByte(diff, '\n')+1:]; tail != "" && tail != line {
		t.Errorf("the cut left a partial line: %q", tail)
	}
}

func TestPRDiffReportsAFailedCall(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "pr diff", Result: runner.Result{ExitCode: 1, Stderr: []byte("gh: no commits between branches")}},
	}}
	if _, _, err := New(f, "gh").PRDiff(t.Context(), fbRef); err == nil {
		t.Error("a non-zero gh exit must be an error: the caller drops the context and reviews anyway")
	}
}
