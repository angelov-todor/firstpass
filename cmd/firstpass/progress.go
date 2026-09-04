package main

import (
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/angelov-todor/firstpass/internal/pipeline"
	"github.com/angelov-todor/firstpass/internal/prref"
)

// heartbeatTickInterval is how often the heartbeat prints an elapsed-time
// line while a review is running. 30s matches the "roughly every 30 seconds"
// requirement: frequent enough that an operator watching a redirected log
// never waits more than half a minute to see firstpass is still alive,
// infrequent enough not to flood the log over a review that can run tens of
// minutes.
const heartbeatTickInterval = 30 * time.Second

// heartbeatTicker abstracts time.Ticker so a test can drive the heartbeat
// deterministically -- sending on C stands in for a real tick -- instead of
// sleeping for 30 seconds.
type heartbeatTicker interface {
	C() <-chan time.Time
	Stop()
}

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }

// progressRenderer renders pipeline.Event values as plain, single-line text
// -- no progress bars, no colour, no ANSI cursor movement, because this runs
// under Task Scheduler and in redirected logs at least as often as in a
// terminal. It writes to whatever io.Writer it is given; cmdScan and
// cmdReplay are what decide that this is always os.Stderr, never os.Stdout,
// since the decision table and status table are stdout output an operator may
// redirect to a file. (cmdWatch builds no renderer at all: it routes events
// through slogProgressHandler instead.)
//
// A renderer is NOT safe for concurrent use, and neither is Handle. The
// heartbeat is guarded by nothing: two overlapping stopHeartbeat calls would
// both close(stop) and panic, and two overlapping startHeartbeat calls would
// leak the first goroutine, which then prints over the second. It is safe
// today only because the pipeline is strictly serial -- one sweep, one review
// at a time -- and emits no other event between review_started and
// review_finished, so Handle is only ever entered from one goroutine and the
// heartbeat's lifetime never overlaps another's. Whoever parallelises reviews
// must give this a mutex (or a renderer per worker) in the same change; see
// the note on pipeline.Pipeline.Progress.
type progressRenderer struct {
	w             io.Writer
	reviewTimeout time.Duration
	now           func() time.Time
	newTicker     func(d time.Duration) heartbeatTicker

	stopHB func() // stops the running heartbeat and waits for its goroutine to exit; nil when none is running

	// onTick, when set, is called synchronously after each heartbeat line is
	// printed. It exists only so a test can wait for a tick to have been
	// fully processed before it triggers a stop -- otherwise a select over
	// both the tick channel and the stop signal could nondeterministically
	// choose to stop first, making the test flaky. Production code never
	// sets it.
	onTick func()
}

// newProgressRenderer builds a renderer that writes to w and knows the
// configured review_timeout, so its heartbeat line can show it alongside the
// elapsed time.
func newProgressRenderer(w io.Writer, reviewTimeout time.Duration) *progressRenderer {
	return &progressRenderer{
		w:             w,
		reviewTimeout: reviewTimeout,
		now:           time.Now,
		newTicker: func(d time.Duration) heartbeatTicker {
			return realTicker{time.NewTicker(d)}
		},
	}
}

// Handle is the pipeline.Progress hook. It never blocks the pipeline: every
// case is a single formatted write, except review_started, which only
// launches the heartbeat goroutine and returns immediately.
func (r *progressRenderer) Handle(ev pipeline.Event) {
	switch ev.Stage {
	case pipeline.StageMessagesFetched, pipeline.StageRecovered:
		fmt.Fprintln(r.w, ev.Detail)
	case pipeline.StageCandidates:
		fmt.Fprintf(r.w, "%d candidate(s) to consider\n", ev.Total)
	case pipeline.StageInspecting:
		fmt.Fprintf(r.w, "[%d/%d] %s — inspecting\n", ev.Index, ev.Total, ev.Ref.Key())
	case pipeline.StagePreparingWorktree:
		fmt.Fprintf(r.w, "[%d/%d] %s — preparing worktree\n", ev.Index, ev.Total, ev.Ref.Key())
	case pipeline.StageReviewStarted:
		fmt.Fprintf(r.w, "[%d/%d] %s — review started\n", ev.Index, ev.Total, ev.Ref.Key())
		r.startHeartbeat(ev.Ref, ev.Index, ev.Total)
	case pipeline.StageReviewFinished:
		// Stopped before the line is printed, not after: the alternative
		// ordering can interleave a heartbeat tick between this line and the
		// one that follows it, which reads like the review kept running
		// after firstpass just said it finished.
		r.stopHeartbeat()
		fmt.Fprintf(r.w, "[%d/%d] %s — %s\n", ev.Index, ev.Total, ev.Ref.Key(), ev.Detail)
	case pipeline.StageSweepFinished:
		fmt.Fprintf(r.w, "sweep finished: %s\n", ev.Detail)
	}
}

