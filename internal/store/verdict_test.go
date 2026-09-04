package store

// The verdict recorded against a review: what firstpass actually submitted on
// the pull request, which is not always what the reviewer decided.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReviewVerdictRoundTrips(t *testing.T) {
	s := openAt(t, t.TempDir())

	for _, v := range []Verdict{VerdictApproved, VerdictFindings, VerdictUnknown, VerdictNone} {
		key := "Example-Org/aex-balances#" + string(v) + "1"
		if err := s.PutReview(Review{Key: key, Outcome: OutcomeReviewed, Verdict: v}); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.Review(key)
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		if got.Verdict != v {
			t.Errorf("Verdict = %q, want %q", got.Verdict, v)
		}
	}
}

// The production database already holds reviewed rows written before the
// verdict existed. They must decode as "no verdict submitted" -- the zero
// value -- and must not be mistaken for an approval.
func TestAReviewRowWrittenBeforeVerdictsDecodesWithNoVerdict(t *testing.T) {
	legacy := `{"key":"Example-Org/aex-balances#12","outcome":"reviewed","head_sha":"abc",` +
		`"started_at":"2026-09-03T12:00:00Z","decided_at":"2026-09-03T12:12:00Z","duration_ms":735000}`

	var r Review
	if err := json.Unmarshal([]byte(legacy), &r); err != nil {
		t.Fatal(err)
	}
	if r.Verdict != VerdictNone {
		t.Errorf("Verdict = %q, want the zero value %q", r.Verdict, VerdictNone)
	}
	if r.Verdict == VerdictApproved {
		t.Error("an old row must never decode as an approval")
	}
	if r.Outcome != OutcomeReviewed {
		t.Errorf("Outcome = %q; the rest of the row must still decode", r.Outcome)
	}
}

// A row with no verdict must not carry the field on disk either, so an
// unset verdict and a pre-verdict row are indistinguishable -- there is no
// third state to reason about.
func TestNoVerdictIsOmittedOnDisk(t *testing.T) {
	b, err := json.Marshal(Review{Key: "o/r#1", Outcome: OutcomeReviewed})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "verdict") {
		t.Errorf("an unset verdict must not be written: %s", b)
	}
	b, err = json.Marshal(Review{Key: "o/r#1", Outcome: OutcomeReviewed, Verdict: VerdictApproved})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"verdict":"approved"`) {
		t.Errorf("a submitted verdict must be written: %s", b)
	}
}
