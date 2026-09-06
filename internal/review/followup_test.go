package review

// Finding 5: a dry run could be reported as a killed review, and a failed dry
// run left the operator nothing to read.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/runner"
)

// blockedReportsDir returns a reports directory that cannot be created,
// because a regular file already occupies the path. That is what a disk-full
// or permission failure looks like to writeReport.
func blockedReportsDir(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reports")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A dry run cannot post anything, so a failure to write its report must never
// be reported as a review that was killed with comments possibly half-posted.
func TestDryRunReportWriteFailureIsDistinguishableFromAReviewFailure(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "code-review", Result: runner.Result{Stdout: []byte("finding: off-by-one")}},
	}}
	rr := New(f, "claude", nil, true, blockedReportsDir(t))

	res, err := rr.Run(context.Background(), "work", ref, nil, nil)
	if err == nil {
		t.Fatal("a report that could not be written must still be surfaced")
	}
	var re *ReportError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v (%T), want a *ReportError so the caller can tell it from a killed review", err, err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d; claude exited cleanly, so nothing may imply it was killed", res.ExitCode)
	}
	if res.ReportPath != "" {
		t.Errorf("ReportPath = %q, want empty: no report was written", res.ReportPath)
	}
}

// A genuinely failed or timed-out review must stay an ordinary error, so the
// pipeline still records needs_attention. That is load-bearing.
func TestReviewFailureIsNotAReportError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply runner.Reply
		dry   bool
	}{
		{"non-zero exit, dry run", runner.Reply{
			Match: "code-review", Result: runner.Result{ExitCode: 1, Stderr: []byte("rate limited")}}, true},
		{"non-zero exit, live", runner.Reply{
			Match: "code-review", Result: runner.Result{ExitCode: 1, Stderr: []byte("rate limited")}}, false},
		{"killed, dry run", runner.Reply{Match: "code-review", Err: context.DeadlineExceeded}, true},
		{"killed, live", runner.Reply{Match: "code-review", Err: context.DeadlineExceeded}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &runner.Fake{Replies: []runner.Reply{tc.reply}}
			rr := New(f, "claude", nil, tc.dry, t.TempDir())

			_, err := rr.Run(context.Background(), "work", ref, nil, nil)
			if err == nil {
				t.Fatal("a failed review must be an error so the pipeline records needs_attention")
			}
			var re *ReportError
			if errors.As(err, &re) {
				t.Errorf("err = %v; a failed review must not be mistaken for a report-write failure", err)
			}
		})
	}
}

// A dry run that failed is exactly when the operator most wants to read what
// happened, and the captured stdout used to be discarded on the non-zero-exit
// path.
func TestDryRunWithNonZeroExitStillWritesAReport(t *testing.T) {
	dir := t.TempDir()
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "code-review", Result: runner.Result{
			ExitCode: 2,
			Stdout:   []byte("finding: unchecked error\nthen claude gave up"),
			Stderr:   []byte("rate limited"),
		}},
	}}
	rr := New(f, "claude", nil, true, dir)

	res, err := rr.Run(context.Background(), "work", ref, nil, nil)
	if err == nil {
		t.Fatal("a non-zero exit must still be an error")
	}
	if res.ReportPath == "" {
		t.Fatal("a failed dry run must still leave a report to read")
	}
	body, rerr := os.ReadFile(res.ReportPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, want := range []string{"finding: unchecked error", "then claude gave up", "exit 2", ref.Key()} {
		if !strings.Contains(string(body), want) {
			t.Errorf("report missing %q:\n%s", want, body)
		}
	}
}

// A failed dry run with no stdout at all still gets a report: the header names
// the failure, which beats an empty reports directory.
func TestDryRunWithNoOutputStillWritesAReportNamingTheFailure(t *testing.T) {
	dir := t.TempDir()
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "code-review", Err: context.DeadlineExceeded},
	}}
	rr := New(f, "claude", nil, true, dir)

	res, err := rr.Run(context.Background(), "work", ref, nil, nil)
	if err == nil {
		t.Fatal("a killed review must be an error")
	}
	if res.ReportPath == "" {
		t.Fatal("a killed dry run must still leave a report to read")
	}
	body, rerr := os.ReadFile(res.ReportPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(body), "deadline exceeded") {
		t.Errorf("the report must name the failure:\n%s", body)
	}
}

// TestALiveFailureKeepsWhatItPrinted replaces TestLiveFailureWritesNoReport,
// which asserted the opposite on the grounds that "a live run's findings live
// on the pull request".
//
// That reasoning holds for a review that finished. It is exactly wrong for one
// that did not: a killed or failed live review was posting comments one at a
// time when it stopped, firstpass cannot tell how far it got, and the operator
// is told as much -- "comments may be partially posted". Whatever the reviewer
// had printed is the only evidence of how far it actually got, and throwing it
// away leaves the operator to work that out by reading the pull request.
func TestALiveFailureKeepsWhatItPrinted(t *testing.T) {
	dir := t.TempDir()
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "code-review", Result: runner.Result{
			ExitCode: 1,
			Stdout:   []byte("half a review"),
			Stderr:   []byte("usage limit reached"),
		}},
	}}
	rr := New(f, "claude", nil, false, dir)

	res, err := rr.Run(context.Background(), "work", ref, nil, nil)
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	if res.ReportPath == "" {
		t.Fatal("a live review that failed must keep what it printed: it is the only " +
			"evidence of how many comments it managed to post")
	}
	body, rerr := os.ReadFile(res.ReportPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	got := string(body)
	if !strings.Contains(got, "half a review") {
		t.Errorf("the report must carry what the reviewer printed:\n%s", got)
	}
	// stderr is where an exit that explains itself does so -- a usage limit, a
	// compaction, a refused tool. For a review that stopped early that is the
	// whole explanation, so dropping it defeats the point of keeping the rest.
	if !strings.Contains(got, "usage limit reached") {
		t.Errorf("the report must carry stderr, where a truncated run explains itself:\n%s", got)
	}
}

// TestALiveReportDoesNotClaimNothingWasPosted is the safety half of writing
// live reports at all.
//
// The report header and the verdict note were written when the only reports
// were dry-run reports, and both said so unconditionally: "dry run, nothing
// posted", and "Nothing was submitted here". Reused for a live review those
// sentences are false about the one thing an operator opens the file to
// establish, and false in the reassuring direction -- the comments are on a
// colleague's pull request.
func TestALiveReportDoesNotClaimNothingWasPosted(t *testing.T) {
	dir := t.TempDir()
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "code-review", Result: runner.Result{Stdout: []byte("a review with no verdict line")}},
	}}
	rr := New(f, "claude", nil, false, dir)

	res, err := rr.Run(context.Background(), "work", ref, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ReportPath == "" {
		t.Fatal("a live review with no verdict must keep its output")
	}
	body, rerr := os.ReadFile(res.ReportPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	got := string(body)
	for _, lie := range []string{"nothing posted", "Nothing was submitted here", "dry run"} {
		if strings.Contains(got, lie) {
			t.Errorf("a live report must not contain %q — the comments are on the pull "+
				"request:\n%s", lie, got)
		}
	}
	if !strings.Contains(got, "posted on the pull request") {
		t.Errorf("a live report must say plainly that the comments are posted:\n%s", got)
	}
}
