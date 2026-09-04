package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
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

type fakeChat struct {
	msgs      []chat.Message
	lastSince string
	lastLimit int
	err       error
}

func (f *fakeChat) Fetch(_ context.Context, since string, limit int) ([]chat.Message, bool, error) {
	f.lastSince, f.lastLimit = since, limit
	if f.err != nil {
		return nil, false, f.err
	}
	if since == "" {
		return f.msgs, true, nil
	}
	for i, m := range f.msgs {
		if m.Name == since {
			return f.msgs[:i], true, nil
		}
	}
	return f.msgs, false, nil
}

type fakePRs struct {
	info map[string]ghpr.PRInfo
	err  map[string]error

	// submitted records every verdict submission attempted through this fake,
	// so a test that does not care about the argv can still prove nothing was
	// submitted. Tests that do care wire the real ghpr client over a
	// runner.Fake instead; see verdict_test.go.
	submitted []submittedVerdict
	submitErr error

	// inspected records every ref this fake was asked about, in order. Some
	// gates exist precisely to decide without a GitHub call, so "Inspect was
	// never called" is the assertion for those -- the outcome alone would be
	// satisfied by the gate having moved below GitHub.
	inspected []string
}

type submittedVerdict struct {
	key     string
	verdict string
	body    string
}

func (f *fakePRs) Inspect(_ context.Context, ref prref.PRRef) (ghpr.PRInfo, error) {
	f.inspected = append(f.inspected, ref.Key())
	if e, ok := f.err[ref.Key()]; ok {
		return ghpr.PRInfo{}, e
	}
	if i, ok := f.info[ref.Key()]; ok {
		return i, nil
	}
	return ghpr.PRInfo{State: "OPEN", Author: "colleague", HeadSHA: "sha-" + ref.Repo}, nil
}

func (f *fakePRs) SubmitReview(_ context.Context, ref prref.PRRef, verdict, body string) error {
	f.submitted = append(f.submitted, submittedVerdict{key: ref.Key(), verdict: verdict, body: body})
	return f.submitErr
}

type fakeWTs struct {
	prepared  []string
	cleanedUp int
	err       error
	// onPrepare runs where the real clone would: after every gate the sweep
	// applies up front, and immediately before the review starts.
	onPrepare func()
}

func (f *fakeWTs) Prepare(_ context.Context, ref prref.PRRef) (string, func(), error) {
	if f.onPrepare != nil {
		f.onPrepare()
	}
	if f.err != nil {
		return "", func() {}, f.err
	}
	f.prepared = append(f.prepared, ref.Key())
	return filepath.Join("work", ref.Repo), func() { f.cleanedUp++ }, nil
}

type fakeRev struct {
	ran []string
	err error
	// result is returned alongside err, so a test can model claude exiting
	// non-zero (a real exit status) separately from claude being killed
	// (no exit status at all).
	result review.Result
	// onRun runs after the review is recorded as having happened, where a
	// Ctrl-C arriving as claude finishes would land.
	onRun func()
	// prevSHAs records the previousHeadSHA handed to each invocation, in
	// order, so a test can prove a second pass told the reviewer which commit
	// the pass before it looked at -- and that a first pass told it nothing.
	prevSHAs []string
}

func (f *fakeRev) Run(ctx context.Context, _ string, ref prref.PRRef, previousHeadSHA string) (review.Result, error) {
	f.ran = append(f.ran, ref.Key())
	f.prevSHAs = append(f.prevSHAs, previousHeadSHA)
	// A cancelled context is an error, never a clean run: runner.OS returns
	// ctx.Err() ahead of any exit status precisely so a review killed by a
	// Ctrl-C or a deadline cannot be mistaken for one that finished.
	if err := ctx.Err(); err != nil {
		return f.result, err
	}
	// After the review, where a Ctrl-C arriving as claude finishes would land.
	if f.onRun != nil {
		f.onRun()
	}
	return f.result, f.err
}

// ---- harness ----

type harness struct {
	p   *Pipeline
	st  *store.Store
	ch  *fakeChat
	prs *fakePRs
	wts *fakeWTs
	rev *fakeRev
	cfg config.Config

	// reactLog is the interleaved log of reaction and review calls. Only
	// reactHarness sets it; see reactions_test.go.
	reactLog *[]string
}

