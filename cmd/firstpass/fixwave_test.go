package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/pipeline"
	"github.com/angelov-todor/firstpass/internal/store"
)

// I1: a fetch window too small to contain the watermark means messages were
// never scanned. The operator has to be told, and told what to change.
func TestRenderSweepWarnsAboutAWatermarkGap(t *testing.T) {
	var buf bytes.Buffer
	renderSweep(&buf, pipeline.SweepReport{WatermarkGap: true, MessagesScanned: 50}, false, true)
	out := buf.String()
	for _, want := range []string{"fetch_limit", "-backfill", "not advanced"} {
		if !strings.Contains(out, want) {
			t.Errorf("a watermark gap must be explained and actionable, missing %q:\n%s", want, out)
		}
	}
}

func TestRenderSweepSaysNothingAboutAGapWhenThereIsNone(t *testing.T) {
	var buf bytes.Buffer
	renderSweep(&buf, pipeline.SweepReport{MessagesScanned: 3}, false, true)
	if strings.Contains(buf.String(), "fetch_limit") {
		t.Errorf("no gap, no warning:\n%s", buf.String())
	}
}

// I7: under Task Scheduler a bare one-line error goes into a log nobody reads.
// A fatal chat error means the sweep did not happen at all, and the likeliest
// cause is that firstpass is authenticated as the wrong Google account -- which
// otherwise looks exactly like "nobody posted a PR".
func TestFatalChatBannerNamesTheCauseAndTheRemedy(t *testing.T) {
	err := &chat.APIError{Code: 403, Status: "PERMISSION_DENIED", Message: "insufficient scope"}
	out := fatalChatBanner(err)

	for _, want := range []string{
		"REFUSED TO SWEEP", "PERMISSION_DENIED", "Google account", "firstpass doctor",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("banner missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "\n") < 4 {
		t.Errorf("the banner must be unmistakable, not a one-liner:\n%s", out)
	}
}

// I3: an orphaned in_flight record is actionable, so status must count it.
func TestRenderStatusCountsInFlightAsActionable(t *testing.T) {
	reviews := []store.Review{
		{Key: "example-org/aex-balances#12", Outcome: store.OutcomeInFlight},
	}
	var buf bytes.Buffer
	renderStatus(&buf, reviews, nil, store.Watermark{}, false, false, true)
	out := buf.String()
	if !strings.Contains(out, "in_flight") {
		t.Errorf("an in_flight record must be visible as an orphan:\n%s", out)
	}
	if !strings.Contains(out, "need attention") {
		t.Errorf("an in_flight record must be counted into the actionable summary:\n%s", out)
	}
}

// M17: a killed review has no exit status. status must not show it as a clean
// zero, and must not show a real non-zero status as "killed".
func TestRenderStatusDistinguishesAKilledReviewsExit(t *testing.T) {
	reviews := []store.Review{
		{Key: "o/r#1", Outcome: store.OutcomeNeedsAttention, ExitCode: pipeline.ExitUnknown},
		{Key: "o/r#2", Outcome: store.OutcomeNeedsAttention, ExitCode: 1},
		{Key: "o/r#3", Outcome: store.OutcomeReviewed, ExitCode: 0},
	}
	var buf bytes.Buffer
	renderStatus(&buf, reviews, nil, store.Watermark{}, false, false, true)
	out := buf.String()
	if !strings.Contains(out, "killed") {
		t.Errorf("a review with no exit status must read as killed, not as exit 0:\n%s", out)
	}
	if !strings.Contains(out, "EXIT") {
		t.Errorf("the exit column must be labelled:\n%s", out)
	}
}
