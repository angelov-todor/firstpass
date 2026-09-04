package pipeline

// The pause must *stop* the expiry clock, not restart it.
//
// This invariant has now been attempted three times. The first attempt stopped
// expiry only while a sweep was paused, so the first sweep after `firstpass
// resume` expired the whole backlog at once. The second reset FirstSeen to now
// on every paused park, which discards all age accrued before the pause rather
// than only the paused interval: an operator who pauses regularly disabled
// age-based expiry outright, and a ref already past pending_max_age became
// un-expirable if a pause landed before the sweep that would have retired it.
//
// Both shipped with tests that could not tell "stop" from "reset", because
// every one of them parked the ref and paused immediately: pre-pause age was
// always ~0, and the two implementations agree there. The test below is the
// one that discriminates them -- it carries six days of pre-pause age.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/store"
)

func mustPending(t *testing.T, h *harness, key string) store.Pending {
	t.Helper()
	pd, ok, err := h.st.Pending(key)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("no pending entry for %s", key)
	}
	return pd
}

// TestPauseShiftsTheExpiryClockAndKeepsPrePauseAge is the discriminating test:
// six days of unpaused age, then one paused hour, then two more unpaused days.
// Eight days of real, unpaused waiting against a 168h budget must expire the
// ref. Under an implementation that resets FirstSeen on a paused park, the
// clock restarts at the pause and the final sweep sees only two days.
func TestPauseShiftsTheExpiryClockAndKeepsPrePauseAge(t *testing.T) {
	h := newHarness(t, nil)
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"

	base := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	now := base
	h.p.Now = func() time.Time { return now }

	// Six days already accrued: parked by the per-sweep cap, which never
	// counts an attempt, so age is this ref's only route to a terminal record.
	firstSeen := base.Add(-6 * 24 * time.Hour)
	if err := h.st.PutPending(store.Pending{
		Key: key, FirstSeen: firstSeen, LastReason: "per-sweep cap reached",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.cfg.PauseFile(), []byte("paused"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The first paused sighting only records when the pause was observed:
	// nothing has been paused yet, so there is nothing to credit.
	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	pd := mustPending(t, h, key)
	if !pd.FirstSeen.Equal(firstSeen) {
		t.Errorf("FirstSeen = %s, want %s: the first paused sweep must not discard the six days "+
			"of age this ref had already accrued", pd.FirstSeen, firstSeen)
	}
	if !pd.LastPausedAt.Equal(base) {
		t.Errorf("LastPausedAt = %s, want %s: the paused park must record when it saw the pause",
			pd.LastPausedAt, base)
	}

	// Still paused an hour later. Exactly that hour is credited to the pause.
	now = base.Add(time.Hour)
	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	pd = mustPending(t, h, key)
	wantFirstSeen := firstSeen.Add(time.Hour)
	if !pd.FirstSeen.Equal(wantFirstSeen) {
		t.Errorf("FirstSeen = %s, want %s: a paused sweep must shift the clock forward by the "+
			"paused interval, not restart it at now", pd.FirstSeen, wantFirstSeen)
	}
	if !pd.LastPausedAt.Equal(now) {
		t.Errorf("LastPausedAt = %s, want %s", pd.LastPausedAt, now)
	}
	if pd.Attempts != 0 {
		t.Errorf("Attempts = %d; a paused park is not a failure of this PR", pd.Attempts)
	}

	// `firstpass resume`, then two more unpaused days. Total unpaused age is
	// six days plus two: 192h against a 168h budget.
	if err := os.Remove(h.cfg.PauseFile()); err != nil {
		t.Fatal(err)
	}
	now = base.Add(time.Hour + 2*24*time.Hour)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Paused {
		t.Fatal("the pause file is gone, so this sweep is not paused")
	}
	rec, ok, _ := h.st.Review(key)
	if !ok {
		t.Fatal("eight unpaused days against a 168h budget must produce a terminal record")
	}
	if rec.Outcome != store.OutcomeExpired {
		t.Errorf("Outcome = %q, want %q: only the paused hour may be excluded from the age, so "+
			"eight days of unpaused waiting must still retire this ref",
			rec.Outcome, store.OutcomeExpired)
	}
	// Expiry retires the whole row, so no LastPausedAt survives the resume.
	if _, still, _ := h.st.Pending(key); still {
		t.Error("an expired entry must leave pending, or it is re-offered forever")
	}
	if d, found := decisionFor(rep, key); !found || d.Action != ActionSkip {
		t.Errorf("decision = %+v (found=%v), want skip: pending expired", d, found)
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("an expired ref must not be reviewed, ran %v", h.rev.ran)
	}
}

// The other side of the same field: every park that is *not* a pause must
// clear LastPausedAt. Left set, the next paused sweep would credit the pause
// with all the unpaused time since the resume -- the reset bug by another
// route.
func TestANonPausedParkClearsLastPausedAt(t *testing.T) {
	base := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	firstSeen := base.Add(-24 * time.Hour)
	stale := base.Add(-time.Hour)

	t.Run("deferAttempt", func(t *testing.T) {
		h := newHarness(t, nil)
		h.seedWatermark(t)
		h.p.Now = func() time.Time { return base }
		key := "example-org/aex-balances#12"

		if err := h.st.PutPending(store.Pending{
			Key: key, FirstSeen: firstSeen, LastPausedAt: stale, LastReason: "paused",
		}); err != nil {
			t.Fatal(err)
		}
		// A draft is the deferAttempt path: parked, and an attempt counted.
		h.prs.info[key] = ghpr.PRInfo{State: "OPEN", IsDraft: true, Author: "colleague", HeadSHA: "sha"}

		if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
			t.Fatal(err)
		}
		pd := mustPending(t, h, key)
		if !pd.LastPausedAt.IsZero() {
			t.Errorf("LastPausedAt = %s, want the zero value: this sweep was not paused, so the "+
				"next paused sweep must not credit the pause with the time since the resume",
				pd.LastPausedAt)
		}
		if !pd.FirstSeen.Equal(firstSeen) {
			t.Errorf("FirstSeen = %s, want %s unchanged: clearing the pause marker must not move "+
				"the clock", pd.FirstSeen, firstSeen)
		}
		if pd.Attempts != 1 {
			t.Errorf("Attempts = %d, want 1", pd.Attempts)
		}
	})

	t.Run("hold via the per-sweep cap", func(t *testing.T) {
		h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("a", 1))})
		h.seedWatermark(t)
		h.cfg.MaxReviewsPerSweep = 1
		h.apply()
		h.p.Now = func() time.Time { return base }
		parked := "example-org/b#2"

		if err := h.st.PutPending(store.Pending{
			Key: parked, FirstSeen: firstSeen, LastPausedAt: stale,
			LastReason: "per-sweep cap reached",
		}); err != nil {
			t.Fatal(err)
		}

		if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
			t.Fatal(err)
		}
		pd := mustPending(t, h, parked)
		if !pd.LastPausedAt.IsZero() {
			t.Errorf("LastPausedAt = %s, want the zero value: a cap park is not a pause",
				pd.LastPausedAt)
		}
		if !pd.FirstSeen.Equal(firstSeen) {
			t.Errorf("FirstSeen = %s, want %s unchanged: a ref over the cap must keep accruing age",
				pd.FirstSeen, firstSeen)
		}
		if pd.Attempts != 0 {
			t.Errorf("Attempts = %d; hitting the cap is not a failure", pd.Attempts)
		}
	})
}
