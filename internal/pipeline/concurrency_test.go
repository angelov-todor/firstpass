package pipeline

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/prref"
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

// TestReviewsRunConcurrentlyWhenConfigured proves the workers genuinely
// overlap, rather than that the code merely compiles with a semaphore in it.
//
// Each review blocks until all three have started. Serially the first one
// waits for siblings that cannot start until it returns, so the barrier times
// out -- which is the failure this test is here to report.
func TestReviewsRunConcurrentlyWhenConfigured(t *testing.T) {
	const n = 3
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("a", 1)+" "+prURL("b", 2)+" "+prURL("c", 3)),
	})
	h.seedWatermark(t)
	h.cfg.MaxReviewsPerSweep = n
	h.cfg.ReviewConcurrency = n
	h.apply()

	var mu sync.Mutex
	started, timedOut := 0, 0
	release := make(chan struct{})
	var once sync.Once

	h.rev.duringRun = func(prref.PRRef) {
		mu.Lock()
		started++
		all := started == n
		mu.Unlock()
		if all {
			once.Do(func() { close(release) })
		}
		select {
		case <-release:
		case <-time.After(2 * time.Second):
			mu.Lock()
			timedOut++
			mu.Unlock()
		}
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != n {
		t.Fatalf("Reviewed = %d, want %d", rep.Reviewed, n)
	}
	mu.Lock()
	defer mu.Unlock()
	if timedOut != 0 {
		t.Errorf("%d of %d reviews waited out the barrier: they did not run at the same time", timedOut, n)
	}
}

// TestDecisionsKeepCandidateOrderWhateverTheCompletionOrder pins the report's
// determinism. Appending each decision as its review finished would be the
// obvious way to collect them and would make the report's row order depend on
// which review happened to end first -- so two runs of the same sweep could
// not be diffed, and every test that reads Decisions positionally would become
// timing-dependent.
//
// The reviews here finish in exactly the reverse of candidate order.
func TestDecisionsKeepCandidateOrderWhateverTheCompletionOrder(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("a", 1)+" "+prURL("b", 2)+" "+prURL("c", 3)),
	})
	h.seedWatermark(t)
	h.cfg.MaxReviewsPerSweep = 3
	h.cfg.ReviewConcurrency = 3
	h.apply()

	delays := map[string]time.Duration{
		"example-org/a#1": 120 * time.Millisecond,
		"example-org/b#2": 60 * time.Millisecond,
		"example-org/c#3": 0,
	}
	var mu sync.Mutex
	var finished []string
	h.rev.duringRun = func(ref prref.PRRef) {
		time.Sleep(delays[ref.Key()])
		mu.Lock()
		finished = append(finished, ref.Key())
		mu.Unlock()
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, d := range rep.Decisions {
		got = append(got, d.Ref.Key())
	}
	want := []string{"example-org/a#1", "example-org/b#2", "example-org/c#3"}
	if !slices.Equal(got, want) {
		t.Errorf("Decisions order = %v, want candidate order %v", got, want)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(finished) == 3 && finished[0] != "example-org/c#3" {
		t.Logf("completion order was %v; the assertion above is only meaningful "+
			"when it differs from candidate order", finished)
	}
}

// TestTheCapHoldsWhenReviewsRunConcurrently is the end-to-end form of
// TestReserveReviewNeverExceedsTheCap: the cap is what bounds how many of
// other people's pull requests firstpass comments on in one sweep, and three
// workers racing for the last slot is exactly the situation the serial
// check-then-increment got wrong.
func TestTheCapHoldsWhenReviewsRunConcurrently(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m1",
			prURL("a", 1)+" "+prURL("b", 2)+" "+prURL("c", 3)+" "+prURL("d", 4)+" "+prURL("e", 5)),
	})
	h.seedWatermark(t)
	h.cfg.MaxReviewsPerSweep = 3
	h.cfg.ReviewConcurrency = 3
	h.apply()

	// Hold every review open briefly, so the workers are genuinely in flight
	// together when the later candidates ask for a slot.
	h.rev.duringRun = func(prref.PRRef) { time.Sleep(30 * time.Millisecond) }

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 3 {
		t.Errorf("Reviewed = %d, want 3 (the cap)", rep.Reviewed)
	}

	h.rev.mu.Lock()
	ran := len(h.rev.ran)
	h.rev.mu.Unlock()
	if ran != 3 {
		t.Errorf("Rev.Run fired %d times against a cap of 3", ran)
	}

	all, err := h.st.AllPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("AllPending() = %d, want the 2 over the cap", len(all))
	}
	for _, pd := range all {
		if pd.Attempts != 0 {
			t.Errorf("%s: Attempts = %d; hitting the cap is not a failure", pd.Key, pd.Attempts)
		}
	}
}
