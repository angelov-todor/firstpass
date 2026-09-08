package pipeline

import (
	"context"
	"time"

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
		q := ghpr.Query{
			Owner:              src.Owner,
			ReviewRequestedFor: src.Login(p.Cfg.GithubLogin),
			RepoPrefixes:       src.RepoPrefixes,
			ExcludeAuthors:     src.ExcludeAuthors,
			Limit:              src.Limit,
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
