package ghpr

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/angelov-todor/firstpass/internal/prref"
)

// Surfaces a review comment can arrive on. All three are in use on the team's
// pull requests, which is why all three are fetched: aex-margin-service#197
// carried two reviews and six general comments and no inline threads at all,
// while aex-backoffice#309 carried three inline threads. Reading one surface
// would miss most of the feedback on a given pull request.
const (
	SurfaceThread  = "thread"
	SurfaceReview  = "review"
	SurfaceComment = "comment"
)

// excerptWidth bounds one index line. The index exists so the reviewer cannot
// miss that feedback exists; the full text is a `gh` call away, and the URL of
// each item is included so it is a cheap one.
const excerptWidth = 160

// maxFeedbackItems caps the index.
//
// Exceeding it sets Truncated, and a truncated index is treated as no index at
// all for approval purposes: "there is more you have not been shown" cannot
// support "everything raised here has been addressed".
const maxFeedbackItems = 60

// FeedbackItem is one piece of existing feedback on a pull request.
type FeedbackItem struct {
	Surface string
	Author  string
	// IsBot marks a machine author. One of #197's six comments is
	// github-actions posting a CI summary; unmarked, that reads as review
	// feedback that must be addressed before an approval.
	IsBot bool

	// Threads only.
	Path     string
	Line     int
	Resolved bool
	// Outdated means the code the thread points at has changed since. It is
	// emphatically not "addressed": all three threads on #309 are unresolved
	// and outdated, which may mean fixed or may mean the lines merely moved.
	// Only reading the code can tell, so this is shown and not interpreted.
	Outdated bool

	// Reviews only: COMMENTED, CHANGES_REQUESTED, APPROVED, DISMISSED.
	State string

	Excerpt string
	URL     string
}

// Feedback is every piece of existing feedback on a pull request, plus
// GitHub's own aggregate verdict on it.
type Feedback struct {
	Items []FeedbackItem
	// ReviewDecision is GitHub's aggregate: APPROVED, CHANGES_REQUESTED,
	// REVIEW_REQUIRED, or empty when the repository requires no review. It is
	// taken from GitHub rather than derived from Items because working out
	// which of several reviews by one author is current is exactly the sort of
	// thing to let GitHub answer.
	ReviewDecision string
	// Truncated says the list is incomplete.
	Truncated bool
}

// ChangesRequested reports whether a human has an outstanding request for
// changes. firstpass never submits an approval over one: appearing to clear a
// colleague's block, under the operator's own identity, is not something an
// automated pass should be able to do.
func (f Feedback) ChangesRequested() bool {
	return f.ReviewDecision == "CHANGES_REQUESTED"
}

// Usable reports whether this Feedback can support an approval. An incomplete
// list cannot.
func (f Feedback) Usable() bool { return !f.Truncated }

// feedbackQuery asks for all three surfaces and reviewDecision in one call.
//
// Verified against real pull requests before being written into the code: one
// call returns reviewDecision, threads with isResolved/isOutdated, review
// states and bodies, and general comments.
const feedbackQuery = `query($o:String!,$r:String!,$n:Int!){
  repository(owner:$o,name:$r){
    pullRequest(number:$n){
      reviewDecision
      reviewThreads(first:50){ totalCount nodes{ isResolved isOutdated path line
        comments(first:1){ nodes{ author{login __typename} body url } } } }
      reviews(first:50){ totalCount nodes{ author{login __typename} state body url } }
      comments(first:50){ totalCount nodes{ author{login __typename} body url } }
    }
  }
}`

type rawAuthor struct {
	Login    string `json:"login"`
	TypeName string `json:"__typename"`
}