func newHarness(t *testing.T, msgs []chat.Message) *harness {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	cfg.StateDir = dir
	cfg.GithubLogin = "angelov-todor"
	cfg.AllowOwners = []string{"Example-Org"}

	h := &harness{
		st:  st,
		ch:  &fakeChat{msgs: msgs},
		prs: &fakePRs{info: map[string]ghpr.PRInfo{}, err: map[string]error{}},
		wts: &fakeWTs{},
		rev: &fakeRev{},
		cfg: cfg,
	}
	h.p = &Pipeline{
		Cfg: cfg, Store: st, Chat: h.ch, PRs: h.prs, WTs: h.wts, Rev: h.rev,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) },
	}
	return h
}

// seedWatermark takes the pipeline past the cold-start guard. It also puts the
// watermarked message at the oldest end of the fetch window, as it is in
// practice: a watermark that has fallen out of the window is a gap, and a
// sweep that finds one deliberately refuses to advance it (see
// TestWatermarkGapHoldsTheWatermark).
func (h *harness) seedWatermark(t *testing.T) {
	t.Helper()
	if err := h.st.SetWatermark(store.Watermark{
		MessageName: "spaces/A/messages/m0",
		CreateTime:  time.Date(2026, 9, 3, 6, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	h.ch.msgs = append(h.ch.msgs, chat.Message{
		Name:       "spaces/A/messages/m0",
		Text:       "the watermarked message",
		CreateTime: time.Date(2026, 9, 3, 6, 0, 0, 0, time.UTC),
	})
}

func (h *harness) apply() { h.p.Cfg = h.cfg }

func msg(name, text string) chat.Message {
	return chat.Message{Name: name, Text: text, CreateTime: time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)}
}

func prURL(repo string, n int) string {
	return "https://github.com/Example-Org/" + repo + "/pull/" + strconv.Itoa(n)
}

func decisionFor(rep SweepReport, key string) (Decision, bool) {
	for _, d := range rep.Decisions {
		if d.Ref.Key() == key {
			return d, true
		}
	}
	return Decision{}, false
}

// ---- tests ----

func TestColdStartReviewsNothingAndSetsWatermark(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m9", prURL("aex-balances", 12))})

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.ColdStart {
		t.Error("a first run must be reported as a cold start")
	}
	if rep.Reviewed != 0 || len(h.rev.ran) != 0 {
		t.Errorf("a first run against a populated space must review nothing, ran %v", h.rev.ran)
	}
	wm, ok, err := h.st.Watermark()
	if err != nil || !ok {
		t.Fatalf("cold start must set the watermark (ok=%v err=%v)", ok, err)
	}
	if wm.MessageName != "spaces/A/messages/m9" {
		t.Errorf("watermark = %q, want the newest message", wm.MessageName)
	}
}

func TestBackfillOverridesColdStart(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m9", prURL("aex-balances", 12))})

	rep, err := h.p.Sweep(context.Background(), Options{Backfill: 10})
	if err != nil {
		t.Fatal(err)
	}
	if rep.ColdStart {
		t.Error("an explicit backfill must not be treated as a cold start")
	}
	if len(h.rev.ran) != 1 {
		t.Errorf("backfill must review the PRs it finds, ran %v", h.rev.ran)
	}
	if h.ch.lastLimit != 10 || h.ch.lastSince != "" {
		t.Errorf("backfill must ignore the watermark and use its own limit (since=%q limit=%d)",
			h.ch.lastSince, h.ch.lastLimit)
	}
}

func TestReviewsNewPR(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 1 {
		t.Fatalf("Reviewed = %d, want 1 (decisions: %+v)", rep.Reviewed, rep.Decisions)
	}
	if len(h.wts.prepared) != 1 {
		t.Errorf("a worktree must be prepared, got %v", h.wts.prepared)
	}
	if h.wts.cleanedUp != 1 {
		t.Errorf("the worktree must be cleaned up, cleanedUp = %d", h.wts.cleanedUp)
	}

	rec, ok, err := h.st.Review("example-org/aex-balances#12")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if rec.Outcome != store.OutcomeReviewed {
		t.Errorf("Outcome = %q, want reviewed", rec.Outcome)
	}
	if rec.TriggerMessage != "spaces/A/messages/m1" {
		t.Errorf("TriggerMessage = %q", rec.TriggerMessage)
	}
	if rec.HeadSHA == "" {
		t.Error("HeadSHA must be recorded")
	}
}

func TestSkipsOwnPR(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	h.prs.info["example-org/aex-balances#12"] = ghpr.PRInfo{
		State: "OPEN", Author: "angelov-todor", HeadSHA: "sha",
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("own PRs must not be reviewed, ran %v", h.rev.ran)
	}
	d, _ := decisionFor(rep, "example-org/aex-balances#12")
	if d.Action != ActionSkip {
		t.Errorf("Action = %q, want skip", d.Action)
	}
	rec, _, _ := h.st.Review("example-org/aex-balances#12")
	if rec.Outcome != store.OutcomeSkippedAuthor {
		t.Errorf("Outcome = %q, want skipped_author", rec.Outcome)
	}
}

func TestSkipsMergedPRTerminally(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	h.prs.info["example-org/aex-balances#12"] = ghpr.PRInfo{State: "MERGED", Author: "colleague"}

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := h.st.Review("example-org/aex-balances#12")
	if !ok || rec.Outcome != store.OutcomeSkippedState {
		t.Errorf("a merged PR must be recorded terminally, got ok=%v outcome=%q", ok, rec.Outcome)
	}
	if _, pending, _ := h.st.Pending("example-org/aex-balances#12"); pending {
		t.Error("a merged PR must not linger in pending")
	}
}

func TestOwnerNotAllowedIsTerminalAndNeverQueried(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", "look at this https://github.com/torvalds/linux/pull/999"),
	})
	h.seedWatermark(t)
	h.prs.err["torvalds/linux#999"] = errors.New("must not be called")

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, ok := decisionFor(rep, "torvalds/linux#999")
	if !ok || d.Action != ActionSkip {
		t.Fatalf("an outside org must be skipped, got %+v", d)
	}
	rec, found, _ := h.st.Review("torvalds/linux#999")
	if !found || rec.Outcome != store.OutcomeSkippedOwner {
		t.Errorf("Outcome = %q, want skipped_owner", rec.Outcome)
	}
	if len(h.wts.prepared) != 0 {
		t.Error("an outside org's repo must never be cloned")
	}
}

