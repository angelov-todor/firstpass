package pipeline

// `firstpass replay` bypasses the record gate, which is where every other
// candidate learns that a pass has already reviewed it. So a replay used to
// send the reviewer in blind.
//
// That is worse for a replay than for a re-post, because of why people replay:
// the documented use is a needs_attention pull request, where a review died
// part-way through posting and some of its comments may already be on the pull
// request. Blind is precisely how the duplicate comment set that
// needs_attention exists to warn about actually happens.
//
// Everything else about replay is unchanged: it still ignores the record for
// the purpose of deciding to review, still honours the owner allowlist, still
// refuses while paused, and still never erases the record before a decision.

import (
	"context"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/review"
	"github.com/angelov-todor/firstpass/internal/store"
)

// replayRecord is a record for the replay ref with every field populated.
func replayRecord(outcome store.Outcome, headSHA string) store.Review {
	return store.Review{
		Key:            replayRef.Key(),
		Outcome:        outcome,
		HeadSHA:        headSHA,
		TriggerMessage: firstTrigger,
		StartedAt:      time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC),
		DecidedAt:      time.Date(2026, 9, 2, 10, 12, 0, 0, time.UTC),
		DurationMS:     735000,
		Pass:           1,
	}
}

// onlyPreviousPass is the single previous-pass argument the reviewer was
// handed, and fails the test if it did not run exactly once.
func onlyPreviousPass(t *testing.T, h *harness) *review.PreviousPass {
	t.Helper()
	if len(h.rev.prevPasses) != 1 {
		t.Fatalf("the reviewer ran %d times, want 1: %v", len(h.rev.prevPasses), h.rev.ran)
	}
	return h.rev.prevPasses[0]
}

func TestReplayOfAReviewedPullRequestTellsTheReviewerAboutThatPass(t *testing.T) {
	h := newHarness(t, nil)
	seeded := replayRecord(store.OutcomeReviewed, oldSHA)
	seeded.Verdict = store.VerdictApproved
	if err := h.st.PutReview(seeded); err != nil {
		t.Fatal(err)
	}
	h.prs.info[replayRef.Key()] = ghprOpen(newSHA)

	d, err := h.p.ReviewOne(context.Background(), replayRef, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionReview {
		t.Fatalf("Action = %q (%s); replay must still review", d.Action, d.Reason)
	}

	pp := onlyPreviousPass(t, h)
	if pp == nil {
		t.Fatal("the reviewer was told nothing; it will restate every finding the author has " +
			"not fixed, on the same lines")
	}
	if pp.HeadSHA != oldSHA {
		t.Errorf("HeadSHA = %q, want the commit the earlier pass reviewed (%q)", pp.HeadSHA, oldSHA)
	}
	if pp.Incomplete {
		t.Error("Incomplete = true, want false: that pass finished and posted its findings")
	}

	rec, _, _ := h.st.Review(replayRef.Key())
	if rec.Pass != 2 {
		t.Errorf("Pass = %d, want 2: this is the second pass over the pull request", rec.Pass)
	}
	if rec.PreviousHeadSHA != oldSHA {
		t.Errorf("PreviousHeadSHA = %q, want %q", rec.PreviousHeadSHA, oldSHA)
	}
	if rec.HeadSHA != newSHA {
		t.Errorf("HeadSHA = %q, want the commit this pass reviewed", rec.HeadSHA)
	}
}

// The case replay exists for. The record says a review started at a commit and
// never finished, so a pass has been here -- and the reviewer must be told,
// with the uncertainty intact rather than a claim that the findings are all
// posted.
func TestReplayOfANeedsAttentionPullRequestTellsTheReviewerThePassDidNotFinish(t *testing.T) {
	h := newHarness(t, nil)
	seeded := replayRecord(store.OutcomeNeedsAttention, oldSHA)
	seeded.Detail = "review did not finish; comments may be partially posted"
	seeded.ExitCode = ExitUnknown
	if err := h.st.PutReview(seeded); err != nil {
		t.Fatal(err)
	}
	h.prs.info[replayRef.Key()] = ghprOpen(newSHA)

	if _, err := h.p.ReviewOne(context.Background(), replayRef, Options{}); err != nil {
		t.Fatal(err)
	}

	pp := onlyPreviousPass(t, h)
	if pp == nil {
		t.Fatal("a needs_attention replay is the case where comments may already be half " +
			"posted; the reviewer must not be sent in blind")
	}
	if pp.HeadSHA != oldSHA {
		t.Errorf("HeadSHA = %q, want %q", pp.HeadSHA, oldSHA)
	}
	if !pp.Incomplete {
		t.Error("Incomplete = false; that pass died mid-post, so \"it posted its findings\" is " +
			"not true and telling the reviewer so invites both a duplicate comment and a silence")
	}
	if rec, _, _ := h.st.Review(replayRef.Key()); rec.Pass != 2 || rec.PreviousHeadSHA != oldSHA {
		t.Errorf("record = %+v, want pass 2 retaining %q", rec, oldSHA)
	}
}

// An in_flight record is the same claim as needs_attention -- a run that
// started and never recorded an outcome -- and reaches ReviewOne unconverted,
// because recoverInFlight only runs inside a sweep.
func TestReplayOfAnInFlightRecordTellsTheReviewerThePassDidNotFinish(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.st.PutReview(replayRecord(store.OutcomeInFlight, oldSHA)); err != nil {
		t.Fatal(err)
	}
	h.prs.info[replayRef.Key()] = ghprOpen(newSHA)

	if _, err := h.p.ReviewOne(context.Background(), replayRef, Options{}); err != nil {
		t.Fatal(err)
	}
	pp := onlyPreviousPass(t, h)
	if pp == nil || !pp.Incomplete || pp.HeadSHA != oldSHA {
		t.Errorf("previous pass = %+v, want an incomplete pass at %q", pp, oldSHA)
	}
}

// No record at all: a first pass, and the reviewer is told nothing. Sending
// the note here would name a commit no pass ever reviewed.
func TestReplayOfAPullRequestWithNoRecordIsAFirstPass(t *testing.T) {
	h := newHarness(t, nil)
	h.prs.info[replayRef.Key()] = ghprOpen(newSHA)

	if _, err := h.p.ReviewOne(context.Background(), replayRef, Options{}); err != nil {
		t.Fatal(err)
	}
	if pp := onlyPreviousPass(t, h); pp != nil {
		t.Errorf("previous pass = %+v, want nil: nothing has reviewed this pull request", pp)
	}
	if rec, _, _ := h.st.Review(replayRef.Key()); rec.Pass != 1 || rec.PreviousHeadSHA != "" {
		t.Errorf("record = %+v, want pass 1 with no previous commit", rec)
	}
}

// A skipped record is a record, but it is not a pass: nothing was reviewed and
// no commit was recorded. "A pass has been here" is read off the recorded
// commit, not off the mere existence of a row.
func TestReplayOfASkippedRecordIsAFirstPass(t *testing.T) {
	for _, outcome := range []store.Outcome{
		store.OutcomeSkippedState,
		store.OutcomeSkippedAuthor,
		store.OutcomeExpired,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			h := newHarness(t, nil)
			// As terminal() writes them: no head SHA, because claude never ran.
			if err := h.st.PutReview(store.Review{
				Key: replayRef.Key(), Outcome: outcome, TriggerMessage: firstTrigger,
				DecidedAt: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC), Detail: "skipped",
			}); err != nil {
				t.Fatal(err)
			}
			h.prs.info[replayRef.Key()] = ghprOpen(newSHA)

			if _, err := h.p.ReviewOne(context.Background(), replayRef, Options{}); err != nil {
				t.Fatal(err)
			}
			if pp := onlyPreviousPass(t, h); pp != nil {
				t.Errorf("previous pass = %+v, want nil: a skip reviewed nothing and posted "+
					"nothing, so there is no earlier pass to warn the reviewer about", pp)
			}
			if rec, _, _ := h.st.Review(replayRef.Key()); rec.Pass != 1 {
				t.Errorf("Pass = %d, want 1", rec.Pass)
			}
		})
	}
}

