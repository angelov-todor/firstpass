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

// TestAFailedFeedbackFetchDefersInsteadOfReviewing replaces
// TestNoApprovalWhenTheFeedbackCouldNotBeRead, whose premise a live outage
// disproved.
//
// That test asserted the review should run anyway and merely lose its
// approval, on the reasoning that a review which cannot approve is still worth
// having. What actually happened: GitHub was unreachable for about ninety
// seconds, two pull requests were reviewed without their feedback lists, both
// had their approvals withheld, and both were recorded `reviewed` -- which is
// terminal. So a colleague's pull request was left permanently unapproved,
// carrying a comment about firstpass's own limitation, with nothing that would
// ever try again.
//
// Retrying just the gate after the review was the tempting fix and is wrong:
// an approval asserts that everything already raised has been addressed, and a
// reviewer that was never shown what was raised cannot support that claim
// however healthy the network is by the time the verdict is submitted.
//
// Deferring costs nothing, which is what makes this the right answer rather
// than a trade: the fetch happens before the clone and before the review, so
// the pull request is offered again on the next sweep with its whole attempt
// and age budget intact.
func TestAFailedFeedbackFetchDefersInsteadOfReviewing(t *testing.T) {
	h, f := gateHarness(t, review.VerdictApprove,
		runner.Result{ExitCode: 1, Stderr: []byte("gh: dial tcp: connection attempt failed")})

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}

	// No review, so no comments on anybody's pull request and no verdict.
	h.rev.mu.Lock()
	ran := len(h.rev.ran)
	h.rev.mu.Unlock()
	if ran != 0 {
		t.Errorf("the review must not run without the feedback list, ran=%d", ran)
	}
	if calls := ghReviewCalls(f); len(calls) != 0 {
		t.Errorf("nothing may be submitted, got %d gh pr review calls", len(calls))
	}
	if d, ok := decisionFor(rep, verdictKey); !ok || d.Action != ActionDefer {
		t.Fatalf("want a deferral, got %+v", d)
	}

	// And it must be parked, or "offered again next sweep" is a hope rather
	// than a mechanism: the chat message that triggered it scrolls out of the
	// fetch window.
	all, perr := h.st.AllPending()
	if perr != nil {
		t.Fatal(perr)
	}
	if len(all) != 1 || all[0].Key != verdictKey {
		t.Fatalf("the pull request must be parked for a later sweep, got %+v", all)
	}
	// No terminal record, which is what made the old behaviour permanent.
	if rec, ok, rerr := h.st.Review(verdictKey); rerr != nil {
		t.Fatal(rerr)
	} else if ok && rec.Outcome == store.OutcomeReviewed {
		t.Errorf("a deferral must not leave a terminal reviewed record: %+v", rec)
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

// TestATruncatedListAlsoWithholdsThePostingClaim is the second half of the
// same distrust.
//
// The posting check compares how many items the operator had authored before
// the review with how many after. A truncated list undercounts the before, so
// a pull request the operator had already commented on can show an increase
// this review did not cause -- and the body would then state as fact that the
// findings are posted when they may not be. An unverifiable answer has to
// produce the hedge, not the claim.
//
// This was live for an hour: during the outage the baseline was zero because
// the fetch had failed, so any pre-existing comment would have been read as
// proof of posting. It was right on the pull request it happened to run on, by
// luck.
func TestATruncatedListAlsoWithholdsThePostingClaim(t *testing.T) {
	// The fixture has to make the guard load-bearing, and the first version of
	// this test did not: with no feedback at all the baseline is zero whether
	// the guard is there or not, so the hedge appeared either way and deleting
	// `&& gate.feedbackUsable` left the test passing. It asserted a case
	// adjacent to the property, which is a mistake this codebase has made
	// enough times to have a name for.
	//
	// So: the pre-review list is truncated AND hides an item the operator had
	// already authored. Without the guard the baseline reads as zero, the
	// after-count reads as one, and firstpass concludes this review posted
	// something -- stating as fact, on a colleague's pull request, that the
	// findings are there.
	truncatedHidingOwnComment := `{"data":{"repository":{"pullRequest":{` +
		`"reviewDecision":"REVIEW_REQUIRED",` +
		`"reviewThreads":{"totalCount":0,"nodes":[]},` +
		`"reviews":{"totalCount":0,"nodes":[]},` +
		// Ninety-nine comments exist; none are shown. One of the hidden ones is
		// the operator's, from an earlier pass.
		`"comments":{"totalCount":99,"nodes":[]}}}}}`

	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	const prJSON = `{"state":"OPEN","isDraft":false,"author":{"login":"colleague"},"headRefOid":"sha1"}`
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "pr view", Result: runner.Result{Stdout: []byte(prJSON)}},
		{Match: "pr review", Result: runner.Result{}},
		{Match: "graphql", Result: runner.Result{Stdout: []byte(truncatedHidingOwnComment)}, Times: 1},
		// After the review, the operator's older comment is visible. Nothing
		// this review did put it there.
		{Match: "graphql", Result: runner.Result{Stdout: []byte(ownCommentFeedbackJSON)}},
	}}
	h.p.PRs = ghpr.New(f, "gh")
	h.rev.result = review.Result{Verdict: review.VerdictFindings}
	h.cfg.DryRun = false
	h.apply()

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(ghReviewCalls(f)[0].Args, " ")
	if strings.Contains(body, "Findings are posted in a comment on this pull request") {
		t.Errorf("posting must not be claimed from a baseline truncation hid:\n%s", body)
	}
	if !strings.Contains(body, "could not confirm they") {
		t.Errorf("the body must hedge instead:\n%s", body)
	}
}

