package pipeline

import (
	"context"
	"slices"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/store"
)

// The three reactions firstpass puts on a chat message that carried pull
// request links. They are per message, not per pull request: one post
// routinely carries several links and the reviews run strictly one at a time,
// so a per-PR reaction would say nothing a reader could act on.
//
//	👀  at least one of this message's pull requests is being reviewed now
//	✅  every one of them is finished, and every review came out reviewed
//	💬  every one of them is finished, and at least one did not
const (
	EmojiWatching = "👀"
	EmojiClean    = "✅"
	EmojiFindings = "💬"
)

// reactionsEnabled is the one gate every reaction goes through.
//
// dry_run is an absolute "no outward effect" switch and a chat space is
// outward, so a dry run reacts to nothing -- and records no reaction state
// either, which keeps "a dry run left no trace of a reaction" a single
// property rather than a claim about three separate call sites. Print-only is
// the same on both counts, and additionally writes nothing to the store at
// all. A nil React is the third case: `firstpass status` and `doctor` wire no
// reactor, and cmd wires none in dry run.
func (p *Pipeline) reactionsEnabled(opts Options) bool {
	switch {
	case p.React == nil:
		return false
	case p.Cfg.DryRun:
		return false
	case opts.PrintOnly:
		return false
	}
	return true
}

// reactionCtx bounds one chat.py call. chat.py drives a network call and can
// hang; an unattended daemon needs the same bound here that Fetch already has.
func (p *Pipeline) reactionCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.Cfg.ChatTimeout.D())
}

// recordMessages remembers which pull requests each chat message carried.
//
// The result reaction cannot be worked out from a review alone: it belongs to
// the message, and is only right once every pull request that message carried
// has reached a terminal outcome. That can be days later, by which time the
// message has scrolled out of the fetch window and the daemon has very likely
// been restarted, so the ref list has to be on disk and keyed by the message
// name.
//
// A failed write here is logged and nothing more. It is deliberately not
// rep.recordFailed: that holds the watermark, and a whole batch of messages
// being re-offered because a cosmetic reaction could not be remembered is a
// far worse trade than a missing reaction.
func (p *Pipeline) recordMessages(msgs []chat.Message, opts Options) {
	if !p.reactionsEnabled(opts) {
		return
	}
	for _, m := range msgs {
		refs := prref.Extract(m.Text)
		if len(refs) == 0 {
			continue
		}
		keys := make([]string, 0, len(refs))
		for _, r := range refs {
			// Extract already de-duplicates within one message.
			keys = append(keys, r.Key())
		}

		rec, ok, err := p.Store.Message(m.Name)
		if err != nil {
			p.Log.Error("read message record", "message", m.Name, "err", err)
			continue
		}
		if ok && slices.Equal(rec.RefKeys, keys) {
			// Nothing to say. Every sweep re-reads the whole fetch window, so
			// without this the common case is fetch_limit bbolt write
			// transactions -- one fsync each -- rewriting rows byte for byte
			// every interval, for the lifetime of the daemon.
			continue
		}
		if !ok {
			rec = store.MessageRecord{Name: m.Name, FirstSeen: p.now()}
		}
		// Replaced rather than merged. The refs a message carries come from
		// its text, so re-reading the same message yields the same list; if an
		// edit ever removed a link, the message must not go on waiting forever
		// for a pull request it no longer mentions. The reaction fields are
		// untouched either way, which is what stops a re-offered message being
		// reacted to a second time.
		rec.RefKeys = keys
		if err := p.Store.PutMessage(rec); err != nil {
			p.Log.Error("record message", "message", m.Name, "err", err)
		}
	}
}

