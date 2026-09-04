package pipeline

// A second pass that defers has to be able to come back.
//
// A fell-through re-post that defers -- a transient Inspect failure, a
// worktree failure, a draft, a pause, or simply the per-sweep cap, which is 3
// by default -- parks a Pending row alongside the existing `reviewed` record.
// candidates() then re-offered it with no trigger at all, so condition 2
// failed and the record gate skipped it. The second pass was lost for good,
// and because that skip returns above expirePending the pending row could
// never be retired either: it sat in `firstpass status` forever.
//
// The fix is to give the pending row its provenance. That also closes the
// finding parked since the first release -- "TriggerMessage is lost for
// pending-derived candidates" in
// docs/superpowers/reviews/2026-09-03-final-review-outcomes.md.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/store"
)

// A re-post deferred by a transient Inspect failure, then retried on the next
// sweep with the message long gone from the window. Three sweeps, because the
// bug only shows from the second onwards.
func TestADeferredSecondPassRetriesFromPending(t *testing.T) {
	repostAt := decidedAt.Add(time.Hour)
	h := newHarness(t, []chat.Message{
		postAt(thirdPost, "another look "+prURL("aex-balances", 12), repostAt),
	})
	h.seedWatermark(t)
	seedFirstPass(t, h, reviewedAfterTwoPosts(), newSHA)
	h.prs.err[secondPassKey] = errors.New("gh: network unreachable")

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := decisionFor(rep, secondPassKey); d.Action != ActionDefer {
		t.Fatalf("Action = %q (%s), want defer", d.Action, d.Reason)
	}
	pd, ok, _ := h.st.Pending(secondPassKey)
	if !ok {
		t.Fatal("the deferred second pass was not parked at all")
	}
	if pd.TriggerMessage != thirdPost || !pd.TriggerTime.Equal(repostAt) {
		t.Errorf("pending = %+v; a parked ref must keep the post that asked for it, or the "+
			"retry cannot tell itself from a sweep re-reading its own past", pd)
	}

	// The message has scrolled out of the window, so the only thing offering
	// this ref now is the pending bucket. gh is healthy again.
	h.ch.msgs = nil
	delete(h.prs.err, secondPassKey)

	rep2, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep2, secondPassKey)
	if d.Action != ActionReview {
		t.Fatalf("Action = %q (%s), want review: the re-post is still outstanding and gh is "+
			"working again", d.Action, d.Reason)
	}
	if len(h.rev.ran) != 1 {
		t.Fatalf("ran = %v, want the one retried review", h.rev.ran)
	}
	rec, _, _ := h.st.Review(secondPassKey)
	if rec.Pass != 2 || rec.TriggerMessage != thirdPost {
		t.Errorf("record = %+v, want pass 2 attributed to the post that asked for it", rec)
	}
	// And the row settles, rather than sitting in status forever.
	if _, ok, _ := h.st.Pending(secondPassKey); ok {
		t.Error("the pending row must be cleared once the review it was holding has run")
	}

	// A third sweep must do nothing at all: no message, no pending row.
	rep3, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep3.Decisions) != 0 {
		t.Errorf("Decisions = %+v, want none: nothing is offering this ref any more", rep3.Decisions)
	}
	if len(h.rev.ran) != 1 {
		t.Errorf("ran = %v; the retry must not repeat once it has succeeded", h.rev.ran)
	}
}