// The gates only ever hold back an approval, never a findings verdict: that is
// already the cautious answer, and gating it would turn an incomplete feedback
// list into silence on a pull request with real findings to report.
//
// A feedback list that cannot be read at all is a different case and defers
// the whole review; see TestAFailedFeedbackFetchDefersInsteadOfReviewing. This
// is the truncated one, where GitHub answered.
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

// TestABodyDoesNotClaimFindingsWerePostedWhenTheyWereNot is the fix for a
// claim that became false when the slash command went away.
//
// The command posted the findings itself. Now posting is an instruction in the
// prompt, and the skills a general prompt selects do not all post: the .NET
// review skill produces a report and posts nothing at all. So a live review
// can finish, print `findings`, post nothing, and firstpass would submit a
// review telling a colleague "the findings are posted as inline comments on
// this pull request" — sending them to hunt for comments that do not exist,
// and leaving them to conclude the tool is broken or, worse, that the review
// found nothing.
//
// The check is a count, not a search: firstpass never sees a finding, so it
// cannot look for one. It asks how many items the operator had authored on the
// pull request before the review and again afterwards.
func TestABodyDoesNotClaimFindingsWerePostedWhenTheyWereNot(t *testing.T) {
	// Both feedback answers are the same, so nothing arrived under the
	// operator's name while the review ran.
	h, f := gateHarness(t, review.VerdictFindings,
		runner.Result{Stdout: []byte(emptyFeedbackJSON)})

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(ghReviewCalls(f)[0].Args, " ")

	if strings.Contains(body, "Findings are posted in a comment on this pull request") {
		t.Errorf("the body asserts the findings are posted when nothing was:\n%s", body)
	}
	if !strings.Contains(body, "could not confirm they") {
		t.Errorf("the body must say plainly that posting was not confirmed:\n%s", body)
	}
	// And it must tell the reader what to do instead, or the hedge is just
	// doubt with no way out of it.
	if !strings.Contains(body, "ask the operator for the review report") {
		t.Errorf("the body must say how to get the findings instead:\n%s", body)
	}
	// The verdict itself is unaffected: findings is still findings.
	if rec := reviewRecord(t, h); rec.Verdict != store.VerdictFindings {
		t.Errorf("Verdict = %q, want findings", rec.Verdict)
	}
}

// The converse, so the hedge cannot become unconditional: when a comment does
// arrive under the operator's name during the review, the body says so plainly.
func TestABodySaysTheFindingsArePostedWhenTheyAre(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	f := ghFake(runner.Result{})
	h.p.PRs = ghpr.New(f, "gh")
	h.rev.result = review.Result{Verdict: review.VerdictFindings}
	h.cfg.DryRun = false
	h.apply()

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(ghReviewCalls(f)[0].Args, " ")
	if !strings.Contains(body, "Findings are posted in a comment on this pull request") {
		t.Errorf("a confirmed post must be stated plainly:\n%s", body)
	}
	if strings.Contains(body, "could not confirm") {
		t.Errorf("a confirmed post must not be hedged:\n%s", body)
	}
}
