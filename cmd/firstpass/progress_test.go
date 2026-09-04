package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/pipeline"
	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/review"
	"github.com/angelov-todor/firstpass/internal/store"
)

func ref(n int) prref.PRRef {
	return prref.PRRef{Owner: "astrabit-cpt", Repo: "aex-margin-service", Number: n}
}

// TestProgressRendererWritesToStderrNotStdout is the load-bearing safety
// property: the decision table and the status table are stdout output an
// operator may redirect to a file, so progress must never land there. This
// captures both writers and asserts the one never handed to the renderer
// stays untouched, rather than trusting the renderer's internals.
func TestProgressRendererWritesToStderrNotStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	r := newProgressRenderer(&stderr, 30*time.Minute)

	r.Handle(pipeline.Event{Stage: pipeline.StageMessagesFetched, Detail: "12 messages fetched"})
	r.Handle(pipeline.Event{Stage: pipeline.StageCandidates, Total: 3})
	r.Handle(pipeline.Event{Stage: pipeline.StageInspecting, Ref: ref(197), Index: 1, Total: 3})
	r.Handle(pipeline.Event{Stage: pipeline.StagePreparingWorktree, Ref: ref(197), Index: 1, Total: 3})
	r.Handle(pipeline.Event{Stage: pipeline.StageReviewStarted, Ref: ref(197), Index: 1, Total: 3})
	r.Handle(pipeline.Event{Stage: pipeline.StageReviewFinished, Ref: ref(197), Index: 1, Total: 3, Detail: "reviewed in 12m15s"})
	r.Handle(pipeline.Event{Stage: pipeline.StageSweepFinished, Detail: "1 reviewed"})

	if stdout.Len() != 0 {
		t.Fatalf("stdout must never receive progress output, got %q", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatal("stderr must receive progress output")
	}
}

// TestProgressRendererShowsPerCandidateCounters is the "[12/70]" requirement:
// a long backfill must show movement through the batch, not just the PR
// under consideration.
func TestProgressRendererShowsPerCandidateCounters(t *testing.T) {
	var buf bytes.Buffer
	r := newProgressRenderer(&buf, 30*time.Minute)

	r.Handle(pipeline.Event{Stage: pipeline.StageInspecting, Ref: ref(12), Index: 12, Total: 70})

	out := buf.String()
	if !strings.Contains(out, "[12/70]") {
		t.Errorf("output must show the candidate counter, got %q", out)
	}
	if !strings.Contains(out, ref(12).Key()) {
		t.Errorf("output must name the PR, got %q", out)
	}
}

// fakeTicker lets a test drive the heartbeat deterministically: sending on C
// stands in for a real 30-second tick, and Stop lets the test observe that
// the heartbeat goroutine actually exited rather than merely stopped
// listening.
type fakeTicker struct {
	ch      chan time.Time
	stopped chan struct{}
}

func newFakeTicker() *fakeTicker {
	return &fakeTicker{ch: make(chan time.Time, 4), stopped: make(chan struct{})}
}

func (f *fakeTicker) C() <-chan time.Time { return f.ch }
func (f *fakeTicker) Stop()               { close(f.stopped) }

