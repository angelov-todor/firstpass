package pipeline

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/store"
)

// C3: pause-then-replay used to delete the review record and the pending
// record first, then hit the pause gate inside handle. The needs_attention
// record -- including its "comments may already be partially posted" detail --
// was destroyed, and the ref was parked in pending, which candidates()
// re-offers on every sweep regardless of the watermark. The operator read
// "defer / paused, 0 reviewed" as "nothing happened"; the first sweep after
// `firstpass resume` then reviewed it unasked, double-posting on top of whatever
// the earlier run had already left on a colleague's PR.
func TestReviewOneRefusesWhilePausedAndDestroysNothing(t *testing.T) {
	h := newHarness(t, nil)
	detail := "comments may already be partially posted"
	if err := h.st.PutReview(store.Review{
		Key: replayRef.Key(), Outcome: store.OutcomeNeedsAttention, Detail: detail,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.cfg.PauseFile(), []byte("paused"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := h.p.ReviewOne(context.Background(), replayRef, Options{})
	if err == nil {
		t.Fatal("a replay while paused must be an error, not a silent requeue")
	}
	if !strings.Contains(err.Error(), "resume") {
		t.Errorf("the error must tell the operator to run `firstpass resume`, got %v", err)
	}

	rec, ok, _ := h.st.Review(replayRef.Key())
	if !ok {
		t.Fatal("the existing review record must survive a refused replay")
	}
	if rec.Outcome != store.OutcomeNeedsAttention || rec.Detail != detail {
		t.Errorf("the record was altered: outcome=%q detail=%q", rec.Outcome, rec.Detail)
	}
	if _, pending, _ := h.st.Pending(replayRef.Key()); pending {
		t.Error("a refused replay must not queue the ref for the automatic path")
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("nothing may be reviewed, ran %v", h.rev.ran)
	}
}

// A failed replay reports to the caller instead of silently queueing itself
// for a later automatic review.
func TestReviewOneLeavesNoPendingEntryWhenInspectFails(t *testing.T) {
	h := newHarness(t, nil)
	h.prs.err[replayRef.Key()] = errors.New("network unreachable")

	d, err := h.p.ReviewOne(context.Background(), replayRef, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionDefer {
		t.Errorf("Action = %q, want defer with the failure reason", d.Action)
	}
	if _, pending, _ := h.st.Pending(replayRef.Key()); pending {
		t.Error("an explicit replay must never leave a pending entry: candidates() would " +
			"re-offer it on every sweep and review it with no further request")
	}
}

func TestReviewOneLeavesNoPendingEntryWhenTheWorktreeFails(t *testing.T) {
	h := newHarness(t, nil)
	h.wts.err = errors.New("clone failed")

	if _, err := h.p.ReviewOne(context.Background(), replayRef, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, pending, _ := h.st.Pending(replayRef.Key()); pending {
		t.Error("a failed replay must not queue itself for the automatic path")
	}
}
