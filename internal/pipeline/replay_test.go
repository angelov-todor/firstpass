package pipeline

import (
	"context"
	"testing"

	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/store"
)

// Canonically lowercase, as prref.Extract produces it -- cmdReplay parses the
// operator's argument through Extract, so this is the shape ReviewOne sees.
var replayRef = prref.PRRef{Owner: "example-org", Repo: "aex-balances", Number: 12}

func TestReviewOneIgnoresATerminalRecord(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.st.PutReview(store.Review{
		Key: replayRef.Key(), Outcome: store.OutcomeReviewed,
	}); err != nil {
		t.Fatal(err)
	}

	d, err := h.p.ReviewOne(context.Background(), replayRef, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionReview {
		t.Fatalf("Action = %q (%s); replay must review despite the existing record", d.Action, d.Reason)
	}
	if len(h.rev.ran) != 1 {
		t.Errorf("ran = %v, want one review", h.rev.ran)
	}
}

func TestReviewOneClearsANeedsAttentionRecord(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.st.PutReview(store.Review{
		Key: replayRef.Key(), Outcome: store.OutcomeNeedsAttention,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.p.ReviewOne(context.Background(), replayRef, Options{}); err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := h.st.Review(replayRef.Key())
	if !ok || rec.Outcome != store.OutcomeReviewed {
		t.Errorf("Outcome = %q, want reviewed after a deliberate replay", rec.Outcome)
	}
}

func TestReviewOneStillHonoursTheOwnerAllowlist(t *testing.T) {
	h := newHarness(t, nil)
	outside := prref.PRRef{Owner: "torvalds", Repo: "linux", Number: 1}

	d, err := h.p.ReviewOne(context.Background(), outside, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionSkip {
		t.Errorf("Action = %q; replay must not be a way around the allowlist", d.Action)
	}
	if len(h.rev.ran) != 0 {
		t.Error("an outside org must never be reviewed, even on an explicit replay")
	}
}

// A replay is not subject to the sweep throttle. It uses a real cap value:
// Validate forbids zero, so a cap of 0 could not occur in production, and
// ReviewOne no longer mutates the shared Cfg to make such a value work.
func TestReviewOneIgnoresThePerSweepCap(t *testing.T) {
	h := newHarness(t, nil)
	h.cfg.MaxReviewsPerSweep = 1
	h.apply()

	d, err := h.p.ReviewOne(context.Background(), replayRef, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionReview {
		t.Errorf("Action = %q (%s); an explicit replay is not subject to the throttle", d.Action, d.Reason)
	}
	if h.p.Cfg.MaxReviewsPerSweep != 1 {
		t.Errorf("MaxReviewsPerSweep = %d; ReviewOne must not mutate shared pipeline state",
			h.p.Cfg.MaxReviewsPerSweep)
	}
}
