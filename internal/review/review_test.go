package review

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/runner"
)

var ref = prref.PRRef{Owner: "Example-Org", Repo: "aex-balances", Number: 12}

// C1a: the prompt is the only channel that can tell the reviewer which pull
// request to review. The prepared worktree is a detached checkout of the PR
// head with no branch and no upstream, so a review that is not told which pull
// request it is looking at sees an empty diff -- and in live mode has no PR to
// comment on.
func TestPromptNamesThePRAndOmitsCommentInDryRun(t *testing.T) {
	rr := New(&runner.Fake{}, "claude", nil, true, t.TempDir())
	got := rr.Prompt(ref)
	// A general instruction naming the pull request, not a slash command. That
	// is what lets the reviewer's own skills be selected for the repository it
	// is looking at -- a .NET review skill for a C# service -- instead of a
	// command's fixed procedure crowding them out.
	if !strings.HasPrefix(got, "Review pull request "+ref.URL()) {
		t.Errorf("Prompt() = %q, want it to open by naming the pull request", got)
	}
	// A dry run says so outright. Under the old slash command, dry run was
	// expressed by withholding a --comment flag that the command never read,
	// so nothing actually stopped it posting except that it happened not to.
	if !strings.Contains(got, "Do NOT post anything to GitHub") {
		t.Errorf("Prompt() = %q; a dry run must forbid posting, not merely omit a flag", got)
	}
}

func TestPromptNamesThePRAndIncludesCommentWhenLive(t *testing.T) {
	rr := New(&runner.Fake{}, "claude", nil, false, t.TempDir())
	got := rr.Prompt(ref)
	if !strings.HasPrefix(got, "Review pull request "+ref.URL()) {
		t.Errorf("Prompt() = %q, want it to open by naming the pull request", got)
	}
	// Live, the reviewer has to be told to post: nothing else will. The skill
	// that reviews .NET changes produces a report and posts nothing at all --
	// which the old slash command did do, and is why the instruction had to
	// become explicit when the command went away.
	if !strings.Contains(got, "Post each finding as an inline comment") {
		t.Errorf("Prompt() = %q; a live review must be told to post its findings", got)
	}
	if strings.Contains(got, "Do NOT post") {
		t.Errorf("Prompt() = %q; a live review must not be told to stay quiet", got)
	}
}

// Dry run and live must differ in exactly one thing: whether the reviewer is
// told to post. Everything else -- what to review, what to judge, what to
// print -- has to be identical, because that equivalence is what makes reading
// a dry-run report a trustworthy preview of what a live run would do.
func TestDryRunAndLivePromptsDifferOnlyInWhetherTheyPost(t *testing.T) {
	dry := New(&runner.Fake{}, "claude", nil, true, t.TempDir()).Prompt(ref)
	live := New(&runner.Fake{}, "claude", nil, false, t.TempDir()).Prompt(ref)

	const dryClause = " Do NOT post anything to GitHub: no comments, no review, nothing. " +
		"Report your findings in your output instead."
	const liveClause = " Post each finding as an inline comment on the pull request, on the " +
		"line it concerns, using gh."

	if strings.Replace(dry, dryClause, liveClause, 1) != live {
		t.Errorf("the prompts differ by more than the posting instruction:\ndry:  %q\nlive: %q",
			dry, live)
	}
}

func TestRunInvokesClaudeInTheWorktreeWithConfiguredArgs(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "Review pull request", Result: runner.Result{Stdout: []byte("no findings")}},
	}}
	rr := New(f, "claude", []string{"--permission-mode", "bypassPermissions"}, true, t.TempDir())

	if _, err := rr.Run(context.Background(), filepath.Join("work", "aex-balances"), ref, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("Calls = %+v", f.Calls)
	}
	c := f.Calls[0]
	if c.Dir != filepath.Join("work", "aex-balances") {
		t.Errorf("Dir = %q; claude must run inside the checkout so it sees the repo's CLAUDE.md and skills", c.Dir)
	}
	// Asserted in order, not by substring presence: "-p" must be immediately
	// followed by the whole prompt, or claude reads the PR URL as a second
	// positional argument and the prompt loses its target -- and the same for
	// --append-system-prompt and the verdict instruction. extraArgs stays
	// last, so operator config can still override anything firstpass sets.
	want := []string{
		"-p", New(&runner.Fake{}, "claude", nil, true, t.TempDir()).Prompt(ref),
		"--append-system-prompt", verdictInstruction,
		"--permission-mode", "bypassPermissions",
	}
	if !slices.Equal(c.Args, want) {
		t.Errorf("Args = %q, want %q", c.Args, want)
	}
}

