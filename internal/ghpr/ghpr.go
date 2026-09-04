// Package ghpr answers the questions firstpass asks about a pull request before
// deciding whether to review it, and submits the one thing firstpass writes
// back: the verdict of a finished review.
package ghpr

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/runner"
)

// PRInfo is the subset of pull request state firstpass's filters need.
type PRInfo struct {
	State   string
	IsDraft bool
	Author  string
	HeadSHA string
}

type rawPR struct {
	State   string `json:"state"`
	IsDraft bool   `json:"isDraft"`
	Author  struct {
		Login string `json:"login"`
	} `json:"author"`
	HeadRefOid string `json:"headRefOid"`
}

// Client wraps the gh CLI, which already holds this machine's GitHub auth.
type Client struct {
	r  runner.Runner
	gh string
}

func New(r runner.Runner, gh string) *Client { return &Client{r: r, gh: gh} }

func (c *Client) Inspect(ctx context.Context, ref prref.PRRef) (PRInfo, error) {
	res, err := c.r.Run(ctx, "", c.gh,
		"pr", "view", strconv.Itoa(ref.Number),
		"--repo", ref.Owner+"/"+ref.Repo,
		"--json", "state,isDraft,author,headRefOid")
	if err != nil {
		return PRInfo{}, fmt.Errorf("gh pr view %s: %w", ref.Key(), err)
	}
	if res.ExitCode != 0 {
		return PRInfo{}, fmt.Errorf("gh pr view %s exit %d: %s",
			ref.Key(), res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	var v rawPR
	if err := json.Unmarshal(res.Stdout, &v); err != nil {
		return PRInfo{}, fmt.Errorf("decode gh output for %s: %w", ref.Key(), err)
	}
	return PRInfo{
		State:   v.State,
		IsDraft: v.IsDraft,
		Author:  v.Author.Login,
		HeadSHA: v.HeadRefOid,
	}, nil
}

// The two review events firstpass can submit. There is deliberately no
// request-changes: see SubmitReview.
const (
	ReviewApprove = "approve"
	ReviewComment = "comment"
)

// SubmitReview submits a GitHub review on ref under the operator's own
// identity, so a reviewed pull request says something even when nothing was
// found. body is the review body and must say it is machine-written.
//
// The asymmetry between the two events is the point. An approve is a real
// approval and clears reviewDecision, which is safe only when nothing needing
// change was raised. Anything Critical or Important gets a COMMENT review
// instead of request-changes: a comment leaves reviewDecision at
// REVIEW_REQUIRED, so the pull request stays in the team's human review queue
// -- whereas request-changes would speak for a human who has not looked yet,
// and an approval would take the PR out of the queue entirely.
//
// An unrecognised event is refused before gh runs. Defaulting to --approve
// here is the single failure the verdict feature exists to prevent.
func (c *Client) SubmitReview(ctx context.Context, ref prref.PRRef, verdict, body string) error {
	var flag string
	switch verdict {
	case ReviewApprove:
		flag = "--approve"
	case ReviewComment:
		flag = "--comment"
	default:
		return fmt.Errorf("submit review %s: unrecognised verdict %q, so nothing was submitted",
			ref.Key(), verdict)
	}

	res, err := c.r.Run(ctx, "", c.gh,
		"pr", "review", strconv.Itoa(ref.Number),
		"--repo", ref.Owner+"/"+ref.Repo,
		flag, "--body", body)
	if err != nil {
		return fmt.Errorf("gh pr review %s: %w", ref.Key(), err)
	}
	// Per the runner contract a non-zero exit arrives as data, not an error:
	// unchecked, a review gh refused to submit would read as submitted.
	if res.ExitCode != 0 {
		return fmt.Errorf("gh pr review %s exit %d: %s",
			ref.Key(), res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}
