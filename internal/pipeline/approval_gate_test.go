package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/review"
	"github.com/angelov-todor/firstpass/internal/runner"
	"github.com/angelov-todor/firstpass/internal/store"
)

// gateHarness is verdictHarness with the feedback reply under the test's
// control, since that reply is what the approval gates read.
func gateHarness(t *testing.T, v review.Verdict, feedback runner.Result) (*harness, *runner.Fake) {
	t.Helper()
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	const prJSON = `{"state":"OPEN","isDraft":false,"author":{"login":"colleague"},"headRefOid":"sha1"}`
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "pr view", Result: runner.Result{Stdout: []byte(prJSON)}},
		{Match: "pr review", Result: runner.Result{}},
		{Match: "graphql", Result: feedback},
	}}
	h.p.PRs = ghpr.New(f, "gh")
	h.rev.result = review.Result{Verdict: v}
	h.cfg.DryRun = false
	h.apply()
	return h, f
}

func feedbackWith(decision string) runner.Result {
	return runner.Result{Stdout: []byte(strings.Replace(emptyFeedbackJSON,
		`"reviewDecision":"REVIEW_REQUIRED"`, `"reviewDecision":"`+decision+`"`, 1))}
}

// TestNoApprovalOverAnOutstandingRequestForChanges is the gate that matters
// most to a colleague.
//
// firstpass submits reviews under the operator's own GitHub identity. An
// approval landing on a pull request a teammate has asked for changes on does
// not read as "the automation is content" -- it reads as the operator clearing
// somebody else's block. The reviewer's judgement about the code is not being
// second-guessed here; turning that judgement into an approving review on
// GitHub is a different act, and this is firstpass declining to perform it.
func TestNoApprovalOverAnOutstandingRequestForChanges(t *testing.T) {
	h, f := gateHarness(t, review.VerdictApprove, feedbackWith("CHANGES_REQUESTED"))

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}

	calls := ghReviewCalls(f)
	if len(calls) != 1 {
		t.Fatalf("want exactly one gh pr review call, got %d", len(calls))
	}
	args := strings.Join(calls[0].Args, " ")
	if strings.Contains(args, "--approve") {
		t.Errorf("an approval was submitted over an outstanding request for changes: %s", args)
	}
	if !strings.Contains(args, "--comment") {
		t.Errorf("a withheld approval must still say something: %s", args)
	}
	if !strings.Contains(args, "requested changes") {
		t.Errorf("the body must say why the approval was withheld: %s", args)
	}

	rec := reviewRecord(t, h)
	if rec.Verdict != store.VerdictWithheld {
		t.Errorf("Verdict = %q, want withheld", rec.Verdict)
	}
	if !strings.Contains(rec.Detail, "requested changes") {
		t.Errorf("Detail must record the reason: %q", rec.Detail)
	}
}

// TestNoApprovalWhenTheFeedbackCouldNotBeRead is the fail-safe.
//
// An approval now asserts something firstpass cannot check for itself: that
// every point already raised has been addressed. That assertion rests entirely
// on the list of prior feedback shown to the reviewer, so when firstpass could
// not build that list, the assertion has nothing under it. The review still
// runs and its comments are still posted -- a review that cannot approve is
// worth having -- but the approval is withheld.
func TestNoApprovalWhenTheFeedbackCouldNotBeRead(t *testing.T) {
	h, f := gateHarness(t, review.VerdictApprove,
		runner.Result{ExitCode: 1, Stderr: []byte("gh: API rate limit exceeded")})

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}

	// The review must still have happened.
	h.rev.mu.Lock()
	ran := len(h.rev.ran)
	h.rev.mu.Unlock()
	if ran != 1 {
		t.Fatalf("the review must still run when the feedback fetch fails, ran=%d", ran)
	}

	args := strings.Join(ghReviewCalls(f)[0].Args, " ")
	if strings.Contains(args, "--approve") {
		t.Errorf("approved without being able to read the existing feedback: %s", args)
	}
	if rec := reviewRecord(t, h); rec.Verdict != store.VerdictWithheld {
		t.Errorf("Verdict = %q, want withheld", rec.Verdict)
	}
}

