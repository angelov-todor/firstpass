package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/review"
	"github.com/angelov-todor/firstpass/internal/store"
)

// ---- fakes ----

type addedReaction struct {
	Message string
	Emoji   string
	Name    string
}

// fakeReactor records every chat reaction the pipeline asks for. log is shared
// with orderedRev, so a test can assert that the 👀 really is added before
// claude starts rather than merely somewhere in the same sweep.
type fakeReactor struct {
	log       *[]string
	added     []addedReaction
	removed   []string
	addErr    error
	removeErr error
	// budgets is the time left on the context of each call, which is how the
	// tests check the chat timeout is applied at the call site.
	budgets []time.Duration
	n       int
}

func (f *fakeReactor) note(ctx context.Context) {
	if dl, ok := ctx.Deadline(); ok {
		f.budgets = append(f.budgets, time.Until(dl))
	} else {
		f.budgets = append(f.budgets, 0)
	}
}

func (f *fakeReactor) AddReaction(ctx context.Context, messageName, emoji string) (string, error) {
	f.note(ctx)
	*f.log = append(*f.log, "add:"+emoji+":"+messageName)
	if f.addErr != nil {
		return "", f.addErr
	}
	f.n++
	name := fmt.Sprintf("%s/reactions/r%d", messageName, f.n)
	f.added = append(f.added, addedReaction{Message: messageName, Emoji: emoji, Name: name})
	return name, nil
}

func (f *fakeReactor) RemoveReaction(ctx context.Context, reactionName string) error {
	f.note(ctx)
	*f.log = append(*f.log, "remove:"+reactionName)
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, reactionName)
	return nil
}

// orderedRev is the harness's reviewer with its invocations interleaved into
// the reactor's log.
type orderedRev struct {
	inner *fakeRev
	log   *[]string
}

func (o *orderedRev) Run(ctx context.Context, dir string, ref prref.PRRef) (review.Result, error) {
	*o.log = append(*o.log, "review:"+ref.Key())
	return o.inner.Run(ctx, dir, ref)
}

// ---- harness ----

// reactHarness is newHarness with a reactor wired and dry run off. Reactions
// are outward-facing, so every test that expects one has to leave dry run
// behind first -- which is the whole of TestDryRunReactsToNothing.
func reactHarness(t *testing.T, msgs []chat.Message) (*harness, *fakeReactor) {
	t.Helper()
	h := newHarness(t, msgs)
	h.cfg.DryRun = false
	h.apply()
	h.seedWatermark(t)

	log := &[]string{}
	rc := &fakeReactor{log: log}
	h.p.React = rc
	h.p.Rev = &orderedRev{inner: h.rev, log: log}
	h.reactLog = log
	return h, rc
}

// reactionCalls is the interleaved log with the reviewer's own entries
// filtered out. A "nothing reacted" assertion has to be made against this and
// not against the whole log: with "review:..." lines in it, an empty-log check
// would be satisfied by a sweep that merely reviewed nothing, which is a
// different claim entirely.
func reactionCalls(h *harness) []string {
	var out []string
	for _, e := range *h.reactLog {
		if !strings.HasPrefix(e, "review:") {
			out = append(out, e)
		}
	}
	return out
}

func emojis(rc *fakeReactor) []string {
	var out []string
	for _, a := range rc.added {
		out = append(out, a.Emoji)
	}
	return out
}

// ---- 👀 on the way in ----

func TestWatchingReactionIsAddedBeforeTheReviewStarts(t *testing.T) {
	h, _ := reactHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-a", 1))})

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 1 {
		t.Fatalf("Reviewed = %d, want 1: %+v", rep.Reviewed, rep.Decisions)
	}

	// The order is the point. A 👀 that lands after claude has finished says
	// nothing useful to the team -- the whole feature is "this one has been
	// picked up".
	want := []string{
		"add:" + EmojiWatching + ":spaces/A/messages/m1",
		"review:example-org/aex-a#1",
		"add:" + EmojiClean + ":spaces/A/messages/m1",
		"remove:spaces/A/messages/m1/reactions/r1",
	}
	if got := *h.reactLog; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("call order:\n got %v\nwant %v", got, want)
	}
}