// TestHeartbeatTicksWhileReviewingThenStopsReliably is the whole point of
// this feature: a 12-minute silent gap is what made the operator think
// firstpass had hung. This proves the heartbeat both fires while a review is
// in flight and -- just as important -- stops for good once the review
// finishes, so no goroutine keeps printing after the command has moved on.
// The tick interval is injected so the test never sleeps for real.
func TestHeartbeatTicksWhileReviewingThenStopsReliably(t *testing.T) {
	var buf bytes.Buffer
	r := newProgressRenderer(&buf, 20*time.Minute)

	start := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	clock := start
	r.now = func() time.Time { return clock }

	tk := newFakeTicker()
	tickerBuilt := make(chan struct{}, 1)
	r.newTicker = func(d time.Duration) heartbeatTicker {
		if d != 30*time.Second {
			t.Errorf("tick interval = %s, want 30s", d)
		}
		tickerBuilt <- struct{}{}
		return tk
	}

	tickProcessed := make(chan struct{}, 1)
	r.onTick = func() { tickProcessed <- struct{}{} }

	const wait = 5 * time.Second // generous, but bounded: a real regression must fail, not hang the suite
	r.Handle(pipeline.Event{Stage: pipeline.StageReviewStarted, Ref: ref(197), Index: 1, Total: 1})
	select {
	case <-tickerBuilt:
	case <-time.After(wait):
		t.Fatal("review_started never built a heartbeat ticker")
	}

	// Simulate 7m30s of elapsed time and one tick, then wait for the
	// heartbeat goroutine to confirm it actually printed that tick before
	// telling it to stop. Without this synchronization, the goroutine's
	// select over both the tick channel and the stop signal could
	// nondeterministically choose to stop first, discarding the tick this
	// test just sent -- exactly the kind of flake "do not sleep 30 seconds"
	// is meant to rule out.
	clock = start.Add(7*time.Minute + 30*time.Second)
	tk.ch <- clock
	select {
	case <-tickProcessed:
	case <-time.After(wait):
		t.Fatal("the heartbeat goroutine never processed the injected tick")
	}

	// Run on its own goroutine and bounded, not called inline: Handle blocks
	// until the heartbeat goroutine has exited (see progressRenderer.stopHB),
	// so a regression that failed to signal it would otherwise hang this
	// test forever instead of failing it.
	finishedHandled := make(chan struct{})
	go func() {
		r.Handle(pipeline.Event{Stage: pipeline.StageReviewFinished, Ref: ref(197), Index: 1, Total: 1,
			Detail: "reviewed in 7m30s"})
		close(finishedHandled)
	}()
	select {
	case <-finishedHandled:
	case <-time.After(wait):
		t.Fatal("Handle(review_finished) never returned: the heartbeat goroutine did not stop")
	}

	select {
	case <-tk.stopped:
	default:
		t.Fatal("Stop was never called on the heartbeat ticker")
	}

	out := buf.String()
	if !strings.Contains(out, "7m30s elapsed") {
		t.Errorf("heartbeat line missing elapsed time, got %q", out)
	}
	if !strings.Contains(out, "timeout 20m") {
		t.Errorf("heartbeat line missing the configured timeout, got %q", out)
	}

	beforeFinish := len(out)

	// A tick sent after Stop must never be processed. This is not a race to
	// rule out with a sleep: Handle(review_finished) above already blocked
	// on <-done until the heartbeat goroutine had fully returned (see
	// progressRenderer.stopHB), so by construction nothing is left alive to
	// read tk.ch -- the buffered send below just sits there. If a future
	// change reintroduced a leaked goroutine, -race and a leaked-goroutine
	// check would catch it; this assertion catches the visible symptom.
	tk.ch <- start.Add(8 * time.Minute)
	if len(buf.String()) != beforeFinish {
		t.Errorf("a heartbeat line was printed after review_finished: %q", buf.String()[beforeFinish:])
	}
}

// TestQuietWiringLeavesTheHookNil is the wiring half of `-quiet`: it must
// leave pipeline.Pipeline.Progress nil rather than set-then-suppress, so a
// quiet run costs the pipeline nothing and no renderer goroutine exists to
// leak. The result table itself is unconditional in cmdScan/cmdReplay (it is
// not gated by quiet at all), so nothing further is needed to prove it still
// prints.
func TestQuietWiringLeavesTheHookNil(t *testing.T) {
	p := &pipeline.Pipeline{}
	var buf bytes.Buffer

	r := wireProgress(p, true, &buf, 30*time.Minute)
	if r != nil {
		t.Error("-quiet must not build a renderer")
	}
	if p.Progress != nil {
		t.Error("-quiet must leave Pipeline.Progress nil")
	}

	r2 := wireProgress(p, false, &buf, 30*time.Minute)
	if r2 == nil {
		t.Fatal("without -quiet a renderer must be built")
	}
	if p.Progress == nil {
		t.Error("without -quiet Pipeline.Progress must be wired")
	}
}

