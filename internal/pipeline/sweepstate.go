package pipeline

import (
	"context"
	"sync"
	"time"

	"github.com/angelov-todor/firstpass/internal/prref"
)

// sweepState is the state that candidates share while a sweep is running.
//
// Every field here was a plain field on SweepReport, written directly by
// handle, which was correct while exactly one candidate was ever in flight.
// Once several are, each of them is either a data race or a lost update, and
// two of them are outward-facing: the review cap decides how much of somebody
// else's pull request gets commented on, and the mid-sweep pause is a kill
// switch.
//
// It lives behind a mutex rather than in atomics because reserveReview has to
// compare and increment as one step, which is exactly what atomics cannot do
// without a loop, and because the cost is irrelevant next to a review.
type sweepState struct {
	mu sync.Mutex

	// maxReviews is the per-sweep cap, copied in at construction so the
	// reservation needs nothing from the config.
	maxReviews int

	reviewAttempts int
	reviewed       int
	recordFailed   bool
	pausedMidSweep bool

	// diffs caches sibling diffs for the life of one sweep; see siblingDiff.
	diffs map[string]cachedDiff
}

// cachedDiff is one sibling's diff, or the failure to fetch it.
type cachedDiff struct {
	diff      string
	truncated bool
	err       error
}

func newSweepState(maxReviews int) *sweepState {
	return &sweepState{maxReviews: maxReviews}
}

// reserveReview claims one of the sweep's review slots, reporting whether a
// slot was free. A caller that reserves and then does not review must
// release.
//
// Comparing and incrementing as one step is the whole point. A separate
// "check the count, then increment it later" -- which is what the serial code
// did, and which reads as equivalent -- lets every worker observe the same
// pre-cap count and proceed: with a cap of 3 and three workers, three of them
// can each see two attempts recorded and all three go on to review, for five
// reviews against a cap of three. The cap exists to bound how many pull
// requests firstpass writes comments on in one sweep, so overshooting it is
// not a cosmetic accounting error.
func (s *sweepState) reserveReview() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reviewAttempts >= s.maxReviews {
		return false
	}
	s.reviewAttempts++
	return true
}

// releaseReview hands a reserved slot back, for a candidate that reserved one
// and then bailed before starting its review.
//
// The slot is reserved before the worktree is prepared rather than immediately
// before the review, so that a cap decision costs no clone. The gap between
// the two is small but real -- a pause taking effect, a store write failing --
// and a slot leaked there would shrink the cap for the rest of the sweep.
func (s *sweepState) releaseReview() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reviewAttempts > 0 {
		s.reviewAttempts--
	}
}

// capReached is an advisory read used to skip work that would be wasted. It is
// not authoritative -- only reserveReview is -- because anything learned
// without holding the lock across the decision can be stale by the time it is
// acted on.
func (s *sweepState) capReached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reviewAttempts >= s.maxReviews
}

func (s *sweepState) noteReviewed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reviewed++
}

func (s *sweepState) reviewedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reviewed
}

// noteRecordFailed records that a store write failed somewhere in this sweep,
// which holds the watermark so the batch is offered again.
func (s *sweepState) noteRecordFailed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordFailed = true
}

func (s *sweepState) recordDidFail() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordFailed
}

// pauseMidSweep latches the kill switch for the rest of this sweep. Reviews
// already running are not stopped -- claude is mid-flight and its comments may
// be half posted -- but nothing further starts.
func (s *sweepState) pauseMidSweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pausedMidSweep = true
}

func (s *sweepState) pausedMid() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pausedMidSweep
}

// siblingDiff fetches a pull request's diff at most once per sweep.
//
// A three-way post produces three reviews, each wanting the other two diffs:
// six fetches of three distinct diffs without a cache. GitHub calls are the
// resource this project has least of -- rate limits are the ceiling on
// concurrency, not the machine -- so halving them is worth a map.
//
// Cached per sweep rather than for longer. A pull request's diff changes when
// somebody pushes, and a sweep is short enough that treating it as fixed
// within one is safe while treating it as fixed across a day is not.
//
// Failures are cached too. A sibling in a forbidden state fails the same way
// for every member of the group, and retrying it twice more only spends the
// budget again to learn the same thing.
func (s *sweepState) siblingDiff(ctx context.Context, ref prref.PRRef,
	fetch func(context.Context) (string, bool, error), timeout time.Duration) (string, bool, error) {

	key := ref.Key()
	s.mu.Lock()
	if s.diffs == nil {
		s.diffs = map[string]cachedDiff{}
	}
	if c, ok := s.diffs[key]; ok {
		s.mu.Unlock()
		return c.diff, c.truncated, c.err
	}
	s.mu.Unlock()

	// Fetched outside the lock: this is a network call, and holding the
	// sweep's lock across it would serialise every concurrent review's
	// bookkeeping behind it. Two reviews racing on the same sibling fetch it
	// twice, which is the cost of not holding a lock across a subprocess and
	// is bounded by the group size.
	fctx, cancel := context.WithTimeout(ctx, timeout)
	diff, truncated, err := fetch(fctx)
	cancel()

	s.mu.Lock()
	s.diffs[key] = cachedDiff{diff: diff, truncated: truncated, err: err}
	s.mu.Unlock()
	return diff, truncated, err
}