// A replay's terminal skip still replaces the record, unlike a re-post's.
// TestReviewOneTerminalSkipReplacesThePriorRecord already pins this, but its
// fixture record carries no head SHA, so it would keep passing by accident
// once a replay started carrying a previous pass. This one carries one.
//
// The asymmetry is deliberate. A replay is an explicit question -- "what does
// firstpass make of this pull request now" -- so the fresh answer is what the
// operator asked for, and a stale "reviewed" detail left standing over it is
// the misreading that test was written for. A re-post asks for a review, and
// firstpass declining to give one is not worth overwriting history with.
func TestReplayOfAMergedPullRequestStillRecordsAFreshDecision(t *testing.T) {
	h := newHarness(t, nil)
	seeded := replayRecord(store.OutcomeReviewed, oldSHA)
	seeded.Verdict = store.VerdictApproved
	seeded.Detail = "the original review"
	if err := h.st.PutReview(seeded); err != nil {
		t.Fatal(err)
	}
	h.prs.info[replayRef.Key()] = ghpr.PRInfo{State: "MERGED", Author: "colleague", HeadSHA: newSHA}

	d, err := h.p.ReviewOne(context.Background(), replayRef, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionSkip {
		t.Fatalf("Action = %q (%s), want skip", d.Action, d.Reason)
	}
	rec, ok, _ := h.st.Review(replayRef.Key())
	if !ok || rec.Outcome != store.OutcomeSkippedState {
		t.Fatalf("Outcome = %q, want skipped_state: a replay's own fresh decision must stand",
			rec.Outcome)
	}
	if rec.Detail == "the original review" {
		t.Error("the stale detail must not survive a deliberate replay")
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("ran = %v; a merged pull request is not reviewed", h.rev.ran)
	}
}
