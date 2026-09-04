package pipeline

import (
	"context"

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
		if !ok {
			rec = store.MessageRecord{Name: m.Name, FirstSeen: p.now()}
		}
		// Replaced rather than merged. The refs a message carries come from
		// its text, so re-reading the same message yields the same list; if an
		// edit ever removed a link, the message must not go on waiting forever
		// for a pull request it no longer mentions.
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
// ✅ versus 💬 is decided by the recorded outcomes: ✅ only when every one of
// the message's refs came out store.OutcomeReviewed, 💬 otherwise. On this
// branch that is the strongest signal available -- a review's findings live in
// claude's free-text output or on the pull request itself, and neither is
// something the pipeline can read. It therefore means "every one of these was
// reviewed without incident" rather than "no findings", and 💬 means "at least
// one of these wants a human's eye", which covers a review that did not finish
// as much as one that produced comments. Once the review-verdict work lands,
// this is the one place to re-point at the verdict.
func (p *Pipeline) settleMessageReaction(ctx context.Context, trigger string, opts Options) {
	if !p.reactionsEnabled(opts) || trigger == "" {
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

	clean := true
	for _, key := range rec.RefKeys {
		r, found, err := p.Store.Review(key)
		if err != nil {
			p.Log.Error("read review", "key", key, "err", err)
			return
		}
		if !found || !r.Outcome.Terminal() {
			// Still being worked through. in_flight is the only non-terminal
			// outcome, so this is either a review running right now or one of
			// the message's refs parked in pending.
			return
		}
		if r.Outcome != store.OutcomeReviewed {
			clean = false
		}
	}

	emoji := EmojiFindings
	if clean {
		emoji = EmojiClean
	}

	// Marked before the call, for the reason given in startMessageReaction.
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
