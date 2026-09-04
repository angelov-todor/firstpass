package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
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
	// Cancellation is honoured because the real reactor cannot do otherwise:
	// chat.Client drives runner.OS, and exec.CommandContext will not start a
	// subprocess on a context that is already done. A fake that ignored this
	// would make an interrupted sweep look survivable when it is not.
	if err := ctx.Err(); err != nil {
		return "", err
	}
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, reactionName)
	return nil
}

// orderedRev is the harness's reviewer with its invocations interleaved into
// the reactor's log, and with a per-ref verdict override so one message can
// carry a clean pull request and a findings one at once.
type orderedRev struct {
	inner    *fakeRev
	log      *[]string
	verdicts map[string]review.Verdict
}

func (o *orderedRev) Run(ctx context.Context, dir string, ref prref.PRRef) (review.Result, error) {
	*o.log = append(*o.log, "review:"+ref.Key())
	res, err := o.inner.Run(ctx, dir, ref)
	if v, ok := o.verdicts[ref.Key()]; ok {
		res.Verdict = v
	}
	return res, err
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

	// ✅ now means "every pull request firstpass reviewed came back approved",
	// so the harness's default review has to be a clean one. Left at the zero
	// verdict, every review would record store.VerdictUnknown and every
	// message would earn 💬 -- which would make most of the tests below pass
	// or fail for reasons that have nothing to do with what they are checking.
	h.rev.result = review.Result{Verdict: review.VerdictApprove}

	log := &[]string{}
	rc := &fakeReactor{log: log}
	h.p.React = rc
	h.p.Rev = &orderedRev{inner: h.rev, log: log, verdicts: map[string]review.Verdict{}}
	h.reactLog = log
	return h, rc
}

// revOf reaches the reviewer reactHarness wired, so a test can give one ref a
// different verdict from another.
func revOf(t *testing.T, h *harness) *orderedRev {
	t.Helper()
	r, ok := h.p.Rev.(*orderedRev)
	if !ok {
		t.Fatalf("Rev = %T, want *orderedRev", h.p.Rev)
	}
	return r
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

// emojisOn is emojis narrowed to one message, for the tests where a sweep
// reacts to more than one.
func emojisOn(rc *fakeReactor, message string) []string {
	var out []string
	for _, a := range rc.added {
		if a.Message == message {
			out = append(out, a.Emoji)
		}
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

func TestResultIsCleanOnlyWhenEveryReviewedRefWasApproved(t *testing.T) {
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
	// Both verdicts were submitted, which is what makes the ✅ below mean
	// "approved" rather than merely "the review process completed".
	for _, key := range []string{"example-org/aex-a#1", "example-org/aex-b#2"} {
		r, ok, err := h.st.Review(key)
		if err != nil || !ok {
			t.Fatalf("%s: ok=%v err=%v", key, ok, err)
		}
		if r.Verdict != store.VerdictApproved {
			t.Fatalf("%s: Verdict = %q, want approved", key, r.Verdict)
		}
	}
	if got := emojis(rc); len(got) != 2 || got[1] != EmojiClean {
		t.Errorf("emojis = %v, want [👀 ✅] when every reviewed PR was approved", got)
	}
}

func TestAFindingsVerdictOnOneRefMakesTheWholeMessageFindings(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("aex-a", 1)+"\n"+prURL("aex-b", 2)),
	})
	// aex-a#1 is approved; aex-b#2 raised something Critical or Important. Both
	// are store.OutcomeReviewed, so nothing but the verdict tells them apart --
	// which is exactly the case the old outcome-only rule got wrong, calling a
	// PR with twenty inline comments clean.
	revOf(t, h).verdicts["example-org/aex-b#2"] = review.VerdictFindings

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]store.Verdict{
		"example-org/aex-a#1": store.VerdictApproved,
		"example-org/aex-b#2": store.VerdictFindings,
	} {
		r, ok, err := h.st.Review(key)
		if err != nil || !ok {
			t.Fatalf("%s: ok=%v err=%v", key, ok, err)
		}
		if r.Outcome != store.OutcomeReviewed {
			t.Fatalf("%s: Outcome = %q, want reviewed -- both refs must be reviewed, or the "+
				"verdict is not what decided this", key, r.Outcome)
		}
		if r.Verdict != want {
			t.Fatalf("%s: Verdict = %q, want %q", key, r.Verdict, want)
		}
	}
	if got := emojis(rc); len(got) != 2 || got[1] != EmojiFindings {
		t.Errorf("emojis = %v, want [👀 💬]: one of the message's PRs has findings on it", got)
	}
}