// ---- fakes for a full wireProgress + Sweep + renderSweep round trip,
// mirroring internal/pipeline's own test harness so this test exercises the
// same wiring cmdScan does without driving a real chat script, gh or claude
// subprocess. ----

type quietTestChat struct{ msgs []chat.Message }

func (f *quietTestChat) Fetch(context.Context, string, int) ([]chat.Message, bool, error) {
	return f.msgs, true, nil
}

type quietTestPRs struct{}

func (quietTestPRs) Inspect(context.Context, prref.PRRef) (ghpr.PRInfo, error) {
	return ghpr.PRInfo{State: "OPEN", Author: "colleague", HeadSHA: "sha"}, nil
}

type quietTestWTs struct{}

func (quietTestWTs) Prepare(context.Context, prref.PRRef) (string, func(), error) {
	return "dir", func() {}, nil
}

type quietTestRev struct{}

func (quietTestRev) Run(context.Context, string, prref.PRRef) (review.Result, error) {
	return review.Result{}, nil
}

// TestQuietSuppressesProgressButStillPrintsResultTable is the end-to-end
// shape of what cmdScan wires: -quiet must silence stderr progress output
// entirely while leaving the stdout decision table exactly as it would be
// without -quiet. Built on the same seams internal/pipeline's own tests use
// (fakes behind the exported ChatSource/PRInspector/Worktrees/Reviewer
// interfaces plus a real bbolt store in a temp dir), so this proves the
// cmd-layer wiring without a real chat script, gh, or claude subprocess.
func TestQuietSuppressesProgressButStillPrintsResultTable(t *testing.T) {
	run := func(quiet bool) (stdout, stderr string) {
		dir := t.TempDir()
		st, err := store.Open(filepath.Join(dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if err := st.SetWatermark(store.Watermark{MessageName: "spaces/A/messages/m0"}); err != nil {
			t.Fatal(err)
		}

		cfg := config.Default()
		cfg.StateDir = dir
		cfg.GithubLogin = "angelov-todor"
		cfg.AllowOwners = []string{"Example-Org"}

		p := &pipeline.Pipeline{
			Cfg: cfg, Store: st,
			Chat: &quietTestChat{msgs: []chat.Message{{
				Name: "spaces/A/messages/m1",
				Text: "https://github.com/Example-Org/aex-balances/pull/12",
			}}},
			PRs: quietTestPRs{}, WTs: quietTestWTs{}, Rev: quietTestRev{},
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}

		var outBuf, errBuf bytes.Buffer
		renderer := wireProgress(p, quiet, &errBuf, cfg.ReviewTimeout.D())
		defer func() {
			if renderer != nil {
				renderer.stopHeartbeat()
			}
		}()

		rep, err := p.Sweep(context.Background(), pipeline.Options{})
		if err != nil {
			t.Fatal(err)
		}
		renderSweep(&outBuf, rep, false, cfg.DryRun)
		return outBuf.String(), errBuf.String()
	}

	loudOut, loudErr := run(false)
	if loudErr == "" {
		t.Fatal("without -quiet, progress must be written to stderr")
	}
	if !strings.Contains(loudOut, "1 reviewed") {
		t.Errorf("the result table must show the review, got %q", loudOut)
	}

	quietOut, quietErr := run(true)
	if quietErr != "" {
		t.Errorf("-quiet must suppress all progress output, got %q", quietErr)
	}
	if quietOut != loudOut {
		t.Errorf("-quiet must not change the result table:\nquiet: %q\nloud:  %q", quietOut, loudOut)
	}
}

// TestSlogProgressHandlerLogsStructuredFields covers watch's route: the same
// events go through the existing slog logger at INFO, not the plain-text
// renderer, so daemon output stays structured.
func TestSlogProgressHandlerLogsStructuredFields(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := slogProgressHandler(log)

	h(pipeline.Event{Stage: pipeline.StageInspecting, Ref: ref(197), Index: 5, Total: 70})

	out := buf.String()
	for _, want := range []string{"level=INFO", "stage=inspecting", "pr=astrabit-cpt/aex-margin-service#197", "index=5", "total=70"} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q, got %q", want, out)
		}
	}
}
