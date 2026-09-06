package review

import (
	"slices"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/runner"
)

func samplePrior() *PriorFeedback {
	return &PriorFeedback{Items: []PriorItem{
		{Surface: "thread", Author: "reviewer-one", Path: "src/A.cs", Line: 9,
			Excerpt: "Null check missing.", URL: "https://example.invalid/t1"},
		{Surface: "thread", Author: "reviewer-one", Path: "src/C.cs", Line: 7, Resolved: true,
			Excerpt: "Naming.", URL: "https://example.invalid/t3"},
		{Surface: "thread", Author: "reviewer-two", Path: "src/B.cs", Outdated: true,
			Excerpt: "This allocates.", URL: "https://example.invalid/t2"},
		{Surface: "review", Author: "reviewer-two", State: "COMMENTED",
			Excerpt: "## Overall", URL: "https://example.invalid/r1"},
		{Surface: "comment", Author: "github-actions", IsBot: true,
			Excerpt: "# Summary", URL: "https://example.invalid/c1"},
	}}
}

// TestTheAskAndTheEvidenceTravelInDifferentChannels is the load-bearing
// assertion of this feature, and it encodes something this project paid to
// learn.
//
// An instruction in --append-system-prompt is not reliably followed over a
// long agentic run: the verdict ask lived there alone and produced nothing for
// fourteen consecutive production reviews. So the demand -- "your verdict
// covers the whole pull request, and only approve if prior points are
// addressed" -- goes in the -p value, next to the ask it constrains. The index
// of prior feedback is evidence rather than a demand, and evidence in the
// system prompt is simply available; it also cannot go in the -p value, which
// is a slash command's arguments.
func TestTheAskAndTheEvidenceTravelInDifferentChannels(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "code-review", Result: runner.Result{Stdout: []byte("done")}},
	}}
	rr := New(f, "claude", nil, false, t.TempDir())

	if _, err := rr.Run(t.Context(), "work", ref, nil, samplePrior()); err != nil {
		t.Fatal(err)
	}
	args := f.Calls[0].Args
	prompt := args[slices.Index(args, "-p")+1]
	system := args[slices.Index(args, "--append-system-prompt")+1]

	// The demand is in the prompt.
	for _, want := range []string{"WHOLE pull request", "every previously raised point", "Minor nits"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the -p value must carry the demand (%q missing): %q", want, prompt)
		}
	}
	// The evidence is in the system prompt, and not in the prompt: the -p
	// value is a slash command's arguments, and a five-item index there would
	// push the verdict ask far away from the end of the prompt -- which is the
	// position that made it work at all.
	if !strings.Contains(system, "src/A.cs:9") {
		t.Errorf("the system prompt must carry the index: %q", system)
	}
	if strings.Contains(prompt, "src/A.cs") {
		t.Errorf("the index must not be in the -p value: %q", prompt)
	}
}

func TestThePriorIndexShowsWhatTheReviewerNeedsToJudge(t *testing.T) {
	note := priorNote(samplePrior())

	// Resolution state, both ways round. A resolved thread is listed so one
	// closed without a real fix is still caught -- the owner's instruction --
	// and an unresolved one has to be distinguishable from it.
	if !strings.Contains(note, "UNRESOLVED") || !strings.Contains(note, "RESOLVED") {
		t.Errorf("thread resolution must be visible:\n%s", note)
	}
	// And listing resolved threads must not turn into re-litigating them.
	if !strings.Contains(note, "do not re-open or re-comment on a thread a colleague has resolved") {
		t.Errorf("the note must forbid re-opening resolved threads:\n%s", note)
	}
	// "outdated" is the trap: on the real pull request this was designed
	// against, every thread was unresolved and outdated, which may mean fixed
	// or may mean the lines merely moved.
	if !strings.Contains(note, "does NOT mean the point") {
		t.Errorf("the note must warn that outdated is not addressed:\n%s", note)
	}
	// A CI summary is not review feedback.
	if !strings.Contains(note, "[bot]") {
		t.Errorf("bot authors must be marked:\n%s", note)
	}
	// Every surface, because the team uses all three.
	for _, want := range []string{"thread", "review (COMMENTED)", "comment"} {
		if !strings.Contains(note, want) {
			t.Errorf("surface %q missing:\n%s", want, note)
		}
	}
	// The URL is what makes "read the full text where it matters" cheap.
	if !strings.Contains(note, "https://example.invalid/t1") {
		t.Errorf("each item must carry its URL:\n%s", note)
	}
}

// An incomplete list has to say so to the reviewer as well as to the gate.
// firstpass refuses the approval either way, but a reviewer that believes it
// has seen everything may write a review that says so.
func TestAnIncompleteIndexSaysSo(t *testing.T) {
	note := priorNote(&PriorFeedback{Incomplete: true})
	if !strings.Contains(note, "INCOMPLETE") {
		t.Errorf("an incomplete list must be flagged:\n%s", note)
	}
	if priorClause(&PriorFeedback{Incomplete: true}) == "" {
		t.Error("an incomplete list with no items must still carry the demand")
	}
}

// With nothing on the pull request there is nothing to say, and saying it
// anyway is how boilerplate teaches a reader to skip the text.
func TestNoFeedbackMeansNoNoteAndNoClause(t *testing.T) {
	for _, p := range []*PriorFeedback{nil, {}} {
		if got := priorNote(p); got != "" {
			t.Errorf("priorNote(%+v) = %q, want empty", p, got)
		}
		if got := priorClause(p); got != "" {
			t.Errorf("priorClause(%+v) = %q, want empty", p, got)
		}
	}
}

// The prompt with no prior feedback must be byte-identical to what it was
// before this feature, so the form already exercised against real claude is
// untouched for the common case.
func TestAPullRequestWithNoFeedbackAsksExactlyWhatItAlwaysDid(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "code-review", Result: runner.Result{Stdout: []byte("done")}},
	}}
	rr := New(f, "claude", nil, false, t.TempDir())
	if _, err := rr.Run(t.Context(), "work", ref, nil, &PriorFeedback{}); err != nil {
		t.Fatal(err)
	}
	args := f.Calls[0].Args
	if got := args[slices.Index(args, "-p")+1]; got != rr.Prompt(ref) {
		t.Errorf("-p = %q, want the unchanged prompt %q", got, rr.Prompt(ref))
	}
	if got := args[slices.Index(args, "--append-system-prompt")+1]; got != verdictInstruction {
		t.Errorf("system prompt gained something with no feedback to report: %q", got)
	}
}
