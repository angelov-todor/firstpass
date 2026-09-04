package pipeline

// The second pass: a pull request posted again is reviewed again, but only if
// it has new commits since the pass that already reviewed it.
//
// The dedupe rule this replaces was "once per pull request". It is now "once
// per commit, and only on a re-post" -- which means the three conditions
// below are the whole of what stands between a colleague's pull request and a
// second set of inline comments on the same lines.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/review"
	"github.com/angelov-todor/firstpass/internal/store"
)

const (
	secondPassKey = "example-org/aex-balances#12"
	firstTrigger  = "spaces/A/messages/m1"
	rePost        = "spaces/A/messages/m2"
	oldSHA        = "0123456789abcdef0123456789abcdef01234567"
	newSHA        = "fedcba9876543210fedcba9876543210fedcba98"
)

// firstPassRecord is what a completed first pass leaves behind: every field
// populated, so a test can prove which ones a later sweep touched.
func firstPassRecord(outcome store.Outcome, headSHA, trigger string) store.Review {
	return store.Review{
		Key:            secondPassKey,
		Outcome:        outcome,
		HeadSHA:        headSHA,
		TriggerMessage: trigger,
		StartedAt:      time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC),
		DecidedAt:      time.Date(2026, 9, 2, 10, 12, 0, 0, time.UTC),
		DurationMS:     735000,
		ReportPath:     `C:\reports\Example-Org_aex-balances_12.md`,
		Detail:         "",
		Verdict:        store.VerdictApproved,
		Pass:           1,
	}
}

// seedFirstPass puts a completed pass in the store and points the fake at a
// live head SHA. It does not seed the watermark: the caller does, because a
// couple of these tests want the triggering message still inside the window.
func seedFirstPass(t *testing.T, h *harness, rec store.Review, liveSHA string) {
	t.Helper()
	if err := h.st.PutReview(rec); err != nil {
		t.Fatal(err)
	}
	h.prs.info[secondPassKey] = ghpr.PRInfo{
		State: "OPEN", Author: "colleague", HeadSHA: liveSHA,
	}
}

// ---- condition 3: new commits ----

// The whole point of the feature, and of condition 3. A re-post with new
// commits is a genuine request for another look, and the record afterwards has
// to say which commit was reviewed this time, which message asked for it, that
// this was the second pass, and which commit the first pass saw.
func TestARepostWithNewCommitsIsReviewedAgain(t *testing.T) {
	h := newHarness(t, []chat.Message{msg(rePost, "another look please "+prURL("aex-balances", 12))})
	h.seedWatermark(t)
	seedFirstPass(t, h, firstPassRecord(store.OutcomeReviewed, oldSHA, firstTrigger), newSHA)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, secondPassKey)
	if d.Action != ActionReview {
		t.Fatalf("Action = %q (%s), want review: a re-post with new commits earns another pass",
			d.Action, d.Reason)
	}
	if rep.Reviewed != 1 || len(h.rev.ran) != 1 {
		t.Fatalf("Reviewed = %d, ran = %v, want exactly one review", rep.Reviewed, h.rev.ran)
	}

	rec, ok, _ := h.st.Review(secondPassKey)
	if !ok {
		t.Fatal("the second pass wrote no record")
	}
	if rec.HeadSHA != newSHA {
		t.Errorf("HeadSHA = %q, want the commit this pass reviewed (%q)", rec.HeadSHA, newSHA)
	}
	if rec.PreviousHeadSHA != oldSHA {
		t.Errorf("PreviousHeadSHA = %q, want the commit the first pass reviewed (%q); "+
			"losing it means nothing can tell which commit already has comments on it",
			rec.PreviousHeadSHA, oldSHA)
	}
	if rec.TriggerMessage != rePost {
		t.Errorf("TriggerMessage = %q, want the message that asked for this pass (%q)",
			rec.TriggerMessage, rePost)
	}
	if rec.Pass != 2 || rec.PassNumber() != 2 {
		t.Errorf("Pass = %d, want 2", rec.Pass)
	}
	if rec.Outcome != store.OutcomeReviewed {
		t.Errorf("Outcome = %q, want reviewed", rec.Outcome)
	}

	// The reviewer has to be told, or it restates every finding the author has
	// not fixed, on the same lines.
	got := h.rev.prevPasses
	if len(got) != 1 || got[0] == nil || got[0].HeadSHA != oldSHA {
		t.Fatalf("previous pass handed to the reviewer = %+v, want one naming %q", got, oldSHA)
	}
	if got[0].Incomplete {
		t.Error("Incomplete = true; a second pass only follows a review that finished")
	}
}

