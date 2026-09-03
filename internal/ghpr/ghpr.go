// Package ghpr answers the questions firstpass asks about a pull request before
// deciding whether to review it.
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
