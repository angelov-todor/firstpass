package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/store"
)

func aSource() config.Source {
	return config.Source{
		Type: "github", Owner: "Example-Org",
		RepoPrefixes: []string{"aex-"}, ExcludeBots: true,
	}
}

func found(repo string, n int, updated time.Time) ghpr.Found {
	return ghpr.Found{
		// prref.New rather than a literal, for the same reason ghpr.Discover
		// uses it: an unfolded ref is a different key for the same pull
		// request, and this test would then be asserting about a pull request
		// nothing else in the sweep recognises.
		Ref:       prref.New("Example-Org", repo, n),
		UpdatedAt: updated,
		Author:    "colleague",
	}
}

// sourceHarness sweeps with a GitHub source configured and no chat messages,
// unless the caller passes some.
func sourceHarness(t *testing.T, msgs []chat.Message, offered ...ghpr.Found) *harness {
	t.Helper()
	h := newHarness(t, msgs)
	h.seedWatermark(t)
	h.prs.discovered = offered
	h.cfg.Sources = []config.Source{aSource()}
	h.apply()
	return h
}

// TestADiscoveredPullRequestIsReviewed is the feature: a pull request with a
// review requested from the operator gets reviewed even though nobody posted
// it in chat.
func TestADiscoveredPullRequestIsReviewed(t *testing.T) {
	h := sourceHarness(t, nil, found("aex-balances", 12, time.Time{}))
	h.prs.info[verdictKey] = ghpr.PRInfo{State: "OPEN", Author: "colleague", HeadSHA: "sha1"}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, ok := decisionFor(rep, verdictKey)
	if !ok || d.Action != ActionReview {
		t.Fatalf("a discovered pull request must be reviewed, got %+v", d)
	}
	// It has no chat message behind it, so it must not pretend to: a trigger
	// is what the reactions and the re-post rule key off.
	rec := reviewRecord(t, h)
	if rec.TriggerMessage != "" {
		t.Errorf("a discovered candidate has no chat message: %q", rec.TriggerMessage)
	}
}

// TestBothChannelsOfferingOnePullRequestReviewsItOnce is the operator's rule
// stated directly: one review, whichever channel found it.
//
// The chat message wins the tie, and not arbitrarily -- it carries the sibling
// group and something to react to, neither of which a search result can
// supply.
func TestBothChannelsOfferingOnePullRequestReviewsItOnce(t *testing.T) {
	h := sourceHarness(t,
		[]chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))},
		found("aex-balances", 12, time.Now()))
	h.prs.info[verdictKey] = ghpr.PRInfo{State: "OPEN", Author: "colleague", HeadSHA: "sha1"}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range rep.Decisions {
		if d.Ref.Key() == verdictKey {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("one pull request offered by both channels must be decided once, got %d", n)
	}
	if rep.Reviewed != 1 {
		t.Errorf("Reviewed = %d, want exactly one review", rep.Reviewed)
	}
	if len(h.rev.ran) != 1 {
		t.Errorf("claude ran %d times for one pull request", len(h.rev.ran))
	}
	// The chat provenance survived the merge, which is what keeps the
	// reactions and the sibling group working for a pull request a source
	// happened to offer as well.
	if rec := reviewRecord(t, h); rec.TriggerMessage != "spaces/A/messages/m1" {
		t.Errorf("the chat message must win the tie: %q", rec.TriggerMessage)
	}
}