func TestReactionsUseTheConfiguredChatTimeout(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-a", 1))})
	// Distinct from every other timeout in the default config, so a bound
	// borrowed from the wrong one would show up here.
	h.cfg.ChatTimeout = config.Duration(90 * time.Second)
	h.apply()

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if len(rc.budgets) != 3 {
		t.Fatalf("want three reaction calls, got %d", len(rc.budgets))
	}
	for i, b := range rc.budgets {
		// chat.py drives a network call and can hang. An unattended daemon
		// needs a bound on it at every call site, as Fetch already has.
		if b <= 0 || b > 90*time.Second {
			t.Errorf("reaction call %d had %s of budget, want at most the 90s chat_timeout", i, b)
		}
	}
}

// ---- one message, several pull requests ----

func TestOneMessageWithTwoRefsGetsOneWatchingReaction(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("aex-a", 1)+"\n"+prURL("aex-b", 2)),
	})

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 2 {
		t.Fatalf("Reviewed = %d, want 2: %+v", rep.Reviewed, rep.Decisions)
	}

	// The reaction is per message, not per pull request: two reviews from one
	// post must not put two 👀 on it.
	var watching int
	for _, a := range rc.added {
		if a.Emoji == EmojiWatching {
			watching++
		}
	}
	if watching != 1 {
		t.Errorf("got %d 👀 reactions for one message carrying two PRs, want 1 (%v)", watching, emojis(rc))
	}
	if got := emojis(rc); len(got) != 2 || got[1] != EmojiClean {
		t.Errorf("emojis = %v, want [👀 ✅]", got)
	}
}

func TestResultReactionWaitsForEveryRefTheMessageCarried(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("aex-a", 1)+"\n"+prURL("aex-b", 2)),
	})
	// The second PR is a draft, so it is deferred rather than decided. The
	// message is not finished until it is.
	h.prs.info["example-org/aex-b#2"] = ghpr.PRInfo{State: "OPEN", Author: "colleague", IsDraft: true}

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if got := emojis(rc); len(got) != 1 || got[0] != EmojiWatching {
		t.Fatalf("emojis = %v, want just [👀]: one of the message's PRs is not decided yet", got)
	}
	if len(rc.removed) != 0 {
		t.Errorf("the 👀 must stay while the message is still being worked through, removed %v", rc.removed)
	}

	// The draft is marked ready. It is still in pending, so it is re-offered
	// independent of the watermark -- and its message has already scrolled out
	// of the fetch window, so nothing in this sweep's candidate list carries a
	// trigger.
	delete(h.prs.info, "example-org/aex-b#2")
	h.ch.msgs = nil
	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if got := emojis(rc); len(got) != 2 || got[1] != EmojiClean {
		t.Errorf("emojis = %v, want [👀 ✅] once the last of the message's PRs is done", got)
	}
	if len(rc.removed) != 1 || rc.removed[0] != "spaces/A/messages/m1/reactions/r1" {
		t.Errorf("the 👀 must come off once the result reaction is on, removed %v", rc.removed)
	}
}

// ---- ✅ versus 💬 ----

func TestResultIsCleanOnlyWhenEveryReviewCameOutReviewed(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-a", 1))})

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if got := emojis(rc); len(got) != 2 || got[1] != EmojiClean {
		t.Errorf("emojis = %v, want [👀 ✅] for a message whose only review succeeded", got)
	}
}

func TestResultIsFindingsWhenAReviewNeedsAttention(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-a", 1))})
	h.rev.err = errors.New("claude exit 1")

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := decisionFor(rep, "example-org/aex-a#1"); d.Action != ActionNeedsAttention {
		t.Fatalf("Action = %q, want needs_attention", d.Action)
	}
	if got := emojis(rc); len(got) != 2 || got[1] != EmojiFindings {
		t.Errorf("emojis = %v, want [👀 💬]: a review that did not come out reviewed is not a "+
			"clean bill of health", got)
	}
	if len(rc.removed) != 1 {
		t.Errorf("the 👀 must come off on the findings path too, removed %v", rc.removed)
	}
}

func TestOneNonCleanRefIsEnoughForFindings(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("aex-a", 1)+"\n"+prURL("aex-b", 2)),
	})
	// aex-a#1 reviews cleanly; aex-b#2 is closed, so it is recorded terminal
	// without ever being reviewed. The message as a whole is not clean.
	h.prs.info["example-org/aex-b#2"] = ghpr.PRInfo{State: "CLOSED", Author: "colleague"}

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if got := emojis(rc); len(got) != 2 || got[1] != EmojiFindings {
		t.Errorf("emojis = %v, want [👀 💬]: one PR of the two never got a clean review", got)
	}
}

