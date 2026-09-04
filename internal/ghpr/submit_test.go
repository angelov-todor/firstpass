package ghpr

// SubmitReview is the only writing gh command firstpass runs. Its argv is
// asserted in order rather than by substring presence: --approve and --comment
// are one word apart and mean opposite things to the team's review queue, and
// a --body that drifted away from its flag would submit the wrong argument
// under the operator's own GitHub identity.

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/runner"
)

func TestSubmitReviewApproveArgv(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{{Match: "pr review", Result: runner.Result{}}}}

	if err := New(f, "gh").SubmitReview(context.Background(), ref, ReviewApprove, "body text"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("Calls = %+v", f.Calls)
	}
	if f.Calls[0].Name != "gh" {
		t.Errorf("Name = %q, want gh", f.Calls[0].Name)
	}
	want := []string{"pr", "review", "12", "--repo", "Example-Org/aex-balances", "--approve", "--body", "body text"}
	if !slices.Equal(f.Calls[0].Args, want) {
		t.Errorf("Args = %q, want %q", f.Calls[0].Args, want)
	}
}

// A findings verdict is deliberately a COMMENT review, not request-changes:
// request-changes would still leave reviewDecision at CHANGES_REQUESTED and
// speak for a human who has not looked yet.
func TestSubmitReviewCommentArgv(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{{Match: "pr review", Result: runner.Result{}}}}

	if err := New(f, "gh").SubmitReview(context.Background(), ref, ReviewComment, "body text"); err != nil {
		t.Fatal(err)
	}
	want := []string{"pr", "review", "12", "--repo", "Example-Org/aex-balances", "--comment", "--body", "body text"}
	if !slices.Equal(f.Calls[0].Args, want) {
		t.Errorf("Args = %q, want %q", f.Calls[0].Args, want)
	}
}

// An unknown verdict must never reach gh at all. Approving by default is the
// one failure mode this whole feature exists to avoid.
func TestSubmitReviewRejectsAnUnknownVerdictWithoutRunningGh(t *testing.T) {
	f := &runner.Fake{}

	err := New(f, "gh").SubmitReview(context.Background(), ref, "approved-ish", "body")
	if err == nil {
		t.Fatal("an unrecognised verdict must be an error")
	}
	if len(f.Calls) != 0 {
		t.Errorf("gh must not run at all, got %+v", f.Calls)
	}
}

// Per the runner contract, a non-zero exit is data rather than a Go error, so
// it has to be checked explicitly or a refused review would read as submitted.
func TestSubmitReviewReportsNonZeroExit(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "pr review", Result: runner.Result{ExitCode: 1, Stderr: []byte("  GraphQL: not authorized  ")}},
	}}

	err := New(f, "gh").SubmitReview(context.Background(), ref, ReviewApprove, "body")
	if err == nil {
		t.Fatal("a non-zero gh exit must be an error")
	}
	if !strings.Contains(err.Error(), ref.Key()) {
		t.Errorf("the error must name the PR, got %q", err)
	}
	if !strings.Contains(err.Error(), "GraphQL: not authorized") {
		t.Errorf("the error must carry gh's stderr, got %q", err)
	}
	if strings.Contains(err.Error(), "  GraphQL") {
		t.Errorf("stderr must be trimmed, got %q", err)
	}
}

func TestSubmitReviewReportsRunnerError(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "pr review", Err: context.DeadlineExceeded},
	}}

	err := New(f, "gh").SubmitReview(context.Background(), ref, ReviewApprove, "body")
	if err == nil {
		t.Fatal("a killed gh must be an error")
	}
	if !strings.Contains(err.Error(), ref.Key()) {
		t.Errorf("the error must name the PR, got %q", err)
	}
}
