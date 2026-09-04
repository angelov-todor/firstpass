package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func openAt(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestWatermarkRoundTrip(t *testing.T) {
	s := openAt(t, t.TempDir())

	if _, ok, err := s.Watermark(); err != nil || ok {
		t.Fatalf("a fresh store must have no watermark (ok=%v err=%v)", ok, err)
	}

	want := Watermark{
		MessageName: "spaces/EXAMPLE123/messages/abc",
		CreateTime:  time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC),
	}
	if err := s.SetWatermark(want); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.Watermark()
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.MessageName != want.MessageName || !got.CreateTime.Equal(want.CreateTime) {
		t.Errorf("Watermark() = %+v, want %+v", got, want)
	}
}

func TestInFlightSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	if err := s.PutReview(Review{
		Key: "o/r#1", Outcome: OutcomeInFlight, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := openAt(t, dir)
	got, ok, err := s2.Review("o/r#1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.Outcome != OutcomeInFlight {
		t.Errorf("Outcome = %q; in_flight must survive a restart or a crashed review is undetectable", got.Outcome)
	}
}

func TestTerminalClassification(t *testing.T) {
	for _, o := range []Outcome{
		OutcomeReviewed, OutcomeSkippedAuthor, OutcomeSkippedState,
		OutcomeSkippedOwner, OutcomeSkippedRepo, OutcomeNeedsAttention, OutcomeExpired,
	} {
		if !o.Terminal() {
			t.Errorf("%q must be terminal", o)
		}
	}
	if OutcomeInFlight.Terminal() {
		t.Error("in_flight must not be terminal: finding one later is how a crash is detected")
	}
}

// A pending row written before LastPausedAt existed carries no such field, and
// must decode as the zero value: not currently parked by a pause, which is
// exactly what it was. The store encodes with encoding/json, so this is the
// whole of the compatibility question.
func TestPendingWithoutLastPausedAtDecodesAsNotPaused(t *testing.T) {
	const legacy = `{"key":"o/r#2","first_seen":"2026-08-28T00:00:00Z",` +
		`"attempts":1,"last_attempt":"2026-09-01T00:00:00Z","last_reason":"draft"}`

	var p Pending
	if err := json.Unmarshal([]byte(legacy), &p); err != nil {
		t.Fatal(err)
	}
	if !p.LastPausedAt.IsZero() {
		t.Errorf("LastPausedAt = %s, want the zero value: an older row was never parked by a pause",
			p.LastPausedAt)
	}
	if !p.FirstSeen.Equal(time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("FirstSeen = %s; the accrued age of an older row must survive the upgrade", p.FirstSeen)
	}
}

func TestPendingLifecycle(t *testing.T) {
	s := openAt(t, t.TempDir())

	p := Pending{Key: "o/r#2", FirstSeen: time.Now().UTC(), Attempts: 1, LastReason: "draft"}
	if err := s.PutPending(p); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.Pending("o/r#2")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.Attempts != 1 || got.LastReason != "draft" {
		t.Errorf("Pending() = %+v", got)
	}

	all, err := s.AllPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("AllPending() = %d entries, want 1", len(all))
	}

	if err := s.DeletePending("o/r#2"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Pending("o/r#2"); ok {
		t.Error("a deleted pending entry must be gone")
	}
}

func TestDeletePendingOnAbsentKeyIsNotAnError(t *testing.T) {
	s := openAt(t, t.TempDir())
	if err := s.DeletePending("o/r#404"); err != nil {
		t.Errorf("deleting an absent pending key must be a no-op: %v", err)
	}
}

func TestReviews(t *testing.T) {
	s := openAt(t, t.TempDir())
	if err := s.PutReview(Review{Key: "o/r#1", Outcome: OutcomeReviewed}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutReview(Review{Key: "o/r#2", Outcome: OutcomeSkippedAuthor}); err != nil {
		t.Fatal(err)
	}

	all, err := s.Reviews()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("Reviews() = %d entries, want 2", len(all))
	}
}
