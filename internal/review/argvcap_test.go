package review

import (
	"slices"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/runner"
)

// TestTheCommandLineStaysWithinTheOperatingSystemsLimit is the assertion that
// stops this feature from silently un-reviewing pull requests.
//
// Windows caps a process's entire command line at 32,767 characters, and the
// sibling diffs travel inside a single --append-system-prompt argument.
// Measured on the target machine: a 33,000-character argument fails with
// "fork/exec: The filename or extension is too long". A failed exec is
// recorded as needs_attention and never retried automatically -- so an
// oversized prompt does not degrade a review, it takes a pull request that
// reviewed fine yesterday and leaves it permanently unreviewed.
//
// The worst case is built here rather than assumed: the maximum number of
// siblings, each at the maximum diff size, on top of a docs note and a full
// prior-feedback index.
func TestTheCommandLineStaysWithinTheOperatingSystemsLimit(t *testing.T) {
	const maxSiblings = 2 // pipeline.maxSiblings; duplicated to keep the packages independent
	const maxDiff = 6 * 1024

	var sibs []Sibling
	for i := range maxSiblings {
		sibs = append(sibs, Sibling{
			Key:    "example-org/some-service#100",
			URL:    "https://github.com/example-org/some-service/pull/100",
			Diff:   strings.Repeat("+ a plausible line of a unified diff, about sixty characters\n", maxDiff/60),
			Status: "not reviewed by firstpass (skipped_author); nothing else will look at it",
			// Alternate, so both branches of the note contribute.
			Truncated: i == 0,
		})
	}

	var prior PriorFeedback
	for range 25 { // ghpr.maxFeedbackItems
		prior.Items = append(prior.Items, PriorItem{
			Surface: "thread", Author: "a-colleague", Path: "src/Some/Deeply/Nested/Path/File.cs",
			Line: 1234, Excerpt: strings.Repeat("x", 100),
			URL: "https://github.com/example-org/some-service/pull/100#discussion_r1234567890",
		})
	}

	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "Review pull request", Result: runner.Result{Stdout: []byte("done")}},
	}}
	rr := New(f, "claude", []string{"--permission-mode", "bypassPermissions"}, false, t.TempDir()).
		WithDocs(`C:\Users\someone\projects\github.com\example-org\example-docs`)

	if _, err := rr.Run(t.Context(), "work", ref, &PreviousPass{HeadSHA: "0123456789abcdef", Posted: true},
		&prior, sibs); err != nil {
		t.Fatal(err)
	}

	total := len(f.Calls[0].Name)
	for _, a := range f.Calls[0].Args {
		total += len(a) + 1
	}
	if total > argvBudget {
		t.Errorf("worst-case command line is %d bytes, budget %d (OS ceiling %d): a review "+
			"this size fails to exec and is recorded needs_attention, never retried",
			total, argvBudget, maxCommandLine)
	}
	if total > maxCommandLine {
		t.Fatalf("worst-case command line is %d bytes and would not exec at all", total)
	}
}

// And when it does not fit, the siblings are what gives way -- not the prompt,
// not the verdict protocol, and not a truncation that could leave the reviewer
// holding half of somebody else's diff.
func TestAnOversizedSiblingBlockIsDroppedNotTruncated(t *testing.T) {
	huge := []Sibling{{Key: "b#2", Diff: strings.Repeat("x", argvBudget+1000)}}

	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "Review pull request", Result: runner.Result{Stdout: []byte("done")}},
	}}
	var logged bool
	rr := New(f, "claude", nil, false, t.TempDir())
	rr.Log = func(string, ...any) { logged = true }

	if _, err := rr.Run(t.Context(), "work", ref, nil, nil, huge); err != nil {
		t.Fatal(err)
	}
	args := f.Calls[0].Args
	system := args[slices.Index(args, "--append-system-prompt")+1]

	if strings.Contains(system, diffBegin) {
		t.Error("an oversized sibling block must be dropped entirely, not included")
	}
	if strings.Contains(system, strings.Repeat("x", 200)) {
		t.Error("no part of the oversized diff may survive: a truncated system prompt can end " +
			"inside somebody else's diff")
	}
	// The verdict protocol -- the part that is the job rather than context --
	// must be untouched.
	if system != verdictInstruction {
		t.Errorf("dropping the siblings must leave the rest of the system prompt exactly as it "+
			"was:\n%q", system)
	}
	// And it must not be silent. Silent degradation is the failure mode this
	// project has been bitten by most often.
	if !logged {
		t.Error("dropping the sibling context must be logged")
	}
}