func TestDeniedRepoIsSkipped(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-secret", 3))})
	h.seedWatermark(t)
	h.cfg.DenyRepos = []string{"Example-Org/aex-secret"}
	h.apply()

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := h.st.Review("example-org/aex-secret#3")
	if !ok || rec.Outcome != store.OutcomeSkippedRepo {
		t.Errorf("Outcome = %q, want skipped_repo", rec.Outcome)
	}
}

func TestDraftGoesToPendingThenIsReviewedWhenReady(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"
	h.prs.info[key] = ghpr.PRInfo{State: "OPEN", IsDraft: true, Author: "colleague", HeadSHA: "sha"}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, key)
	if d.Action != ActionDefer {
		t.Fatalf("a draft must be deferred, got %q", d.Action)
	}
	pd, ok, _ := h.st.Pending(key)
	if !ok || pd.Attempts != 1 {
		t.Fatalf("a draft must count an attempt, got ok=%v attempts=%d", ok, pd.Attempts)
	}
	if _, found, _ := h.st.Review(key); found {
		t.Error("a draft must not get a terminal record; it may become ready later")
	}

	// Marked ready, and the message has since scrolled past the watermark.
	h.prs.info[key] = ghpr.PRInfo{State: "OPEN", IsDraft: false, Author: "colleague", HeadSHA: "sha"}
	h.ch.msgs = nil

	rep2, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Reviewed != 1 {
		t.Fatalf("a pending draft must be picked up once ready, decisions %+v", rep2.Decisions)
	}
	if _, ok, _ := h.st.Pending(key); ok {
		t.Error("a reviewed PR must be cleared from pending")
	}
}

func TestInspectFailureDefersAndRetries(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"
	h.prs.err[key] = errors.New("network unreachable")

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	pd, ok, _ := h.st.Pending(key)
	if !ok || pd.Attempts != 1 {
		t.Fatalf("a transient gh failure must defer with an attempt, ok=%v attempts=%d", ok, pd.Attempts)
	}

	delete(h.prs.err, key)
	h.ch.msgs = nil
	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 1 {
		t.Errorf("the retry must review it, decisions %+v", rep.Decisions)
	}
}