// Re-posted with nothing new. This is the case that stops the feature being a
// way to review the same commit twice: the comments from the first pass are
// already on those exact lines.
//
// The trigger is still updated, so the same re-post is not re-inspected on
// every subsequent sweep while it sits in the fetch window -- and nothing else
// about the record moves, because nothing else happened.
func TestARepostWithNoNewCommitsIsSkippedAndOnlyTheTriggerIsUpdated(t *testing.T) {
	h := newHarness(t, []chat.Message{msg(rePost, "bump "+prURL("aex-balances", 12))})
	h.seedWatermark(t)
	seeded := firstPassRecord(store.OutcomeReviewed, oldSHA, firstTrigger)
	seedFirstPass(t, h, seeded, oldSHA)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, secondPassKey)
	if d.Action != ActionSkip {
		t.Fatalf("Action = %q (%s), want skip: the same commit must never be reviewed twice",
			d.Action, d.Reason)
	}
	if !strings.Contains(d.Reason, "no new commits") {
		t.Errorf("Reason = %q; the operator has to be able to tell this skip from an "+
			"ordinary already-decided one", d.Reason)
	}
	if len(h.rev.ran) != 0 {
		t.Fatalf("ran = %v; a pull request must never be reviewed twice for the same commit", h.rev.ran)
	}
	if len(h.prs.submitted) != 0 {
		t.Errorf("submitted = %+v; nothing was reviewed, so no verdict may be submitted", h.prs.submitted)
	}

	rec, ok, _ := h.st.Review(secondPassKey)
	if !ok {
		t.Fatal("the record vanished")
	}
	want := seeded
	want.TriggerMessage = rePost
	want.TriggerTime = time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	if !reviewsEqual(rec, want) {
		t.Errorf("record =\n %+v\nwant only the trigger changed:\n %+v", rec, want)
	}
}

// ---- condition 2: a different, non-empty trigger ----

// The same message still sitting in the fetch window -- a backfill, or a
// watermark gap -- is not a re-post. It must be skipped at the record gate,
// which is above every GitHub call: asking `gh pr view` about every already
// reviewed pull request in the window on every sweep is exactly the cost the
// record gate exists to avoid.
//
// The absence of the Inspect call is the assertion. A test that only checked
// the outcome would pass just as happily with the gate moved below GitHub.
func TestTheSameTriggerMessageIsSkippedAtTheRecordGateWithoutAskingGitHub(t *testing.T) {
	// The triggering message itself, still inside the window.
	h := newHarness(t, []chat.Message{msg(firstTrigger, "please review "+prURL("aex-balances", 12))})
	h.seedWatermark(t)
	seedFirstPass(t, h, firstPassRecord(store.OutcomeReviewed, oldSHA, firstTrigger), newSHA)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, secondPassKey)
	if d.Action != ActionSkip {
		t.Fatalf("Action = %q (%s), want skip", d.Action, d.Reason)
	}
	if d.Reason != "already decided: reviewed" {
		t.Errorf("Reason = %q, want the unchanged already-decided skip", d.Reason)
	}
	if len(h.prs.inspected) != 0 {
		t.Errorf("Inspect called for %v; the record gate must decide this one with no GitHub call at all",
			h.prs.inspected)
	}
	if len(h.rev.ran) != 0 {
		t.Fatalf("ran = %v", h.rev.ran)
	}
	rec, _, _ := h.st.Review(secondPassKey)
	if rec.TriggerMessage != firstTrigger || rec.Pass != 1 {
		t.Errorf("record = %+v; nothing about it may change", rec)
	}
}

