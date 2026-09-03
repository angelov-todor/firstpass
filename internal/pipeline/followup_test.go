package pipeline

// Findings from the self-review of the published branch. Every test here
// guards one of the two invariants the whole design rests on: no pull request
// is ever reviewed twice, and no pull request is ever silently dropped.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/review"
	"github.com/angelov-todor/firstpass/internal/store"
)

// ---- Finding 1: a failed replay must not destroy the dedupe record ----
//
// ReviewOne used to delete the review record and the pending record before
// calling handle, with noEnqueue set. Any defer path inside handle -- Inspect
// fails, Prepare fails, the ref is a draft -- then wrote nothing to replace
// them, so the PR left the store entirely. The next automatic sweep saw a PR
// with no record at all and reviewed it: a second set of machine-written
// comments on a colleague's pull request.
//
// The existing replay tests could not see this because their harness has no
// chat messages, so nothing ever re-offered the ref. These carry the message.

func TestReviewOneInspectFailureKeepsTheRecordAndTheNextSweepDoesNotRereview(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	key := replayRef.Key()

	prior := store.Review{
		Key: key, Outcome: store.OutcomeReviewed, HeadSHA: "sha-original",
		TriggerMessage: "spaces/A/messages/m1", Detail: "the original review",
		DecidedAt: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC),
	}
	if err := h.st.PutReview(prior); err != nil {
		t.Fatal(err)
	}
	h.prs.err[key] = errors.New("network unreachable")

	d, err := h.p.ReviewOne(context.Background(), replayRef, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionDefer {
		t.Fatalf("Action = %q (%s), want defer", d.Action, d.Reason)
	}

	rec, ok, _ := h.st.Review(key)
	if !ok {
		t.Fatal("a replay that never reached a review must leave the dedupe record behind")
	}
	if rec.Outcome != prior.Outcome || rec.Detail != prior.Detail || rec.HeadSHA != prior.HeadSHA {
		t.Errorf("the prior record was altered: %+v", rec)
	}

	// The half that hid this: the triggering message is still in the fetch
	// window, so the next sweep re-offers the ref. Only the surviving record
	// stops it being reviewed a second time.
	delete(h.prs.err, key)
	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("the sweep after a failed replay must not review an already-reviewed PR, ran %v", h.rev.ran)
	}
	if rep.Reviewed != 0 {
		t.Errorf("Reviewed = %d, want 0", rep.Reviewed)
	}
	if sd, found := decisionFor(rep, key); !found || sd.Action != ActionSkip {
		t.Errorf("decision = %+v (found=%v), want skip: already decided", sd, found)
	}
}

func TestReviewOneWorktreeFailureKeepsTheRecordAndTheNextSweepDoesNotRereview(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	key := replayRef.Key()

	detail := "a previous run died mid-review, so comments may already be posted"
	if err := h.st.PutReview(store.Review{
		Key: key, Outcome: store.OutcomeNeedsAttention, Detail: detail, ExitCode: ExitUnknown,
	}); err != nil {
		t.Fatal(err)
	}
	h.wts.err = errors.New("clone failed")

	if _, err := h.p.ReviewOne(context.Background(), replayRef, Options{}); err != nil {
		t.Fatal(err)
	}

	rec, ok, _ := h.st.Review(key)
	if !ok {
		t.Fatal("a replay whose worktree failed must leave the needs_attention record behind")
	}
	if rec.Outcome != store.OutcomeNeedsAttention || rec.Detail != detail {
		t.Errorf("the record was altered: outcome=%q detail=%q", rec.Outcome, rec.Detail)
	}

	h.wts.err = nil
	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("a needs_attention PR must never be reviewed unasked: comments may already be on it, ran %v",
			h.rev.ran)
	}
}

