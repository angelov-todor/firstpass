package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/store"
)

func TestRenderStatusShowsReviewsPendingAndMode(t *testing.T) {
	reviews := []store.Review{
		{Key: "Example-Org/aex-balances#12", Outcome: store.OutcomeReviewed,
			DecidedAt: time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC), DurationMS: 91000},
		{Key: "Example-Org/aex-margin-service#3", Outcome: store.OutcomeNeedsAttention,
			DecidedAt: time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC),
			Detail:    "a previous run died mid-review"},
	}
	pending := []store.Pending{
		{Key: "Example-Org/aex-history-service#48", Attempts: 2, LastReason: "draft"},
	}
	wm := store.Watermark{MessageName: "spaces/A/messages/m9",
		CreateTime: time.Date(2026, 9, 3, 11, 30, 0, 0, time.UTC)}

	var buf bytes.Buffer
	renderStatus(&buf, reviews, pending, wm, true, false, true)
	out := buf.String()

	for _, want := range []string{
		"Example-Org/aex-balances#12", "reviewed",
		"Example-Org/aex-margin-service#3", "needs_attention", "died mid-review",
		"Example-Org/aex-history-service#48", "draft",
		"spaces/A/messages/m9", "dry run",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
}

func TestRenderStatusCallsOutNeedsAttentionAsActionable(t *testing.T) {
	reviews := []store.Review{
		{Key: "o/r#1", Outcome: store.OutcomeNeedsAttention, Detail: "review did not finish"},
	}
	var buf bytes.Buffer
	renderStatus(&buf, reviews, nil, store.Watermark{}, false, false, false)
	out := buf.String()
	if !strings.Contains(out, "replay") {
		t.Errorf("a needs_attention row must tell the user how to act on it:\n%s", out)
	}
}

func TestRenderStatusShowsPaused(t *testing.T) {
	var buf bytes.Buffer
	renderStatus(&buf, nil, nil, store.Watermark{}, false, true, false)
	if !strings.Contains(buf.String(), "PAUSED") {
		t.Errorf("a paused daemon must be obvious:\n%s", buf.String())
	}
}

func TestRenderStatusHandlesAFreshStore(t *testing.T) {
	var buf bytes.Buffer
	renderStatus(&buf, nil, nil, store.Watermark{}, false, false, true)
	out := buf.String()
	if !strings.Contains(out, "no watermark") {
		t.Errorf("a fresh store must say the next run is a cold start:\n%s", out)
	}
}