func TestRunPassesTheLivePromptInOrder(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "Review pull request", Result: runner.Result{Stdout: []byte("posted")}},
	}}
	rr := New(f, "claude", []string{"--permission-mode", "bypassPermissions"}, false, t.TempDir())

	if _, err := rr.Run(context.Background(), "work", ref, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("Calls = %+v", f.Calls)
	}
	want := []string{
		"-p", rr.Prompt(ref),
		"--append-system-prompt", verdictInstruction,
		"--permission-mode", "bypassPermissions",
	}
	if !slices.Equal(f.Calls[0].Args, want) {
		t.Errorf("Args = %q, want %q", f.Calls[0].Args, want)
	}
}

func TestRunWritesAReportInDryRun(t *testing.T) {
	dir := t.TempDir()
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "Review pull request", Result: runner.Result{Stdout: []byte("finding: off-by-one")}},
	}}
	rr := New(f, "claude", nil, true, dir)

	res, err := rr.Run(context.Background(), "work", ref, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ReportPath == "" {
		t.Fatal("a dry run must write a report to read")
	}
	body, err := os.ReadFile(res.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"finding: off-by-one", ref.Key(), ref.URL(), "nothing posted"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("report missing %q:\n%s", want, body)
		}
	}
}

func TestRunWritesNoReportWhenLive(t *testing.T) {
	dir := t.TempDir()
	f := &runner.Fake{Replies: []runner.Reply{
		// A verdict line, because a live review that produced one has nothing
		// left to explain: its findings are on the pull request and its
		// verdict is in the record. The no-verdict case is the exception and
		// is covered by the test below.
		{Match: "Review pull request", Result: runner.Result{
			Stdout: []byte("posted\n" + VerdictMarker + " findings\n"),
		}},
	}}
	rr := New(f, "claude", nil, false, dir)

	res, err := rr.Run(context.Background(), "work", ref, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ReportPath != "" {
		t.Errorf("ReportPath = %q; a live run's findings live on the PR", res.ReportPath)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("no report files expected, found %d", len(entries))
	}
}

// TestALiveReviewWithNoVerdictKeepsItsOutput is the instrumentation that
// should have existed from the start.
//
// A live review that finishes without a verdict line is the one outcome
// firstpass cannot explain from its own records: the review worked, the
// comments are posted, and the only thing missing is the line firstpass
// needed. Live output used to be discarded unconditionally, so fourteen
// consecutive production reviews recorded "verdict unknown" and left nothing
// whatsoever to read. Working out why in the end needed a throwaway pull
// request and a dry run to reproduce what had already happened fourteen times.
func TestALiveReviewWithNoVerdictKeepsItsOutput(t *testing.T) {
	dir := t.TempDir()
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "Review pull request", Result: runner.Result{Stdout: []byte("a full review, ending in prose")}},
	}}
	rr := New(f, "claude", nil, false, dir)

	res, err := rr.Run(context.Background(), "work", ref, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != VerdictUnknown {
		t.Fatalf("Verdict = %q, want unknown", res.Verdict)
	}
	if res.ReportPath == "" {
		t.Fatal("a live review that printed no verdict must keep its output: " +
			"without it there is no way to tell a reviewer that ignored the ask " +
			"from one that was never asked")
	}
	body, err := os.ReadFile(res.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "a full review, ending in prose") {
		t.Errorf("the kept output must be what the reviewer actually printed:\n%s", body)
	}
}

func TestRunSurfacesNonZeroExit(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "Review pull request", Result: runner.Result{ExitCode: 1, Stderr: []byte("rate limited")}},
	}}
	rr := New(f, "claude", nil, true, t.TempDir())

	res, err := rr.Run(context.Background(), "work", ref, nil, nil)
	if err == nil {
		t.Fatal("a non-zero claude exit must be an error so the PR is not recorded as reviewed")
	}
	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", res.ExitCode)
	}
}

func TestRunSurfacesTimeout(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "Review pull request", Err: context.DeadlineExceeded},
	}}
	rr := New(f, "claude", nil, false, t.TempDir())

	if _, err := rr.Run(context.Background(), "work", ref, nil, nil); err == nil {
		t.Fatal("a timeout must be an error: the deadline can fire mid-post")
	}
}
