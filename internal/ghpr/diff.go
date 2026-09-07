package ghpr

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/angelov-todor/firstpass/internal/prref"
)

// MaxDiffBytes bounds one sibling diff.
//
// 6 KB, and the number is set by Windows rather than by taste. The whole
// command line of a process is capped at 32,767 characters, and the sibling
// diffs travel inside a single --append-system-prompt argument alongside the
// docs note, the prior-feedback index and the prompt itself. Measured on the
// target machine: a 33,000-character argument fails with "fork/exec: The
// filename or extension is too long". A failed exec is a review that did not
// happen, recorded as needs_attention and never retried automatically -- so an
// oversized diff would take a pull request that reviewed fine yesterday and
// leave it permanently unreviewed.
//
// 6 KB is about fifteen hundred tokens: enough for the file list and the
// changed lines, which is what a cross-repo check needs. Two of them leave
// ample room under the cap, and review.Run enforces the total independently
// rather than trusting this arithmetic.
//
// A cut diff is reported as cut rather than silently shortened, because a
// reviewer that believes it saw the whole thing reads the absence of a change
// as evidence there was none.
const MaxDiffBytes = 6 * 1024

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
		} else {
			// No newline in the whole slice: a minified or generated
			// single-line file. Cutting mid-rune would put invalid UTF-8 into
			// the prompt, so back off to the last valid boundary.
			for len(out) > 0 && !utf8.ValidString(out) {
				out = out[:len(out)-1]
			}
		}
		return out, true, nil
	}
	return out, false, nil
}