// A truncated list is the same problem as no list: the reviewer was shown some
// of the feedback and told there was more, which cannot support "everything
// raised has been addressed".
func TestNoApprovalWhenTheFeedbackListIsIncomplete(t *testing.T) {
	truncated := strings.Replace(emptyFeedbackJSON,
		`"comments":{"totalCount":0`, `"comments":{"totalCount":99`, 1)
	h, f := gateHarness(t, review.VerdictApprove, runner.Result{Stdout: []byte(truncated)})

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(ghReviewCalls(f)[0].Args, " "); strings.Contains(args, "--approve") {
		t.Errorf("approved on an incomplete list of prior feedback: %s", args)
	}
	if rec := reviewRecord(t, h); rec.Verdict != store.VerdictWithheld {
		t.Errorf("Verdict = %q, want withheld", rec.Verdict)
	}
}

// The gates only ever hold back an approval. A findings verdict is already the
// cautious answer, and gating it would turn a fetch failure into silence on a
// pull request that has real findings to report.
func TestTheGatesDoNotTouchAFindingsVerdict(t *testing.T) {
	h, f := gateHarness(t, review.VerdictFindings, feedbackWith("CHANGES_REQUESTED"))

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(ghReviewCalls(f)[0].Args, " ")
	if !strings.Contains(args, "--comment") {
		t.Errorf("a findings verdict must still be submitted: %s", args)
	}
	if strings.Contains(args, "approval withheld") {
		t.Errorf("a findings verdict is not a withheld approval: %s", args)
	}
	if rec := reviewRecord(t, h); rec.Verdict != store.VerdictFindings {
		t.Errorf("Verdict = %q, want findings", rec.Verdict)
	}
}

// TestTheReviewerIsShownTheExistingFeedback is the other half: the gates stop
// a bad approval, but the point of the change is that the reviewer can make a
// good one. That requires it to actually be handed what is already on the pull
// request -- an instruction to go and look is the shape that was ignored for
// fourteen reviews.
func TestTheReviewerIsShownTheExistingFeedback(t *testing.T) {
	withFeedback := `{"data":{"repository":{"pullRequest":{` +
		`"reviewDecision":"REVIEW_REQUIRED",` +
		`"reviewThreads":{"totalCount":1,"nodes":[` +
		`{"isResolved":false,"isOutdated":false,"path":"src/A.cs","line":9,` +
		`"comments":{"nodes":[{"author":{"login":"reviewer-one","__typename":"User"},` +
		`"body":"Null check missing.","url":"https://example.invalid/t1"}]}}]},` +
		`"reviews":{"totalCount":0,"nodes":[]},` +
		`"comments":{"totalCount":0,"nodes":[]}}}}}`
	h, _ := gateHarness(t, review.VerdictApprove, runner.Result{Stdout: []byte(withFeedback)})

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}

	h.rev.mu.Lock()
	defer h.rev.mu.Unlock()
	if len(h.rev.priors) != 1 || h.rev.priors[0] == nil {
		t.Fatalf("the reviewer was handed no prior feedback: %+v", h.rev.priors)
	}
	p := h.rev.priors[0]
	if len(p.Items) != 1 {
		t.Fatalf("Items = %d, want the one unresolved thread", len(p.Items))
	}
	if p.Items[0].Path != "src/A.cs" || p.Items[0].Line != 9 {
		t.Errorf("the thread must arrive with its location: %+v", p.Items[0])
	}
	if p.Incomplete {
		t.Error("the list was complete and must not be flagged otherwise")
	}
}

// A dry run submits nothing at all, so a withheld approval must not turn into
// a posted comment review either. What it does record is the reason, which is
// the whole point of watching a dry run before going live.
func TestADryRunPostsNothingWhenAnApprovalIsWithheld(t *testing.T) {
	h, f := gateHarness(t, review.VerdictApprove, feedbackWith("CHANGES_REQUESTED"))
	h.cfg.DryRun = true
	h.apply()

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if calls := ghReviewCalls(f); len(calls) != 0 {
		t.Errorf("a dry run must submit nothing, got %d gh pr review calls", len(calls))
	}
	rec := reviewRecord(t, h)
	if rec.Verdict != store.VerdictWithheld {
		t.Errorf("Verdict = %q, want withheld even in a dry run", rec.Verdict)
	}
	if !strings.Contains(rec.Detail, "requested changes") {
		t.Errorf("Detail must say why: %q", rec.Detail)
	}
}