// The per-sweep cap is the likeliest way to reach this, because it needs
// nothing to go wrong: three re-posted pull requests and the fourth is parked.
func TestASecondPassParkedByThePerSweepCapRetriesOnTheNextSweep(t *testing.T) {
	repostAt := decidedAt.Add(time.Hour)
	h := newHarness(t, []chat.Message{
		postAt(thirdPost, "all three please "+prURL("aex-balances", 12)+" "+prURL("aex-margin", 3),
			repostAt),
	})
	h.seedWatermark(t)
	h.cfg.MaxReviewsPerSweep = 1
	h.apply()
	seedFirstPass(t, h, reviewedAfterTwoPosts(), newSHA)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	// The cap parks whichever of the two came second; the point is only that
	// the parked one comes back.
	if rep.Reviewed != 1 {
		t.Fatalf("Reviewed = %d, want 1 under a cap of 1: %+v", rep.Reviewed, rep.Decisions)
	}
	h.ch.msgs = nil

	rep2, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Reviewed != 1 {
		t.Fatalf("Reviewed = %d on the next sweep, want the parked one to run: %+v",
			rep2.Reviewed, rep2.Decisions)
	}
	if len(h.rev.ran) != 2 {
		t.Errorf("ran = %v, want both pull requests reviewed across the two sweeps", h.rev.ran)
	}
}

// Provenance is only restored where it was recorded. A pending row written
// before the fields existed, or parked by something that names no chat message,
// still yields no trigger -- and so still cannot be a second pass.
func TestAPendingRowWithNoRecordedProvenanceIsStillNeverASecondPass(t *testing.T) {
	h := newHarness(t, nil)
	h.seedWatermark(t)
	seedFirstPass(t, h, reviewedAfterTwoPosts(), newSHA)
	// As upsertPending wrote them before this change.
	if err := h.st.PutPending(store.Pending{
		Key: secondPassKey, FirstSeen: time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC), Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, ok := decisionFor(rep, secondPassKey)
	if !ok {
		t.Fatal("the pending ref was never offered as a candidate")
	}
	if d.Action != ActionSkip {
		t.Fatalf("Action = %q (%s), want skip: nothing says a post asked for this", d.Action, d.Reason)
	}
	if len(h.rev.ran) != 0 {
		t.Fatalf("ran = %v", h.rev.ran)
	}
	if len(h.prs.inspected) != 0 {
		t.Errorf("Inspect called for %v; this is decided at the record gate", h.prs.inspected)
	}
}

// The round trip: a ref parked, re-offered from pending, and parked again
// keeps its provenance and counts both defers against the expiry budget.
func TestARepostReparkedFromPendingKeepsItsProvenance(t *testing.T) {
	repostAt := decidedAt.Add(time.Hour)
	h := newHarness(t, []chat.Message{
		postAt(thirdPost, "another look "+prURL("aex-balances", 12), repostAt),
	})
	h.seedWatermark(t)
	seedFirstPass(t, h, reviewedAfterTwoPosts(), newSHA)
	h.prs.err[secondPassKey] = errors.New("gh: network unreachable")

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	h.ch.msgs = nil
	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}

	pd, ok, _ := h.st.Pending(secondPassKey)
	if !ok {
		t.Fatal("the ref fell out of pending")
	}
	if pd.TriggerMessage != thirdPost || !pd.TriggerTime.Equal(repostAt) {
		t.Errorf("pending = %+v; the provenance must survive the round trip out of the bucket "+
			"and back into it, or the retry stops being possible after one failure", pd)
	}
	if pd.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2: both defers count against the expiry budget", pd.Attempts)
	}
}

// The guard on the write itself: a park that names no chat message must leave
// what an earlier park recorded alone.
//
// Called directly rather than through a sweep, deliberately. candidates()
// round-trips the provenance, so no sweep can currently offer an anonymous
// candidate for a ref whose row has one -- which means an end-to-end test here
// would pass with the guard removed and prove nothing (it did: the round-trip
// test above is insensitive to it). This pins the guard, and the guard is what
// stops one anonymous park from stranding a re-post for good if any future
// path parks a ref without a candidate behind it.
func TestAnAnonymousReparkDoesNotWipeRecordedProvenance(t *testing.T) {
	h := newHarness(t, nil)
	repostAt := decidedAt.Add(time.Hour)
	named := candidate{ref: replayRef, trigger: thirdPost, triggerAt: repostAt}

	if err := h.p.deferAttempt(named, "draft", Options{}); err != nil {
		t.Fatal(err)
	}
	if err := h.p.deferAttempt(candidate{ref: replayRef}, "draft", Options{}); err != nil {
		t.Fatal(err)
	}

	pd, ok, _ := h.st.Pending(replayRef.Key())
	if !ok {
		t.Fatal("no pending row")
	}
	if pd.TriggerMessage != thirdPost || !pd.TriggerTime.Equal(repostAt) {
		t.Errorf("pending = %+v; a park with nothing to say must not erase the post that "+
			"asked for this pull request", pd)
	}
	if pd.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", pd.Attempts)
	}
}

