package store

// The two fields a second pass adds to a review record: which pass this was,
// and the commit the pass before it reviewed.

import (
	"encoding/json"
	"strings"
	"testing"
)

// The production database is full of reviewed rows written before a pass
// number existed. Every one of them was a first pass, so PassNumber must say
// 1 -- a row rendered as "pass 0" would be a state that never existed.
func TestAReviewRowWrittenBeforePassNumbersIsPassOne(t *testing.T) {
	legacy := `{"key":"Example-Org/aex-balances#12","outcome":"reviewed","head_sha":"abc",` +
		`"trigger_message":"spaces/A/messages/m1","started_at":"2026-09-03T12:00:00Z",` +
		`"decided_at":"2026-09-03T12:12:00Z","duration_ms":735000,"verdict":"approved"}`

	var r Review
	if err := json.Unmarshal([]byte(legacy), &r); err != nil {
		t.Fatal(err)
	}
	if r.Pass != 0 {
		t.Errorf("Pass = %d; an absent field must decode as the zero value", r.Pass)
	}
	if got := r.PassNumber(); got != 1 {
		t.Errorf("PassNumber() = %d, want 1: every row written before this field was a first pass", got)
	}
	if r.PreviousHeadSHA != "" {
		t.Errorf("PreviousHeadSHA = %q, want empty: there was no pass before it", r.PreviousHeadSHA)
	}
	if r.Verdict != VerdictApproved || r.Outcome != OutcomeReviewed {
		t.Errorf("the rest of the row must still decode: %+v", r)
	}
}

func TestPassAndPreviousHeadSHARoundTrip(t *testing.T) {
	s := openAt(t, t.TempDir())

	const key = "Example-Org/aex-balances#12"
	if err := s.PutReview(Review{
		Key: key, Outcome: OutcomeReviewed, HeadSHA: "new-sha",
		Pass: 2, PreviousHeadSHA: "old-sha",
	}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Review(key)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.Pass != 2 || got.PassNumber() != 2 {
		t.Errorf("Pass = %d, PassNumber() = %d, want 2", got.Pass, got.PassNumber())
	}
	if got.PreviousHeadSHA != "old-sha" {
		t.Errorf("PreviousHeadSHA = %q, want the commit the first pass reviewed", got.PreviousHeadSHA)
	}
	if got.HeadSHA != "new-sha" {
		t.Errorf("HeadSHA = %q, want the commit this pass reviewed", got.HeadSHA)
	}
}

// "pass":0 is a state no code ever means. Keeping it off disk is what makes a
// legacy row and a freshly written first pass the same shape to read.
func TestPassZeroIsOmittedOnDisk(t *testing.T) {
	b, err := json.Marshal(Review{Key: "o/r#1", Outcome: OutcomeReviewed})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "pass") {
		t.Errorf("an unset pass must not be written: %s", b)
	}
	if strings.Contains(string(b), "previous_head_sha") {
		t.Errorf("an absent previous head SHA must not be written: %s", b)
	}
	b, err = json.Marshal(Review{Key: "o/r#1", Outcome: OutcomeReviewed, Pass: 2, PreviousHeadSHA: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"pass":2`) || !strings.Contains(string(b), `"previous_head_sha":"old"`) {
		t.Errorf("a later pass must record both: %s", b)
	}
}

// The commit set. HeadSHA alone is the *last* pass's commit, so comparing only
// against it lets a head force-pushed back to any earlier reviewed commit
// through -- onto lines that already carry that pass's comments.
func TestReviewedCommitsIsEveryCommitAPassHasReviewed(t *testing.T) {
	r := Review{
		Key: "o/r#1", Outcome: OutcomeReviewed, Pass: 3,
		HeadSHA: "ccc", PreviousHeadSHA: "bbb",
		ReviewedSHAs: []string{"aaa", "bbb", "ccc"},
	}
	for _, sha := range []string{"aaa", "bbb", "ccc"} {
		if !r.HasReviewedCommit(sha) {
			t.Errorf("HasReviewedCommit(%q) = false; every pass's commit counts, not just the last",
				sha)
		}
	}
	if r.HasReviewedCommit("ddd") {
		t.Error("HasReviewedCommit(\"ddd\") = true; that commit has never been reviewed")
	}
	if r.HasReviewedCommit("") {
		t.Error("an unknown head must never count as reviewed")
	}
}

// A row written before the field existed. Deriving the set from the two SHAs it
// does carry is what makes an existing record behave correctly rather than
// letting its first pass's commit through.
func TestReviewedCommitsFallsBackToTheTwoRecordedSHAs(t *testing.T) {
	legacy := `{"key":"o/r#1","outcome":"reviewed","head_sha":"ccc","previous_head_sha":"bbb",` +
		`"pass":2,"started_at":"2026-09-03T12:00:00Z","decided_at":"2026-09-03T12:12:00Z"}`
	var r Review
	if err := json.Unmarshal([]byte(legacy), &r); err != nil {
		t.Fatal(err)
	}
	if got := r.ReviewedCommits(); len(got) != 2 || got[0] != "bbb" || got[1] != "ccc" {
		t.Errorf("ReviewedCommits() = %v, want [bbb ccc] derived from the two recorded SHAs", got)
	}
	for _, sha := range []string{"bbb", "ccc"} {
		if !r.HasReviewedCommit(sha) {
			t.Errorf("HasReviewedCommit(%q) = false on a pre-existing row", sha)
		}
	}
	if r.HasReviewedCommit("aaa") {
		t.Error("a commit the row knows nothing about must not count as reviewed")
	}
}

// A first-pass row, old or new, has exactly one reviewed commit.
func TestReviewedCommitsOnAFirstPassRow(t *testing.T) {
	old := Review{Key: "o/r#1", Outcome: OutcomeReviewed, HeadSHA: "aaa"}
	if got := old.ReviewedCommits(); len(got) != 1 || got[0] != "aaa" {
		t.Errorf("ReviewedCommits() = %v, want [aaa]", got)
	}
	none := Review{Key: "o/r#2", Outcome: OutcomeSkippedState}
	if got := none.ReviewedCommits(); len(got) != 0 {
		t.Errorf("ReviewedCommits() = %v, want empty: nothing was ever reviewed", got)
	}
}

func TestReviewedSHAsRoundTripAndAreOmittedWhenEmpty(t *testing.T) {
	s := openAt(t, t.TempDir())
	const key = "Example-Org/aex-balances#12"
	if err := s.PutReview(Review{
		Key: key, Outcome: OutcomeReviewed, HeadSHA: "ccc", Pass: 3,
		ReviewedSHAs: []string{"aaa", "bbb", "ccc"},
	}); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Review(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ReviewedSHAs) != 3 || got.ReviewedSHAs[2] != "ccc" {
		t.Errorf("ReviewedSHAs = %v, want the whole set in order", got.ReviewedSHAs)
	}
	b, err := json.Marshal(Review{Key: key, Outcome: OutcomeReviewed})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "reviewed_shas") {
		t.Errorf("an empty set must not be written, so a pre-existing row and a fresh one with "+
			"nothing to say are the same shape: %s", b)
	}
}