// startMessageReaction puts 👀 on the message that triggered this review, and
// is called immediately before the first Rev.Run for that message. The point
// of the feature is telling the team a post has been picked up, so a reaction
// that lands after claude has finished would be worthless.
//
// Every failure is logged and nothing else. A reaction is cosmetic: no
// review's outcome, no pending entry and no later review depends on it.
func (p *Pipeline) startMessageReaction(ctx context.Context, trigger string, opts Options) {
	if !p.reactionsEnabled(opts) || trigger == "" {
		return
	}
	if ctx.Err() != nil {
		// The context is already done -- a Ctrl-C during a review, which is
		// the ordinary way to stop the daemon, and reviews run for up to
		// thirty minutes. Sweep guards its own end-of-sweep pass for exactly
		// this reason; these inline calls from handle need it just as much.
		//
		// Returning here rather than trying and failing is not an
		// optimisation. Each stage latches its intent in the store before the
		// call, so a crash cannot produce a second reaction -- but on a dead
		// context the call cannot possibly succeed, so latching first would
		// convert a transient interrupt into a permanent one: the message
		// would keep 👀 for good, never get its result, and never be looked at
		// again, because the latch says it is finished. Not reacting at all
		// leaves the work for the next sweep.
		return
	}
	rec, ok, err := p.Store.Message(trigger)
	if err != nil {
		p.Log.Error("read message record", "message", trigger, "err", err)
		return
	}
	if !ok {
		// No record for this trigger, and that is deliberate rather than
		// incidental. Two candidates get here: a ref re-offered from the
		// pending bucket, which candidates() gives no trigger at all, and a
		// `firstpass replay`, whose trigger is the literal "replay" and not a
		// message name. Neither identifies a chat message, and there is
		// nothing to react to without one.
		return
	}
	if rec.WatchApplied || rec.ResultApplied {
		// Once per message per stage. Reviews of one message's pull requests
		// run one after another, and a backfill or a watermark gap re-offers
		// the same message outright.
		return
	}

	// Marked before the call, not after. An outward act that might be repeated
	// is worse than one that is occasionally missed, and this is the same
	// discipline as writing a review record as in_flight before claude starts:
	// the store remembers the intent, so a crash or a failure cannot turn into
	// a second reaction.
	//
	// What that trades away is a reaction lost to a transient failure, which
	// is why the one transient failure that is not rare -- an interrupt -- is
	// caught by the context guard above rather than allowed to reach here.
	rec.WatchApplied = true
	if err := p.Store.PutMessage(rec); err != nil {
		p.Log.Error("record watching reaction", "message", trigger, "err", err)
		return
	}

	rctx, cancel := p.reactionCtx(ctx)
	name, err := p.React.AddReaction(rctx, trigger, EmojiWatching)
	cancel()
	if err != nil {
		p.Log.Warn("could not add the watching reaction; the review is unaffected",
			"message", trigger, "err", err)
		return
	}
	rec.WatchReaction = name
	if err := p.Store.PutMessage(rec); err != nil {
		// The reaction is on the message but its name is not on disk, so it
		// can no longer be removed. Cosmetic, and strictly better than not
		// having reacted at all.
		p.Log.Error("record watching reaction name", "message", trigger, "reaction", name, "err", err)
	}
}