// ---- what the reviewer is told about the pass before it ----

// A pass following a dry-run pass must not be told that comments were posted:
// a dry run withholds --comment, so its findings went to a report on disk.
func TestAPassFollowingADryRunPassIsToldNothingAboutIt(t *testing.T) {
	h := newHarness(t, []chat.Message{
		postAt(thirdPost, "another look "+prURL("aex-balances", 12), decidedAt.Add(time.Hour)),
	})
	h.seedWatermark(t)
	seeded := reviewedAfterTwoPosts()
	seeded.DryRun = true
	seedFirstPass(t, h, seeded, newSHA)

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	pp := onlyPreviousPass(t, h)
	if pp == nil {
		t.Fatal("the previous pass must still be carried: the dry-run report filename needs it")
	}
	if pp.Posted {
		t.Error("Posted = true after a dry-run pass; nothing of it reached the pull request, so " +
			"the reviewer would be told to hold back findings that are not there")
	}
	// And the pass still counts, so the record and the report name are right.
	if rec, _, _ := h.st.Review(secondPassKey); rec.Pass != 2 || rec.PreviousHeadSHA != oldSHA {
		t.Errorf("record = %+v, want pass 2 retaining %q", rec, oldSHA)
	}
}

// A live pass is what the reviewer is warned about.
func TestAPassFollowingALivePassIsToldItsFindingsArePosted(t *testing.T) {
	h := newHarness(t, []chat.Message{
		postAt(thirdPost, "another look "+prURL("aex-balances", 12), decidedAt.Add(time.Hour)),
	})
	h.seedWatermark(t)
	h.cfg.DryRun = false
	h.apply()
	seedFirstPass(t, h, reviewedAfterTwoPosts(), newSHA)

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	pp := onlyPreviousPass(t, h)
	if pp == nil || !pp.Posted {
		t.Fatalf("previous pass = %+v, want one whose findings are on the pull request", pp)
	}
	if pp.HeadUnchanged {
		t.Error("HeadUnchanged = true; a re-post requires a commit no pass has reviewed")
	}
}

// Whether this pass is a dry run is recorded as it starts, so the pass after
// it can be told the truth.
func TestTheDryRunModeOfAPassIsRecorded(t *testing.T) {
	for _, dry := range []bool{true, false} {
		h := newHarness(t, []chat.Message{msg(firstTrigger, prURL("aex-balances", 12))})
		h.seedWatermark(t)
		h.cfg.DryRun = dry
		h.apply()

		if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
			t.Fatal(err)
		}
		rec, ok, _ := h.st.Review(secondPassKey)
		if !ok {
			t.Fatal("no record")
		}
		if rec.DryRun != dry {
			t.Errorf("dry_run = %v, DryRun = %v; the pass after this one reads this field to "+
				"decide whether any comment is on the pull request", dry, rec.DryRun)
		}
	}
}

// A replay at the very commit the earlier pass reviewed: the reviewer must not
// be pointed at changes that do not exist.
func TestAReplayAtAnAlreadyReviewedCommitSaysTheHeadHasNotMoved(t *testing.T) {
	for _, tc := range []struct {
		name string
		live string
	}{
		{"the commit the last pass reviewed", newSHA},
		{"a commit an earlier pass reviewed", oldSHA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.cfg.DryRun = false
			h.apply()
			rec := twoPassRecord()
			rec.Key = replayRef.Key()
			if err := h.st.PutReview(rec); err != nil {
				t.Fatal(err)
			}
			h.prs.info[replayRef.Key()] = ghprOpen(tc.live)

			if _, err := h.p.ReviewOne(context.Background(), replayRef, Options{}); err != nil {
				t.Fatal(err)
			}
			pp := onlyPreviousPass(t, h)
			if pp == nil {
				t.Fatal("the reviewer was told nothing")
			}
			if !pp.HeadUnchanged {
				t.Errorf("HeadUnchanged = false for live head %q, which a pass has already "+
					"reviewed; the reviewer would be asked to concentrate on changes that do "+
					"not exist and to withhold the findings it is being asked for", tc.live)
			}
		})
	}
}

