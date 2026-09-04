package main

// status has to show the verdict, and has to show the difference between a
// verdict that was submitted and one that was not. "reviewed" alone is what
// the operator has been reading for a clean PR, and it is exactly the
// ambiguity the verdict exists to remove.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/store"
)

// statusLineFor returns the rendered table row naming key.
func statusLineFor(t *testing.T, out, key string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, key) {
			return line
		}
	}
	t.Fatalf("no row for %s in:\n%s", key, out)
	return ""
}

func TestRenderStatusShowsEachVerdictStateDistinguishably(t *testing.T) {
	decided := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	reviews := []store.Review{
		{Key: "o/r#1", Outcome: store.OutcomeReviewed, DecidedAt: decided,
			Verdict: store.VerdictApproved},
		{Key: "o/r#2", Outcome: store.OutcomeReviewed, DecidedAt: decided,
			Verdict: store.VerdictFindings},
		{Key: "o/r#3", Outcome: store.OutcomeReviewed, DecidedAt: decided,
			Verdict: store.VerdictUnknown, Detail: "printed no FIRSTPASS-VERDICT line"},
		{Key: "o/r#4", Outcome: store.OutcomeReviewed, DecidedAt: decided,
			Detail: "submitting the approve verdict failed (gh pr review exit 1)"},
		{Key: "o/r#5", Outcome: store.OutcomeNeedsAttention, DecidedAt: decided,
			Detail: "review did not finish"},
	}

	var buf bytes.Buffer
	renderStatus(&buf, reviews, nil, store.Watermark{}, false, false, false)
	out := buf.String()

	// Each row's outcome cell must be its own string, so the five states can
	// be told apart at a glance rather than by reading Detail.
	cells := map[string]string{
		"o/r#1": "reviewed / approved",
		"o/r#2": "reviewed / findings",
		"o/r#3": "reviewed / verdict unknown",
		"o/r#4": "reviewed / no verdict",
		"o/r#5": "needs_attention",
	}
	for key, want := range cells {
		line := statusLineFor(t, out, key)
		if !strings.Contains(line, want) {
			t.Errorf("row %s = %q, want it to show %q", key, line, want)
		}
	}

	// The successful ones must not be confusable with the unsuccessful ones.
	if strings.Contains(statusLineFor(t, out, "o/r#1"), "unknown") ||
		strings.Contains(statusLineFor(t, out, "o/r#1"), "no verdict") {
		t.Errorf("an approved row must not read as a failed verdict: %q", statusLineFor(t, out, "o/r#1"))
	}
	if strings.Contains(statusLineFor(t, out, "o/r#4"), "approved") {
		t.Errorf("a failed verdict must never read as approved: %q", statusLineFor(t, out, "o/r#4"))
	}

	// The verdict is not a substitute for the detail: the operator has to be
	// able to see why a verdict is missing.
	if !strings.Contains(statusLineFor(t, out, "o/r#4"), "gh pr review exit 1") {
		t.Errorf("a failed submission's error must still be shown: %q", statusLineFor(t, out, "o/r#4"))
	}
}

// A row that is not a review carries no verdict, and must not sprout one.
func TestRenderStatusLeavesNonReviewedOutcomesAlone(t *testing.T) {
	reviews := []store.Review{
		{Key: "o/r#7", Outcome: store.OutcomeSkippedAuthor, Detail: "authored by angelov-todor"},
	}
	var buf bytes.Buffer
	renderStatus(&buf, reviews, nil, store.Watermark{}, false, false, false)

	line := statusLineFor(t, buf.String(), "o/r#7")
	if strings.Contains(line, "verdict") || strings.Contains(line, " / ") {
		t.Errorf("a skipped row must carry no verdict: %q", line)
	}
}
