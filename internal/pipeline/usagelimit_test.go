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

// limitHarness reviews the given pull requests with claude failing the way it
// fails when the account has no capacity left.
//
// Driven through the real review.Runner over a runner.Fake, not the pipeline's
// reviewer fake, because the classification being tested lives in
// review.Run: the pipeline must recognise a usage limit from what the runner
// hands back, and a fake reviewer that returns a plain error would prove
// nothing about that seam.
func limitHarness(t *testing.T, text, claudeStdout, claudeStderr string, live bool) (
	h *harness, gh, claude *runner.Fake) {
	t.Helper()
	h = newHarness(t, []chat.Message{msg("spaces/A/messages/m1", text)})
	h.seedWatermark(t)

	const prJSON = `{"state":"OPEN","isDraft":false,"author":{"login":"colleague"},"headRefOid":"sha1"}`
	gh = &runner.Fake{Replies: []runner.Reply{
		{Match: "pr view", Result: runner.Result{Stdout: []byte(prJSON)}},
		{Match: "pr review", Result: runner.Result{}},
		{Match: "pr diff", Result: runner.Result{}},
		{Match: "graphql", Result: runner.Result{Stdout: []byte(emptyFeedbackJSON)}},
	}}
	h.p.PRs = ghpr.New(gh, "gh")

	claude = &runner.Fake{Replies: []runner.Reply{
		{Match: "Review pull request", Result: runner.Result{
			ExitCode: 1, Stdout: []byte(claudeStdout), Stderr: []byte(claudeStderr),
		}},
	}}
	h.p.Rev = review.New(claude, "claude", nil, !live, t.TempDir())
	h.cfg.DryRun = !live
	h.cfg.MaxReviewsPerSweep = 5
	h.cfg.ReviewConcurrency = 1 // serial, so "the rest of the sweep" is well defined
	h.apply()
	return h, gh, claude
}

// callsMatching counts the invocations a fake actually received whose command
// line contains sub.
//
// Safe to read only after the sweep returns, and only because these tests set
// ReviewConcurrency to 1: runner.Fake appends to Calls without a lock.
func callsMatching(f *runner.Fake, sub string) int {
	n := 0
	for _, c := range f.Calls {
		if strings.Contains(c.String(), sub) {
			n++
		}
	}
	return n
}

// TestAUsageLimitDefersInsteadOfStrandingThePR is the fix.
//
// Every other claude failure is needs_attention: terminal, never retried
// automatically, and warning that comments may be half posted. For an account
// out of capacity all three are wrong -- nothing was posted, it will succeed
// unchanged once the limit resets, and it is not this pull request's fault.
//
// The operator's own defence was to notice and run `firstpass pause`, which
// worked. It should not have been necessary.
func TestAUsageLimitDefersInsteadOfStrandingThePR(t *testing.T) {
	h, _, _ := limitHarness(t, prURL("aex-balances", 12), "",
		"You've hit your limit · resets at 3pm", true)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}

	d, ok := decisionFor(rep, verdictKey)
	if !ok || d.Action != ActionDefer {
		t.Fatalf("a usage limit must defer, got %+v", d)
	}
	if !strings.Contains(d.Reason, "usage limit") {
		t.Errorf("the reason must name the cause: %q", d.Reason)
	}

	// The record is what made the old behaviour permanent.
	if rec, found, rerr := h.st.Review(verdictKey); rerr != nil {
		t.Fatal(rerr)
	} else if found && rec.Outcome == store.OutcomeNeedsAttention {
		t.Errorf("a usage limit must not be recorded needs_attention: %+v", rec)
	}

	// Parked, and without spending an attempt: the limit is account-wide and
	// time-based, so counting attempts would retire every parked pull request
	// inside about ninety minutes of being rate-limited.
	all, perr := h.st.AllPending()
	if perr != nil {
		t.Fatal(perr)
	}
	if len(all) != 1 || all[0].Key != verdictKey {
		t.Fatalf("the pull request must be parked, got %+v", all)
	}
	if all[0].Attempts != 0 {
		t.Errorf("Attempts = %d; running out of tokens is not this pull request's failure",
			all[0].Attempts)
	}
}

