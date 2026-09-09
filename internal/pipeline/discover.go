package pipeline

import (
	"context"
	"time"

	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/store"
)

// sourceFound is one pull request a configured source offered.
type sourceFound struct {
	ref       prref.PRRef
	updatedAt time.Time
}

// discover asks every configured source what it has.
//
// Failures are logged and skipped rather than failing the sweep. A source is
// an addition to the chat space, never a replacement for it: a GitHub outage
// or a rate limit must not stop firstpass reviewing what the team posted, and
// the pull requests a source would have offered are offered again on the next
// sweep, five minutes later, having lost nothing but the delay.
func (p *Pipeline) discover(ctx context.Context) []sourceFound {
	if len(p.Cfg.Sources) == 0 {
		return nil
	}
	var out []sourceFound
	for _, src := range p.Cfg.Sources {
		// The chat source is declared alongside the others so the config file
		// names every source, but it is not searched: Sweep has already
		// fetched its messages, with a watermark, reactions and the sibling
		// grouping that go with them.
		if src.Type != config.SourceGitHub {
			continue
		}
		q := ghpr.Query{
			Owner:              src.Owner,
			ReviewRequestedFor: src.Login(p.Cfg.GithubLogin),
			RepoPrefixes:       src.RepoPrefixes,
			ExcludeAuthors:     src.ExcludeAuthors,
			Limit:              src.Limit,
		}
		// The cold start, and the reason it is checked before the search
		// rather than after: a source switched on for the first time has no
		// business reviewing what was already open. Those pull requests are
		// its history -- eight on the organisation this runs against, half of
		// them months old -- and firstpass would post on all of them within a
		// quarter of an hour of being started.
		//
		// Exactly the rule the chat side has always had for a first run
		// against a populated space, applied to a source. What comes after
		// the moment firstpass started watching is offered; what was already
		// there is not.
		id := src.ID(p.Cfg.GithubLogin)
		since, watched, serr := p.Store.SourceSince(id)
		if serr != nil {
			p.Log.Error("could not read when this source was first watched; skipping it "+
				"rather than risking its whole backlog", "owner", src.Owner, "err", serr)
			continue
		}
		if !watched && !src.ReviewBacklog {
			// Recorded before anything is offered, so a crash between here and
			// the next sweep cannot turn the cold start into a backlog sweep.
			now := p.now()
			if werr := p.Store.SetSourceSince(id, now); werr != nil {
				p.Log.Error("could not record when this source was first watched; skipping it "+
					"rather than risking its whole backlog", "owner", src.Owner, "err", werr)
				continue
			}
			p.Log.Info("new source: the pull requests already open are its history and will "+
				"not be reviewed; anything touched from now on will be",
				"owner", src.Owner, "since", now.Format(time.RFC3339))
			continue
		}
		if !watched {
			// review_backlog: the operator asked for the history on purpose.
			// Still recorded, so it happens once rather than every sweep.
			if werr := p.Store.SetSourceSince(id, p.now()); werr != nil {
				p.Log.Warn("could not record when this source was first watched",
					"owner", src.Owner, "err", werr)
			}
			p.Log.Warn("new source with review_backlog set: every open pull request it finds "+
				"will be reviewed, however old", "owner", src.Owner)
			since = time.Time{}
		}

		dctx, cancel := context.WithTimeout(ctx, p.Cfg.GHTimeout.D())
		page, err := p.PRs.Discover(dctx, q)
		cancel()
		if err != nil {
			p.Log.Warn("a source could not be read; the sweep continues without it",
				"owner", src.Owner, "err", err)
			continue
		}
		// Reported once per sweep, because the operator's fix -- adding the
		// noisy author to exclude_authors -- is not something they can guess
		// from a page of results they never see. Without the exclusions this
		// operator's own query returns 327 pull requests of which 314 are
		// dependabot, so a full page is the expected symptom.
		// Info, not Warn: nothing the operator can configure fixes a partial
		// search, and the next sweep re-runs the query, so a pull request this
		// one missed is offered five minutes later.
		if page.Partial {
			p.Log.Info("a source's search came back partial; anything it missed will be "+
				"offered again next sweep", "owner", src.Owner, "scanned", page.Scanned)
		}
		if page.Truncated {
			p.Log.Warn("a source returned a full page, so some pull requests were not seen; "+
				"add the noisiest authors to exclude_authors",
				"owner", src.Owner, "scanned", page.Scanned, "kept", len(page.Found))
		}

		kept := 0
		for _, f := range page.Found {
			if src.ExcludeBots && f.IsBot {
				continue
			}
			// The same allowlist every candidate passes, applied here as well
			// so a misconfigured source is visible as an empty result rather
			// than as a sweep full of refusals.
			if !p.Cfg.OwnerAllowed(f.Ref.Owner) || p.Cfg.RepoDenied(f.Ref.Owner, f.Ref.Repo) {
				continue
			}
			// Untouched since firstpass started watching this source, so it is
			// part of the history the cold start excluded. A pull request
			// somebody pushes to, comments on, or requests a review on lands
			// after that moment and is offered.
			//
			// A missing timestamp is treated as old rather than new. GitHub
			// always sends one, so an absent one means something unexpected,
			// and the safe reading of "unknown age" is not "review it".
			if !since.IsZero() && !f.UpdatedAt.After(since) {
				continue
			}
			out = append(out, sourceFound{ref: f.Ref, updatedAt: f.UpdatedAt})
			kept++
		}
		p.Log.Info("source offered pull requests", "owner", src.Owner,
			"scanned", page.Scanned, "matched", len(page.Found), "kept", kept)
	}
	return out
}