func TestDedupeWithinBatch(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m2", "reposting "+prURL("aex-balances", 12)),
		msg("spaces/A/messages/m1", "please review "+prURL("aex-balances", 12)),
	})
	h.seedWatermark(t)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 1 || len(h.rev.ran) != 1 {
		t.Fatalf("the same PR in two messages must be reviewed once, ran %v", h.rev.ran)
	}
	rec, _, _ := h.st.Review("example-org/aex-balances#12")
	if rec.TriggerMessage != "spaces/A/messages/m1" {
		t.Errorf("TriggerMessage = %q, want the oldest post in the batch", rec.TriggerMessage)
	}
}

func TestAlreadyReviewedIsSkipped(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"
	if err := h.st.PutReview(store.Review{Key: key, Outcome: store.OutcomeReviewed}); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("an already-reviewed PR must not be reviewed again, ran %v", h.rev.ran)
	}
	d, _ := decisionFor(rep, key)
	if d.Action != ActionSkip {
		t.Errorf("Action = %q, want skip", d.Action)
	}
}

func TestPerSweepCapDefersTheRestWithoutBurningAttempts(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("a", 1)+" "+prURL("b", 2)+" "+prURL("c", 3)),
	})
	h.seedWatermark(t)
	h.cfg.MaxReviewsPerSweep = 2
	h.apply()

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 2 {
		t.Fatalf("Reviewed = %d, want 2 (the cap)", rep.Reviewed)
	}

	all, err := h.st.AllPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("AllPending() = %d, want the one over the cap", len(all))
	}
	if all[0].Attempts != 0 {
		t.Errorf("Attempts = %d; hitting the cap is not a failure and must not count against expiry", all[0].Attempts)
	}
}

func TestPauseFileStopsReviewsWithoutBurningAttempts(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	if err := os.WriteFile(h.cfg.PauseFile(), []byte("paused"), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Paused {
		t.Error("the report must say the sweep was paused")
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("a paused sweep must post nothing, ran %v", h.rev.ran)
	}
	pd, ok, _ := h.st.Pending("example-org/aex-balances#12")
	if !ok {
		t.Fatal("a paused sweep must still queue the ref")
	}
	if pd.Attempts != 0 {
		t.Errorf("Attempts = %d; a long pause must not silently expire the backlog", pd.Attempts)
	}
}

func TestInFlightBecomesNeedsAttentionAndIsNotRetried(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"
	if err := h.st.PutReview(store.Review{
		Key: key, Outcome: store.OutcomeInFlight, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, key)
	if d.Action != ActionNeedsAttention {
		t.Fatalf("Action = %q, want needs_attention", d.Action)
	}
	if len(h.rev.ran) != 0 {
		t.Error("a review that died mid-post must not be retried automatically: comments may already be on the PR")
	}
	rec, _, _ := h.st.Review(key)
	if rec.Outcome != store.OutcomeNeedsAttention {
		t.Errorf("Outcome = %q, want needs_attention", rec.Outcome)
	}
	if rec.Detail == "" {
		t.Error("the record must explain what happened and how to act on it")
	}
}

func TestReviewFailureBecomesNeedsAttention(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	h.rev.err = context.DeadlineExceeded

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	key := "example-org/aex-balances#12"
	d, _ := decisionFor(rep, key)
	if d.Action != ActionNeedsAttention {
		t.Fatalf("Action = %q, want needs_attention", d.Action)
	}
	rec, _, _ := h.st.Review(key)
	if rec.Outcome != store.OutcomeNeedsAttention {
		t.Errorf("Outcome = %q; a timeout may have fired mid-post, so it must not be retried", rec.Outcome)
	}
	if _, ok, _ := h.st.Pending(key); ok {
		t.Error("a needs_attention PR must not also sit in pending, or it would be retried")
	}
	if h.wts.cleanedUp != 1 {
		t.Error("the worktree must be cleaned up even when the review fails")
	}
}

func TestWorktreeFailureDefers(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	h.wts.err = errors.New("clone failed")

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	key := "example-org/aex-balances#12"
	pd, ok, _ := h.st.Pending(key)
	if !ok || pd.Attempts != 1 {
		t.Fatalf("a clone failure happens before claude starts, so it must defer: ok=%v attempts=%d", ok, pd.Attempts)
	}
	if _, found, _ := h.st.Review(key); found {
		t.Error("nothing was posted, so there must be no terminal record")
	}
}

func TestWatermarkAdvancesToNewestMessage(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m3", "chatter"),
		msg("spaces/A/messages/m2", prURL("aex-balances", 12)),
	})
	h.seedWatermark(t)

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	wm, ok, _ := h.st.Watermark()
	if !ok || wm.MessageName != "spaces/A/messages/m3" {
		t.Errorf("watermark = %q, want the newest message even though it held no PR", wm.MessageName)
	}
}