// TestADiscoveredPullRequestIsNotReviewedTwiceForTheSameCommit is the other
// half of the rule, and the one that costs money when it is wrong.
//
// GitHub's updated_at moves for a comment as readily as for a push, so a
// discussed pull request is offered again on every sweep. What decides is the
// head SHA, and it is the same gate a chat re-post meets: one review per
// commit, whatever found it.
func TestADiscoveredPullRequestIsNotReviewedTwiceForTheSameCommit(t *testing.T) {
	h := sourceHarness(t, nil, found("aex-balances", 12, time.Now()))
	h.prs.info[verdictKey] = ghpr.PRInfo{State: "OPEN", Author: "colleague", HeadSHA: "sha1"}
	// Already reviewed at that very commit, five minutes ago.
	if err := h.st.PutReview(store.Review{
		Key: verdictKey, Outcome: store.OutcomeReviewed,
		HeadSHA: "sha1", ReviewedSHAs: []string{"sha1"},
		DecidedAt: time.Now().Add(-5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := decisionFor(rep, verdictKey); d.Action != ActionSkip {
		t.Errorf("no new commit means no second review: %+v", d)
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("claude must not run again for a commit already reviewed, ran %d times",
			len(h.rev.ran))
	}
}

// And a follow-up commit is exactly what does justify another review -- the
// rule's other direction, which a source must honour as a chat re-post does.
func TestADiscoveredPullRequestIsReviewedAgainAfterAFollowUpCommit(t *testing.T) {
	h := sourceHarness(t, nil, found("aex-balances", 12, time.Now()))
	h.prs.info[verdictKey] = ghpr.PRInfo{State: "OPEN", Author: "colleague", HeadSHA: "sha2"}
	if err := h.st.PutReview(store.Review{
		Key: verdictKey, Outcome: store.OutcomeReviewed,
		HeadSHA: "sha1", ReviewedSHAs: []string{"sha1"},
		DecidedAt: time.Now().Add(-5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := decisionFor(rep, verdictKey); d.Action != ActionReview {
		t.Errorf("a follow-up commit must be reviewed: %+v", d)
	}
}

// A pull request nothing has touched since the review is not even inspected.
// The gate before Inspect exists to keep discovery from spending a GitHub call
// per pull request per sweep for no new information -- firstpass shares that
// rate limit with the reviews themselves.
func TestAQuietDiscoveredPullRequestCostsNoGitHubCall(t *testing.T) {
	reviewedAt := time.Now()
	h := sourceHarness(t, nil, found("aex-balances", 12, reviewedAt.Add(-time.Hour)))
	h.prs.info[verdictKey] = ghpr.PRInfo{State: "OPEN", Author: "colleague", HeadSHA: "sha1"}
	if err := h.st.PutReview(store.Review{
		Key: verdictKey, Outcome: store.OutcomeReviewed,
		HeadSHA: "sha1", ReviewedSHAs: []string{"sha1"}, DecidedAt: reviewedAt,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	h.prs.mu.Lock()
	inspected := len(h.prs.inspected)
	h.prs.mu.Unlock()
	if inspected != 0 {
		t.Errorf("Inspect ran %d times for a pull request untouched since its review", inspected)
	}
}

// A source is an addition to the chat space, never a replacement, so its
// failure must not stop firstpass reviewing what the team actually posted.
func TestASourceFailureDoesNotStopTheSweep(t *testing.T) {
	h := sourceHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.prs.discoverErr = errors.New("HTTP 403: secondary rate limit")
	h.prs.info[verdictKey] = ghpr.PRInfo{State: "OPEN", Author: "colleague", HeadSHA: "sha1"}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatalf("a source failure must not fail the sweep: %v", err)
	}
	if d, _ := decisionFor(rep, verdictKey); d.Action != ActionReview {
		t.Errorf("the chat candidate must still be reviewed: %+v", d)
	}
}

// Bots are the reason exclude_authors exists, and the reason this second net
// exists: of 327 pull requests with a review requested from this operator, 314
// were dependabot. A bot that nobody has added to the query's exclusions must
// still not reach a review.
func TestBotsAreNotReviewed(t *testing.T) {
	bot := found("aex-balances", 12, time.Time{})
	bot.IsBot = true
	bot.Author = "dependabot[bot]"
	h := sourceHarness(t, nil, bot)
	h.prs.info[verdictKey] = ghpr.PRInfo{State: "OPEN", Author: "dependabot[bot]", HeadSHA: "sha1"}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decisionFor(rep, verdictKey); ok {
		t.Error("a bot's pull request must not even become a candidate")
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("claude ran %d times on a bot's pull request", len(h.rev.ran))
	}
}

// The owner allowlist is what keeps firstpass off strangers' pull requests. It
// is applied per candidate wherever the candidate came from, and again at
// discovery so a misconfigured source reads as an empty result rather than as
// a sweep full of refusals.
func TestADiscoveredPullRequestOutsideTheAllowlistIsDropped(t *testing.T) {
	outside := ghpr.Found{Ref: prref.New("Someone-Else", "aex-x", 1)}
	h := sourceHarness(t, nil, outside)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Decisions) != 0 {
		t.Errorf("a pull request outside allow_owners must not be a candidate: %+v", rep.Decisions)
	}
}

// With no sources configured, nothing is asked of GitHub at all -- the
// behaviour every installation had before this existed.
func TestNoSourcesAsksNothing(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	h.prs.info[verdictKey] = ghpr.PRInfo{State: "OPEN", Author: "colleague", HeadSHA: "sha1"}
	h.apply()

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	h.prs.mu.Lock()
	queries := len(h.prs.discoverQueries)
	h.prs.mu.Unlock()
	if queries != 0 {
		t.Errorf("no sources configured must mean no search, got %d", queries)
	}
}

// The query a source produces is the source's own configuration, including the
// login it falls back to.
func TestASourceQueriesWhatItWasConfiguredWith(t *testing.T) {
	h := sourceHarness(t, nil)
	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	h.prs.mu.Lock()
	defer h.prs.mu.Unlock()
	if len(h.prs.discoverQueries) != 1 {
		t.Fatalf("want one query per source per sweep, got %d", len(h.prs.discoverQueries))
	}
	q := h.prs.discoverQueries[0]
	if q.Owner != "Example-Org" {
		t.Errorf("Owner = %q", q.Owner)
	}
	// Unset in the source, so it falls back to github_login rather than
	// searching for pull requests with a review requested from nobody.
	if q.ReviewRequestedFor != "angelov-todor" {
		t.Errorf("ReviewRequestedFor = %q, want the operator's login", q.ReviewRequestedFor)
	}
}

// A chat source is declared in the config so the file names every place
// firstpass looks. It is not searched: Sweep has already fetched its messages,
// with the watermark, the reactions and the sibling grouping that go with
// them. Searching for it would be a second, wrong way to read the same space.
func TestAChatSourceIsNotSearched(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	h.prs.info[verdictKey] = ghpr.PRInfo{State: "OPEN", Author: "colleague", HeadSHA: "sha1"}
	h.cfg.Sources = []config.Source{
		{Type: config.SourceChat, Space: "spaces/A"},
		aSource(),
	}
	h.apply()

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	h.prs.mu.Lock()
	defer h.prs.mu.Unlock()
	if len(h.prs.discoverQueries) != 1 {
		t.Fatalf("only the github source has a query to run, got %d", len(h.prs.discoverQueries))
	}
	if h.prs.discoverQueries[0].Owner != "Example-Org" {
		t.Errorf("the wrong source was searched: %+v", h.prs.discoverQueries[0])
	}
}