// A ref re-offered out of the pending bucket carries no trigger message at
// all -- pending is keyed by pull request, not by post -- so it can never look
// like a re-post. Without the non-empty half of condition 2 every deferred ref
// with a reviewed record would be re-reviewed on the next sweep.
func TestARefFromPendingIsNeverASecondPass(t *testing.T) {
	// No message carries this link: the only thing offering it is pending.
	h := newHarness(t, nil)
	h.seedWatermark(t)
	seedFirstPass(t, h, firstPassRecord(store.OutcomeReviewed, oldSHA, firstTrigger), newSHA)
	if err := h.st.PutPending(store.Pending{
		Key: secondPassKey, FirstSeen: time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC), Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, ok := decisionFor(rep, secondPassKey)
	if !ok {
		t.Fatal("the pending ref was never offered as a candidate")
	}
	if d.Action != ActionSkip {
		t.Fatalf("Action = %q (%s), want skip: a pending ref names no post, so it is not a re-post",
			d.Action, d.Reason)
	}
	if len(h.rev.ran) != 0 {
		t.Fatalf("ran = %v", h.rev.ran)
	}
	if len(h.prs.inspected) != 0 {
		t.Errorf("Inspect called for %v; this is decided at the record gate", h.prs.inspected)
	}
}

// ---- condition 1: only `reviewed` qualifies ----

// needs_attention means a review died mid-post and comments may be half
// posted. A re-post is not consent to duplicate them, so it still takes an
// explicit `firstpass replay` -- whatever the head SHA says.
func TestNeedsAttentionIsNeverRetriedByARepost(t *testing.T) {
	h := newHarness(t, []chat.Message{msg(rePost, "any news on "+prURL("aex-balances", 12))})
	h.seedWatermark(t)
	rec := firstPassRecord(store.OutcomeNeedsAttention, oldSHA, firstTrigger)
	rec.Verdict = store.VerdictNone
	rec.Detail = "comments may be partially posted"
	seedFirstPass(t, h, rec, newSHA)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, secondPassKey)
	if d.Action != ActionSkip {
		t.Fatalf("Action = %q (%s), want skip: needs_attention is never retried automatically",
			d.Action, d.Reason)
	}
	if len(h.rev.ran) != 0 {
		t.Fatalf("ran = %v; a half-posted comment set must never be re-run without being asked", h.rev.ran)
	}
	if len(h.prs.inspected) != 0 {
		t.Errorf("Inspect called for %v; needs_attention is decided at the record gate, and the "+
			"head SHA is not allowed to be part of that decision", h.prs.inspected)
	}
	got, _, _ := h.st.Review(secondPassKey)
	if !reviewsEqual(got, rec) {
		t.Errorf("record =\n %+v\nwant untouched:\n %+v", got, rec)
	}
}

// Every skipped outcome, and expiry. None of them is a review, so none of
// them can be a first pass for a second pass to follow: a re-post of a merged
// pull request, or of one outside the allowlist, must stay exactly as skipped
// as it was.
func TestASkippedOutcomeIsNeverSecondPassed(t *testing.T) {
	for _, outcome := range []store.Outcome{
		store.OutcomeSkippedAuthor,
		store.OutcomeSkippedState,
		store.OutcomeSkippedOwner,
		store.OutcomeSkippedRepo,
		store.OutcomeExpired,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			h := newHarness(t, []chat.Message{msg(rePost, "reposting "+prURL("aex-balances", 12))})
			h.seedWatermark(t)
			rec := store.Review{
				Key: secondPassKey, Outcome: outcome, HeadSHA: oldSHA,
				TriggerMessage: firstTrigger,
				DecidedAt:      time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC),
				Detail:         "skipped once, skipped for good",
			}
			seedFirstPass(t, h, rec, newSHA)

			rep, err := h.p.Sweep(context.Background(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			d, _ := decisionFor(rep, secondPassKey)
			if d.Action != ActionSkip {
				t.Fatalf("Action = %q (%s), want skip", d.Action, d.Reason)
			}
			if d.Reason != "already decided: "+string(outcome) {
				t.Errorf("Reason = %q, want the unchanged already-decided skip", d.Reason)
			}
			if len(h.rev.ran) != 0 {
				t.Fatalf("ran = %v; only a `reviewed` record can be followed by a second pass", h.rev.ran)
			}
			if got, _, _ := h.st.Review(secondPassKey); !reviewsEqual(got, rec) {
				t.Errorf("record =\n %+v\nwant untouched:\n %+v", got, rec)
			}
		})
	}
}

