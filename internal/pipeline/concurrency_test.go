package pipeline

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/angelov-todor/firstpass/internal/chat"
)

// TestReserveReviewNeverExceedsTheCap is the property the serial code got for
// free and concurrency takes away.
//
// The old shape was "read the attempt count, and increment it much later, just
// before the review". Read as a pair of statements that is equivalent to a
// reservation; run concurrently it is not, because every worker can observe
// the same pre-cap count and each go on to review. The cap bounds how many of
// other people's pull requests firstpass writes comments on in one sweep, so
// exceeding it is not an accounting slip.
func TestReserveReviewNeverExceedsTheCap(t *testing.T) {
	const cap, workers = 3, 24
	st := newSweepState(cap)

	var granted int
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // pile every worker onto the same instant
			if st.reserveReview() {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if granted != cap {
		t.Errorf("%d of %d workers were granted a slot against a cap of %d", granted, workers, cap)
	}
}

func TestAReleasedSlotCanBeClaimedAgain(t *testing.T) {
	st := newSweepState(1)
	if !st.reserveReview() {
		t.Fatal("the first reservation must succeed")
	}
	if st.reserveReview() {
		t.Fatal("the cap is 1, so a second reservation must fail")
	}
	st.releaseReview()
	if !st.reserveReview() {
		t.Error("a released slot must be claimable again")
	}
}

// TestReleaseCannotGoBelowZero guards the accounting against a double release,
// which would inflate the cap rather than shrink it -- the direction that
// posts comments nobody asked for.
func TestReleaseCannotGoBelowZero(t *testing.T) {
	st := newSweepState(1)
	st.releaseReview()
	st.releaseReview()
	if !st.reserveReview() {
		t.Fatal("the first reservation must succeed")
	}
	if st.reserveReview() {
		t.Error("releases below zero must not raise the cap")
	}
}

// TestACandidateThatBailsAfterReservingDoesNotConsumeTheCap is the wiring
// test, as opposed to the state-machine tests above: it drives a whole sweep
// and proves the release is actually reached on a real bail path.
//
// The slot is claimed before the clone, so that a candidate turned away by the
// cap costs no clone -- which means every path between the reservation and the
// review has to hand the slot back. A clone that fails is the most likely of
// them, and without the release a cap of one plus one failed clone would mean
// nothing else was reviewed for the rest of the sweep.
func TestACandidateThatBailsAfterReservingDoesNotConsumeTheCap(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("a", 1)+" "+prURL("b", 2)),
	})
	h.seedWatermark(t)
	h.cfg.MaxReviewsPerSweep = 1
	h.apply()

	// The first candidate reserves the sweep's only slot and then fails its
	// clone.
	h.wts.errFor = map[string]error{"example-org/a#1": errors.New("clone failed")}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}

	if d, ok := decisionFor(rep, "example-org/a#1"); !ok || d.Action != ActionDefer {
		t.Fatalf("the failed clone should defer, got %+v", d)
	}
	d, ok := decisionFor(rep, "example-org/b#2")
	if !ok {
		t.Fatal("no decision for the second pull request")
	}
	if d.Action != ActionReview {
		t.Errorf("Action = %q (%s), want review: the failed clone released its slot, "+
			"so the cap of 1 was still unspent", d.Action, d.Reason)
	}
	if rep.Reviewed != 1 {
		t.Errorf("Reviewed = %d, want 1", rep.Reviewed)
	}
}
