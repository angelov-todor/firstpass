package review

import (
	"fmt"
	"strings"
)

// PriorItem is one piece of feedback already on the pull request, as the
// reviewer is shown it.
//
// It mirrors ghpr.FeedbackItem rather than reusing it, for the same reason
// PreviousPass mirrors part of store.Review: this package's inputs are its
// own, so it can be tested without the GitHub client and cannot acquire a
// dependency on how that client happens to model a thread today.
type PriorItem struct {
	Surface  string // "thread" | "review" | "comment"
	Author   string
	IsBot    bool
	Path     string
	Line     int
	Resolved bool
	Outdated bool
	State    string
	Excerpt  string
	URL      string
}

// PriorFeedback is everything already raised on the pull request, plus whether
// the list can be trusted to be complete.
type PriorFeedback struct {
	Items []PriorItem
	// Incomplete says firstpass could not enumerate the feedback fully. The
	// reviewer is told, because "there is more you have not been shown" changes
	// what an approval can honestly mean -- and firstpass refuses to submit one
	// in that case regardless.
	Incomplete bool
}

// priorNote renders the index that goes into the system prompt.
//
// An index rather than the full text, and evidence rather than an instruction:
// the reviewer has `gh` and every item's URL, so fetching what matters is
// cheap, while pasting six multi-page comments would push the actual ask
// thousands of tokens away from the output -- which is exactly the failure
// that made the verdict line never appear.
//
// Bots are marked because they post here too: one of the six comments on the
// pull request this was designed against is a CI summary. Unmarked, a build
// report reads as review feedback that must be addressed before an approval.
//
// Resolved threads are listed as well, on the owner's instruction, so a thread
// closed without a fix is still caught. The note is explicit that listing them
// is for the verdict and not an invitation to re-open what a colleague closed.
func priorNote(p *PriorFeedback) string {
	if p == nil || (len(p.Items) == 0 && !p.Incomplete) {
		return ""
	}

	var b strings.Builder
	b.WriteString("Existing feedback on this pull request, from every source firstpass can see " +
		"(inline review threads, review bodies, and plain comments), including your own earlier " +
		"passes and your colleagues':\n\n")

	for _, it := range p.Items {
		b.WriteString("  - ")
		b.WriteString(it.Surface)
		if it.State != "" {
			b.WriteString(" (" + it.State + ")")
		}
		b.WriteString(" by " + it.Author)
		if it.IsBot {
			b.WriteString(" [bot]")
		}
		if it.Path != "" {
			b.WriteString(" on " + it.Path)
			if it.Line > 0 {
				b.WriteString(fmt.Sprintf(":%d", it.Line))
			}
		}
		switch {
		case it.Surface == "thread" && it.Resolved:
			b.WriteString(" — RESOLVED")
		case it.Surface == "thread":
			b.WriteString(" — UNRESOLVED")
		}
		if it.Outdated {
			b.WriteString(", outdated")
		}
		if it.URL != "" {
			b.WriteString("\n    " + it.URL)
		}
		if it.Excerpt != "" {
			b.WriteString("\n    " + it.Excerpt)
		}
		b.WriteString("\n")
	}

	b.WriteString("\nEach line is the first line only. Read the full text with `gh` where it " +
		"matters; the URL is on the line above it.\n\n" +
		"\"outdated\" means the code the thread pointed at has changed. It does NOT mean the point " +
		"was addressed — the lines may merely have moved. Only the current code can tell you.\n\n" +
		"A [bot] item is machine output such as a CI summary, not review feedback.\n\n" +
		"RESOLVED threads are listed so that one closed without a real fix is still caught. That is " +
		"for your verdict only: do not re-open or re-comment on a thread a colleague has resolved.\n")

	if p.Incomplete {
		b.WriteString("\nThis list is INCOMPLETE — firstpass could not enumerate all of the " +
			"feedback. Say so in your review, and do not treat the absence of an item as evidence " +
			"that nothing was raised.\n")
	}
	return b.String()
}

// priorClause is the instruction that travels in the -p value, where this
// project has measured that instructions are actually followed.
//
// The index alone is not enough: it is data, and data does not tell the
// reviewer what to do at the moment it prints a verdict. The two travel in
// different channels on purpose -- evidence in the system prompt, the demand
// next to the ask it constrains.
func priorClause(p *PriorFeedback) string {
	if p == nil || (len(p.Items) == 0 && !p.Incomplete) {
		return ""
	}
	return "\n\nThis pull request already carries feedback from earlier passes and from your " +
		"colleagues; it is listed in your system prompt. Your verdict covers the WHOLE pull " +
		"request, not only the newest commits: print `approve` only if the current code is sound " +
		"AND every previously raised point that requires a change has been addressed in it. Minor " +
		"nits need not block an approval. Do not re-post feedback that is already on the pull " +
		"request."
}
