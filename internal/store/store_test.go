package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
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

func TestMessageRecordLifecycle(t *testing.T) {
	s := openAt(t, t.TempDir())

	if _, ok, err := s.Message("spaces/A/messages/m1"); err != nil || ok {
		t.Fatalf("a fresh store must know no messages (ok=%v err=%v)", ok, err)
	}

	m := MessageRecord{
		Name:      "spaces/A/messages/m1",
		RefKeys:   []string{"o/r#1", "o/r#2"},
		FirstSeen: time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC),
	}
	if err := s.PutMessage(m); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.Message("spaces/A/messages/m1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if len(got.RefKeys) != 2 || got.RefKeys[0] != "o/r#1" || got.RefKeys[1] != "o/r#2" {
		t.Errorf("RefKeys = %v; the refs a message carried are what the result reaction waits on", got.RefKeys)
	}
	if got.WatchApplied || got.ResultApplied {
		t.Errorf("a freshly recorded message has reacted to nothing: %+v", got)
	}

	got.WatchApplied = true
	got.WatchReaction = "spaces/A/messages/m1/reactions/r1"
	if err := s.PutMessage(got); err != nil {
		t.Fatal(err)
	}

	all, err := s.AllMessages()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].WatchReaction != "spaces/A/messages/m1/reactions/r1" {
		t.Fatalf("AllMessages() = %+v", all)
	}

	// No DeleteMessage on purpose: see MessageRecord's doc comment. These
	// records are the memory of what has already been reacted to, so nothing
	// prunes them.
}

// A message record survives a restart, so the 👀 added before a review can
// still be found and removed by a later process -- long after the message has
// scrolled out of the fetch window.
func TestMessageRecordSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	if err := s.PutMessage(MessageRecord{
		Name:          "spaces/A/messages/m1",
		RefKeys:       []string{"o/r#1"},
		WatchApplied:  true,
		WatchReaction: "spaces/A/messages/m1/reactions/r1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	got, ok, err := openAt(t, dir).Message("spaces/A/messages/m1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.WatchReaction != "spaces/A/messages/m1/reactions/r1" {
		t.Errorf("WatchReaction = %q; without it the 👀 can never be removed", got.WatchReaction)
	}
}

// The messages bucket was added after firstpass was already running in
// production, so the live database on disk does not have it. Opening such a
// database must create the bucket and leave every existing record alone --
// and Store.get dereferences tx.Bucket() without a nil check, so an absent
// bucket is a panic, not a miss.
func TestOpenUpgradesADatabaseWithoutTheMessagesBucket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	// Exactly what an older firstpass left behind: the three original buckets
	// and a review record, and no messages bucket at all.
	legacy, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = legacy.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketMeta, bucketReviews, bucketPending} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		rec, err := json.Marshal(Review{Key: "o/r#1", Outcome: OutcomeReviewed})
		if err != nil {
			return err
		}
		return tx.Bucket(bucketReviews).Put([]byte("o/r#1"), rec)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.View(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketMessages) != nil {
			t.Fatal("the fixture must not already have the messages bucket, or it proves nothing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("a pre-existing database must open cleanly: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// The pre-existing record is untouched: reactions are additional state,
	// never a migration of what is already there.
	rev, ok, err := s.Review("o/r#1")
	if err != nil || !ok {
		t.Fatalf("the existing review record must survive the upgrade (ok=%v err=%v)", ok, err)
	}
	if rev.Outcome != OutcomeReviewed {
		t.Errorf("Outcome = %q, want %q", rev.Outcome, OutcomeReviewed)
	}

	// And the new bucket is usable -- reading an absent key included, which is
	// the call a sweep makes for every trigger message it has never seen.
	if _, ok, err := s.Message("spaces/A/messages/m1"); err != nil || ok {
		t.Fatalf("reading an absent message on an upgraded database (ok=%v err=%v)", ok, err)
	}
	if err := s.PutMessage(MessageRecord{Name: "spaces/A/messages/m1", RefKeys: []string{"o/r#1"}}); err != nil {
		t.Fatalf("the messages bucket must exist after the upgrade: %v", err)
	}
	if _, ok, err := s.Message("spaces/A/messages/m1"); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}
