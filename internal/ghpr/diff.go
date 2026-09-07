package ghpr

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/angelov-todor/firstpass/internal/prref"
)

// MaxDiffBytes bounds one sibling diff.
//
// 40 KB is roughly ten thousand tokens, so two siblings cost about twenty
// thousand -- affordable next to a review, and small enough that a single
// enormous pull request in a post cannot crowd out the change actually under
// review. A cut diff is reported as cut rather than silently shortened,
// because a reviewer that believes it saw the whole thing will read the
// absence of a change as evidence there was none.
const MaxDiffBytes = 40 * 1024

// PRDiff returns the unified diff of a pull request, and whether it was cut.
//
// Used for the pull requests posted alongside the one under review: firstpass
// gives the reviewer their diffs as context so it can tell whether a coupled
// change agrees with itself across repositories.
func (c *Client) PRDiff(ctx context.Context, ref prref.PRRef) (diff string, truncated bool, err error) {
	res, err := c.r.Run(ctx, "", c.gh,
		"pr", "diff", strconv.Itoa(ref.Number),
		"--repo", ref.Owner+"/"+ref.Repo)
	if err != nil {
		return "", false, fmt.Errorf("gh pr diff %s: %w", ref.Key(), err)
	}
	if res.ExitCode != 0 {
		return "", false, fmt.Errorf("gh pr diff %s exit %d: %s",
			ref.Key(), res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	out := string(res.Stdout)
	if len(out) > MaxDiffBytes {
		// Cut on a line boundary. A diff ending mid-hunk reads as a malformed
		// hunk rather than a truncated file, and the reviewer would be left
		// reasoning about a change that appears to end in the middle of a
		// line.
		out = out[:MaxDiffBytes]
		if i := strings.LastIndexByte(out, '\n'); i > 0 {
			out = out[:i]
		}
		return out, true, nil
	}
	return out, false, nil
}