// A replay after new commits is the ordinary case and keeps the ordinary
// framing, so the unchanged-head flag cannot be implemented as "always true
// for a replay".
func TestAReplayAfterNewCommitsSaysTheHeadHasMoved(t *testing.T) {
	h := newHarness(t, nil)
	h.cfg.DryRun = false
	h.apply()
	rec := twoPassRecord()
	rec.Key = replayRef.Key()
	if err := h.st.PutReview(rec); err != nil {
		t.Fatal(err)
	}
	h.prs.info[replayRef.Key()] = ghprOpen(thirdSHA)

	if _, err := h.p.ReviewOne(context.Background(), replayRef, Options{}); err != nil {
		t.Fatal(err)
	}
	pp := onlyPreviousPass(t, h)
	if pp == nil || pp.HeadUnchanged {
		t.Errorf("previous pass = %+v, want HeadUnchanged false: the head has moved to a commit "+
			"no pass has reviewed", pp)
	}
}

// ---- the other way a pending row could get stuck ----

// A second pass deferred by a transient failure whose re-post then turns out
// to have nothing new lands in the no-new-commits branch, which is above
// expirePending -- and on every later sweep the record gate skips the ref
// before the expiry can run. Without a delete there, the row is immortal: it
// sits in `firstpass status` for ever, describing work that will never happen.
//
// Driven across three sweeps and then a year, because a single sweep cannot
// tell a row that will be retired from one that never can be.
func TestARepostWithNothingNewClearsTheDeferredPendingRow(t *testing.T) {
	repostAt := decidedAt.Add(time.Hour)
	h := newHarness(t, []chat.Message{
		postAt(thirdPost, "another look "+prURL("aex-balances", 12), repostAt),
	})
	h.seedWatermark(t)
	seedFirstPass(t, h, reviewedAfterTwoPosts(), newSHA)
	h.prs.err[secondPassKey] = errors.New("gh: network unreachable")

	// Sweep 1: the re-post defers, so a pending row is parked beside the
	// existing reviewed record.
	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.st.Pending(secondPassKey); !ok {
		t.Fatal("the deferred second pass was not parked at all")
	}

	// Sweep 2: gh is healthy, the message has scrolled away, and it turns out
	// the head never moved -- the author pushed nothing after all.
	h.ch.msgs = nil
	delete(h.prs.err, secondPassKey)
	h.prs.info[secondPassKey] = ghprOpen(oldSHA)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, secondPassKey)
	if d.Action != ActionSkip {
		t.Fatalf("Action = %q (%s), want skip: the recorded commit is still the head",
			d.Action, d.Reason)
	}
	if len(h.rev.ran) != 0 {
		t.Fatalf("ran = %v", h.rev.ran)
	}
	if _, ok, _ := h.st.Pending(secondPassKey); ok {
		t.Error("the pending row must go: this branch is above expirePending, and the record " +
			"gate skips the ref on every later sweep, so nothing else will ever retire it")
	}

	// Sweep 3, and then a year later: nothing is offering the ref and nothing
	// is left behind.
	rep3, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep3.Decisions) != 0 {
		t.Errorf("Decisions = %+v, want none", rep3.Decisions)
	}
	h.p.Now = func() time.Time { return decidedAt.AddDate(1, 0, 0) }
	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.st.Pending(secondPassKey); ok {
		t.Error("a pending row still there a year later is one nothing can retire")
	}
}