func TestPrintOnlyChangesNoState(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)

	rep, err := h.p.Sweep(context.Background(), Options{PrintOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, "example-org/aex-balances#12")
	if d.Action != ActionWouldReview {
		t.Fatalf("Action = %q, want would_review", d.Action)
	}
	if len(h.rev.ran) != 0 || len(h.wts.prepared) != 0 {
		t.Error("print-only must not prepare a worktree or run a review")
	}
	if _, found, _ := h.st.Review("example-org/aex-balances#12"); found {
		t.Error("print-only must write no review record")
	}
	wm, _, _ := h.st.Watermark()
	if wm.MessageName != "spaces/A/messages/m0" {
		t.Errorf("print-only must not advance the watermark, got %q", wm.MessageName)
	}
}

func TestPendingExpiresAfterTooManyAttempts(t *testing.T) {
	h := newHarness(t, nil)
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"
	h.cfg.PendingMaxAttempts = 3
	h.apply()
	if err := h.st.PutPending(store.Pending{
		Key: key, FirstSeen: time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC),
		Attempts: 3, LastReason: "draft",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := h.st.Review(key)
	if !ok || rec.Outcome != store.OutcomeExpired {
		t.Errorf("Outcome = %q, want expired", rec.Outcome)
	}
	if _, still, _ := h.st.Pending(key); still {
		t.Error("an expired entry must leave pending, or it is re-inspected forever")
	}
}

func TestPendingExpiresAfterMaxAge(t *testing.T) {
	h := newHarness(t, nil)
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"
	if err := h.st.PutPending(store.Pending{
		Key: key, FirstSeen: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := h.st.Review(key)
	if !ok || rec.Outcome != store.OutcomeExpired {
		t.Errorf("Outcome = %q, want expired after max age", rec.Outcome)
	}
}

func TestChatFailurePropagatesAndHoldsTheWatermark(t *testing.T) {
	h := newHarness(t, nil)
	h.seedWatermark(t)
	h.ch.err = errors.New("chat.py exit 1")

	if _, err := h.p.Sweep(context.Background(), Options{}); err == nil {
		t.Fatal("a chat failure must be returned")
	}
	wm, _, _ := h.st.Watermark()
	if wm.MessageName != "spaces/A/messages/m0" {
		t.Errorf("the watermark must not advance when the fetch failed, got %q", wm.MessageName)
	}
}

// ---- fix-report tests: findings from the Task 7 code review ----

// Fix 1: age-based expiry must not run during a pause. PendingMaxAge
// defaults to 168h (one week), so without this fix a week-long kill switch
// silently voids the whole backlog it is supposed to be protecting.
func TestPauseDoesNotExpireStalePending(t *testing.T) {
	h := newHarness(t, nil)
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"
	if err := h.st.PutPending(store.Pending{
		Key: key, FirstSeen: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.cfg.PauseFile(), []byte("paused"), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Paused {
		t.Fatal("the sweep must report paused")
	}
	pd, ok, _ := h.st.Pending(key)
	if !ok {
		t.Fatal("a pause must not let a stale pending entry expire: it must still be in pending")
	}
	if pd.Attempts != 1 {
		t.Errorf("Attempts = %d, must be unchanged by a pause", pd.Attempts)
	}
	if _, found, _ := h.st.Review(key); found {
		t.Error("a paused sweep must not write an expired terminal record for a stale pending entry")
	}
}

// Fix 2: a defer that happens before a ref reaches any bucket must still park
// it, or it falls out of every bucket while the watermark advances past the
// message that produced it and is never seen again.
//
// Both failure paths this fixes (store.Review read failure, and the
// PutReview that records in_flight failing) require Pipeline.Store -- a
// concrete *store.Store, not an interface -- to fail on one specific call
// while every other call on the same Store keeps working (Store.Pending and
// Store.PutPending must still succeed for hold() to actually park the ref).
// There is no seam to inject that with the existing fakes: closing the
// underlying bbolt.DB fails every subsequent call, including the very first
// one Sweep makes (Store.Watermark), so it never reaches handle() at all,
// and it also breaks hold()'s own store calls, defeating the very behaviour
// under test. Reaching only the intended call would require either wrapping
// Store behind an interface (a larger, cross-task change) or reshaping the
// harness, both out of scope here. Per the coordinator's note, this is
// recorded rather than forced: the fix at pipeline.go was verified by
// reading, not by a store-failure test.

// Fix 3: print-only must write nothing, for every gate that can be reached
// before the explicit PrintOnly bail, not just the would-review path.
func TestPrintOnlyDoesNotWriteForSkippedOwner(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", "look at this https://github.com/torvalds/linux/pull/999"),
	})
	h.seedWatermark(t)

	rep, err := h.p.Sweep(context.Background(), Options{PrintOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, "torvalds/linux#999")
	if d.Action != ActionSkip {
		t.Fatalf("Action = %q, want skip even in print-only", d.Action)
	}
	if _, found, _ := h.st.Review("torvalds/linux#999"); found {
		t.Error("print-only must not write a terminal record for a disallowed owner")
	}
}

func TestPrintOnlyDoesNotWriteForDraft(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"
	h.prs.info[key] = ghpr.PRInfo{State: "OPEN", IsDraft: true, Author: "colleague", HeadSHA: "sha"}

	rep, err := h.p.Sweep(context.Background(), Options{PrintOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, key)
	if d.Action != ActionDefer {
		t.Fatalf("Action = %q, want defer even in print-only", d.Action)
	}
	pd, found, _ := h.st.Pending(key)
	if found {
		t.Errorf("print-only must not write a pending entry for a draft, got %+v", pd)
	}
}

func TestPrintOnlyDoesNotWriteForOwnPR(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"
	h.prs.info[key] = ghpr.PRInfo{State: "OPEN", Author: "angelov-todor", HeadSHA: "sha"}

	rep, err := h.p.Sweep(context.Background(), Options{PrintOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, key)
	if d.Action != ActionSkip {
		t.Fatalf("Action = %q, want skip even in print-only", d.Action)
	}
	if _, found, _ := h.st.Review(key); found {
		t.Error("print-only must not write a terminal record for an own PR")
	}
}

func TestPrintOnlyLeavesInFlightRecordUntouched(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	key := "example-org/aex-balances#12"
	if err := h.st.PutReview(store.Review{
		Key: key, Outcome: store.OutcomeInFlight, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{PrintOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, key)
	if d.Action != ActionNeedsAttention {
		t.Fatalf("Action = %q, want needs_attention even in print-only", d.Action)
	}
	rec, ok, _ := h.st.Review(key)
	if !ok || rec.Outcome != store.OutcomeInFlight {
		t.Errorf("print-only must not convert an in_flight record in the store, got ok=%v outcome=%q", ok, rec.Outcome)
	}
}

// Fix 4: the per-sweep cap must bound Rev.Run invocations, not just
// successful ones -- otherwise a run of failures (precisely the degraded
// state a throttle exists for) never trips the gate, and every candidate in
// the batch gets a review attempt, each of which may already post comments
// before failing.
func TestPerSweepCapBoundsFailedReviewAttempts(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("a", 1)+" "+prURL("b", 2)+" "+prURL("c", 3)),
	})
	h.seedWatermark(t)
	h.cfg.MaxReviewsPerSweep = 2
	h.apply()
	h.rev.err = errors.New("claude crashed")

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if len(h.rev.ran) != 2 {
		t.Fatalf("Rev.Run must stop at the cap even though every run is failing, ran %v (want 2)", h.rev.ran)
	}
}
