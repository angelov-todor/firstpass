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
	if got != "/code-review "+ref.URL() {
		t.Errorf("Prompt() = %q, want the command plus the PR URL and nothing else", got)
	}
	if strings.Contains(got, "--comment") {
		t.Errorf("Prompt() = %q; a dry run must not post", got)
	}
}

func TestPromptNamesThePRAndIncludesCommentWhenLive(t *testing.T) {
	rr := New(&runner.Fake{}, "claude", nil, false, t.TempDir())
	got := rr.Prompt(ref)
	if got != "/code-review "+ref.URL()+" --comment" {
		t.Errorf("Prompt() = %q, want the command, the PR URL and --comment", got)
	}
}

// Dry run and live must differ by exactly the --comment flag: that
// equivalence is what makes reading a dry-run report a trustworthy preview of
// what would be posted.
func TestDryRunAndLivePromptsDifferOnlyByComment(t *testing.T) {
	dry := New(&runner.Fake{}, "claude", nil, true, t.TempDir()).Prompt(ref)
	live := New(&runner.Fake{}, "claude", nil, false, t.TempDir()).Prompt(ref)
	if live != dry+" --comment" {
		t.Errorf("dry = %q, live = %q; the only difference must be --comment", dry, live)
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
		"-p", "/code-review " + ref.URL(),
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
		"-p", "/code-review " + ref.URL() + " --comment",
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
		{Match: "code-review", Result: runner.Result{Stdout: []byte("posted")}},
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
