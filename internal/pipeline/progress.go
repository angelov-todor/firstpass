package pipeline

import (
	"fmt"
	"time"

	"github.com/angelov-todor/firstpass/internal/prref"
)

// Stage names one point in a sweep's progress that Pipeline.Progress, when
// set, is notified at. The pipeline stays terminal-agnostic: it emits these
// as plain data, and never formats, colours, or paces them -- that is the
// CLI's job.
type Stage string

const (
	// StageMessagesFetched fires once per sweep, right after the chat fetch
	// returns. Detail names how many messages came back and whether a
	// watermark gap was detected.
	StageMessagesFetched Stage = "messages_fetched"
	// StageRecovered fires at most once per sweep, and only when
	// recoverInFlight actually converted at least one in_flight record left
	// by a dead run. Total carries how many were converted.
	StageRecovered Stage = "recovered"
	// StageCandidates fires once per sweep, after the candidate list is
	// built. Total carries the candidate count; the per-candidate stages
	// below use it as their Total too, so a renderer can show "[12/70]".
	StageCandidates Stage = "candidates"
	// StageInspecting fires once per candidate that reaches the GitHub
	// lookup -- not every candidate does; several gates (owner not allowed,
	// an existing terminal record, a pause, the per-sweep cap) return before
	// it. Ref, Index and Total are set.
	StageInspecting Stage = "inspecting"
	// StagePreparingWorktree fires once per candidate that clears every gate
	// up to and including the GitHub inspection and is about to get a
	// worktree. Ref, Index and Total are set.
	StagePreparingWorktree Stage = "preparing_worktree"
	// StageReviewStarted fires immediately before claude is invoked -- the
	// point a long, silent wait can begin. Ref, Index and Total are set.
	StageReviewStarted Stage = "review_started"
	// StageReviewFinished fires once claude has returned, on every outcome
	// (success, failure, or timeout). Detail names the outcome and how long
	// the review took. Ref, Index and Total are set.
	StageReviewFinished Stage = "review_finished"
	// StageSweepFinished fires once, at the end of a sweep that ran to
	// completion (including a cold start or an interrupted sweep). Detail
	// carries a short summary.
	StageSweepFinished Stage = "sweep_finished"
)

// Event carries one point of progress. Ref is the zero prref.PRRef for
// stages that are not about one specific pull request. Index and Total are
// the 1-based position and count for the per-candidate stages; both are zero
// where they do not apply. Detail is free text for a human to read.
type Event struct {
	Stage  Stage
	Ref    prref.PRRef
	Index  int
	Total  int
	Detail string
}

// progress delivers one event to the hook, one caller at a time. Every call
// site in this package goes through it, so a nil Progress is free and cannot
// panic.
//
// Reviews run concurrently, so this is reached from several goroutines. The
// lock is here rather than in the hook because the hook is supplied by the
// caller: the promise on Pipeline.Progress is that it is never entered twice
// at once, and it is cheaper to keep that promise in one place than to require
// every present and future implementation to be safe for concurrent use.
//
// It does not make the *ordering* guarantee that a serial sweep had. Events
// for different pull requests interleave, and a renderer that assumed
// otherwise had to change; see cmd/firstpass/progressRenderer.
func (p *Pipeline) progress(ev Event) {
	if p.Progress == nil {
		return
	}
	p.progressMu.Lock()
	defer p.progressMu.Unlock()
	p.Progress(ev)
}

// messagesFetchedDetail is the free-text detail for StageMessagesFetched.
func messagesFetchedDetail(n int, gap bool) string {
	if gap {
		return fmt.Sprintf("%d messages fetched (watermark gap: some messages were not scanned)", n)
	}
	return fmt.Sprintf("%d messages fetched", n)
}

// reviewFinishedDetail is the free-text detail for StageReviewFinished.
func reviewFinishedDetail(outcome string, took time.Duration) string {
	return fmt.Sprintf("%s in %s", outcome, took.Round(time.Second))
}
