package main

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
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
// Reviews can run concurrently (review_concurrency), so several are in flight
// at once and their events interleave: a heartbeat for one review prints while
// Handle is reporting a stage of another. That makes this renderer shared
// mutable state on two counts, and both are guarded here rather than left to
// the caller.
//
//   - heartbeats: one per review, keyed by pull request, so starting and
//     stopping them cannot collide. The previous single field could not
//     represent two at once -- a second review_started overwrote the first
//     review's stop func, leaking its goroutine to print over the second's
//     output for the rest of the run.
//   - wmu: writes to w. The pipeline serialises its calls into Handle, but a
//     heartbeat goroutine is not one of those calls and writes on its own
//     schedule.
//
// The two locks are never held at the same time, and a heartbeat's stop func
// is always called holding neither: stopping waits for that goroutine to
// exit, and the goroutine needs a lock to finish a tick.
type progressRenderer struct {
	w             io.Writer
	reviewTimeout time.Duration
	now           func() time.Time
	newTicker     func(d time.Duration) heartbeatTicker

	// mu guards heartbeats; wmu guards writes to w. Separate, and never
	// nested; see the type comment.
	mu         sync.Mutex
	heartbeats map[string]func() // pull request key -> stop, which waits for its goroutine to exit
	wmu        sync.Mutex

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
		heartbeats:    map[string]func(){},
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
		r.line("%s\n", ev.Detail)
	case pipeline.StageCandidates:
		r.line("%d candidate(s) to consider\n", ev.Total)
	case pipeline.StageInspecting:
		r.line("[%d/%d] %s — inspecting\n", ev.Index, ev.Total, ev.Ref.Key())
	case pipeline.StagePreparingWorktree:
		r.line("[%d/%d] %s — preparing worktree\n", ev.Index, ev.Total, ev.Ref.Key())
	case pipeline.StageReviewStarted:
		r.line("[%d/%d] %s — review started\n", ev.Index, ev.Total, ev.Ref.Key())
		r.startHeartbeat(ev.Ref, ev.Index, ev.Total)
	case pipeline.StageReviewFinished:
		// This review's own heartbeat stops before the line is printed, not
		// after: the alternative ordering can interleave its own tick between
		// this line and the one that follows, which reads like the review kept
		// running after firstpass just said it finished. Other reviews'
		// heartbeats keep ticking, because those reviews are still running.
		r.stopHeartbeatFor(ev.Ref.Key())
		r.line("[%d/%d] %s — %s\n", ev.Index, ev.Total, ev.Ref.Key(), ev.Detail)
	case pipeline.StageSweepFinished:
		r.line("sweep finished: %s\n", ev.Detail)
	}
}

// line is the only writer to w, so a heartbeat tick cannot land in the middle
// of a stage line.
func (r *progressRenderer) line(format string, args ...any) {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	fmt.Fprintf(r.w, format, args...)
}

// startHeartbeat begins printing an updating elapsed-time line for one review
// roughly every 30 seconds -- the whole point of this feature, since a
// 12-minute silent gap is what made an operator conclude firstpass had hung.
//
// Any heartbeat already running for the same pull request is stopped first.
// That is defensive rather than expected, and it is deliberately per pull
// request: with concurrent reviews, other reviews having live heartbeats is
// the normal state of affairs and stopping them here would silence them.
func (r *progressRenderer) startHeartbeat(ref prref.PRRef, idx, total int) {
	r.stopHeartbeatFor(ref.Key())

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
				r.line("[%d/%d] reviewing %s — %s elapsed (timeout %s)\n",
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
	stopFn := func() {
		close(stop)
		<-done
	}
	r.mu.Lock()
	r.heartbeats[ref.Key()] = stopFn
	r.mu.Unlock()
}

// stopHeartbeatFor stops one review's heartbeat and waits for its goroutine to
// exit. A no-op when that review has none, so every exit path can call it
// unconditionally.
//
// The stop func is taken out of the map under the lock and called with the
// lock released. Two things rest on that: only one caller can ever obtain a
// given stop func, so no channel is closed twice -- the panic the old
// single-field version was one concurrent call away from -- and the goroutine
// being waited on can still take the write lock to finish its tick, which
// holding mu across the wait would deadlock against.
func (r *progressRenderer) stopHeartbeatFor(key string) {
	r.mu.Lock()
	stop := r.heartbeats[key]
	delete(r.heartbeats, key)
	r.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// stopHeartbeat stops every running heartbeat. It is the safety net cmdScan
// and cmdReplay defer, for the paths where a sweep ends without a
// review_finished for each review it started -- an interrupt, most obviously,
// and with concurrency there can be several such reviews at once. A goroutine
// that outlives the command it belongs to is worse than the silence this
// feature exists to fix.
func (r *progressRenderer) stopHeartbeat() {
	r.mu.Lock()
	stops := make([]func(), 0, len(r.heartbeats))
	for k, stop := range r.heartbeats {
		stops = append(stops, stop)
		delete(r.heartbeats, k)
	}
	r.mu.Unlock()
	for _, stop := range stops {
		stop()
	}
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
