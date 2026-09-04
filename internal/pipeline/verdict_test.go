package pipeline

// The verdict: after a successful review, firstpass submits a GitHub review so
// a reviewed pull request is never silent. The reviewer decides it, firstpass
// submits it, and the store records what was actually submitted.
//
// These tests wire the real ghpr client over a runner.Fake rather than the
// harness's fake inspector, so the assertion reaches all the way down to the
// argv handed to gh. --approve and --comment are one word apart and mean
// opposite things to the team's review queue, so the argv is asserted in
// order, not by substring presence.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/review"
	"github.com/angelov-todor/firstpass/internal/runner"
	"github.com/angelov-todor/firstpass/internal/store"
)

const verdictKey = "example-org/aex-balances#12"

// ghFake replies to the two gh commands a reviewed PR involves. "pr view" is
// not a substring of "gh pr review 12", so the two replies never collide.
func ghFake(reviewResult runner.Result) *runner.Fake {
	const prJSON = `{"state":"OPEN","isDraft":false,"author":{"login":"colleague"},"headRefOid":"sha1"}`
	return &runner.Fake{Replies: []runner.Reply{
		{Match: "pr view", Result: runner.Result{Stdout: []byte(prJSON)}},
		{Match: "pr review", Result: reviewResult},
	}}
}

// verdictHarness is one PR posted to the space, reviewed with the given
// verdict, with gh behind a runner.Fake.
func verdictHarness(t *testing.T, v review.Verdict, live bool, ghReview runner.Result) (*harness, *runner.Fake) {
	t.Helper()
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	f := ghFake(ghReview)
	h.p.PRs = ghpr.New(f, "gh")
	h.rev.result = review.Result{Verdict: v}
	h.cfg.DryRun = !live
	h.apply()
	return h, f
}

// ghReviewCalls returns every `gh pr review` invocation recorded.
func ghReviewCalls(f *runner.Fake) []runner.Call {
	var out []runner.Call
	for _, c := range f.Calls {
		if len(c.Args) >= 2 && c.Args[0] == "pr" && c.Args[1] == "review" {
			out = append(out, c)
		}
	}
	return out
}

func reviewRecord(t *testing.T, h *harness) store.Review {
	t.Helper()
	rec, ok, err := h.st.Review(verdictKey)
	if err != nil || !ok {
		t.Fatalf("no review record (ok=%v err=%v)", ok, err)
	}
	return rec
}

// Nothing needing change: an approve, which clears reviewDecision.
func TestLiveApproveVerdictSubmitsAnApprovingReview(t *testing.T) {
	h, f := verdictHarness(t, review.VerdictApprove, true, runner.Result{})

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 1 {
		t.Fatalf("Reviewed = %d, want 1 (decisions: %+v)", rep.Reviewed, rep.Decisions)
	}

	calls := ghReviewCalls(f)
	if len(calls) != 1 {
		t.Fatalf("want exactly one gh pr review call, got %+v", calls)
	}
	if calls[0].Name != "gh" {
		t.Errorf("Name = %q, want gh", calls[0].Name)
	}
	want := []string{"pr", "review", "12", "--repo", "example-org/aex-balances",
		"--approve", "--body", verdictBodyApprove}
	if !slices.Equal(calls[0].Args, want) {
		t.Errorf("Args = %q,\nwant %q", calls[0].Args, want)
	}

	rec := reviewRecord(t, h)
	if rec.Outcome != store.OutcomeReviewed {
		t.Errorf("Outcome = %q, want reviewed", rec.Outcome)
	}
	if rec.Verdict != store.VerdictApproved {
		t.Errorf("Verdict = %q, want %q", rec.Verdict, store.VerdictApproved)
	}
}