// sourcePassDue is the second-pass prompt for a candidate no chat message
// asked for.
//
// It answers the same question secondPassDue answers for a re-post -- is this
// worth spending an Inspect on -- and it is not the rule. The rule is one
// review per commit, and the head SHA gate below Inspect enforces it for every
// candidate whatever found it. This only decides whether firstpass pays a `gh
// pr view` to ask.
//
// GitHub's updated_at moves for a comment as readily as for a push, so this
// is deliberately generous: a pull request touched since the last decision is
// worth asking about, and the SHA gate turns away the ones that were only
// talked about. The alternative -- inspecting every discovered pull request
// every sweep -- costs one GitHub call per pull request per five minutes for
// no new information, and firstpass shares its rate limit with the reviews.
func sourcePassDue(prev store.Review, activityAt time.Time) bool {
	// Same first condition as a re-post, and for the same reasons: only a
	// completed review can have a second pass, and a record that does not say
	// which commit it reviewed cannot support "one review per commit".
	if prev.Outcome != store.OutcomeReviewed || prev.HeadSHA == "" {
		return false
	}
	if prev.DecidedAt.IsZero() || activityAt.IsZero() {
		return false
	}
	return activityAt.After(prev.DecidedAt)
}

// secondPassPrompted reports whether this candidate is worth an Inspect even
// though its pull request already has a completed review, by whichever
// condition suits the source that offered it.
//
// The two conditions are prompts and not permissions; see sourcePassDue.
func (p *Pipeline) secondPassPrompted(prev store.Review, c candidate) bool {
	if c.fromChat {
		return secondPassDue(prev, c.trigger, c.triggerAt)
	}
	// A candidate with neither chat provenance nor an activity time is the
	// backlog's anonymous re-offer, and it must never be a second pass: it
	// carries no evidence that anybody asked for one, and pending is keyed by
	// pull request rather than by post, so it comes back every sweep. That
	// falls out of sourcePassDue refusing a zero time -- see
	// TestARefFromPendingIsNeverASecondPass and
	// TestAPendingRowWithNoRecordedProvenanceIsStillNeverASecondPass, both of
	// which fail if this stops being true.
	return sourcePassDue(prev, c.activityAt)
}