func TestAnUnknownVerdictIsNotGoodEnoughForATick(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-a", 1))})
	// The reviewer printed no verdict line firstpass recognises, so firstpass
	// does not know whether this pull request is clean.
	revOf(t, h).verdicts["example-org/aex-a#1"] = review.VerdictUnknown

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 1 {
		t.Fatalf("Reviewed = %d, want 1: the review itself succeeded (%+v)", rep.Reviewed, rep.Decisions)
	}
	r, _, _ := h.st.Review("example-org/aex-a#1")
	if r.Outcome != store.OutcomeReviewed || r.Verdict != store.VerdictUnknown {
		t.Fatalf("record = %q / %q, want reviewed / unknown", r.Outcome, r.Verdict)
	}
	if got := emojis(rc); len(got) != 2 || got[1] != EmojiFindings {
		t.Errorf("emojis = %v, want [👀 💬]: not knowing must never be read as ✅", got)
	}
}

func TestAVerdictThatCouldNotBeSubmittedIsNotClean(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-a", 1))})
	// The reviewer said approve, but gh refused the submission, so nothing was
	// approved on the pull request and the record's verdict is left unset. The
	// team must not be told ✅ about an approval that never happened.
	h.prs.submitErr = errors.New("gh: HTTP 403")

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 1 {
		t.Fatalf("Reviewed = %d, want 1: a failed submission leaves the review reviewed (%+v)",
			rep.Reviewed, rep.Decisions)
	}
	r, _, _ := h.st.Review("example-org/aex-a#1")
	if r.Outcome != store.OutcomeReviewed || r.Verdict != store.VerdictNone {
		t.Fatalf("record = %q / %q, want reviewed with the verdict left unset", r.Outcome, r.Verdict)
	}
	if got := emojis(rc); len(got) != 2 || got[1] != EmojiFindings {
		t.Errorf("emojis = %v, want [👀 💬]: an approval that failed to submit is not an approval", got)
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

func TestASkippedRefDoesNotSpoilAnOtherwiseCleanMessage(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("aex-a", 1)+"\n"+prURL("aex-b", 2)),
	})
	// aex-a#1 is reviewed and approved; aex-b#2 is closed, so it is recorded
	// terminal without ever being reviewed. A skip is not a finding: nothing
	// was wrong with it, firstpass simply had no business reviewing it, and it
	// says nothing at all about the code. It must not drag the message to 💬.
	h.prs.info["example-org/aex-b#2"] = ghpr.PRInfo{State: "CLOSED", Author: "colleague"}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := decisionFor(rep, "example-org/aex-b#2"); d.Action != ActionSkip {
		t.Fatalf("aex-b#2 must have been skipped, got %q", d.Action)
	}
	if b, _, _ := h.st.Review("example-org/aex-b#2"); b.Outcome != store.OutcomeSkippedState {
		t.Fatalf("aex-b#2 Outcome = %q, want skipped_state", b.Outcome)
	}
	if got := emojis(rc); len(got) != 2 || got[1] != EmojiClean {
		t.Errorf("emojis = %v, want [👀 ✅]: the one PR firstpass actually reviewed was approved", got)
	}
}