// startHeartbeat begins printing an updating elapsed-time line roughly every
// 30 seconds -- the whole point of this feature, since a 12-minute silent
// gap is what made an operator conclude firstpass had hung. Any previous
// heartbeat is stopped first: defensive, since review_started should always
// be paired with a prior review_finished, but a second heartbeat goroutine
// printing over the first would be worse than useless.
func (r *progressRenderer) startHeartbeat(ref prref.PRRef, idx, total int) {
	r.stopHeartbeat()

	started := r.now()
	tk := r.newTicker(heartbeatTickInterval)
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				tk.Stop()
				return
			case now := <-tk.C():
				elapsed := now.Sub(started).Round(time.Second)
				fmt.Fprintf(r.w, "[%d/%d] reviewing %s — %s elapsed (timeout %s)\n",
					idx, total, ref.Key(), elapsed, r.reviewTimeout)
				if r.onTick != nil {
					r.onTick()
				}
			}
		}
	}()

	// stopHB both signals the goroutine to exit and blocks until it has: the
	// caller (Handle, on review_finished) must not return -- and so must not
	// let the CLI command exit -- while the goroutine could still be about to
	// print. A goroutine that outlives the command it belongs to is worse
	// than the silence this feature exists to fix.
	r.stopHB = func() {
		close(stop)
		<-done
	}
}

// stopHeartbeat is a no-op when no heartbeat is running, so every exit path
// -- review_finished, a deferred safety-net call in cmdScan or cmdReplay, or
// a second call from startHeartbeat's defensive reset -- can call it
// unconditionally. It must be called from one goroutine only: two concurrent
// calls would both close(stop) and panic. See progressRenderer.
func (r *progressRenderer) stopHeartbeat() {
	if r.stopHB == nil {
		return
	}
	stop := r.stopHB
	r.stopHB = nil
	stop()
}

// wireProgress sets up (or deliberately skips) progress rendering for scan
// and replay. Returning nil for quiet, rather than building a renderer and
// suppressing its output, means a quiet run costs the pipeline nothing: no
// Progress hook is even set, and no heartbeat goroutine can ever exist to
// leak.
func wireProgress(p *pipeline.Pipeline, quiet bool, w io.Writer, reviewTimeout time.Duration) *progressRenderer {
	if quiet {
		return nil
	}
	r := newProgressRenderer(w, reviewTimeout)
	p.Progress = r.Handle
	return r
}

// slogProgressHandler routes pipeline.Event values through the daemon's
// existing slog logger at INFO instead of the plain-text renderer, so
// `watch`'s output stays structured and consistent with the lines it already
// emits -- no separate, differently-formatted progress stream to reconcile
// against the log the operator is actually watching.
func slogProgressHandler(log *slog.Logger) func(pipeline.Event) {
	return func(ev pipeline.Event) {
		args := []any{"stage", string(ev.Stage)}
		if ev.Ref != (prref.PRRef{}) {
			args = append(args, "pr", ev.Ref.Key())
		}
		if ev.Total > 0 {
			args = append(args, "index", ev.Index, "total", ev.Total)
		}
		if ev.Detail != "" {
			args = append(args, "detail", ev.Detail)
		}
		log.Info("progress", args...)
	}
}