// ---- print-only ----

// -print-only is the operator's way of asking what a sweep would do. It has to
// say a second pass would happen, and it has to write nothing at all -- not
// even the trigger update, which is a state write like any other.
func TestPrintOnlyOnARepostWithNewCommitsWouldReviewAndWritesNoState(t *testing.T) {
	h := newHarness(t, []chat.Message{msg(rePost, "another look "+prURL("aex-balances", 12))})
	h.seedWatermark(t)
	seeded := firstPassRecord(store.OutcomeReviewed, oldSHA, firstTrigger)
	seedFirstPass(t, h, seeded, newSHA)

	rep, err := h.p.Sweep(context.Background(), Options{PrintOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, secondPassKey)
	if d.Action != ActionWouldReview {
		t.Fatalf("Action = %q (%s), want would_review", d.Action, d.Reason)
	}
	if !strings.Contains(d.Reason, "second pass") {
		t.Errorf("Reason = %q; print-only has to say this would be a second pass, not a first one",
			d.Reason)
	}
	if len(h.rev.ran) != 0 {
		t.Fatalf("ran = %v; print-only reviews nothing", h.rev.ran)
	}
	if got, _, _ := h.st.Review(secondPassKey); !reviewsEqual(got, seeded) {
		t.Errorf("record =\n %+v\nwant untouched by a print-only sweep:\n %+v", got, seeded)
	}
}

// And the same for a re-post with nothing new: the trigger update is a write,
// so print-only must not make it either.
func TestPrintOnlyOnARepostWithNoNewCommitsWritesNoState(t *testing.T) {
	h := newHarness(t, []chat.Message{msg(rePost, "bump "+prURL("aex-balances", 12))})
	h.seedWatermark(t)
	seeded := firstPassRecord(store.OutcomeReviewed, oldSHA, firstTrigger)
	seedFirstPass(t, h, seeded, oldSHA)

	if _, err := h.p.Sweep(context.Background(), Options{PrintOnly: true}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := h.st.Review(secondPassKey); !reviewsEqual(got, seeded) {
		t.Errorf("record =\n %+v\nwant untouched:\n %+v", got, seeded)
	}
}

// ---- dry run ----

// A dry run posts nothing and submits nothing, second pass or not.
func TestADryRunSecondPassPostsNothingAndSubmitsNoVerdict(t *testing.T) {
	h := newHarness(t, []chat.Message{msg(rePost, "another look "+prURL("aex-balances", 12))})
	h.seedWatermark(t)
	h.cfg.DryRun = true
	h.apply()
	h.rev.result = review.Result{Verdict: review.VerdictApprove}
	seedFirstPass(t, h, firstPassRecord(store.OutcomeReviewed, oldSHA, firstTrigger), newSHA)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 1 {
		t.Fatalf("Reviewed = %d, want 1: a dry run still reviews", rep.Reviewed)
	}
	if len(h.prs.submitted) != 0 {
		t.Errorf("submitted = %+v; a dry run submits no verdict, on any pass", h.prs.submitted)
	}
	rec, _, _ := h.st.Review(secondPassKey)
	if rec.Verdict != store.VerdictNone {
		t.Errorf("Verdict = %q, want unset: nothing was submitted", rec.Verdict)
	}
	if rec.Pass != 2 {
		t.Errorf("Pass = %d, want 2", rec.Pass)
	}
}

// ---- the verdict and the reaction follow on their own ----

// Neither feature is special-cased for a second pass, so this is a
// confirmation rather than a mechanism: the new review submits its own
// verdict, and the message that asked for it gets its own 👀 then ✅.
func TestASecondPassSubmitsItsOwnVerdictAndReactsToTheNewMessage(t *testing.T) {
	h, rc := reactHarness(t, []chat.Message{msg(rePost, "another look "+prURL("aex-balances", 12))})
	seedFirstPass(t, h, firstPassRecord(store.OutcomeReviewed, oldSHA, firstTrigger), newSHA)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 1 {
		t.Fatalf("Reviewed = %d, want 1: %+v", rep.Reviewed, rep.Decisions)
	}
	if len(h.prs.submitted) != 1 || h.prs.submitted[0].verdict != ghpr.ReviewApprove {
		t.Errorf("submitted = %+v, want one approving review from the second pass", h.prs.submitted)
	}
	if rec, _, _ := h.st.Review(secondPassKey); rec.Verdict != store.VerdictApproved || rec.Pass != 2 {
		t.Errorf("record = %+v, want an approved second pass", rec)
	}
	want := []string{EmojiWatching, EmojiClean}
	if got := emojisOn(rc, rePost); strings.Join(got, "") != strings.Join(want, "") {
		t.Errorf("reactions on the re-post = %v, want %v", got, want)
	}
	if got := emojisOn(rc, firstTrigger); len(got) != 0 {
		t.Errorf("reactions on the first pass's message = %v, want none: it was settled long ago", got)
	}
}

// ---- replay ----

// replay bypasses the record gate entirely, which is the whole point of it,
// and must keep doing so however the second-pass conditions read. Here not one
// of the three holds -- same trigger, same SHA -- and the operator still gets
// their review.
func TestReplayStillIgnoresTheRecordWhenNoSecondPassWouldTrigger(t *testing.T) {
	h := newHarness(t, nil)
	seedFirstPass(t, h, firstPassRecord(store.OutcomeReviewed, oldSHA, firstTrigger), oldSHA)

	d, err := h.p.ReviewOne(context.Background(), replayRef, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionReview {
		t.Fatalf("Action = %q (%s); replay must review regardless of the second-pass conditions",
			d.Action, d.Reason)
	}
	if len(h.rev.ran) != 1 {
		t.Errorf("ran = %v, want one review", h.rev.ran)
	}
}

// A reviewed record with no head SHA on it -- one written by a review that ran
// off the pending backlog, where the trigger and the SHA are both unknown --
// cannot establish that the commit now on the pull request is a new one. It
// stays skipped: condition 3 is what guarantees no pull request is reviewed
// twice for the same commit, and reading a missing field as "different" would
// post a second comment set on the strength of not knowing.
func TestAReviewedRecordWithNoHeadSHAIsNeverSecondPassed(t *testing.T) {
	h := newHarness(t, []chat.Message{msg(rePost, "reposting "+prURL("aex-balances", 12))})
	h.seedWatermark(t)
	rec := firstPassRecord(store.OutcomeReviewed, "", firstTrigger)
	seedFirstPass(t, h, rec, newSHA)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, secondPassKey)
	if d.Action != ActionSkip {
		t.Fatalf("Action = %q (%s), want skip", d.Action, d.Reason)
	}
	if len(h.rev.ran) != 0 {
		t.Fatalf("ran = %v; a record that names no commit cannot prove this is a different one", h.rev.ran)
	}
	if len(h.prs.inspected) != 0 {
		t.Errorf("Inspect called for %v; this is decidable at the record gate", h.prs.inspected)
	}
	if got, _, _ := h.st.Review(secondPassKey); !reviewsEqual(got, rec) {
		t.Errorf("record =\n %+v\nwant untouched:\n %+v", got, rec)
	}
}