// ---- safety rule 1: a dry run and print-only react to nothing ----

func TestDryRunReactsToNothing(t *testing.T) {
	h, _ := reactHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-a", 1))})
	h.cfg.DryRun = true
	h.apply()

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	// The review really did run: this is a dry run, not a no-op sweep, so the
	// absence of reactions below is about dry_run and nothing else.
	if rep.Reviewed != 1 {
		t.Fatalf("Reviewed = %d, want 1: %+v", rep.Reviewed, rep.Decisions)
	}
	if got := reactionCalls(h); len(got) != 0 {
		t.Errorf("dry_run must have no outward effect at all, and chat is outward: %v", got)
	}
	if _, ok, err := h.st.Message("spaces/A/messages/m1"); err != nil || ok {
		t.Errorf("a dry run must not even record reaction state (ok=%v err=%v)", ok, err)
	}
}

func TestPrintOnlyReactsToNothing(t *testing.T) {
	h, _ := reactHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-a", 1))})

	rep, err := h.p.Sweep(context.Background(), Options{PrintOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := decisionFor(rep, "example-org/aex-a#1"); d.Action != ActionWouldReview {
		t.Fatalf("Action = %q, want would_review", d.Action)
	}
	if got := reactionCalls(h); len(got) != 0 {
		t.Errorf("print-only writes nothing and posts nothing: %v", got)
	}
	if _, ok, err := h.st.Message("spaces/A/messages/m1"); err != nil || ok {
		t.Errorf("print-only must not record reaction state either (ok=%v err=%v)", ok, err)
	}
}

// ---- safety rule 2: a reaction failure changes nothing about a review ----

func TestReactionFailureLeavesTheReviewAloneAndTheSweepRunning(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("aex-a", 1)),
		msg("spaces/A/messages/m2", prURL("aex-b", 2)),
	})
	rc.addErr = errors.New("chat api 403 PERMISSION_DENIED: reactions scope missing")
	rc.removeErr = rc.addErr

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatalf("a reaction failure must not fail the sweep: %v", err)
	}

	// Both reviews still happened, in full, and were recorded as reviewed.
	if rep.Reviewed != 2 {
		t.Fatalf("Reviewed = %d, want 2: a failed reaction must not stop the next review (%+v)",
			rep.Reviewed, rep.Decisions)
	}
	for _, key := range []string{"example-org/aex-a#1", "example-org/aex-b#2"} {
		d, ok := decisionFor(rep, key)
		if !ok || d.Action != ActionReview {
			t.Errorf("%s: Action = %q, want review", key, d.Action)
		}
		rec, found, err := h.st.Review(key)
		if err != nil || !found {
			t.Fatalf("%s: found=%v err=%v", key, found, err)
		}
		if rec.Outcome != store.OutcomeReviewed {
			t.Errorf("%s: Outcome = %q; a cosmetic reaction failure must never make a review "+
				"needs_attention", key, rec.Outcome)
		}
		if _, parked, _ := h.st.Pending(key); parked {
			t.Errorf("%s must not be deferred because a reaction failed", key)
		}
	}

	// And the watermark still advanced: a reaction is not part of the batch
	// whose recording the watermark waits on.
	wm, hasWM, err := h.st.Watermark()
	if err != nil {
		t.Fatal(err)
	}
	if !hasWM || wm.MessageName != "spaces/A/messages/m1" {
		t.Errorf("Watermark = %+v (has=%v), want the newest message: a failed reaction must not "+
			"hold the sweep back", wm, hasWM)
	}
}

// ---- safety rule 3: no reaction for a PR that was not reviewed ----

func TestMessageWhoseEveryRefIsSkippedNeverGetsAReaction(t *testing.T) {
	h, _ := reactHarness(t, []chat.Message{
		// Four ways to be turned away, none of which runs claude.
		msg("spaces/A/messages/m1", "https://github.com/Outsider-Org/thing/pull/7"),
		msg("spaces/A/messages/m2", prURL("aex-b", 2)),
		msg("spaces/A/messages/m3", prURL("aex-c", 3)),
		msg("spaces/A/messages/m4", prURL("aex-d", 4)),
	})
	h.prs.info["example-org/aex-b#2"] = ghpr.PRInfo{State: "MERGED", Author: "colleague"}
	h.prs.info["example-org/aex-c#3"] = ghpr.PRInfo{State: "OPEN", Author: "angelov-todor"}
	h.prs.info["example-org/aex-d#4"] = ghpr.PRInfo{State: "OPEN", Author: "colleague", IsDraft: true}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 0 || len(h.rev.ran) != 0 {
		t.Fatalf("nothing may have been reviewed here: Reviewed=%d ran=%v", rep.Reviewed, h.rev.ran)
	}
	if got := reactionCalls(h); len(got) != 0 {
		t.Errorf("a message whose every PR was skipped was never picked up, so it gets no reaction "+
			"at all -- not even a result one: %v", got)
	}
}

