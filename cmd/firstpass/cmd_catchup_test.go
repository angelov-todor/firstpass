package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/store"
)

func catchupStore(t *testing.T, recs ...store.Review) *store.Store {
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

func catchupMsg(name, text string) chat.Message {
	return chat.Message{Name: name, Text: text, CreateTime: time.Now()}
}

// The listing is the whole safety of this command: it skips reviews, and the
// operator's only chance to notice that it skips one they wanted is the list
// printed before it moves.
func TestSkippedRefsListsEachPullRequestOnce(t *testing.T) {
	st := catchupStore(t)
	refs, decided := skippedRefs(st, []chat.Message{
		catchupMsg("m3", "https://github.com/Example-Org/aex-a/pull/1 and "+
			"https://github.com/Example-Org/aex-b/pull/2"),
		// The same pull request again, in an older message.
		catchupMsg("m2", "ping https://github.com/Example-Org/aex-a/pull/1"),
		catchupMsg("m1", "no links here"),
	})
	if len(refs) != 2 {
		t.Fatalf("want each pull request once, got %d: %+v", len(refs), refs)
	}
	if decided != 0 {
		t.Errorf("nothing was decided, got %d", decided)
	}
}

// The already-decided count is what turns the list into a judgement the
// operator can make quickly: a window whose pull requests were all reviewed is
// nothing to think about, and one carrying untouched pull requests is.
func TestSkippedRefsCountsWhatWasAlreadyDecided(t *testing.T) {
	st := catchupStore(t,
		store.Review{Key: "example-org/aex-a#1", Outcome: store.OutcomeReviewed},
		// in_flight is the one non-terminal outcome: a run that died, which is
		// emphatically not "already dealt with".
		store.Review{Key: "example-org/aex-b#2", Outcome: store.OutcomeInFlight},
	)
	refs, decided := skippedRefs(st, []chat.Message{
		catchupMsg("m1", "https://github.com/Example-Org/aex-a/pull/1 "+
			"https://github.com/Example-Org/aex-b/pull/2 "+
			"https://github.com/Example-Org/aex-c/pull/3"),
	})
	if len(refs) != 3 {
		t.Fatalf("want three pull requests, got %d", len(refs))
	}
	if decided != 1 {
		t.Errorf("decided = %d; only the reviewed one is settled, and in_flight is not", decided)
	}
}
