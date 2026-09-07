package review

import (
	"fmt"
	"strings"
)

// Sibling is another pull request that was posted in the same chat message as
// the one under review.
//
// It carries that pull request's diff rather than a checkout of it. Three
// alternatives were considered and this is the one that survives contact with
// how firstpass already works:
//
//   - A checkout per sibling collides. Worktree paths are per pull request, so
//     two concurrent reviews of the same group would both want the same
//     directory for the sibling they share.
//   - Collision-free copies mean up to nine checkouts of large service
//     repositories on disk for one three-way post, most of them identical.
//   - The URL alone leaves the reviewer to go and fetch things, which is the
//     shape of instruction this project has repeatedly measured being ignored.
//
// A diff is also the artifact the cross-check actually needs. "The frontend
// pull request posted with this one does not update the renamed field" is
// answered by seeing what the frontend pull request changed -- and, just as
// often, by seeing what it did not.
type Sibling struct {
	Key  string // owner/repo#n
	URL  string
	Diff string
	// Truncated says the diff was cut. The reviewer is told, so it does not
	// read the absence of a change as evidence there was none.
	Truncated bool
	// Reviewing says firstpass is reviewing this sibling too, in its own
	// separate review. The distinction matters: a sibling that is being
	// reviewed will get its own comments, so raising its problems here would
	// duplicate them, while a sibling that is not -- a draft, one of the
	// operator's own, one already reviewed -- gets no other look.
	Reviewing bool
}

// siblingNote renders the context block for the system prompt.
//
// The instruction it carries is narrow on purpose: use these to judge the pull
// request under review, and do not review them. A reviewer that starts
// commenting on a sibling would post findings on a pull request that is either
// getting its own review or was deliberately excluded from one.
func siblingNote(under string, sibs []Sibling) string {
	if len(sibs) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("These pull requests were posted together in one message, which on "+
		"this team means they are usually related -- an API change and its frontend, a service "+
		"change and its deployment. You are reviewing %s. The others are here as CONTEXT so you "+
		"can judge whether they agree with each other.\n\n", under))

	for _, s := range sibs {
		b.WriteString("---\n" + s.Key + "  " + s.URL + "\n")
		if s.Reviewing {
			b.WriteString("(firstpass is reviewing this one separately.)\n")
		} else {
			b.WriteString("(firstpass is NOT reviewing this one -- it is a draft, the operator's " +
				"own, or already reviewed. Nothing else will look at it.)\n")
		}
		b.WriteString("\n```diff\n" + s.Diff + "\n```\n")
		if s.Truncated {
			b.WriteString("\n[diff truncated -- fetch the rest with `gh pr diff` if it matters. " +
				"Do not read the absence of a change here as evidence there was none.]\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("What to do with this:\n" +
		"  - Look for disagreement between them. A field renamed on one side and not the other, " +
		"a contract one half assumes and the other does not implement, a migration one needs and " +
		"the other never runs, a config key added in one and read in neither.\n" +
		"  - Raise what you find AS A FINDING ON " + under + ", phrased so the author can see " +
		"which other pull request it concerns.\n" +
		"  - Do NOT review the others and do NOT post anything on them. They are context. Where " +
		"one is being reviewed separately, it will get its own comments; where it is not, it was " +
		"deliberately excluded.\n" +
		"  - `gh` is available if you need more of one than the diff shows.")
	return b.String()
}