func TestNoReactionsWhileFirstpassIsPaused(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("aex-a", 1)+"\n"+prURL("aex-b", 2)),
	})
	h.prs.info["example-org/aex-b#2"] = ghpr.PRInfo{State: "OPEN", Author: "colleague", IsDraft: true}

	// First sweep, unpaused: aex-a#1 is reviewed, so the 👀 goes on, and
	// aex-b#2 is a draft, so the message is not finished and gets no result
	// reaction yet.
	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if got := emojis(rc); len(got) != 1 || got[0] != EmojiWatching {
		t.Fatalf("emojis = %v, want just the watching one", got)
	}

	// The last of the message's PRs finishes, but the process dies before it
	// can react -- exactly the state a crash mid-sweep leaves behind: a
	// terminal review record, a 👀 still on the message, and no result.
	// Writing the record straight into the store is that dead process.
	if err := h.st.PutReview(store.Review{
		Key: "example-org/aex-b#2", Outcome: store.OutcomeReviewed,
		DecidedAt: time.Date(2026, 9, 3, 11, 30, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.DeletePending("example-org/aex-b#2"); err != nil {
		t.Fatal(err)
	}
	before := len(reactionCalls(h))

	// Now the operator hits the kill switch. This message is ready for its
	// result reaction and would get one on any unpaused sweep -- which is what
	// makes this a test of the pause rather than of an idle sweep.
	if err := os.WriteFile(h.cfg.PauseFile(), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.ch.msgs = nil
	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Paused {
		t.Fatal("the sweep must have seen the pause file")
	}
	if got := reactionCalls(h); len(got) != before {
		t.Errorf("the kill switch stops everything outward-facing, reactions included: "+
			"%d calls before the pause, %v after", before, got)
	}

	// And it is only deferred, not lost: resume, and the reaction lands.
	// Otherwise this test would be satisfied by a pause that silently dropped
	// the reaction for good.
	if err := os.Remove(h.cfg.PauseFile()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if got := emojis(rc); len(got) != 2 || got[1] != EmojiClean {
		t.Errorf("emojis = %v, want the watching one then the clean one after the resume", got)
	}
}

// ---- safety rule 4: never twice for the same stage ----

func TestASecondSweepOverTheSameMessageDoesNotReactAgain(t *testing.T) {
	h, _ := reactHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-a", 1))})

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	first := append([]string(nil), *h.reactLog...)
	if len(first) != 4 {
		t.Fatalf("the first sweep should have added a 👀, reviewed, added a ✅ and removed the 👀: %v", first)
	}

	// A backfill re-offers the same message, exactly as `scan -backfill N`
	// does, and as a watermark gap does by itself.
	if _, err := h.p.Sweep(context.Background(), Options{Backfill: 10}); err != nil {
		t.Fatal(err)
	}
	if got := *h.reactLog; len(got) != len(first) {
		t.Errorf("the same message must not be reacted to twice for the same stage:\nfirst %v\nthen  %v",
			first, got)
	}

	// The call count alone is too weak: on a re-offered message every ref is
	// already decided, so no review runs and no reaction would be attempted
	// even with the memory of the first one wiped. What has to hold is that
	// re-recording the message *kept* that memory -- otherwise the next thing
	// to run a review for this message (an in_flight recovery, a ref coming
	// back from pending) reacts to it all over again.
	rec, ok, err := h.st.Message("spaces/A/messages/m1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !rec.WatchApplied || !rec.ResultApplied {
		t.Errorf("re-recording a message must not forget that it was already reacted to: %+v", rec)
	}
	if rec.ResultReaction == "" {
		t.Errorf("the result reaction name must survive re-recording: %+v", rec)
	}
}

// ---- safety rule 5: reaction state is strictly additional ----

func TestRecordingAReactionCannotLoseAReviewRecord(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("aex-a", 1)+"\n"+prURL("aex-b", 2)),
	})

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if len(rc.added) != 2 {
		t.Fatalf("added = %v, want a 👀 and a result", emojis(rc))
	}

	// Every review record written during the sweep is still exactly what the
	// review left: the reaction writes go to their own bucket, under their own
	// key, and never re-put a Review.
	for _, key := range []string{"example-org/aex-a#1", "example-org/aex-b#2"} {
		rec, ok, err := h.st.Review(key)
		if err != nil || !ok {
			t.Fatalf("%s: the review record must survive the reaction writes (ok=%v err=%v)", key, ok, err)
		}
		if rec.Outcome != store.OutcomeReviewed {
			t.Errorf("%s: Outcome = %q, want reviewed", key, rec.Outcome)
		}
		if rec.TriggerMessage != "spaces/A/messages/m1" {
			t.Errorf("%s: TriggerMessage = %q", key, rec.TriggerMessage)
		}
	}
}

// ---- refs with no trigger message ----

func TestARefFromPendingWithNoTriggerMessageDoesNotReact(t *testing.T) {
	// No messages at all this sweep: the only candidate comes out of the
	// pending bucket, and candidates() gives those no trigger.
	h, _ := reactHarness(t, nil)
	if err := h.st.PutPending(store.Pending{
		Key: "example-org/aex-a#1", FirstSeen: time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	// And a message record that does mention this ref, recorded by an earlier
	// sweep and never reacted to. Without it the test proves nothing: there
	// would be no message for a wrong implementation to find, so one that
	// guessed a trigger by searching the messages bucket would pass anyway.
	// With it, the only thing keeping the 👀 off is that a pending candidate
	// carries no trigger.
	if err := h.st.PutMessage(store.MessageRecord{
		Name:      "spaces/A/messages/m1",
		RefKeys:   []string{"example-org/aex-a#1"},
		FirstSeen: time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 1 {
		t.Fatalf("Reviewed = %d, want 1: the pending ref must still be reviewed (%+v)",
			rep.Reviewed, rep.Decisions)
	}
	if got := reactionCalls(h); len(got) != 0 {
		t.Errorf("a ref with no trigger message has nothing to react to: %v", got)
	}
	// Nor a result reaction: nothing ever put a 👀 on that message, so as far
	// as the team can see firstpass never picked it up, and a bare result
	// reaction would be the first they heard of it.
	rec, ok, err := h.st.Message("spaces/A/messages/m1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if rec.WatchApplied || rec.ResultApplied {
		t.Errorf("the message must be left untouched: %+v", rec)
	}
}

func TestReplayDoesNotReact(t *testing.T) {
	h, _ := reactHarness(t, nil)
	ref := prref.PRRef{Owner: "example-org", Repo: "aex-a", Number: 1}

	dec, err := h.p.ReviewOne(context.Background(), ref, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != ActionReview {
		t.Fatalf("Action = %q, want review", dec.Action)
	}
	if got := reactionCalls(h); len(got) != 0 {
		t.Errorf("a replay's trigger is the literal replay marker, not a message name: %v", got)
	}
}

// ---- surviving a restart ----

func TestTheWatchingReactionIsFinishedByALaterProcess(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("aex-a", 1)+"\n"+prURL("aex-b", 2)),
	})
	h.prs.info["example-org/aex-b#2"] = ghpr.PRInfo{State: "OPEN", Author: "colleague", IsDraft: true}

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if got := emojis(rc); len(got) != 1 || got[0] != EmojiWatching {
		t.Fatalf("emojis = %v, want just [👀]", got)
	}

	// A different Pipeline over the same database: the daemon restarted, and
	// the triggering message is long out of the fetch window. Everything the
	// second process needs is on disk.
	log2 := &[]string{}
	rc2 := &fakeReactor{log: log2, n: 9}
	p2 := &Pipeline{
		Cfg: h.cfg, Store: h.st, Chat: &fakeChat{}, PRs: h.prs, WTs: h.wts,
		Rev: &orderedRev{inner: h.rev, log: log2}, React: rc2,
		Log: h.p.Log, Now: h.p.Now,
	}
	delete(h.prs.info, "example-org/aex-b#2")

	if _, err := p2.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if got := emojis(rc2); len(got) != 1 || got[0] != EmojiClean {
		t.Fatalf("emojis = %v, want [✅] from the second process", got)
	}
	if len(rc2.removed) != 1 || rc2.removed[0] != "spaces/A/messages/m1/reactions/r1" {
		t.Errorf("the 👀 the first process created must be removable by the second, removed %v", rc2.removed)
	}
}
