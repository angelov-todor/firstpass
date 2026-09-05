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
// head with no branch and no upstream, so a bare "/code-review" reviews an
// empty diff -- and in live mode has no PR to comment on.
func TestPromptNamesThePRAndOmitsCommentInDryRun(t *testing.T) {
	rr := New(&runner.Fake{}, "claude", nil, true, t.TempDir())
	got := rr.Prompt(ref)
	// The command and its target lead the prompt; the verdict ask follows it.
	// See promptVerdictAsk for why it is there.
	if !strings.HasPrefix(got, "/code-review "+ref.URL()) {
		t.Errorf("Prompt() = %q, want it to open with the command and the PR URL", got)
	}
	if strings.Contains(got, "--comment") {
		t.Errorf("Prompt() = %q; a dry run must not post", got)
	}
}

func TestPromptNamesThePRAndIncludesCommentWhenLive(t *testing.T) {
	rr := New(&runner.Fake{}, "claude", nil, false, t.TempDir())
	got := rr.Prompt(ref)
	if !strings.HasPrefix(got, "/code-review "+ref.URL()+" --comment") {
		t.Errorf("Prompt() = %q, want it to open with the command, the PR URL and --comment", got)
	}
}

// Dry run and live must differ by exactly the --comment flag: that
// equivalence is what makes reading a dry-run report a trustworthy preview of
// what would be posted. Asserted by removing the flag rather than by
// appending it, because the flag now sits between the command and the verdict
// ask rather than at the end.
func TestDryRunAndLivePromptsDifferOnlyByComment(t *testing.T) {
	dry := New(&runner.Fake{}, "claude", nil, true, t.TempDir()).Prompt(ref)
	live := New(&runner.Fake{}, "claude", nil, false, t.TempDir()).Prompt(ref)
	if strings.Replace(live, " --comment", "", 1) != dry {
		t.Errorf("dry = %q, live = %q; the only difference must be --comment", dry, live)
	}
	if !strings.Contains(live, "--comment") {
		t.Errorf("live = %q must carry --comment", live)
	}
}

func TestRunInvokesClaudeInTheWorktreeWithConfiguredArgs(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "code-review", Result: runner.Result{Stdout: []byte("no findings")}},
	}}
	rr := New(f, "claude", []string{"--permission-mode", "bypassPermissions"}, true, t.TempDir())

	if _, err := rr.Run(context.Background(), filepath.Join("work", "aex-balances"), ref, nil); err != nil {
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
		{Match: "code-review", Result: runner.Result{Stdout: []byte("posted")}},
	}}
	rr := New(f, "claude", []string{"--permission-mode", "bypassPermissions"}, false, t.TempDir())

	if _, err := rr.Run(context.Background(), "work", ref, nil); err != nil {
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
		{Match: "code-review", Result: runner.Result{Stdout: []byte("finding: off-by-one")}},
	}}
	rr := New(f, "claude", nil, true, dir)

	res, err := rr.Run(context.Background(), "work", ref, nil)
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
		{Match: "code-review", Result: runner.Result{
			Stdout: []byte("posted\n" + VerdictMarker + " findings\n"),
		}},
	}}
	rr := New(f, "claude", nil, false, dir)

	res, err := rr.Run(context.Background(), "work", ref, nil)
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
		{Match: "code-review", Result: runner.Result{Stdout: []byte("a full review, ending in prose")}},
	}}
	rr := New(f, "claude", nil, false, dir)

	res, err := rr.Run(context.Background(), "work", ref, nil)
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
		{Match: "code-review", Result: runner.Result{ExitCode: 1, Stderr: []byte("rate limited")}},
	}}
	rr := New(f, "claude", nil, true, t.TempDir())

	res, err := rr.Run(context.Background(), "work", ref, nil)
	if err == nil {
		t.Fatal("a non-zero claude exit must be an error so the PR is not recorded as reviewed")
	}
	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", res.ExitCode)
	}
}

func TestRunSurfacesTimeout(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "code-review", Err: context.DeadlineExceeded},
	}}
	rr := New(f, "claude", nil, false, t.TempDir())

	if _, err := rr.Run(context.Background(), "work", ref, nil); err == nil {
		t.Fatal("a timeout must be an error: the deadline can fire mid-post")
	}
}