func TestAnExpiredRefDoesNotSpoilAnOtherwiseCleanMessage(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("aex-a", 1)+"\n"+prURL("aex-b", 2)),
	})
	// aex-b#2 has been a draft for so long that its pending entry aged out.
	// Expiry is firstpass giving up on ever reviewing it, not a finding.
	h.prs.info["example-org/aex-b#2"] = ghpr.PRInfo{State: "OPEN", Author: "colleague", IsDraft: true}
	h.cfg.PendingMaxAttempts = 1
	h.apply()
	if err := h.st.PutPending(store.Pending{
		Key: "example-org/aex-b#2", FirstSeen: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Attempts: 5,
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := decisionFor(rep, "example-org/aex-b#2"); d.Reason != "pending expired" {
		t.Fatalf("aex-b#2 must have expired, got %q / %q", d.Action, d.Reason)
	}
	if b, _, _ := h.st.Review("example-org/aex-b#2"); b.Outcome != store.OutcomeExpired {
		t.Fatalf("aex-b#2 Outcome = %q, want expired", b.Outcome)
	}
	if got := emojis(rc); len(got) != 2 || got[1] != EmojiClean {
		t.Errorf("emojis = %v, want [👀 ✅]: an expired PR was never reviewed, so it is not a finding", got)
	}
}

// The refs a message carries are re-read from its text every sweep, and chat
// messages can be edited. A message can therefore end up with a 👀 on it -- a
// review really did start -- and a ref list in which nothing was reviewed at
// all. Without the "no reviewed refs" guard the ref loop is vacuous, clean
// stays true, and the message earns a bare ✅ for work firstpass never did.
func TestAMessageLeftWithNoReviewedRefsGetsNoResultReaction(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("aex-a", 1)+"\n"+prURL("aex-b", 2)),
	})
	h.prs.info["example-org/aex-b#2"] = ghpr.PRInfo{State: "OPEN", Author: "colleague", IsDraft: true}

	// aex-a#1 is reviewed, so the 👀 goes on; aex-b#2 is a draft, so the
	// message is not finished and gets no result reaction yet.
	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if got := emojis(rc); len(got) != 1 || got[0] != EmojiWatching {
		t.Fatalf("emojis = %v, want just the watching one", got)
	}

	// The message is edited to point at a third, closed pull request instead.
	// Its ref list is refreshed to one that holds nothing firstpass reviewed.
	h.prs.info["example-org/aex-c#3"] = ghpr.PRInfo{State: "CLOSED", Author: "colleague"}
	h.ch.msgs = []chat.Message{msg("spaces/A/messages/m1", prURL("aex-c", 3))}
	h.seedWatermark(t)
	if _, err := h.p.Sweep(context.Background(), Options{Backfill: 10}); err != nil {
		t.Fatal(err)
	}

	rec, ok, err := h.st.Message("spaces/A/messages/m1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if len(rec.RefKeys) != 1 || rec.RefKeys[0] != "example-org/aex-c#3" {
		t.Fatalf("RefKeys = %v, want just the edited-in ref: without that the test proves nothing",
			rec.RefKeys)
	}
	if !rec.WatchApplied {
		t.Fatal("the message must still carry its watching reaction")
	}
	if got := emojis(rc); len(got) != 1 || got[0] != EmojiWatching {
		t.Errorf("emojis = %v, want no result reaction: nothing in this message's ref list was "+
			"ever reviewed, so there is nothing to report", got)
	}
	if rec.ResultApplied {
		t.Errorf("no result reaction may have been marked: %+v", rec)
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
		Verdict:   store.VerdictApproved,
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

// ---- an interrupted sweep must not burn the reaction ----

// Ctrl-C during a review is the ordinary way to stop the daemon, and reviews
// run for up to thirty minutes, so the sweep context being dead by the time a
// reaction is attempted is routine rather than exotic.
//
// The hazard is the latch. Each stage is marked in the store before the API
// call so a crash cannot produce a second reaction -- but on a dead context
// the call cannot possibly succeed, so latching first converts a transient
// interrupt into a permanent one: the message keeps 👀 forever, never gets its
// result, and no later sweep will look at it again because the latch says it
// is done. Sweep already guards its own end-of-sweep pass for exactly this
// reason; the inline calls from handle need the same guard.
func TestAnInterruptedSweepDoesNotBurnTheResultReaction(t *testing.T) {
	// Two messages, so the candidate loop gets another turn after the
	// interrupt and the sweep reports the cancellation the way a real Ctrl-C
	// does. aex-a#1 is in the older message, so it is handled first.
	const m1 = "spaces/A/messages/m1"
	h, rc := reactHarness(t, []chat.Message{
		msg("spaces/A/messages/m2", prURL("aex-b", 2)),
		msg(m1, prURL("aex-a", 1)),
	})

	// The review succeeds and the operator hits Ctrl-C on the way out -- the
	// window between Rev.Run returning and the reaction being attempted. A
	// review runs for up to thirty minutes, so this window is most of the
	// daemon's life.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.rev.onRun = cancel

	rep, err := h.p.Sweep(ctx, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sweep err = %v, want context.Canceled", err)
	}
	// The review itself completed and was recorded, verdict and all. That is
	// what makes a missing reaction a bug rather than a review that never
	// happened.
	if rep.Reviewed != 1 {
		t.Fatalf("Reviewed = %d, want 1: %+v", rep.Reviewed, rep.Decisions)
	}
	r, ok, err := h.st.Review("example-org/aex-a#1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if r.Outcome != store.OutcomeReviewed || r.Verdict != store.VerdictApproved {
		t.Fatalf("record = %q / %q, want reviewed / approved", r.Outcome, r.Verdict)
	}
	// And the 👀 did land: it was added before the cancel, so the message is
	// now showing "being reviewed right now" and needs its result.
	if got := emojisOn(rc, m1); len(got) != 1 || got[0] != EmojiWatching {
		t.Fatalf("emojis on %s = %v, want just the watching one", m1, got)
	}

	rec, ok, err := h.st.Message(m1)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if rec.ResultApplied {
		t.Errorf("the result latch must not be set on a dead context: the call cannot succeed, so "+
			"latching turns a Ctrl-C into a message that keeps 👀 for good (%+v)", rec)
	}

	// The recovery is the point. A healthy sweep afterwards must deliver the
	// result reaction and take the 👀 off. aex-a#1 is terminal and will not be
	// offered again, so this can only arrive through the end-of-sweep pass
	// over the messages bucket.
	h.rev.onRun = nil
	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if got := emojisOn(rc, m1); len(got) != 2 || got[1] != EmojiClean {
		t.Errorf("emojis on %s = %v, want [👀 ✅] once a healthy sweep runs", m1, got)
	}
	// m2's own 👀 is removed on this sweep too -- it was reviewed here -- so
	// this looks for m1's specifically.
	if !slices.Contains(rc.removed, m1+"/reactions/r1") {
		t.Errorf("m1's 👀 must come off on the recovery sweep, removed %v", rc.removed)
	}
}

func TestAnInterruptedSweepDoesNotBurnTheWatchingReaction(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("aex-a", 1)+"\n"+prURL("aex-b", 2)),
	})

	// Cancelled where the clone would be: past every gate the sweep applies
	// up front, and immediately before the 👀 would be added.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.wts.onPrepare = cancel

	if _, err := h.p.Sweep(ctx, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sweep err = %v, want context.Canceled", err)
	}
	rec, ok, err := h.st.Message("spaces/A/messages/m1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if rec.WatchApplied {
		t.Errorf("the watch latch must not be set on a dead context, or this message can never "+
			"get its 👀 (%+v)", rec)
	}
	if rec.WatchReaction != "" {
		t.Errorf("nothing can have been created on a dead context: %+v", rec)
	}

	// aex-a#1 was killed mid-review, so it is needs_attention and will not be
	// reviewed again. aex-b#2 never got its turn and still is, and its review
	// is what puts the 👀 on the message the interrupted sweep failed to mark.
	if a, _, _ := h.st.Review("example-org/aex-a#1"); a.Outcome != store.OutcomeNeedsAttention {
		t.Fatalf("aex-a#1 Outcome = %q, want needs_attention: a review killed by the interrupt "+
			"must not read as one that finished", a.Outcome)
	}
	h.wts.onPrepare = nil
	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if got := emojis(rc); len(got) == 0 || got[0] != EmojiWatching {
		t.Fatalf("emojis = %v, want the 👀 first: the recovery sweep must still be able to add it", got)
	}
	if got := emojis(rc); len(got) != 2 || got[1] != EmojiFindings {
		t.Errorf("emojis = %v, want [👀 💬]: aex-a#1 needs attention, so the message is not clean", got)
	}
}

