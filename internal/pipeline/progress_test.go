package pipeline

import (
	"context"
	"testing"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/store"
)

// TestSweepEmitsProgressInOrderForOneReview covers the happy path an
// operator actually hits: one PR posted, nothing recovered, one review that
// succeeds. The stage sequence is the contract a CLI renderer builds on.
func TestSweepEmitsProgressInOrderForOneReview(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)

	var got []Stage
	h.p.Progress = func(ev Event) { got = append(got, ev.Stage) }

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 1 {
		t.Fatalf("Reviewed = %d, want 1", rep.Reviewed)
	}

	want := []Stage{
		StageMessagesFetched,
		StageCandidates,
		StageInspecting,
		StagePreparingWorktree,
		StageReviewStarted,
		StageReviewFinished,
		StageSweepFinished,
	}
	if len(got) != len(want) {
		t.Fatalf("stages = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stage[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestNilProgressHookChangesNothing is the explicit "don't break it"
// guarantee: a Pipeline with Progress left unset must sweep exactly as it
// did before progress reporting existed. newHarness never sets Progress, so
// this exercises the same nil path as every other pipeline test -- this test
// just says so out loud.
func TestNilProgressHookChangesNothing(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)

	if h.p.Progress != nil {
		t.Fatal("the harness must not set Progress by default")
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 1 {
		t.Fatalf("Reviewed = %d, want 1 with a nil Progress hook", rep.Reviewed)
	}
}

// TestProgressPerCandidateIndexAndTotal checks that Index/Total reflect each
// candidate's position in the whole sweep, not merely a count among the
// candidates that happen to reach a given stage. The first PR is skipped
// before Inspect (owner not allowed), so it emits no per-candidate stage at
// all, but the second and third must still carry indexes 2 and 3 of 3 -- not
// 1 and 2 -- or a renderer's "[N/total]" line would silently relabel the
// backlog every time an early candidate drops out.
func TestProgressPerCandidateIndexAndTotal(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m1",
			"look at this https://github.com/torvalds/linux/pull/999 "+
				prURL("aex-balances", 12)+" "+prURL("aex-margin-service", 34)),
	})
	h.seedWatermark(t)

	type seen struct {
		stage Stage
		idx   int
		total int
	}
	var got []seen
	h.p.Progress = func(ev Event) {
		if ev.Stage == StageInspecting {
			got = append(got, seen{ev.Stage, ev.Index, ev.Total})
		}
	}

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}

	// Candidates are walked oldest-first within a message, so within one
	// message they appear in the order they were written: linux#999 (skipped
	// before Inspect), aex-balances#12, aex-margin-service#34.
	want := []seen{
		{StageInspecting, 2, 3},
		{StageInspecting, 3, 3},
	}
	if len(got) != len(want) {
		t.Fatalf("inspecting events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestRecoverInFlightEmitsProgressOnlyWhenItConverts: recoverInFlight must
// not fire StageRecovered when there is nothing to recover -- a quiet sweep
// must not claim it did recovery work.
func TestRecoverInFlightEmitsProgressOnlyWhenItConverts(t *testing.T) {
	h := newHarness(t, nil)
	h.seedWatermark(t)

	var stages []Stage
	h.p.Progress = func(ev Event) { stages = append(stages, ev.Stage) }

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	for _, s := range stages {
		if s == StageRecovered {
			t.Fatalf("StageRecovered fired with nothing to recover: %v", stages)
		}
	}
}

// TestRecoverInFlightEmitsProgressWhenItConverts is the positive half: a
// dead run's in_flight record must produce exactly one StageRecovered event
// naming how many were converted.
func TestRecoverInFlightEmitsProgressWhenItConverts(t *testing.T) {
	h := newHarness(t, nil)
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"
	if err := h.st.PutReview(store.Review{Key: key, Outcome: store.OutcomeInFlight}); err != nil {
		t.Fatal(err)
	}

	var recovered []Event
	h.p.Progress = func(ev Event) {
		if ev.Stage == StageRecovered {
			recovered = append(recovered, ev)
		}
	}

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 {
		t.Fatalf("StageRecovered events = %d, want 1 (%+v)", len(recovered), recovered)
	}
	if recovered[0].Total != 1 {
		t.Errorf("Total = %d, want 1", recovered[0].Total)
	}
}

// TestReviewOneEmitsProgress makes sure a replay -- which never calls Sweep
// -- still drives the same per-candidate stages, since the CLI's `replay`
// command wires the same renderer.
func TestReviewOneEmitsProgress(t *testing.T) {
	h := newHarness(t, nil)
	ref := prref.PRRef{Owner: "example-org", Repo: "aex-balances", Number: 12}
	h.prs.info[ref.Key()] = ghpr.PRInfo{State: "OPEN", Author: "colleague", HeadSHA: "sha"}

	var got []Stage
	h.p.Progress = func(ev Event) { got = append(got, ev.Stage) }

	d, err := h.p.ReviewOne(context.Background(), ref, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionReview {
		t.Fatalf("Action = %q, want review (decisions: %+v)", d.Action, d)
	}

	want := []Stage{StageInspecting, StagePreparingWorktree, StageReviewStarted, StageReviewFinished}
	if len(got) != len(want) {
		t.Fatalf("stages = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stage[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
