package main

// status has to show that a pull request has been reviewed more than once.
// The operator's question on seeing comments arrive twice on one pull request
// is "did firstpass do that deliberately?", and the pass number is the answer.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/store"
)

func TestRenderStatusShowsALaterPassDistinguishably(t *testing.T) {
	decided := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	reviews := []store.Review{
		// A first pass, written by this version.
		{Key: "o/r#1", Outcome: store.OutcomeReviewed, DecidedAt: decided,
			Verdict: store.VerdictApproved, Pass: 1, HeadSHA: "aaaa"},
		// A second pass.
		{Key: "o/r#2", Outcome: store.OutcomeReviewed, DecidedAt: decided,
			Verdict: store.VerdictFindings, Pass: 2, HeadSHA: "cccc", PreviousHeadSHA: "bbbb"},
		// A row from before the pass number existed. It was a first pass, and
		// must render as one -- never as "pass 0".
		{Key: "o/r#3", Outcome: store.OutcomeReviewed, DecidedAt: decided,
			Verdict: store.VerdictApproved, HeadSHA: "dddd"},
	}

	var buf bytes.Buffer
	renderStatus(&buf, reviews, nil, store.Watermark{}, false, false, false)
	out := buf.String()

	if strings.Contains(out, "pass 0") {
		t.Errorf("no row may render as pass 0; it is a state that has never existed:\n%s", out)
	}

	second := statusLineFor(t, out, "o/r#2")
	if !strings.Contains(second, "pass 2") {
		t.Errorf("row o/r#2 = %q, want it to say this was pass 2", second)
	}
	if !strings.Contains(second, "reviewed / findings") {
		t.Errorf("row o/r#2 = %q; a later pass must still show its verdict", second)
	}

	// A first pass says nothing extra: almost every row is one, and a column
	// that says "pass 1" on all of them makes the one row that matters harder
	// to see, not easier.
	for _, key := range []string{"o/r#1", "o/r#3"} {
		if line := statusLineFor(t, out, key); strings.Contains(line, "pass") {
			t.Errorf("row %s = %q, want no pass note on a first pass", key, line)
		}
	}
}

// A pass number is only meaningful for a review. A skipped or expired row was
// never reviewed at all.
func TestRenderStatusPutsNoPassNoteOnANonReview(t *testing.T) {
	var buf bytes.Buffer
	renderStatus(&buf, []store.Review{
		{Key: "o/r#9", Outcome: store.OutcomeSkippedState, Detail: "state MERGED", Pass: 2},
	}, nil, store.Watermark{}, false, false, false)

	if line := statusLineFor(t, buf.String(), "o/r#9"); strings.Contains(line, "pass") {
		t.Errorf("row o/r#9 = %q; a skipped row was never reviewed, so it has no pass", line)
	}
}
