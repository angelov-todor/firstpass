// Package pipeline holds the whole decision table for one sweep: what to
// review, what to skip, what to defer, and what to hand back to a human.
package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/review"
	"github.com/angelov-todor/firstpass/internal/store"
)

// The four seams to the outside world. Each has a fake in the tests, which is
// why every rule below is verified without a subprocess.
type (
	ChatSource interface {
		// Fetch returns the messages newer than sinceName, newest-first, and
		// reports whether sinceName was inside the fetched window. A false
		// foundSince means messages were missed; see Sweep.
		Fetch(ctx context.Context, sinceName string, limit int) (msgs []chat.Message, foundSince bool, err error)
	}
	PRInspector interface {
		Inspect(ctx context.Context, ref prref.PRRef) (ghpr.PRInfo, error)
	}
	Worktrees interface {
		Prepare(ctx context.Context, ref prref.PRRef) (dir string, cleanup func(), err error)
	}
	Reviewer interface {
		Run(ctx context.Context, dir string, ref prref.PRRef) (review.Result, error)
	}
)

// Action is what the sweep did about one pull request.
type Action string

const (
	ActionReview         Action = "review"
	ActionSkip           Action = "skip"
	ActionDefer          Action = "defer"
	ActionNeedsAttention Action = "needs_attention"
	ActionWouldReview    Action = "would_review"
)

// inFlightReason is the single wording for "we found a record from a run that
// died part-way through a review".
const inFlightReason = "previous run died mid-review"

// ExitUnknown is recorded as a review's exit code when the review produced no
// exit status at all -- killed by its deadline, or never started. A persisted
// zero would read as a clean success in `firstpass status`.
const ExitUnknown = -1

// inFlightDetail is the human-facing note stored on a recovered record.
func inFlightDetail(ref prref.PRRef) string {
	return "a previous run died mid-review, so comments may already be posted; " +
		"run `firstpass replay " + ref.URL() + "` to review it again deliberately"
}

// Decision is one line of the sweep's report.
type Decision struct {
	Ref    prref.PRRef
	Action Action
	Reason string
}

// SweepReport summarises one sweep.
type SweepReport struct {
	MessagesScanned int
	ColdStart       bool
	Paused          bool

	// WatermarkGap reports that a watermark was set but was not inside the
	// fetched window, so every message between it and the oldest message
	// fetched went unscanned. The watermark is deliberately not advanced when
	// this is set: re-scanning and relying on the store's dedupe is far
	// cheaper than skipping messages permanently and silently.
	WatermarkGap bool

	Decisions []Decision
	// Reviewed counts successful reviews only. A later task renders this
	// field, so its meaning must not change.
	Reviewed int

	// reviewAttempts counts every Rev.Run invocation, successful or not, and
	// is what the per-sweep cap actually bounds. Reviewed alone cannot bound
	// it: a run of failures never increments Reviewed, so the cap would never
	// trip while reviews are failing -- precisely when a throttle matters
	// most, since a failed run may already have posted comments.
	reviewAttempts int

	// recordFailed is set when any store write for this batch failed. The
	// spec advances the watermark only once the entire batch is recorded, so
	// a swallowed write failure must still hold it back: the alternative is a
	// PR that was decided, not recorded, and then scrolled past the window.
	recordFailed bool

	// pausedMidSweep latches once the pre-review pause check has fired, so
	// the remaining candidates are parked at the top of handle rather than
	// each one being inspected and cloned only to be turned away. Paused
	// stays the sweep-start observation the report renders.
	pausedMidSweep bool

	// recovered holds the store keys whose in_flight record this sweep
	// converted to needs_attention before considering any candidate.
	recovered map[string]bool
}

// Options tune a single sweep.
type Options struct {
	// PrintOnly reports each PR's decision without writing any state: no
	// review record, no pending entry, no attempt counted. It still queries
	// GitHub, because reporting a PR's decision and reason requires knowing
	// its state.
	PrintOnly bool
	// Backfill takes the last N messages and ignores the watermark.
	Backfill int

	// noEnqueue suppresses the pending writes that handle's defer paths make.
	// ReviewOne sets it, and only ReviewOne: the operator asked for one named
	// PR and is watching the result, so a failure must be reported back to
	// them rather than parked in pending -- which candidates() re-offers on
	// every sweep, independent of the watermark, turning a deliberate one-off
	// into an unattended automatic review.
	noEnqueue bool
}

