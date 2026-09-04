package pipeline

// A re-post falls through the record gate, which every gate below it was
// written without an existing review record in mind. Three of those gates can
// write a terminal record, and each of them would overwrite the only evidence
// of what firstpass actually did to this pull request: the commit it reviewed,
// the verdict it submitted, and which pass that was.
//
// Skipping is still right at all three. Losing the history is not.

import (
	"context"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/store"
)

// assertRecordIntact compares the whole record, field by field, against what
// was seeded. Asserting only the outcome would pass with the verdict, the
// reviewed SHA and the pass count all wiped -- which is exactly the loss these
// tests exist to catch.
func assertRecordIntact(t *testing.T, h *harness, want store.Review) {
	t.Helper()
	got, ok, err := h.st.Review(secondPassKey)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the record of the pass that reviewed this pull request is gone entirely")
	}
	if got == want {
		return
	}
	t.Errorf("record was rewritten:\n got %+v\nwant %+v", got, want)
	// Named individually, because each of these is a separate thing to lose.
	if got.Outcome != want.Outcome {
		t.Errorf("Outcome = %q, want %q: firstpass reviewed this pull request", got.Outcome, want.Outcome)
	}
	if got.Verdict != want.Verdict {
		t.Errorf("Verdict = %q, want %q: the submitted verdict is not recoverable from anywhere else",
			got.Verdict, want.Verdict)
	}
	if got.HeadSHA != want.HeadSHA {
		t.Errorf("HeadSHA = %q, want %q: the commit that carries the comments", got.HeadSHA, want.HeadSHA)
	}
	if got.Pass != want.Pass {
		t.Errorf("Pass = %d, want %d", got.Pass, want.Pass)
	}
	if got.PreviousHeadSHA != want.PreviousHeadSHA {
		t.Errorf("PreviousHeadSHA = %q, want %q", got.PreviousHeadSHA, want.PreviousHeadSHA)
	}
}

// The state gate. A pull request reviewed, pushed to, merged and then
// re-posted is the likeliest way to reach a gate below the fall-through: the
// re-post is genuine and the new commits are real, so all three conditions
// hold, and only `gh pr view` reveals the pull request is already merged.
//
// Before the fall-through existed this candidate skipped at the record gate
// and kept everything. It must still keep everything.
func TestARepostThatIsNowMergedKeepsThePreviousPassesRecord(t *testing.T) {
	h := newHarness(t, []chat.Message{msg(rePost, "another look "+prURL("aex-balances", 12))})
	h.seedWatermark(t)
	seeded := firstPassRecord(store.OutcomeReviewed, oldSHA, firstTrigger)
	seedFirstPass(t, h, seeded, newSHA)
	h.prs.info[secondPassKey] = ghpr.PRInfo{State: "MERGED", Author: "colleague", HeadSHA: newSHA}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, secondPassKey)
	if d.Action != ActionSkip {
		t.Fatalf("Action = %q (%s), want skip: a merged pull request is not reviewed", d.Action, d.Reason)
	}
	if len(h.rev.ran) != 0 {
		t.Fatalf("ran = %v", h.rev.ran)
	}
	assertRecordIntact(t, h, seeded)
}

// The author gate, the other terminal write below the fall-through. Reaching
// it needs `github_login` to have changed, or the record to have been written
// when the pull request was attributed to someone else -- unlikely, and the
// gate is one line away from the state gate either way, so the record must
// survive it for the same reason.
func TestARepostAttributedToYouKeepsThePreviousPassesRecord(t *testing.T) {
	h := newHarness(t, []chat.Message{msg(rePost, "another look "+prURL("aex-balances", 12))})
	h.seedWatermark(t)
	seeded := firstPassRecord(store.OutcomeReviewed, oldSHA, firstTrigger)
	seedFirstPass(t, h, seeded, newSHA)
	h.prs.info[secondPassKey] = ghpr.PRInfo{
		State: "OPEN", Author: h.cfg.GithubLogin, HeadSHA: newSHA,
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, secondPassKey)
	if d.Action != ActionSkip {
		t.Fatalf("Action = %q (%s), want skip: firstpass does not review your own pull requests",
			d.Action, d.Reason)
	}
	if len(h.rev.ran) != 0 {
		t.Fatalf("ran = %v", h.rev.ran)
	}
	assertRecordIntact(t, h, seeded)
}