// A fresh terminal decision made by the replay itself must stand: it is
// accurate, and overwriting it with the restored stale record would undo it.
func TestReviewOneTerminalSkipReplacesThePriorRecord(t *testing.T) {
	h := newHarness(t, nil)
	key := replayRef.Key()
	if err := h.st.PutReview(store.Review{
		Key: key, Outcome: store.OutcomeReviewed, Detail: "the original review",
	}); err != nil {
		t.Fatal(err)
	}
	h.prs.info[key] = ghpr.PRInfo{State: "MERGED", Author: "colleague"}

	d, err := h.p.ReviewOne(context.Background(), replayRef, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionSkip {
		t.Fatalf("Action = %q (%s), want skip", d.Action, d.Reason)
	}
	rec, ok, _ := h.st.Review(key)
	if !ok || rec.Outcome != store.OutcomeSkippedState {
		t.Fatalf("Outcome = %q, want skipped_state: the replay's own fresh decision must stand", rec.Outcome)
	}
	if rec.Detail == "the original review" {
		t.Error("the stale detail must not survive a fresh terminal decision")
	}
}

// The pending record is the other half of the same hazard: a ref parked in the
// backlog and then replayed unsuccessfully used to lose its pending entry too,
// so the automatic path forgot about it as soon as the triggering message
// scrolled out of the window.
func TestReviewOneFailureLeavesAPreexistingPendingEntryAlone(t *testing.T) {
	h := newHarness(t, nil)
	key := replayRef.Key()
	firstSeen := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := h.st.PutPending(store.Pending{
		Key: key, FirstSeen: firstSeen, Attempts: 5, LastReason: "draft",
	}); err != nil {
		t.Fatal(err)
	}
	h.prs.err[key] = errors.New("network unreachable")

	if _, err := h.p.ReviewOne(context.Background(), replayRef, Options{}); err != nil {
		t.Fatal(err)
	}
	pd, ok, _ := h.st.Pending(key)
	if !ok {
		t.Fatal("a failed replay must not delete the backlog entry the automatic path tracks")
	}
	if pd.Attempts != 5 || !pd.FirstSeen.Equal(firstSeen) {
		t.Errorf("the pending entry was altered: %+v", pd)
	}
}

// An explicit replay must not be expired out from under the operator by the
// stale pending entry it is meant to act on.
func TestReviewOneIsNotExpiredByAStalePendingEntry(t *testing.T) {
	h := newHarness(t, nil)
	key := replayRef.Key()
	if err := h.st.PutPending(store.Pending{
		Key: key, FirstSeen: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Attempts: 99, LastReason: "draft",
	}); err != nil {
		t.Fatal(err)
	}

	d, err := h.p.ReviewOne(context.Background(), replayRef, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionReview {
		t.Fatalf("Action = %q (%s); the operator asked for this one explicitly", d.Action, d.Reason)
	}
	rec, _, _ := h.st.Review(key)
	if rec.Outcome != store.OutcomeReviewed {
		t.Errorf("Outcome = %q, want reviewed", rec.Outcome)
	}
}

// ---- Finding 2: a pause must stop the expiry clock, not merely delay it ----

// expirePending sits below the pause gate, so nothing expires *while* paused.
// But it measures age from Pending.FirstSeen, which hold never refreshed, so
// the first sweep after `firstpass resume` found every parked ref older than
// pending_max_age and expired the lot -- exactly the outcome the pause exists
// to prevent. The old test asserted only during the paused sweep.
func TestPauseDoesNotExpirePendingAfterResume(t *testing.T) {
	h := newHarness(t, nil)
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"

	start := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	now := start
	h.p.Now = func() time.Time { return now }

	// Parked as a draft, moments before the operator pauses.
	h.prs.info[key] = ghpr.PRInfo{State: "OPEN", IsDraft: true, Author: "colleague", HeadSHA: "sha"}
	if err := h.st.PutPending(store.Pending{
		Key: key, FirstSeen: start, Attempts: 1, LastReason: "draft",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.cfg.PauseFile(), []byte("paused"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Eight days of pause: longer than pending_max_age (168h by default).
	for _, d := range []time.Duration{time.Hour, 4 * 24 * time.Hour, 8 * 24 * time.Hour} {
		now = start.Add(d)
		if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
			t.Fatal(err)
		}
	}

	// `firstpass resume`.
	if err := os.Remove(h.cfg.PauseFile()); err != nil {
		t.Fatal(err)
	}
	now = start.Add(8*24*time.Hour + time.Hour)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Paused {
		t.Fatal("the pause file is gone, so this sweep is not paused")
	}
	if rec, found, _ := h.st.Review(key); found {
		t.Errorf("the first sweep after a resume expired a ref parked across the pause: outcome=%q; "+
			"a pause must exclude its own duration from the expiry clock", rec.Outcome)
	}
	pd, ok, _ := h.st.Pending(key)
	if !ok {
		t.Fatal("the ref must still be in the backlog the pause was protecting")
	}
	if pd.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2: one before the pause, one from the sweep after the resume", pd.Attempts)
	}
}

// The other half of the same fix: only the paused path may stop the clock. A
// ref parked by the per-sweep cap during normal operation must keep accruing
// age, or age-based expiry never fires for a persistently over-capped backlog.
func TestPerSweepCapParkDoesNotResetTheExpiryClock(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("a", 1))})
	h.seedWatermark(t)
	h.cfg.MaxReviewsPerSweep = 1
	h.apply()

	parked := "example-org/b#2"
	firstSeen := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := h.st.PutPending(store.Pending{
		Key: parked, FirstSeen: firstSeen, LastReason: "per-sweep cap reached",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	pd, ok, _ := h.st.Pending(parked)
	if !ok {
		t.Fatal("the ref over the cap must stay parked")
	}
	if !pd.FirstSeen.Equal(firstSeen) {
		t.Errorf("FirstSeen = %s, want %s unchanged: only a pause may stop the expiry clock",
			pd.FirstSeen, firstSeen)
	}
}

// ---- Finding 3: a negative -backfill must not bypass the cold-start guard --

// The guard tested Backfill == 0 while the window selection tested
// Backfill > 0, so a negative value fell between them: `scan -live
// -backfill -1` on a fresh install skipped launch day's protection, processed
// the whole fetch_limit window, posted on all of it, and advanced the
// watermark.
func TestNegativeBackfillDoesNotBypassTheColdStartGuard(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m9", prURL("aex-balances", 12))})
	// No watermark seeded: this is a fresh install.

	rep, err := h.p.Sweep(context.Background(), Options{Backfill: -1})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.ColdStart {
		t.Fatal("a negative -backfill must still be a cold start, not a full-window sweep of history")
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("a cold start must review nothing, ran %v", h.rev.ran)
	}
}

// ---- Finding 5: a dry run's report-write failure is not a killed review ----

func TestDryRunReportWriteFailureIsNotRecordedAsAKilledReview(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"
	h.rev.err = &review.ReportError{Err: errors.New("no space left on device")}
	h.rev.result = review.Result{ExitCode: 0}

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := h.st.Review(key)
	if !ok {
		t.Fatal("no record")
	}
	if rec.ExitCode == ExitUnknown {
		t.Errorf("ExitCode = %d; the review was not killed, only its report could not be written", rec.ExitCode)
	}
	if strings.Contains(rec.Detail, "comments may be partially posted") {
		t.Errorf("a dry run posts nothing, so the detail must not say it might have: %q", rec.Detail)
	}
	if !strings.Contains(rec.Detail, "report") {
		t.Errorf("the detail must name the report write as the failure, got %q", rec.Detail)
	}
	if rec.Outcome != store.OutcomeNeedsAttention {
		t.Errorf("Outcome = %q; there is no report to read, so it must be surfaced to the operator",
			rec.Outcome)
	}
}

// ---- Finding 7: a pending read failure must hold the watermark ----

// expirePending collapsed `err != nil || !ok` into "no pending entry", so a
// failing read was indistinguishable from an absent one: the error never
// reached note() and the watermark advanced over a batch whose reads were
// failing. The Store.Review read earlier in handle already guards this case.
//
// Pipeline.Store is a concrete *store.Store with no failure seam, so the read
// failure is induced by writing a value into the pending bucket that is not
// valid JSON -- which is what a torn write or a corrupted page looks like from
// store.get. bbolt allows one open handle per file, so the store is closed,
// the value planted, and the store reopened.
func TestPendingReadFailureHoldsTheWatermark(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"

	dbPath := filepath.Join(h.cfg.StateDir, "state.db")
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(dbPath, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("pending")).Put([]byte(key), []byte("{not json"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h.st, h.p.Store = st, st

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	wm, _, _ := h.st.Watermark()
	if wm.MessageName != "spaces/A/messages/m0" {
		t.Errorf("watermark = %q; a failing store read must hold it, exactly as a failing write does",
			wm.MessageName)
	}
}