// Anything Critical or Important: a COMMENT review, never request-changes, so
// reviewDecision stays REVIEW_REQUIRED and the PR stays in the human queue.
func TestLiveFindingsVerdictSubmitsACommentReview(t *testing.T) {
	h, f := verdictHarness(t, review.VerdictFindings, true, runner.Result{})

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}

	calls := ghReviewCalls(f)
	if len(calls) != 1 {
		t.Fatalf("want exactly one gh pr review call, got %+v", calls)
	}
	want := []string{"pr", "review", "12", "--repo", "example-org/aex-balances",
		"--comment", "--body", verdictBodyFindings}
	if !slices.Equal(calls[0].Args, want) {
		t.Errorf("Args = %q,\nwant %q", calls[0].Args, want)
	}
	for _, arg := range calls[0].Args {
		if arg == "--request-changes" || arg == "--approve" {
			t.Errorf("a findings verdict must be a comment review, got %q", calls[0].Args)
		}
	}

	rec := reviewRecord(t, h)
	if rec.Outcome != store.OutcomeReviewed {
		t.Errorf("Outcome = %q, want reviewed", rec.Outcome)
	}
	if rec.Verdict != store.VerdictFindings {
		t.Errorf("Verdict = %q, want %q", rec.Verdict, store.VerdictFindings)
	}
}

// The operator's colleagues see these under the operator's own GitHub account,
// so the body has to say what wrote it in its first sentence.
func TestVerdictBodiesSayTheyAreMachineWritten(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		first string
	}{
		{"approve", verdictBodyApprove,
			"Automated first pass by firstpass — no findings. This is machine-written, not a human review."},
		{"findings", verdictBodyFindings,
			"Automated first pass by firstpass — findings posted inline. This is machine-written and is not a substitute for human review."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.HasPrefix(tc.body, tc.first) {
				t.Errorf("the body must open with %q, got %q", tc.first, tc.body)
			}
			if !strings.Contains(tc.body, "https://github.com/angelov-todor/firstpass") {
				t.Errorf("the body must name what produced it: %q", tc.body)
			}
		})
	}
}

// A dry run posts no comments and must submit no verdict either. The record
// says what it would have been, so the operator can watch it decide before it
// decides for real.
func TestDryRunSubmitsNoVerdict(t *testing.T) {
	for _, v := range []review.Verdict{review.VerdictApprove, review.VerdictFindings} {
		t.Run(string(v), func(t *testing.T) {
			h, f := verdictHarness(t, v, false, runner.Result{})

			rep, err := h.p.Sweep(context.Background(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Reviewed != 1 {
				t.Fatalf("Reviewed = %d, want 1", rep.Reviewed)
			}
			if calls := ghReviewCalls(f); len(calls) != 0 {
				t.Fatalf("a dry run must submit nothing, got %+v", calls)
			}

			rec := reviewRecord(t, h)
			if rec.Outcome != store.OutcomeReviewed {
				t.Errorf("Outcome = %q, want reviewed", rec.Outcome)
			}
			if rec.Verdict != store.VerdictNone {
				t.Errorf("Verdict = %q; a dry run submitted nothing, so nothing may be recorded", rec.Verdict)
			}
			if !strings.Contains(rec.Detail, "would have been "+string(v)) {
				t.Errorf("the record must say what the verdict would have been, got %q", rec.Detail)
			}
		})
	}
}

// A review that failed, was killed or timed out has posted nothing complete
// and may have posted part of a comment set. It must submit no verdict, and it
// must still be needs_attention.
func TestFailedReviewSubmitsNoVerdictAndStillNeedsAttention(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"killed", context.DeadlineExceeded},
		{"failed", errors.New("claude exit 1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, f := verdictHarness(t, review.VerdictApprove, true, runner.Result{})
			h.rev.err = tc.err

			if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
				t.Fatal(err)
			}
			if calls := ghReviewCalls(f); len(calls) != 0 {
				t.Fatalf("a failed review must submit no verdict, got %+v", calls)
			}
			rec := reviewRecord(t, h)
			if rec.Outcome != store.OutcomeNeedsAttention {
				t.Errorf("Outcome = %q, want needs_attention", rec.Outcome)
			}
			if rec.Verdict != store.VerdictNone {
				t.Errorf("Verdict = %q, want nothing recorded", rec.Verdict)
			}
		})
	}
}

