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
	"fmt"
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

// emptyFeedbackJSON is a pull request nobody has commented on: no threads, no
// reviews, no comments, and review still required. It is the state in which an
// approval is permitted, so it is the right default for tests about verdicts.
//
// A reply for it is mandatory rather than optional. Without one the GraphQL
// call fails, and a failed feedback fetch withholds the approval by design --
// which is how these tests failed when the gates first landed. That is the
// safety property doing its job, and the fixture is what lets a test about
// approval bodies be about approval bodies.
const emptyFeedbackJSON = `{"data":{"repository":{"pullRequest":{` +
	`"reviewDecision":"REVIEW_REQUIRED",` +
	`"reviewThreads":{"totalCount":0,"nodes":[]},` +
	`"reviews":{"totalCount":0,"nodes":[]},` +
	`"comments":{"totalCount":0,"nodes":[]}}}}}`

// ghFake replies to the gh commands a reviewed PR involves. "pr view" is not a
// substring of "gh pr review 12", so the replies never collide.
func ghFake(reviewResult runner.Result) *runner.Fake {
	const prJSON = `{"state":"OPEN","isDraft":false,"author":{"login":"colleague"},"headRefOid":"sha1"}`
	return &runner.Fake{Replies: []runner.Reply{
		{Match: "pr view", Result: runner.Result{Stdout: []byte(prJSON)}},
		{Match: "pr review", Result: reviewResult},
		{Match: "graphql", Result: runner.Result{Stdout: []byte(emptyFeedbackJSON)}},
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
		"--approve", "--body", verdictBodyApprove(1)}
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
		"--comment", "--body", verdictBodyFindings(1)}
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
		{"approve", verdictBodyApprove(1),
			"Automated first pass by firstpass — no findings. This is machine-written, not a human review."},
		{"findings", verdictBodyFindings(1),
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

// /code-review posts each inline comment as its own review event, so the
// findings do not hang off the verdict review. Saying they did would send a
// colleague looking for comments that are not there.
func TestTheFindingsBodyDoesNotClaimTheCommentsAreOnThisReview(t *testing.T) {
	if strings.Contains(verdictBodyFindings(1), "on this review") {
		t.Errorf("the findings are not comments on this review: %q", verdictBodyFindings(1))
	}
	if !strings.Contains(verdictBodyFindings(1), "inline comments on this pull request") {
		t.Errorf("the body must say where the findings actually are: %q", verdictBodyFindings(1))
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

// The dry-run corner the first version of this feature got wrong: a dry run
// with no verdict line recorded a detail that said "its comments are posted".
// A dry run withholds --comment and can post nothing, and `status` shows that
// string to the operator -- sending them looking for damage that cannot
// exist, which is a rule this codebase established twice before (see
// review.ReportError and the killed-review detail).
func TestDryRunWithNoVerdictDoesNotClaimAnythingWasPosted(t *testing.T) {
	h, f := verdictHarness(t, review.VerdictUnknown, false, runner.Result{})

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if calls := ghReviewCalls(f); len(calls) != 0 {
		t.Fatalf("a dry run must submit nothing, got %+v", calls)
	}

	rec := reviewRecord(t, h)
	if rec.Outcome != store.OutcomeReviewed {
		t.Errorf("Outcome = %q, want reviewed", rec.Outcome)
	}
	// Unknown, not unset: the reviewer produced no verdict at all, which is
	// the thing the operator is watching for during the dry-run phase.
	if rec.Verdict != store.VerdictUnknown {
		t.Errorf("Verdict = %q, want %q", rec.Verdict, store.VerdictUnknown)
	}
	if strings.Contains(rec.Detail, "comments are posted") {
		t.Errorf("a dry run posts nothing, so the detail must not say comments are posted: %q", rec.Detail)
	}
	if !strings.Contains(rec.Detail, "nothing was posted") {
		t.Errorf("the detail must say plainly that nothing was posted: %q", rec.Detail)
	}
	if !strings.Contains(rec.Detail, review.VerdictMarker) {
		t.Errorf("the detail must still name the missing verdict line: %q", rec.Detail)
	}
}

// The live half of the same wording, so the dry-run fix cannot be made by
// dropping the true statement instead of qualifying it.
func TestLiveWithNoVerdictSaysTheCommentsArePosted(t *testing.T) {
	h, _ := verdictHarness(t, review.VerdictUnknown, true, runner.Result{})

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rec := reviewRecord(t, h)
	if !strings.Contains(rec.Detail, "its comments are posted") {
		t.Errorf("a live review did post its comments, and the detail must say so: %q", rec.Detail)
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

// replayVerdictHarness is the same wiring as verdictHarness, but with no chat
// message: ReviewOne is handed the ref directly, the way cmdReplay does.
func replayVerdictHarness(t *testing.T, v review.Verdict, live bool) (*harness, *runner.Fake) {
	t.Helper()
	h := newHarness(t, nil)
	f := ghFake(runner.Result{})
	h.p.PRs = ghpr.New(f, "gh")
	h.rev.result = review.Result{Verdict: v}
	h.cfg.DryRun = !live
	h.apply()
	return h, f
}

// `replay -live` is the one path where submitting a verdict on a named
// colleague's pull request is an intentional act, so it gets its own test
// rather than relying on ReviewOne sharing handle with the sweep.
func TestLiveReplaySubmitsExactlyOneVerdict(t *testing.T) {
	h, f := replayVerdictHarness(t, review.VerdictApprove, true)

	d, err := h.p.ReviewOne(context.Background(), replayRef, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionReview {
		t.Fatalf("Action = %q (%s), want review", d.Action, d.Reason)
	}

	calls := ghReviewCalls(f)
	if len(calls) != 1 {
		t.Fatalf("want exactly one gh pr review call, got %+v", calls)
	}
	want := []string{"pr", "review", "12", "--repo", "example-org/aex-balances",
		"--approve", "--body", verdictBodyApprove(1)}
	if !slices.Equal(calls[0].Args, want) {
		t.Errorf("Args = %q,\nwant %q", calls[0].Args, want)
	}

	rec, ok, err := h.st.Review(replayRef.Key())
	if err != nil || !ok {
		t.Fatalf("no review record (ok=%v err=%v)", ok, err)
	}
	if rec.Outcome != store.OutcomeReviewed {
		t.Errorf("Outcome = %q, want reviewed", rec.Outcome)
	}
	if rec.Verdict != store.VerdictApproved {
		t.Errorf("Verdict = %q, want %q", rec.Verdict, store.VerdictApproved)
	}
}

// A replay respects dry_run unless -live is passed, which is what makes
// replaying to inspect the output safe. That has to cover the verdict too.
func TestDryRunReplaySubmitsNoVerdict(t *testing.T) {
	for _, v := range []review.Verdict{review.VerdictApprove, review.VerdictFindings} {
		t.Run(string(v), func(t *testing.T) {
			h, f := replayVerdictHarness(t, v, false)

			if _, err := h.p.ReviewOne(context.Background(), replayRef, Options{}); err != nil {
				t.Fatal(err)
			}
			if calls := ghReviewCalls(f); len(calls) != 0 {
				t.Fatalf("a dry-run replay must submit nothing, got %+v", calls)
			}
			rec, ok, _ := h.st.Review(replayRef.Key())
			if !ok {
				t.Fatal("no review record")
			}
			if rec.Verdict != store.VerdictNone {
				t.Errorf("Verdict = %q; nothing was submitted, so nothing may be recorded", rec.Verdict)
			}
			if !strings.Contains(rec.Detail, "would have been "+string(v)) {
				t.Errorf("the record must say what the verdict would have been, got %q", rec.Detail)
			}
		})
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

// TestALaterPassDoesNotCallItselfTheFirst is the fix for text that was about
// to go onto colleagues' pull requests.
//
// The bodies were constants reading "Automated first pass by firstpass".
// Re-reviews have been live for days -- production records show passes 2, 3
// and 5 -- so the fifth review of a pull request would have introduced itself
// as the first. The only reason it never did is that no verdict was ever
// submitted, which was a separate bug; fixing that one armed this one.
func TestALaterPassDoesNotCallItselfTheFirst(t *testing.T) {
	for _, pass := range []int{2, 3, 5} {
		for _, body := range []string{verdictBodyApprove(pass), verdictBodyFindings(pass)} {
			if strings.Contains(body, "first pass") {
				t.Errorf("pass %d body still calls itself the first pass:\n%s", pass, body)
			}
			if !strings.Contains(body, fmt.Sprintf("pass %d", pass)) {
				t.Errorf("pass %d body must say which pass it is:\n%s", pass, body)
			}
		}
	}
	// Pass 1 keeps the wording that is true of it.
	if !strings.Contains(verdictBodyApprove(1), "first pass") {
		t.Errorf("the first pass may say so: %q", verdictBodyApprove(1))
	}
}

// TestALaterPassApprovalSaysWhatItCovers is the honesty this feature needs
// most, and it is not about the word "first".
//
// A later pass is asked to concentrate on the commits added since the previous
// one and explicitly not to restate its findings. So an approval on pass 2
// means "the new commits raised nothing", not "this pull request is fine" --
// and those readings come apart exactly when it matters, when an earlier pass
// raised findings the author has not addressed. An approval submitted under
// the operator's own identity that reads as a blessing of the whole change is
// the most expensive thing this tool can get wrong.
func TestALaterPassApprovalSaysWhatItCovers(t *testing.T) {
	body := verdictBodyApprove(2)
	for _, want := range []string{
		"commits added since the previous automated pass",
		"may be unaddressed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("a later pass's approval must scope itself (%q missing):\n%s", want, body)
		}
	}
	// A first pass has nothing earlier to qualify, and the caveat would be
	// noise that trains readers to skip the body.
	if strings.Contains(verdictBodyApprove(1), "may be unaddressed") {
		t.Errorf("the first pass has no earlier findings to warn about: %q", verdictBodyApprove(1))
	}
}
