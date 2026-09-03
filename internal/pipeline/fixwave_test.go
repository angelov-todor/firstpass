package pipeline

// Findings from the whole-branch cross-package review: defects that existed
// only in the composition of the packages, not in any one of them.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/review"
	"github.com/angelov-todor/firstpass/internal/store"
)

// I1: chat.Fetch returns everything when the watermark has scrolled out of the
// window, which Sweep cannot tell from "all of these are new". Advancing the
// watermark there skips every message between the old watermark and the oldest
// one fetched -- silently, permanently, with no log line and no pending entry.
func TestWatermarkGapHoldsTheWatermark(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m9", prURL("aex-balances", 12)),
		msg("spaces/A/messages/m8", "chatter"),
	})
	// Deliberately not appended to the window: the watermark has scrolled out.
	if err := h.st.SetWatermark(store.Watermark{
		MessageName: "spaces/A/messages/gone",
		CreateTime:  time.Date(2026, 9, 1, 6, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.WatermarkGap {
		t.Error("a watermark missing from the window must be reported as a gap")
	}
	wm, _, _ := h.st.Watermark()
	if wm.MessageName != "spaces/A/messages/gone" {
		t.Errorf("watermark = %q; it must be held so the next sweep re-scans rather than skipping messages",
			wm.MessageName)
	}
}

func TestNoWatermarkGapWhenTheWatermarkIsInTheWindow(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.WatermarkGap {
		t.Error("the watermark was in the window; that is not a gap")
	}
	wm, _, _ := h.st.Watermark()
	if wm.MessageName != "spaces/A/messages/m1" {
		t.Errorf("watermark = %q, want the newest message", wm.MessageName)
	}
}

// A backfill sets since to the empty string on purpose, which is not a gap.
func TestBackfillIsNotAWatermarkGap(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m9", prURL("aex-balances", 12))})
	h.seedWatermark(t)

	rep, err := h.p.Sweep(context.Background(), Options{Backfill: 10})
	if err != nil {
		t.Fatal(err)
	}
	if rep.WatermarkGap {
		t.Error("a backfill ignores the watermark deliberately; that is not a gap")
	}
}

// I3: the spec says a sweep that finds an in_flight record from a previous run
// marks the PR needs_attention. Doing that from the candidate list only worked
// while the triggering message was still in the fetch window, and never worked
// for a `firstpass replay` that died mid-review -- such a record stayed
// in_flight forever and the PR appeared in no report at all.
func TestSweepRecoversAnOrphanedInFlightRecordWithNoChatMessage(t *testing.T) {
	h := newHarness(t, nil)
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"
	if err := h.st.PutReview(store.Review{
		Key: key, Outcome: store.OutcomeInFlight, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := h.st.Review(key)
	if !ok || rec.Outcome != store.OutcomeNeedsAttention {
		t.Fatalf("an orphaned in_flight record must be converted whether or not it reappears "+
			"as a candidate, got ok=%v outcome=%q", ok, rec.Outcome)
	}
	if rec.Detail == "" {
		t.Error("the record must explain what happened and how to act on it")
	}
	d, found := decisionFor(rep, key)
	if !found || d.Action != ActionNeedsAttention {
		t.Errorf("the recovery must appear in the report, got %+v (found=%v)", d, found)
	}
	if len(h.rev.ran) != 0 {
		t.Error("an orphan must not be reviewed automatically: comments may already be on the PR")
	}
}

func TestSweepRecoveryClearsAnyPendingEntryForTheOrphan(t *testing.T) {
	h := newHarness(t, nil)
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"
	if err := h.st.PutReview(store.Review{Key: key, Outcome: store.OutcomeInFlight}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.PutPending(store.Pending{
		Key: key, FirstSeen: time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if _, still, _ := h.st.Pending(key); still {
		t.Error("a needs_attention PR must not also sit in pending, or it would be retried")
	}
}

// M17: a review killed by its deadline reports no exit status, and persisting
// 0 would read as a clean success in `firstpass status`.
func TestKilledReviewDoesNotRecordExitZero(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	h.rev.err = context.DeadlineExceeded

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := h.st.Review("example-org/aex-balances#12")
	if !ok {
		t.Fatal("no record")
	}
	if rec.ExitCode != ExitUnknown {
		t.Errorf("ExitCode = %d, want ExitUnknown (%d); a killed run must not read as a success",
			rec.ExitCode, ExitUnknown)
	}
}

func TestNonZeroClaudeExitIsPreserved(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	h.rev.err = errors.New("claude exit 1")
	h.rev.result = review.Result{ExitCode: 1}

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := h.st.Review("example-org/aex-balances#12")
	if rec.ExitCode != 1 {
		t.Errorf("ExitCode = %d; a real exit status must not be replaced by the sentinel", rec.ExitCode)
	}
}

// I12: the pause file is the kill switch for a tool that writes to other
// people's pull requests. Sampled only at sweep start, a pause issued just
// after a sweep began could take an hour to bite, and two more comment sets
// could land on colleagues' PRs meanwhile.
func TestPauseCreatedMidSweepStopsTheNextReview(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	// Prepare runs immediately before the review, so this is the sweep's last
	// chance to notice.
	h.wts.onPrepare = func() {
		if err := os.WriteFile(h.cfg.PauseFile(), []byte("paused"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Paused {
		t.Error("Paused must stay the sweep-start observation, and this sweep did start unpaused")
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("a pause that lands before the review must stop it, ran %v", h.rev.ran)
	}
	key := "example-org/aex-balances#12"
	d, _ := decisionFor(rep, key)
	if d.Action != ActionDefer {
		t.Errorf("Action = %q, want defer", d.Action)
	}
	pd, ok, _ := h.st.Pending(key)
	if !ok {
		t.Fatal("the ref must be parked for a later sweep")
	}
	if pd.Attempts != 0 {
		t.Errorf("Attempts = %d; a pause is not a failure of this PR", pd.Attempts)
	}
	if _, found, _ := h.st.Review(key); found {
		t.Error("no review started, so no in_flight record may be left behind")
	}
}

// Ctrl-C mid-sweep used to keep iterating: each remaining candidate burned a
// pending attempt on the cancelled-context Inspect failure, and then the
// watermark advanced over the whole batch.
func TestCancelledContextStopsTheSweepWithoutAdvancingTheWatermark(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("a", 1)+" "+prURL("b", 2)+" "+prURL("c", 3)),
	})
	h.seedWatermark(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := h.p.Sweep(ctx, Options{}); err == nil {
		t.Fatal("an interrupted sweep must report the cancellation")
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("no review may start after the interrupt, ran %v", h.rev.ran)
	}
	all, err := h.st.AllPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Errorf("an interrupt must not burn a pending attempt on every remaining candidate, got %+v", all)
	}
	wm, _, _ := h.st.Watermark()
	if wm.MessageName != "spaces/A/messages/m0" {
		t.Errorf("watermark = %q; an interrupted sweep must not advance it", wm.MessageName)
	}
}

// I11: the spec advances the watermark only once the whole batch is recorded.
// PutReview/PutPending failures used to be logged and swallowed while the
// watermark moved on regardless, so a PR could be decided, not recorded, and
// then scroll out of the fetch window -- gone from every bucket and every
// report.
//
// Pipeline.Store is a concrete *store.Store with no failure seam, so the
// failure is induced through bbolt's own key-size limit: a key over 32KB is
// rejected on write while every read on the same store keeps working, which
// is precisely the shape this gate needs. A repo name that long is
// artificial; the code path it exercises is the real one.
func TestWatermarkIsHeldWhenACandidateCouldNotBeRecorded(t *testing.T) {
	huge := strings.Repeat("r", 40000)
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL(huge, 1)+" "+prURL("aex-balances", 12)),
	})
	h.seedWatermark(t)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}

	// Record what you can: the healthy PR is still reviewed and recorded.
	if rep.Reviewed != 1 {
		t.Errorf("Reviewed = %d; one bad candidate must not abort the whole sweep (decisions: %d)",
			rep.Reviewed, len(rep.Decisions))
	}
	if _, ok, _ := h.st.Review("example-org/aex-balances#12"); !ok {
		t.Error("the healthy candidate must still be recorded")
	}

	wm, _, _ := h.st.Watermark()
	if wm.MessageName != "spaces/A/messages/m0" {
		t.Errorf("watermark = %q; it must be held while part of the batch went unrecorded",
			wm.MessageName)
	}
}

// Once a mid-sweep pause has been seen, the remaining candidates are parked at
// the top of handle rather than each being inspected and cloned only to be
// turned away at the pause gate.
func TestPauseMidSweepParksTheRemainingCandidatesWithoutCloning(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("a", 1)+" "+prURL("b", 2)+" "+prURL("c", 3)),
	})
	h.seedWatermark(t)
	h.wts.onPrepare = func() {
		if err := os.WriteFile(h.cfg.PauseFile(), []byte("paused"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.wts.prepared) != 1 {
		t.Errorf("prepared = %v; only the candidate that raced the pause may reach Prepare",
			h.wts.prepared)
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("nothing may be reviewed, ran %v", h.rev.ran)
	}
	if len(rep.Decisions) != 3 {
		t.Fatalf("every candidate must still get a decision, got %d", len(rep.Decisions))
	}
	all, err := h.st.AllPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("all three must be parked for a later sweep, got %d", len(all))
	}
	for _, pd := range all {
		if pd.Attempts != 0 {
			t.Errorf("%s: Attempts = %d; a pause is not a failure of any PR", pd.Key, pd.Attempts)
		}
	}
}