// The case that started all this, inverted: the review ran and its comments
// are posted, but no verdict line came back. Nothing is submitted and nothing
// is guessed -- least of all an approval.
func TestMissingOrUnrecognisedVerdictSubmitsNothing(t *testing.T) {
	for _, v := range []review.Verdict{review.VerdictUnknown, review.Verdict(""), review.Verdict("lgtm")} {
		t.Run("verdict="+string(v), func(t *testing.T) {
			h, f := verdictHarness(t, v, true, runner.Result{})

			rep, err := h.p.Sweep(context.Background(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if calls := ghReviewCalls(f); len(calls) != 0 {
				t.Fatalf("an unknown verdict must submit nothing, got %+v", calls)
			}
			// The review itself happened and its comments are posted, so it
			// is reviewed -- not needs_attention, which means "do not retry,
			// comments may be partial".
			if rep.Reviewed != 1 {
				t.Errorf("Reviewed = %d, want 1: the review did happen", rep.Reviewed)
			}
			rec := reviewRecord(t, h)
			if rec.Outcome != store.OutcomeReviewed {
				t.Errorf("Outcome = %q, want reviewed", rec.Outcome)
			}
			if rec.Verdict != store.VerdictUnknown {
				t.Errorf("Verdict = %q, want %q", rec.Verdict, store.VerdictUnknown)
			}
			if !strings.Contains(rec.Detail, review.VerdictMarker) {
				t.Errorf("the detail must name the missing verdict line, got %q", rec.Detail)
			}
		})
	}
}

// needs_attention means "comments may be partially posted, do not retry". A
// failed verdict submission is not that: the review completed and its comments
// are all posted. It stays reviewed, with the error where status shows it.
func TestFailedVerdictSubmissionStaysReviewed(t *testing.T) {
	h, f := verdictHarness(t, review.VerdictApprove, true, runner.Result{
		ExitCode: 1, Stderr: []byte("GraphQL: Could not resolve to a node"),
	})

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ghReviewCalls(f)) != 1 {
		t.Fatalf("the submission must have been attempted once, got %+v", ghReviewCalls(f))
	}
	if rep.Reviewed != 1 {
		t.Errorf("Reviewed = %d, want 1: the review itself succeeded", rep.Reviewed)
	}
	d, ok := decisionFor(rep, verdictKey)
	if !ok {
		t.Fatal("no decision for the PR")
	}
	if d.Action != ActionReview {
		t.Errorf("Action = %q, want %q: a failed verdict is not a failed review", d.Action, ActionReview)
	}

	rec := reviewRecord(t, h)
	if rec.Outcome != store.OutcomeReviewed {
		t.Errorf("Outcome = %q, want reviewed: the review completed, only the verdict failed", rec.Outcome)
	}
	if rec.Verdict != store.VerdictNone {
		t.Errorf("Verdict = %q; nothing was submitted, so nothing may be recorded", rec.Verdict)
	}
	if !strings.Contains(rec.Detail, "Could not resolve to a node") {
		t.Errorf("the submission error must be in Detail so status surfaces it, got %q", rec.Detail)
	}
}

// print-only queries GitHub but must never write anything, verdict included.
func TestPrintOnlySubmitsNoVerdict(t *testing.T) {
	h, f := verdictHarness(t, review.VerdictApprove, true, runner.Result{})

	if _, err := h.p.Sweep(context.Background(), Options{PrintOnly: true}); err != nil {
		t.Fatal(err)
	}
	if calls := ghReviewCalls(f); len(calls) != 0 {
		t.Fatalf("print-only must submit nothing, got %+v", calls)
	}
}
