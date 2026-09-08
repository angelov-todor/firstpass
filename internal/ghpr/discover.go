package ghpr

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/angelov-todor/firstpass/internal/prref"
)

// Query describes which pull requests a GitHub source should offer.
//
// Deliberately narrow. The obvious query -- every open pull request in the
// organisation -- returns over five hundred, each of which would cost a clone
// and a claude run, and would comment on colleagues' pull requests nobody
// asked firstpass to look at. Requiring a review request keeps the property
// that makes the chat source work: somebody asked.
type Query struct {
	// Owner is the organisation, e.g. AstraBit-CPT.
	Owner string
	// ReviewRequestedFor is the login a review must be requested from. The
	// whole point of the source, so it is required rather than optional: the
	// same query without it is the five-hundred-pull-request one.
	ReviewRequestedFor string
	// RepoPrefixes, when set, keeps only repositories whose name starts with
	// one of them. GitHub search has no prefix qualifier, so this is applied
	// to the results.
	RepoPrefixes []string
	// ExcludeAuthors are logins excluded in the query itself, which is what
	// keeps the result to one page. Bots are the reason it exists: of 327 pull
	// requests with a review requested from this operator, 314 were
	// dependabot.
	ExcludeAuthors []string
	// Limit caps the page. GitHub allows 100 per request and paging beyond
	// that trips a secondary rate limit quickly, which matters because the
	// daemon shares that budget with every review it runs.
	Limit int
}

// Found is one pull request a source offers, with the timestamp that says
// whether it is worth looking at again.
type Found struct {
	Ref prref.PRRef
	// UpdatedAt is GitHub's last-activity time. It is a cheap filter and not a
	// decision: a comment moves it as surely as a push does, so it says "worth
	// asking about", never "has new commits". What decides is the head SHA,
	// after Inspect, exactly as it does for a re-post in chat.
	UpdatedAt time.Time
	Author    string
	IsBot     bool
}

type rawSearch struct {
	TotalCount        int  `json:"total_count"`
	IncompleteResults bool `json:"incomplete_results"`
	Items             []struct {
		Number        int    `json:"number"`
		RepositoryURL string `json:"repository_url"`
		UpdatedAt     string `json:"updated_at"`
		Draft         bool   `json:"draft"`
		User          struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
		PullRequest *struct{} `json:"pull_request"`
	} `json:"items"`
}

// Page is one search's worth of results.
//
// Truncated is carried here rather than computed from len(Found) because the
// two counts are not the same: Found has been filtered by repo_prefixes, so a
// full page of a hundred pull requests in repositories the source does not
// want yields a handful of results. Deriving truncation from the filtered
// count, as this first did, silently disabled the warning in exactly the
// configuration that needs it -- a narrow prefix over a queue full of
// dependabot, where real pull requests are the ones being pushed off the page.
type Page struct {
	Found []Found
	// Scanned is how many items GitHub returned, before any filtering.
	Scanned int
	// Truncated reports that the page was full, so pull requests exist that
	// this search did not see.
	Truncated bool
}

// Discover lists the pull requests matching the query.
//
// One request per sweep, through the search API rather than `gh search prs`,
// because the qualifiers that keep it to one request -- the author exclusions
// -- have no flags on that command: it quotes a positional query as free-text
// keywords, which silently returns the wrong thing rather than failing.
//
// Truncated rather than paged. Paging this API tripped GitHub's secondary rate
// limit within a handful of requests during development, and the daemon spends
// the same budget on every review it runs, so a discovery step that can starve
// reviews is a bad trade for pull requests further down a list sorted by
// recency.
func (c *Client) Discover(ctx context.Context, q Query) (Page, error) {
	if q.Owner == "" || q.ReviewRequestedFor == "" {
		return Page{}, fmt.Errorf("a github source needs both an owner and a login to " +
			"require a review request from")
	}
	limit := q.Limit
	if limit <= 0 || limit > 100 {
		limit = 100
	}

	terms := []string{
		"org:" + q.Owner,
		"is:pr",
		"state:open",
		"draft:false",
		// The login, not "@me": that is a gh CLI convenience and means nothing
		// to the API this calls.
		"review-requested:" + q.ReviewRequestedFor,
	}
	for _, a := range q.ExcludeAuthors {
		if a = strings.TrimSpace(a); a != "" {
			terms = append(terms, "-author:"+a)
		}
	}

	res, err := c.r.Run(ctx, "", c.gh, "api", "-X", "GET", "search/issues",
		"-f", "q="+strings.Join(terms, " "),
		"-F", "per_page="+strconv.Itoa(limit),
		// Most recently touched first, so a truncated page keeps the pull
		// requests most likely to have moved since the last review.
		"-f", "sort=updated", "-f", "order=desc")
	if err != nil {
		return Page{}, fmt.Errorf("gh api search/issues: %w", err)
	}
	if res.ExitCode != 0 {
		return Page{}, fmt.Errorf("gh api search/issues exit %d: %s",
			res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	var raw rawSearch
	if err := json.Unmarshal(res.Stdout, &raw); err != nil {
		return Page{}, fmt.Errorf("decode search results: %w", err)
	}

	var out []Found
	for _, it := range raw.Items {
		// search/issues returns issues and pull requests together; only a pull
		// request carries this key.
		if it.PullRequest == nil {
			continue
		}
		owner, repo, ok := ownerRepoFromAPIURL(it.RepositoryURL)
		if !ok {
			continue
		}
		if !matchesPrefix(repo, q.RepoPrefixes) {
			continue
		}
		f := Found{
			// prref.New, never a literal: it folds the case that Key and the
			// store assume is folded. GitHub answers "AstraBit-CPT" where a
			// chat link says "astrabit-cpt", and an unfolded ref is a second
			// key for a pull request firstpass already knows about -- which is
			// a second review of it.
			Ref:    prref.New(owner, repo, it.Number),
			Author: it.User.Login,
			IsBot:  it.User.Type == "Bot",
		}
		if t, terr := time.Parse(time.RFC3339, it.UpdatedAt); terr == nil {
			f.UpdatedAt = t
		}
		out = append(out, f)
	}
	return Page{
		Found:   out,
		Scanned: len(raw.Items),
		// Counted before filtering: see Page. GitHub also reports
		// incomplete_results when it timed out internally, which means the
		// same thing to an operator -- pull requests exist that this search
		// did not see.
		Truncated: len(raw.Items) >= limit || raw.IncompleteResults,
	}, nil
}

// ownerRepoFromAPIURL reads the pieces out of an api.github.com/repos/O/R URL,
// which is the only place the search response names the repository.
func ownerRepoFromAPIURL(u string) (owner, repo string, ok bool) {
	const marker = "/repos/"
	i := strings.LastIndex(u, marker)
	if i < 0 {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u[i+len(marker):], "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// matchesPrefix reports whether the repository is one the source wants. No
// prefixes means every repository under the owner.
func matchesPrefix(repo string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		// Case-insensitively, because the repository name has already been
		// folded and a prefix written "AEX-" in a config file means the same
		// thing as "aex-".
		if p != "" && strings.HasPrefix(strings.ToLower(repo), strings.ToLower(p)) {
			return true
		}
	}
	return false
}