// Pipeline runs sweeps.
type Pipeline struct {
	Cfg   config.Config
	Store *store.Store
	Chat  ChatSource
	PRs   PRInspector
	WTs   Worktrees
	Rev   Reviewer
	Log   *slog.Logger
	Now   func() time.Time
}

type candidate struct {
	ref     prref.PRRef
	trigger string
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

func (p *Pipeline) paused() bool {
	_, err := os.Stat(p.Cfg.PauseFile())
	return err == nil
}

// Sweep reads the space once and acts on whatever it finds.
func (p *Pipeline) Sweep(ctx context.Context, opts Options) (SweepReport, error) {
	rep := SweepReport{Paused: p.paused()}

	wm, hasWM, err := p.Store.Watermark()
	if err != nil {
		return rep, err
	}

	since, limit := wm.MessageName, p.Cfg.FetchLimit
	if opts.Backfill > 0 {
		since, limit = "", opts.Backfill
	}

	// chat.py drives a network call and can hang; an unattended daemon needs a
	// bound on it, not context.Background.
	fctx, cancelFetch := context.WithTimeout(ctx, p.Cfg.ChatTimeout.D())
	msgs, foundSince, err := p.Chat.Fetch(fctx, since, limit)
	cancelFetch()
	if err != nil {
		return rep, err
	}
	rep.MessagesScanned = len(msgs)

	// Cold start. A first run against a populated space must review nothing:
	// otherwise launch day sweeps months of history and comments on PRs that
	// were merged long ago.
	if !hasWM && opts.Backfill == 0 {
		rep.ColdStart = true
		if len(msgs) > 0 && !opts.PrintOnly {
			if err := p.setWatermark(msgs[0]); err != nil {
				return rep, err
			}
		}
		return rep, nil
	}

	// The watermark fell out of the window: chat.py returned everything it had,
	// which is indistinguishable from "all of these are new" unless the gap is
	// reported. Advancing the watermark here would skip every message between
	// the old watermark and the oldest one fetched, with no log line and no
	// pending entry -- a laptop closed over a weekend is enough to trigger it.
	//
	// An empty sinceName (cold start or backfill) is not a gap. Neither is an
	// empty window: with no messages fetched at all there is nothing between
	// the watermark and "the oldest message fetched", and a loud warning on a
	// quiet space would only train the operator to ignore it.
	if since != "" && !foundSince && len(msgs) > 0 {
		rep.WatermarkGap = true
		oldest := msgs[len(msgs)-1].Name
		p.Log.Warn("watermark not in the fetched window: messages between it and the oldest "+
			"message fetched were not scanned; holding the watermark so the next sweep re-scans",
			"watermark", since, "oldest_fetched", oldest, "fetch_limit", limit)
	}

	if err := p.recoverInFlight(&rep, opts); err != nil {
		return rep, err
	}

	interrupted := false
	for _, c := range p.candidates(msgs) {
		// Ctrl-C mid-sweep used to keep iterating: every remaining candidate
		// burned a pending attempt on the cancelled-context Inspect failure,
		// and then the watermark advanced over the lot.
		if ctx.Err() != nil {
			interrupted = true
			p.Log.Warn("sweep interrupted; holding the watermark", "err", ctx.Err())
			break
		}
		rep.Decisions = append(rep.Decisions, p.handle(ctx, c, &rep, opts))
	}

	p.appendRecoveredDecisions(&rep)

	if interrupted {
		return rep, ctx.Err()
	}

	switch {
	case opts.PrintOnly, opts.Backfill > 0, len(msgs) == 0:
		// Nothing to advance, or nothing may be written.
	case rep.WatermarkGap:
		// Already warned about above; re-scan next sweep instead.
	case rep.recordFailed:
		p.Log.Warn("holding the watermark: part of this batch could not be recorded, " +
			"so the next sweep must see these messages again")
	default:
		if err := p.setWatermark(msgs[0]); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func (p *Pipeline) setWatermark(m chat.Message) error {
	return p.Store.SetWatermark(store.Watermark{MessageName: m.Name, CreateTime: m.CreateTime})
}

// recoverInFlight converts every in_flight record left behind by a dead run
// into needs_attention, before any candidate is considered.
//
// Sweeps are serial and in-process, so an in_flight record present at sweep
// start is by definition from a run that is no longer alive. Driving this from
// the store rather than from the candidate list is what makes the recovery
// reliable: the per-candidate gate in handle only fires if the ref happens to
// reappear as a candidate, which stops happening once the fetch window has
// moved past the triggering message, and never happens at all for a `firstpass
// replay` that died mid-review -- leaving the record in_flight forever and the
// PR invisible in every report.
func (p *Pipeline) recoverInFlight(rep *SweepReport, opts Options) error {
	if opts.PrintOnly {
		// Print-only writes nothing. handle's per-candidate gate still reports
		// such a record as needs_attention without touching it.
		return nil
	}
	recs, err := p.Store.Reviews()
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if rec.Outcome.Terminal() {
			continue
		}
		ref, perr := prref.ParseKey(rec.Key)
		if perr != nil {
			p.Log.Error("unparseable review key", "key", rec.Key, "err", perr)
			continue
		}
		rec.Outcome = store.OutcomeNeedsAttention
		rec.DecidedAt = p.now()
		rec.Detail = inFlightDetail(ref)
		if rec.ExitCode == 0 {
			rec.ExitCode = ExitUnknown
		}
		if err := p.Store.PutReview(rec); err != nil {
			p.Log.Error("put review", "key", rec.Key, "err", err)
			rep.recordFailed = true
			continue
		}
		if err := p.Store.DeletePending(rec.Key); err != nil {
			p.Log.Error("delete pending", "key", rec.Key, "err", err)
			rep.recordFailed = true
		}
		if rep.recovered == nil {
			rep.recovered = map[string]bool{}
		}
		rep.recovered[rec.Key] = true
		p.Log.Warn("needs attention", "key", rec.Key, "reason", inFlightReason)
	}
	return nil
}

// appendRecoveredDecisions gives a line in the report to every record this
// sweep recovered whose ref never turned up as a candidate -- the case the old
// candidate-driven recovery could not see at all.
func (p *Pipeline) appendRecoveredDecisions(rep *SweepReport) {
	if len(rep.recovered) == 0 {
		return
	}
	seen := map[string]bool{}
	for _, d := range rep.Decisions {
		seen[d.Ref.Key()] = true
	}
	keys := make([]string, 0, len(rep.recovered))
	for k := range rep.recovered {
		keys = append(keys, k)
	}
	sort.Strings(keys) // bbolt iteration order is stable, but be explicit
	for _, k := range keys {
		if seen[k] {
			continue
		}
		ref, err := prref.ParseKey(k)
		if err != nil {
			continue
		}
		rep.Decisions = append(rep.Decisions, Decision{
			Ref: ref, Action: ActionNeedsAttention, Reason: inFlightReason,
		})
	}
}

// candidates lists the refs to consider: everything in the new messages,
// walked oldest-first so the earliest post is recorded as the trigger, followed
// by refs still parked in the pending bucket.
func (p *Pipeline) candidates(msgs []chat.Message) []candidate {
	var out []candidate
	seen := map[string]bool{}

	for i := len(msgs) - 1; i >= 0; i-- {
		for _, ref := range prref.Extract(msgs[i].Text) {
			if seen[ref.Key()] {
				continue
			}
			seen[ref.Key()] = true
			out = append(out, candidate{ref: ref, trigger: msgs[i].Name})
		}
	}

	pend, err := p.Store.AllPending()
	if err != nil {
		p.Log.Error("read pending", "err", err)
		return out
	}
	for _, pd := range pend {
		if seen[pd.Key] {
			continue
		}
		ref, err := prref.ParseKey(pd.Key)
		if err != nil {
			p.Log.Error("unparseable pending key", "key", pd.Key, "err", err)
			continue
		}
		seen[pd.Key] = true
		out = append(out, candidate{ref: ref})
	}
	return out
}

// handle applies the decision table to one candidate. The order of the checks
// is load-bearing; see the comments at each gate.
func (p *Pipeline) handle(ctx context.Context, c candidate, rep *SweepReport, opts Options) Decision {
	ref := c.ref
	dec := func(a Action, reason string) Decision {
		return Decision{Ref: ref, Action: a, Reason: reason}
	}
	// note records that a store write for this candidate failed. The decision
	// still stands, but the watermark must not move past the batch.
	note := func(err error) {
		if err != nil {
			rep.recordFailed = true
		}
	}

	// Owner first: a repo outside the allowlist must never be queried, let
	// alone cloned. The space is a chat room, so unrelated links do turn up.
	if !p.Cfg.OwnerAllowed(ref.Owner) {
		note(p.terminal(ref, store.OutcomeSkippedOwner, c.trigger, "owner not in allow_owners", opts))
		return dec(ActionSkip, "owner not allowed")
	}
	if p.Cfg.RepoDenied(ref.Owner, ref.Repo) {
		note(p.terminal(ref, store.OutcomeSkippedRepo, c.trigger, "repo in deny_repos", opts))
		return dec(ActionSkip, "repo in deny_repos")
	}

	// The existing record comes next, so a run that died mid-post is converted
	// before any other rule can send this PR back through a review.
	if prev, ok, err := p.Store.Review(ref.Key()); err != nil {
		p.Log.Error("read review", "key", ref.Key(), "err", err)
		// A read failure must still park the ref -- otherwise it falls out of
		// every bucket while the watermark advances past the message that
		// produced it, and it is never seen again.
		note(p.hold(ref, "store read failed", opts))
		return dec(ActionDefer, "store read failed")
	} else if ok {
		if !prev.Outcome.Terminal() {
			// A writing sweep has already converted this in recoverInFlight,
			// so this gate is now reached only by print-only runs (which must
			// touch nothing) and by ReviewOne. Kept because it is the cheaper
			// of the two paths and because it must not regress.
			if !opts.PrintOnly {
				prev.Outcome = store.OutcomeNeedsAttention
				prev.DecidedAt = p.now()
				prev.Detail = inFlightDetail(ref)
				if prev.ExitCode == 0 {
					prev.ExitCode = ExitUnknown
				}
				if err := p.Store.PutReview(prev); err != nil {
					p.Log.Error("put review", "key", ref.Key(), "err", err)
					note(err)
				}
				if err := p.Store.DeletePending(ref.Key()); err != nil {
					p.Log.Error("delete pending", "key", ref.Key(), "err", err)
					note(err)
				}
				p.Log.Warn("needs attention", "key", ref.Key(), "reason", inFlightReason)
			}
			return dec(ActionNeedsAttention, inFlightReason)
		}
		if rep.recovered[ref.Key()] {
			// Converted moments ago by this very sweep. Reporting "already
			// decided" here would read as a PR that was dealt with cleanly.
			return dec(ActionNeedsAttention, inFlightReason)
		}
		return dec(ActionSkip, "already decided: "+string(prev.Outcome))
	}

	// Pause first: a paused sweep must park every ref without even asking
	// whether it has aged out. expirePending must not run during a pause, or
	// PendingMaxAge (168h by default -- exactly one week) would silently void
	// the whole backlog exactly when the pause exists to protect it.
	if rep.Paused || rep.pausedMidSweep {
		note(p.hold(ref, "paused", opts))
		return dec(ActionDefer, "paused")
	}

	expired, err := p.expirePending(ref, opts)
	note(err)
	if expired {
		return dec(ActionSkip, "pending expired")
	}

	// The cap parks the ref without counting an attempt: hitting it is not a
	// failure, and letting it burn attempts would expire a backlog over
	// nothing but bad luck in scheduling.
	if rep.reviewAttempts >= p.Cfg.MaxReviewsPerSweep {
		note(p.hold(ref, "per-sweep cap reached", opts))
		return dec(ActionDefer, "per-sweep cap reached")
	}

	ictx, cancelInspect := context.WithTimeout(ctx, p.Cfg.GHTimeout.D())
	info, err := p.PRs.Inspect(ictx, ref)
	cancelInspect()
	if err != nil {
		note(p.deferAttempt(ref, "inspect failed: "+err.Error(), opts))
		return dec(ActionDefer, "inspect failed: "+err.Error())
	}
	if info.State != "OPEN" {
		note(p.terminal(ref, store.OutcomeSkippedState, c.trigger, "state "+info.State, opts))
		return dec(ActionSkip, "state "+info.State)
	}
	if info.IsDraft {
		// Deferred, not terminal: a draft is routinely marked ready later, and
		// by then the message has scrolled past the watermark.
		note(p.deferAttempt(ref, "draft", opts))
		return dec(ActionDefer, "draft")
	}
	if strings.EqualFold(info.Author, p.Cfg.GithubLogin) {
		note(p.terminal(ref, store.OutcomeSkippedAuthor, c.trigger, "authored by "+info.Author, opts))
		return dec(ActionSkip, "own PR")
	}

	if opts.PrintOnly {
		return dec(ActionWouldReview, "OPEN, not draft, author "+info.Author)
	}

	// A bare clone of a whole repository is the longest subprocess firstpass
	// runs, and on Windows a credential prompt can stall it indefinitely.
	wctx, cancelPrepare := context.WithTimeout(ctx, p.Cfg.CloneTimeout.D())
	dir, cleanup, err := p.WTs.Prepare(wctx, ref)
	cancelPrepare()
	if err != nil {
		note(p.deferAttempt(ref, "worktree failed: "+err.Error(), opts))
		return dec(ActionDefer, "worktree failed: "+err.Error())
	}
	defer cleanup()

	// The pause file is re-read here, not just at sweep start. A sweep can run
	// for the better part of an hour (three reviews at a twenty-minute
	// timeout), and a kill switch for a tool that writes to other people's
	// pull requests has to take effect on the next review, not the next sweep.
	// No attempt is counted: a pause is not a failure of this PR.
	if p.paused() {
		rep.pausedMidSweep = true
		note(p.hold(ref, "paused after the worktree was prepared", opts))
		return dec(ActionDefer, "paused before the review started")
	}

	started := p.now()
	rec := store.Review{
		Key:            ref.Key(),
		Outcome:        store.OutcomeInFlight,
		HeadSHA:        info.HeadSHA,
		TriggerMessage: c.trigger,
		StartedAt:      started,
	}
	// Written before claude starts: this record is the only evidence that a
	// review was underway if the process dies while posting comments.
	if err := p.Store.PutReview(rec); err != nil {
		p.Log.Error("record in_flight", "key", ref.Key(), "err", err)
		note(err)
		// The worktree was already prepared -- a clone happened -- so this
		// must still park the ref, or it is discarded with nothing to show
		// for the clone and never reconsidered.
		note(p.hold(ref, "could not record in_flight", opts))
		return dec(ActionDefer, "could not record in_flight")
	}

	rctx, cancel := context.WithTimeout(ctx, p.Cfg.ReviewTimeout.D())
	defer cancel()

	// Counts every invocation, not just successes: this is what the per-sweep
	// cap bounds, so a run of failures cannot make Rev.Run fire for every
	// candidate in the batch.
	rep.reviewAttempts++
	res, rerr := p.Rev.Run(rctx, dir, ref)
	done := p.now()

	rec.DecidedAt = done
	rec.DurationMS = done.Sub(started).Milliseconds()
	rec.ExitCode = res.ExitCode
	rec.ReportPath = res.ReportPath

	if rerr != nil {
		if res.ExitCode == 0 {
			// A review killed by its deadline never reported an exit status,
			// and a persisted 0 would read as a clean success in `status`.
			rec.ExitCode = ExitUnknown
		}
		rec.Outcome = store.OutcomeNeedsAttention
		rec.Detail = "review did not finish (" + rerr.Error() + "); comments may be partially posted, " +
			"so it will not be retried automatically"
		if err := p.Store.PutReview(rec); err != nil {
			p.Log.Error("put review", "key", ref.Key(), "err", err)
			note(err)
		}
		if err := p.Store.DeletePending(ref.Key()); err != nil {
			p.Log.Error("delete pending", "key", ref.Key(), "err", err)
			note(err)
		}
		p.Log.Warn("needs attention", "key", ref.Key(), "err", rerr)
		return dec(ActionNeedsAttention, "review did not finish: "+rerr.Error())
	}

	rec.Outcome = store.OutcomeReviewed
	if err := p.Store.PutReview(rec); err != nil {
		p.Log.Error("put review", "key", ref.Key(), "err", err)
		note(err)
	}
	if err := p.Store.DeletePending(ref.Key()); err != nil {
		p.Log.Error("delete pending", "key", ref.Key(), "err", err)
		note(err)
	}
	rep.Reviewed++
	p.Log.Info("reviewed", "key", ref.Key(), "ms", rec.DurationMS, "report", rec.ReportPath)
	return dec(ActionReview, "reviewed")
}

// ReviewOne reviews a single pull request on demand, clearing any record that
// would otherwise skip it. The owner allowlist and deny list still apply —
// replay is a way past the dedupe, not past the safety rails — but the
// per-sweep cap does not, since the user asked for this one explicitly.
func (p *Pipeline) ReviewOne(ctx context.Context, ref prref.PRRef, opts Options) (Decision, error) {
	// Checked before anything is deleted. A paused replay used to delete the
	// review record -- including a needs_attention record's "comments may
	// already be partially posted" detail -- and then park the ref in pending,
	// which candidates() re-offers on every sweep regardless of the watermark.
	// The operator saw "defer / paused" and "0 reviewed", read that as
	// "nothing happened", and the first sweep after `firstpass resume` reviewed
	// it with no further request -- double-posting on top of whatever the
	// earlier run had already left on a colleague's PR.
	if p.paused() {
		return Decision{Ref: ref}, fmt.Errorf(
			"firstpass is paused (%s), so nothing was changed: run `firstpass resume` before replaying %s",
			p.Cfg.PauseFile(), ref.URL())
	}

	if err := p.Store.DeleteReview(ref.Key()); err != nil {
		return Decision{Ref: ref}, err
	}
	if err := p.Store.DeletePending(ref.Key()); err != nil {
		return Decision{Ref: ref}, err
	}

	// An explicit replay must never queue itself for the automatic path.
	opts.noEnqueue = true

	// The per-sweep cap needs no adjustment: a replay starts with zero
	// attempts and Validate forbids a cap of zero, so the gate cannot trip.
	rep := SweepReport{}
	return p.handle(ctx, candidate{ref: ref, trigger: "replay"}, &rep, opts), nil
}

// terminal closes the book on a ref and clears any pending entry for it. In
// print-only mode it writes nothing: the caller still gets the decision that
// would result, but no state changes. A failed write is logged and returned,
// so the sweep can hold the watermark rather than move past an unrecorded
// decision.
func (p *Pipeline) terminal(ref prref.PRRef, o store.Outcome, trigger, detail string, opts Options) error {
	if opts.PrintOnly {
		return nil
	}
	if err := p.Store.PutReview(store.Review{
		Key:            ref.Key(),
		Outcome:        o,
		TriggerMessage: trigger,
		DecidedAt:      p.now(),
		Detail:         detail,
	}); err != nil {
		p.Log.Error("put review", "key", ref.Key(), "err", err)
		return err
	}
	if err := p.Store.DeletePending(ref.Key()); err != nil {
		p.Log.Error("delete pending", "key", ref.Key(), "err", err)
		return err
	}
	return nil
}

// hold parks a ref for a later sweep without counting an attempt.
func (p *Pipeline) hold(ref prref.PRRef, reason string, opts Options) error {
	return p.upsertPending(ref, reason, false, opts)
}

// deferAttempt parks a ref and counts an attempt against its expiry budget.
func (p *Pipeline) deferAttempt(ref prref.PRRef, reason string, opts Options) error {
	return p.upsertPending(ref, reason, true, opts)
}

// upsertPending writes nothing in print-only mode, so a dry run cannot burn
// attempt budget or otherwise leave a mark on the store, and nothing in
// noEnqueue mode, so an explicit replay never queues itself.
func (p *Pipeline) upsertPending(ref prref.PRRef, reason string, countAttempt bool, opts Options) error {
	if opts.PrintOnly || opts.noEnqueue {
		return nil
	}
	pd, ok, err := p.Store.Pending(ref.Key())
	if err != nil {
		p.Log.Error("read pending", "key", ref.Key(), "err", err)
		return err
	}
	if !ok {
		pd = store.Pending{Key: ref.Key(), FirstSeen: p.now()}
	}
	if countAttempt {
		pd.Attempts++
		pd.LastAttempt = p.now()
	}
	pd.LastReason = reason
	if err := p.Store.PutPending(pd); err != nil {
		p.Log.Error("put pending", "key", ref.Key(), "err", err)
		return err
	}
	return nil
}

// expirePending retires a ref that has been retried too often or waited too
// long, so a PR abandoned in draft is not re-inspected forever. It still
// reads and evaluates the pending entry in print-only mode -- the caller
// needs the true decision -- but terminal() (called with the same opts) is
// what suppresses the write.
func (p *Pipeline) expirePending(ref prref.PRRef, opts Options) (bool, error) {
	pd, ok, err := p.Store.Pending(ref.Key())
	if err != nil || !ok {
		return false, nil
	}
	age := p.now().Sub(pd.FirstSeen)
	tooMany := pd.Attempts >= p.Cfg.PendingMaxAttempts
	tooOld := age > p.Cfg.PendingMaxAge.D()
	if !tooMany && !tooOld {
		return false, nil
	}

	reason := "expired after " + strconv.Itoa(pd.Attempts) + " attempts"
	if tooOld {
		reason = "expired after " + age.Round(time.Hour).String() + " pending"
	}
	p.Log.Warn("pending expired", "key", pd.Key, "reason", reason, "last", pd.LastReason)
	return true, p.terminal(ref, store.OutcomeExpired, "", reason, opts)
}