// TestAUsageLimitStopsTheRestOfTheSweep bounds the waste.
//
// The limit is account-wide, so every remaining candidate fails identically --
// and each one would spend an Inspect, a clone and a claude start to discover
// it. Latched, exactly as a mid-sweep pause is.
func TestAUsageLimitStopsTheRestOfTheSweep(t *testing.T) {
	h, gh, claude := limitHarness(t, prURL("a", 1)+" "+prURL("b", 2)+" "+prURL("c", 3), "",
		"You've hit your monthly limit", true)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Decisions) != 3 {
		t.Fatalf("want a decision for each pull request, got %d", len(rep.Decisions))
	}
	for _, d := range rep.Decisions {
		if d.Action != ActionDefer {
			t.Errorf("%s: Action = %q, want defer", d.Ref.Key(), d.Action)
		}
	}
	// Only the first one actually reached claude. This is the assertion the
	// first version of this test got wrong: it counted Inspect calls on
	// h.prs, the pipeline's fake PR client, which limitHarness replaces with
	// a real ghpr.Client over the gh fake -- so the counter was always zero
	// and the assertion held whether the latch existed or not. Count what the
	// fakes were actually asked instead.
	if n := callsMatching(claude, "Review pull request"); n != 1 {
		t.Errorf("claude ran %d times; once the limit is known the remaining candidates "+
			"should be parked without starting a review", n)
	}
	if n := callsMatching(gh, "pr view"); n != 1 {
		t.Errorf("gh pr view ran %d times; a parked candidate should cost no GitHub call", n)
	}
	all, _ := h.st.AllPending()
	if len(all) != 3 {
		t.Errorf("all three must be parked for a later sweep, got %d", len(all))
	}
}

// TestAUsageLimitAfterPostingIsNotDeferred is the safety half, and the reason
// this is not simply "defer on a usage limit".
//
// A limit reached after the reviewer had begun posting cannot be retried:
// re-reviewing would put a second copy of those comments on a colleague's pull
// request, which is the damage needs_attention exists to warn about. So the
// deferral is conditional on having established that nothing landed.
func TestAUsageLimitAfterPostingIsNotDeferred(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)

	const prJSON = `{"state":"OPEN","isDraft":false,"author":{"login":"colleague"},"headRefOid":"sha1"}`
	gh := &runner.Fake{Replies: []runner.Reply{
		{Match: "pr view", Result: runner.Result{Stdout: []byte(prJSON)}},
		{Match: "pr review", Result: runner.Result{}},
		// Nothing of the operator's before the review; one comment after, so
		// the reviewer demonstrably posted before it ran out.
		{Match: "graphql", Result: runner.Result{Stdout: []byte(emptyFeedbackJSON)}, Times: 1},
		{Match: "graphql", Result: runner.Result{Stdout: []byte(ownCommentFeedbackJSON)}},
	}}
	h.p.PRs = ghpr.New(gh, "gh")

	claude := &runner.Fake{Replies: []runner.Reply{
		{Match: "Review pull request", Result: runner.Result{
			ExitCode: 1,
			Stdout:   []byte("posted the first finding\nYou've hit your limit"),
		}},
	}}
	h.p.Rev = review.New(claude, "claude", nil, false, t.TempDir())
	h.cfg.DryRun = false
	h.apply()

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, verdictKey)
	if d.Action != ActionNeedsAttention {
		t.Errorf("a limit reached after posting began must not be deferred: %+v", d)
	}
	rec := reviewRecord(t, h)
	if rec.Outcome != store.OutcomeNeedsAttention {
		t.Errorf("Outcome = %q, want needs_attention", rec.Outcome)
	}
}

// A dry run posts nothing by construction, so a usage limit there is always
// safe to retry -- no GitHub call needed to establish it.
func TestAUsageLimitInADryRunIsAlwaysDeferred(t *testing.T) {
	h, _, _ := limitHarness(t, prURL("aex-balances", 12), "You've hit your fast limit", "", false)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := decisionFor(rep, verdictKey); d.Action != ActionDefer {
		t.Errorf("a dry run posts nothing, so a usage limit must defer: %+v", d)
	}
}

// And every other failure keeps the behaviour it had. A phrase this list
// misses costs a needs_attention record -- today's outcome -- and never
// something worse, which is what makes an incomplete list of phrases an
// acceptable design rather than a gamble.
func TestAnOrdinaryClaudeFailureIsStillNeedsAttention(t *testing.T) {
	h, _, _ := limitHarness(t, prURL("aex-balances", 12), "",
		"panic: something else went wrong entirely", true)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := decisionFor(rep, verdictKey); d.Action != ActionNeedsAttention {
		t.Errorf("an unrecognised failure must stay needs_attention: %+v", d)
	}
}