// settleMessageReaction adds the result reaction to one message and takes the
// 👀 off, once every pull request that message carried has reached a terminal
// outcome. It does nothing until then, and nothing at all if none of them was
// ever reviewed.
//
// ✅ versus 💬 is decided by the verdict firstpass submitted on each pull
// request, which is the only signal that actually distinguishes "clean" from
// "has findings": the findings themselves are inline comments on the pull
// request, and the pipeline never sees one. ✅ means every pull request this
// message carried *that firstpass reviewed* was approved. Anything else is 💬.
//
// Two asymmetries are deliberate.
//
// Refs firstpass never reviewed -- skipped for their owner, the deny list,
// their state, their author, or retired by pending expiry -- are left out of
// the decision entirely. A skip is not a finding: nothing was wrong with the
// code, firstpass simply had no business reviewing it, so a message carrying
// one approved review and one merged link is clean.
//
// Everything short of an outright approval is 💬. store.VerdictApproved is
// only ever set when a submission actually succeeded, so a findings verdict, a
// reviewer that printed no verdict line, a submission that failed, and a
// review that did not finish at all are all "firstpass does not know that this
// is clean" -- and not knowing must never be rendered as ✅. A misleading tick
// on a pull request with twenty comments waiting is worse than no reaction.
func (p *Pipeline) settleMessageReaction(ctx context.Context, trigger string, opts Options) {
	if !p.reactionsEnabled(opts) || trigger == "" {
		return
	}
	if ctx.Err() != nil {
		// The context is already done -- a Ctrl-C during a review, which is
		// the ordinary way to stop the daemon, and reviews run for up to
		// thirty minutes. Sweep guards its own end-of-sweep pass for exactly
		// this reason; these inline calls from handle need it just as much.
		//
		// Returning here rather than trying and failing is not an
		// optimisation. Each stage latches its intent in the store before the
		// call, so a crash cannot produce a second reaction -- but on a dead
		// context the call cannot possibly succeed, so latching first would
		// convert a transient interrupt into a permanent one: the message
		// would keep 👀 for good, never get its result, and never be looked at
		// again, because the latch says it is finished. Not reacting at all
		// leaves the work for the next sweep.
		return
	}
	rec, ok, err := p.Store.Message(trigger)
	if err != nil {
		p.Log.Error("read message record", "message", trigger, "err", err)
		return
	}
	if !ok {
		return
	}
	if rec.ResultApplied {
		return // once per message per stage
	}
	if !rec.WatchApplied {
		// Nothing this message carried was ever reviewed: every ref was
		// skipped for its owner, the deny list, its state, its author, being a
		// draft, or was already decided. There is no result to report, and a
		// bare ✅ on a message firstpass never acted on would be a lie.
		return
	}
	if len(rec.RefKeys) == 0 {
		return
	}

	reviewed, clean := 0, true
	for _, key := range rec.RefKeys {
		r, found, err := p.Store.Review(key)
		if err != nil {
			p.Log.Error("read review", "key", key, "err", err)
			return
		}
		if !found || !r.Outcome.Terminal() {
			// Still being worked through. in_flight is the only non-terminal
			// outcome, so this is either a review running right now or one of
			// the message's refs parked in pending. Every ref has to be
			// finished before the message is: that is what makes one result
			// reaction per message possible at all.
			return
		}
		switch r.Outcome {
		case store.OutcomeSkippedAuthor, store.OutcomeSkippedState,
			store.OutcomeSkippedOwner, store.OutcomeSkippedRepo, store.OutcomeExpired:
			// Never reviewed, so it says nothing about the code either way.
		case store.OutcomeReviewed:
			reviewed++
			if r.Verdict != store.VerdictApproved {
				clean = false
			}
		default:
			// needs_attention today, and whatever is added later. A review
			// that may have posted half a comment set is not a clean bill of
			// health, and an outcome this switch does not recognise must never
			// be given the optimistic reading -- the cost of being wrong here
			// is a ✅ nobody earned.
			reviewed++
			clean = false
		}
	}
	if reviewed == 0 {
		// Nothing in this message's current ref list was ever reviewed, so
		// there is no result to report -- the same reasoning as the
		// WatchApplied guard above, and the loop's own vacuous "clean" is
		// exactly why it has to be said separately. The two can disagree: the
		// ref list is re-read from the message text every sweep, and a chat
		// message that is edited after its review started ends up with a 👀 on
		// it and a ref list holding nothing firstpass reviewed.
		return
	}

	emoji := EmojiFindings
	if clean {
		emoji = EmojiClean
	}

	// Marked before the call, for the reason given in startMessageReaction --
	// including why an already-cancelled context is turned away above instead
	// of being latched and then failed.
	rec.ResultApplied = true
	if err := p.Store.PutMessage(rec); err != nil {
		p.Log.Error("record result reaction", "message", trigger, "err", err)
		return
	}

	rctx, cancel := p.reactionCtx(ctx)
	name, err := p.React.AddReaction(rctx, trigger, emoji)
	cancel()
	if err != nil {
		p.Log.Warn("could not add the result reaction; the reviews are unaffected",
			"message", trigger, "emoji", emoji, "err", err)
		return
	}
	rec.ResultReaction = name
	if err := p.Store.PutMessage(rec); err != nil {
		p.Log.Error("record result reaction name", "message", trigger, "reaction", name, "err", err)
	}

	// The 👀 comes off only after the result is on: a message with neither
	// reaction reads as one firstpass never noticed, which is the one thing
	// this feature exists to prevent.
	if rec.WatchReaction == "" {
		return
	}
	rctx, cancel = p.reactionCtx(ctx)
	err = p.React.RemoveReaction(rctx, rec.WatchReaction)
	cancel()
	if err != nil {
		p.Log.Warn("could not remove the watching reaction; the result reaction is on the message",
			"message", trigger, "reaction", rec.WatchReaction, "err", err)
		return
	}
	rec.WatchReaction = ""
	if err := p.Store.PutMessage(rec); err != nil {
		p.Log.Error("clear watching reaction name", "message", trigger, "err", err)
	}
}

// settleOutstandingReactions is the end-of-sweep catch-all: every message
// still waiting on a result reaction is offered one.
//
// Settling from the candidate that finished last is not enough on its own. The
// last of a message's pull requests can be decided by a candidate that carries
// no trigger -- one re-offered from the pending bucket, or a replay -- or by a
// process that died before it could react, or in a sweep where that message is
// long past the fetch window. Driving it from the store instead is what makes
// the reaction arrive at all in those cases, and it is the same reasoning that
// moved in_flight recovery out of the candidate loop.
//
// Every stage is idempotent, so running both this and the per-review settle
// costs a store read and cannot react twice.
func (p *Pipeline) settleOutstandingReactions(ctx context.Context, opts Options) {
	if !p.reactionsEnabled(opts) {
		return
	}
	recs, err := p.Store.AllMessages()
	if err != nil {
		p.Log.Error("read message records", "err", err)
		return
	}
	for _, rec := range recs {
		if rec.ResultApplied || !rec.WatchApplied {
			continue
		}
		p.settleMessageReaction(ctx, rec.Name, opts)
	}
}