// ---- the wait for every ref to be terminal ----

// An in_flight sibling is not a decided one. recoverInFlight converts these at
// the top of every writing sweep, but it logs and carries on when the write
// fails -- so a record really can still be in_flight while another of the same
// message's refs finishes. Falling through to the "not recognised, be
// pessimistic" arm would earn the message a premature 💬 while a review of it
// is still, as far as the store knows, in progress.
//
// Called directly: getting a sweep to hold an in_flight record past
// recoverInFlight needs a store write failure, and there is no seam for one.
func TestAnInFlightSiblingHoldsTheResultReaction(t *testing.T) {
	h, rc := reactHarness(t, nil)
	const m1 = "spaces/A/messages/m1"
	if err := h.st.PutMessage(store.MessageRecord{
		Name:          m1,
		RefKeys:       []string{"example-org/aex-a#1", "example-org/aex-b#2"},
		WatchApplied:  true,
		WatchReaction: m1 + "/reactions/r1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.PutReview(store.Review{
		Key: "example-org/aex-a#1", Outcome: store.OutcomeReviewed, Verdict: store.VerdictApproved,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.PutReview(store.Review{
		Key: "example-org/aex-b#2", Outcome: store.OutcomeInFlight,
	}); err != nil {
		t.Fatal(err)
	}

	h.p.settleMessageReaction(context.Background(), m1, Options{})
	if got := reactionCalls(h); len(got) != 0 {
		t.Errorf("an in_flight ref is not decided, so the message is not finished: %v", got)
	}

	// And once it is decided, the reaction lands -- so this is a wait, not a
	// refusal.
	if err := h.st.PutReview(store.Review{
		Key: "example-org/aex-b#2", Outcome: store.OutcomeReviewed, Verdict: store.VerdictApproved,
	}); err != nil {
		t.Fatal(err)
	}
	h.p.settleMessageReaction(context.Background(), m1, Options{})
	if got := emojis(rc); len(got) != 1 || got[0] != EmojiClean {
		t.Errorf("emojis = %v, want [✅] once the in_flight ref is decided", got)
	}
}

// ---- a pause that arrives mid-sweep ----

// Sweep observes the pause file once at the start, and again before each
// review. A pause that lands after the sweep began therefore shows up only as
// pausedMidSweep, and the end-of-sweep reaction pass has to honour that as
// well as the sweep-start reading -- a sweep can run for the better part of
// two hours, which is plenty of time for `firstpass pause` to be run.
func TestAPauseArrivingMidSweepStopsTheResultReaction(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("aex-a", 1)+"\n"+prURL("aex-b", 2)),
	})
	h.prs.info["example-org/aex-b#2"] = ghpr.PRInfo{State: "OPEN", Author: "colleague", IsDraft: true}

	// First sweep: aex-a#1 is reviewed so the 👀 goes on, aex-b#2 is a draft
	// so the message is not finished.
	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if got := emojis(rc); len(got) != 1 || got[0] != EmojiWatching {
		t.Fatalf("emojis = %v, want just the watching one", got)
	}

	// aex-b#2 finishes in a process that died before it could react, so m1 is
	// now ready for its result reaction.
	if err := h.st.PutReview(store.Review{
		Key: "example-org/aex-b#2", Outcome: store.OutcomeReviewed, Verdict: store.VerdictApproved,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.DeletePending("example-org/aex-b#2"); err != nil {
		t.Fatal(err)
	}

	// Second sweep starts unpaused -- so rep.Paused is false and only
	// pausedMidSweep can hold the reaction back -- and the kill switch is
	// thrown while the sweep is preparing a worktree for a third PR.
	h.ch.msgs = []chat.Message{
		msg("spaces/A/messages/m2", prURL("aex-c", 3)),
		msg("spaces/A/messages/m1", prURL("aex-a", 1)+"\n"+prURL("aex-b", 2)),
	}
	h.seedWatermark(t)
	h.wts.onPrepare = func() {
		if err := os.WriteFile(h.cfg.PauseFile(), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := len(reactionCalls(h))

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Paused {
		t.Fatal("the sweep must have started unpaused, or this tests rep.Paused and not the mid-sweep pause")
	}
	if d, _ := decisionFor(rep, "example-org/aex-c#3"); d.Reason != "paused before the review started" {
		t.Fatalf("the pause must have fired mid-sweep, got %q / %q", d.Action, d.Reason)
	}
	if got := reactionCalls(h); len(got) != before {
		t.Errorf("a pause that lands mid-sweep stops the reactions too: %d calls before, %v after",
			before, got)
	}

	// Deferred, not dropped.
	h.wts.onPrepare = nil
	if err := os.Remove(h.cfg.PauseFile()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if got := emojis(rc); len(got) != 2 || got[1] != EmojiClean {
		t.Errorf("emojis = %v, want [👀 ✅] after the resume", got)
	}
}

// ---- a removal that fails ----

// The remove is the one reaction call that had no failure test of its own: the
// only test setting removeErr also set addErr, so the add failed first and
// returned before any removal was attempted.
func TestAFailedRemovalLeavesTheResultReactionInPlace(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-a", 1))})
	rc.removeErr = errors.New("chat api 404 NOT_FOUND: reaction not found")

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatalf("a failed removal must not fail the sweep: %v", err)
	}
	if rep.Reviewed != 1 {
		t.Fatalf("Reviewed = %d, want 1: %+v", rep.Reviewed, rep.Decisions)
	}
	if r, _, _ := h.st.Review("example-org/aex-a#1"); r.Outcome != store.OutcomeReviewed {
		t.Errorf("Outcome = %q; a failed removal must not touch the review", r.Outcome)
	}

	// Both adds went through, and the removal was genuinely attempted -- which
	// is what the old test could not say.
	if got := emojis(rc); len(got) != 2 || got[1] != EmojiClean {
		t.Fatalf("emojis = %v, want [👀 ✅]", got)
	}
	var removes int
	for _, e := range reactionCalls(h) {
		if strings.HasPrefix(e, "remove:") {
			removes++
		}
	}
	if removes != 1 {
		t.Fatalf("the removal must have been attempted exactly once, calls = %v", reactionCalls(h))
	}
	if len(rc.removed) != 0 {
		t.Fatalf("nothing can have been removed, removed = %v", rc.removed)
	}

	// The result reaction stays marked, so a later sweep does not add a second
	// one; and the 👀 name stays on record, because it is still on the message.
	rec, ok, err := h.st.Message("spaces/A/messages/m1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !rec.ResultApplied || rec.ResultReaction == "" {
		t.Errorf("the result reaction landed and must stay recorded: %+v", rec)
	}
	if rec.WatchReaction == "" {
		t.Errorf("the 👀 is still on the message, so its name must not be cleared: %+v", rec)
	}

	// Rule 4 still holds over the failure: no second result reaction.
	if _, err := h.p.Sweep(context.Background(), Options{Backfill: 10}); err != nil {
		t.Fatal(err)
	}
	if got := emojis(rc); len(got) != 2 {
		t.Errorf("emojis = %v, want no third reaction after a failed removal", got)
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