// The expiry gate, which is above Inspect and therefore fires on a re-post
// before condition 3 is ever asked. A reviewed pull request can still have a
// stale pending entry -- a sibling deferred it, or an earlier attempt parked
// it -- and retiring that entry must not retire the review with it.
func TestARepostWithAnExpiredPendingEntryKeepsThePreviousPassesRecord(t *testing.T) {
	h := newHarness(t, []chat.Message{msg(rePost, "another look "+prURL("aex-balances", 12))})
	h.seedWatermark(t)
	seeded := firstPassRecord(store.OutcomeReviewed, oldSHA, firstTrigger)
	seedFirstPass(t, h, seeded, newSHA)
	// Older than pending_max_age (168h) against the harness's frozen clock.
	if err := h.st.PutPending(store.Pending{
		Key: secondPassKey, FirstSeen: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC), Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, secondPassKey)
	if d.Action != ActionSkip || d.Reason != "pending expired" {
		t.Fatalf("Action = %q (%s), want skip / pending expired", d.Action, d.Reason)
	}
	if len(h.rev.ran) != 0 {
		t.Fatalf("ran = %v", h.rev.ran)
	}
	assertRecordIntact(t, h, seeded)
	// The pending entry itself is housekeeping, not history, and is still
	// cleared -- otherwise it is re-offered on every sweep forever.
	if _, ok, _ := h.st.Pending(secondPassKey); ok {
		t.Error("the expired pending entry must still be cleared")
	}
}

// The draft gate defers rather than recording anything terminal, so it cannot
// clobber the record. Pinned anyway: it is the third gate a re-post reaches,
// and "cannot" is a claim about code that is one refactor from being wrong.
func TestARepostThatIsNowADraftKeepsThePreviousPassesRecord(t *testing.T) {
	h := newHarness(t, []chat.Message{msg(rePost, "another look "+prURL("aex-balances", 12))})
	h.seedWatermark(t)
	seeded := firstPassRecord(store.OutcomeReviewed, oldSHA, firstTrigger)
	seedFirstPass(t, h, seeded, newSHA)
	h.prs.info[secondPassKey] = ghpr.PRInfo{
		State: "OPEN", IsDraft: true, Author: "colleague", HeadSHA: newSHA,
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, secondPassKey)
	if d.Action != ActionDefer {
		t.Fatalf("Action = %q (%s), want defer", d.Action, d.Reason)
	}
	if len(h.rev.ran) != 0 {
		t.Fatalf("ran = %v", h.rev.ran)
	}
	assertRecordIntact(t, h, seeded)
}

// The owner and deny gates sit *above* the record gate, so a re-post can never
// reach them carrying a previous record: the candidate is refused before the
// record is even read.
//
// Two cases, because only the first of them can actually tell that ordering
// apart. A disallowed owner whose record would otherwise skip it as "already
// decided" must still be refused *for the owner*: that reason is only
// reachable if the allowlist is consulted first. The second case then pins
// that the second-pass fall-through cannot smuggle a disallowed owner past
// into a review or even into a `gh pr view`.
func TestARepostFromADisallowedOwnerIsRefusedAboveTheRecordGate(t *testing.T) {
	const key = "torvalds/linux#7"
	const url = "https://github.com/torvalds/linux/pull/7"

	// Same trigger as the record, so the record gate would skip this one as
	// already decided if it were consulted first.
	t.Run("the allowlist is consulted before the record", func(t *testing.T) {
		h := newHarness(t, []chat.Message{msg(firstTrigger, "please review "+url)})
		h.seedWatermark(t)
		if err := h.st.PutReview(store.Review{
			Key: key, Outcome: store.OutcomeReviewed, HeadSHA: oldSHA,
			TriggerMessage: firstTrigger, Verdict: store.VerdictApproved, Pass: 1,
		}); err != nil {
			t.Fatal(err)
		}

		rep, err := h.p.Sweep(context.Background(), Options{})
		if err != nil {
			t.Fatal(err)
		}
		d, _ := decisionFor(rep, key)
		if d.Reason != "owner not allowed" {
			t.Errorf("Reason = %q, want \"owner not allowed\": the allowlist is the first gate, "+
				"and \"already decided\" here would mean the record was read first", d.Reason)
		}
		if rec, _, _ := h.st.Review(key); rec.Outcome != store.OutcomeSkippedOwner {
			t.Errorf("Outcome = %q, want skipped_owner: the allowlist refusal is firstpass's own "+
				"decision and is recorded unconditionally, as it always was", rec.Outcome)
		}
	})

	// A genuine re-post -- different message, new commits -- from an owner
	// that is not allowed. The fall-through must not reach GitHub.
	t.Run("the fall-through cannot smuggle one past", func(t *testing.T) {
		h := newHarness(t, []chat.Message{msg(rePost, "another look "+url)})
		h.seedWatermark(t)
		if err := h.st.PutReview(store.Review{
			Key: key, Outcome: store.OutcomeReviewed, HeadSHA: oldSHA,
			TriggerMessage: firstTrigger, Verdict: store.VerdictApproved, Pass: 1,
		}); err != nil {
			t.Fatal(err)
		}

		rep, err := h.p.Sweep(context.Background(), Options{})
		if err != nil {
			t.Fatal(err)
		}
		d, _ := decisionFor(rep, key)
		if d.Action != ActionSkip || d.Reason != "owner not allowed" {
			t.Fatalf("Action = %q (%s), want skip / owner not allowed", d.Action, d.Reason)
		}
		if len(h.prs.inspected) != 0 {
			t.Errorf("Inspect called for %v; a disallowed owner must never be queried, re-post or not",
				h.prs.inspected)
		}
		if len(h.rev.ran) != 0 {
			t.Fatalf("ran = %v", h.rev.ran)
		}
	})
}
