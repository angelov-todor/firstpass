package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/store"
)

func clearStore(t *testing.T, recs ...store.Review) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "firstpass.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, r := range recs {
		if err := st.PutReview(r); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

const clearKey = "AstraBit-CPT/aex-user-service#103"

// TestClearMarksTheRecordHandled is the gap this command fills. A review that
// did not finish is needs_attention -- terminal and correct, because a human
// has to look. Once the human has looked there was no way to say so: the row
// asked for attention forever, and the only thing that moved it was `firstpass
// replay`, which re-reviews a pull request that may be merged and closed.
func TestClearMarksTheRecordHandled(t *testing.T) {
	st := clearStore(t, store.Review{
		Key:     clearKey,
		Outcome: store.OutcomeNeedsAttention,
		Detail:  "review did not finish: claude for x exit 1: network unreachable",
	})

	was, unparked, err := clearRecord(st, clearKey, "handled by hand", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if was != store.OutcomeNeedsAttention {
		t.Errorf("was = %q, want needs_attention", was)
	}
	if unparked {
		t.Error("nothing was parked, so nothing should be reported removed")
	}

	rec, found, err := st.Review(clearKey)
	if err != nil || !found {
		t.Fatalf("the record must still exist: found=%v err=%v", found, err)
	}
	if rec.Outcome != store.OutcomeCleared {
		t.Errorf("Outcome = %q, want cleared", rec.Outcome)
	}
	// Deliberately not "reviewed": firstpass did not review this pull request,
	// and recording that it had would be a false record.
	if rec.Outcome == store.OutcomeReviewed {
		t.Error("clearing must not claim a review happened")
	}
	// The original detail is the only account of what went wrong and exists
	// nowhere else once the log rotates.
	if !strings.Contains(rec.Detail, "network unreachable") {
		t.Errorf("the original detail must survive: %q", rec.Detail)
	}
	if !strings.Contains(rec.Detail, "handled by hand") {
		t.Errorf("the operator's note must be recorded: %q", rec.Detail)
	}
	if !strings.Contains(rec.Detail, "needs_attention") {
		t.Errorf("the detail must say what it was: %q", rec.Detail)
	}
}

// A cleared pull request must leave the backlog with it. Left parked, the next
// sweep would re-offer and review the very thing just declared handled.
func TestClearRemovesTheBacklogEntry(t *testing.T) {
	st := clearStore(t, store.Review{
		Key: clearKey, Outcome: store.OutcomeInFlight, Detail: "orphaned run",
	})
	if err := st.PutPending(store.Pending{Key: clearKey}); err != nil {
		t.Fatal(err)
	}

	if _, unparked, err := clearRecord(st, clearKey, "", time.Now()); err != nil {
		t.Fatal(err)
	} else if !unparked {
		t.Error("the removal must be reported, or the operator cannot tell it happened")
	}
	if _, found, err := st.Pending(clearKey); err != nil {
		t.Fatal(err)
	} else if found {
		t.Error("a cleared pull request must not still be in the backlog")
	}
}

// A typo must not look like success: it would leave the record it was aimed at
// exactly where it was, having printed nothing wrong.
func TestClearRefusesAKeyItDoesNotKnow(t *testing.T) {
	st := clearStore(t)
	if _, _, err := clearRecord(st, clearKey, "", time.Now()); err == nil {
		t.Fatal("clearing an unknown key must be an error")
	} else if !strings.Contains(err.Error(), "no record") {
		t.Errorf("the error must say why: %v", err)
	}
}

// Only the two outcomes that ask for attention are clearable. Clearing a
// settled one would only lose information -- why a pull request was skipped,
// or that a review completed and what verdict it submitted -- and clearing a
// reviewed row is the more dangerous half: it would overwrite the record of a
// verdict firstpass actually submitted on somebody's pull request.
func TestClearRefusesASettledRecord(t *testing.T) {
	for _, outcome := range []store.Outcome{
		store.OutcomeReviewed,
		store.OutcomeSkippedAuthor,
		store.OutcomeExpired,
		store.OutcomeCleared,
	} {
		st := clearStore(t, store.Review{
			Key: clearKey, Outcome: outcome, Verdict: store.VerdictApproved, Detail: "keep me",
		})
		if _, _, err := clearRecord(st, clearKey, "", time.Now()); err == nil {
			t.Errorf("%q is settled and must not be clearable", outcome)
		}
		rec, _, _ := st.Review(clearKey)
		if rec.Outcome != outcome || rec.Detail != "keep me" {
			t.Errorf("%q: the record must be untouched, got %+v", outcome, rec)
		}
	}
}

// A cleared outcome is terminal, like every outcome but in_flight. That is
// what stops clearing from quietly reopening a pull request to review: were it
// non-terminal, a link posted again in chat -- a colleague bumping an old
// thread -- would review from scratch something already dealt with.
func TestClearedIsTerminal(t *testing.T) {
	if !store.OutcomeCleared.Terminal() {
		t.Error("cleared must be terminal, or clearing reopens the pull request")
	}
}