type rawFeedback struct {
	Data struct {
		Repository struct {
			PullRequest struct {
				ReviewDecision string `json:"reviewDecision"`
				ReviewThreads  struct {
					TotalCount int `json:"totalCount"`
					Nodes      []struct {
						IsResolved bool   `json:"isResolved"`
						IsOutdated bool   `json:"isOutdated"`
						Path       string `json:"path"`
						Line       *int   `json:"line"`
						Comments   struct {
							Nodes []struct {
								Author rawAuthor `json:"author"`
								Body   string    `json:"body"`
								URL    string    `json:"url"`
							} `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
				Reviews struct {
					TotalCount int `json:"totalCount"`
					Nodes      []struct {
						Author rawAuthor `json:"author"`
						State  string    `json:"state"`
						Body   string    `json:"body"`
						URL    string    `json:"url"`
					} `json:"nodes"`
				} `json:"reviews"`
				Comments struct {
					TotalCount int `json:"totalCount"`
					Nodes      []struct {
						Author rawAuthor `json:"author"`
						Body   string    `json:"body"`
						URL    string    `json:"url"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
}

// FetchFeedback returns every piece of existing feedback on a pull request.
//
// It exists because an approval has to mean something about the whole pull
// request. firstpass used to know four things about a PR -- state, draft,
// author, head SHA -- so it would approve a change a colleague had already
// asked for changes on, and a later pass would approve while its own earlier
// findings sat unaddressed above it.
func (c *Client) FetchFeedback(ctx context.Context, ref prref.PRRef) (Feedback, error) {
	res, err := c.r.Run(ctx, "", c.gh,
		"api", "graphql",
		"-f", "query="+feedbackQuery,
		"-F", "o="+ref.Owner,
		"-F", "r="+ref.Repo,
		"-F", "n="+strconv.Itoa(ref.Number))
	if err != nil {
		return Feedback{}, fmt.Errorf("gh api graphql for %s: %w", ref.Key(), err)
	}
	if res.ExitCode != 0 {
		return Feedback{}, fmt.Errorf("gh api graphql for %s exit %d: %s",
			ref.Key(), res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return parseFeedback(res.Stdout, ref)
}

func parseFeedback(stdout []byte, ref prref.PRRef) (Feedback, error) {
	var raw rawFeedback
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return Feedback{}, fmt.Errorf("decode feedback for %s: %w", ref.Key(), err)
	}
	pr := raw.Data.Repository.PullRequest

	f := Feedback{ReviewDecision: pr.ReviewDecision}

	for _, t := range pr.ReviewThreads.Nodes {
		item := FeedbackItem{
			Surface:  SurfaceThread,
			Path:     t.Path,
			Resolved: t.IsResolved,
			Outdated: t.IsOutdated,
		}
		if t.Line != nil {
			item.Line = *t.Line
		}
		if len(t.Comments.Nodes) > 0 {
			c0 := t.Comments.Nodes[0]
			item.Author, item.IsBot = author(c0.Author)
			item.Excerpt = excerpt(c0.Body)
			item.URL = c0.URL
		}
		f.Items = append(f.Items, item)
	}
	for _, r := range pr.Reviews.Nodes {
		a, bot := author(r.Author)
		// A review with an empty body carries no feedback of its own -- its
		// state is already in reviewDecision, and #197 has exactly one of
		// these (an APPROVED with nothing written). Listing it would pad the
		// index with a line the reviewer can do nothing about.
		if strings.TrimSpace(r.Body) == "" {
			continue
		}
		f.Items = append(f.Items, FeedbackItem{
			Surface: SurfaceReview,
			Author:  a,
			IsBot:   bot,
			State:   r.State,
			Excerpt: excerpt(r.Body),
			URL:     r.URL,
		})
	}
	for _, c := range pr.Comments.Nodes {
		if strings.TrimSpace(c.Body) == "" {
			continue
		}
		a, bot := author(c.Author)
		f.Items = append(f.Items, FeedbackItem{
			Surface: SurfaceComment,
			Author:  a,
			IsBot:   bot,
			Excerpt: excerpt(c.Body),
			URL:     c.URL,
		})
	}

	// Truncated is set from the totals GitHub reports, not from how many items
	// were built: a pull request with 80 threads returns the first 50, and the
	// index must say so rather than look complete.
	if pr.ReviewThreads.TotalCount > len(pr.ReviewThreads.Nodes) ||
		pr.Reviews.TotalCount > len(pr.Reviews.Nodes) ||
		pr.Comments.TotalCount > len(pr.Comments.Nodes) {
		f.Truncated = true
	}
	if len(f.Items) > maxFeedbackItems {
		f.Items = f.Items[:maxFeedbackItems]
		f.Truncated = true
	}
	return f, nil
}

// author flattens a GraphQL actor. A deleted account comes back as null, which
// is neither an error nor a bot.
func author(a rawAuthor) (string, bool) {
	if a.Login == "" {
		return "(unknown)", false
	}
	return a.Login, a.TypeName == "Bot" || strings.HasSuffix(a.Login, "[bot]")
}

// excerpt is the first non-empty line, bounded. Markdown headings are kept as
// written: "## Adversarial review round 1" is exactly the sort of line that
// tells the reviewer whether an item is worth fetching in full.
func excerpt(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > excerptWidth {
			return line[:excerptWidth] + "…"
		}
		return line
	}
	return ""
}
