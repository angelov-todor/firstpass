package review

// A second pass reviews the whole pull request again, so the reviewer has to
// be told a pass already happened -- otherwise it restates every finding the
// author has not fixed, on the same lines.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/runner"
)

const prevSHA = "0123456789abcdef0123456789abcdef01234567"

func fakeWithReply(stdout string) *runner.Fake {
	return &runner.Fake{Replies: []runner.Reply{
		{Match: "code-review", Result: runner.Result{Stdout: []byte(stdout)}},
	}}
}

// The note travels as --append-system-prompt, never inside the -p value.
// Everything after "/code-review" there becomes the slash command's
// $ARGUMENTS, which parses an effort level, a --comment flag and a target: a
// paragraph of prose appended there is at best dropped, and at worst perturbs
// the target parsing -- and firstpass would not find out until it silently
// failed in production.
//
// Asserted on ordered argv, not by substring presence, because "-p" must be
// immediately followed by the whole prompt and nothing else.
func TestSecondPassNoteReachesTheSystemPromptAndNotThePrompt(t *testing.T) {
	f := fakeWithReply("posted")
	rr := New(f, "claude", []string{"--permission-mode", "bypassPermissions"}, false, t.TempDir())

	if _, err := rr.Run(context.Background(), "work", ref, &PreviousPass{HeadSHA: prevSHA}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("Calls = %+v", f.Calls)
	}
	args := f.Calls[0].Args
	want := []string{
		"-p", "/code-review " + ref.URL() + " --comment",
		"--append-system-prompt", verdictInstruction + "\n\n" + secondPassNote(PreviousPass{HeadSHA: prevSHA}),
		"--permission-mode", "bypassPermissions",
	}
	if !slices.Equal(args, want) {
		t.Errorf("Args = %q, want %q", args, want)
	}

	// The -p value itself, checked independently of the ordered comparison
	// above so a future reshuffle of the argv cannot smuggle the note in.
	i := slices.Index(args, "-p")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no -p in %q", args)
	}
	prompt := args[i+1]
	if strings.Contains(prompt, "\n") {
		t.Errorf("-p = %q; a newline in the prompt is a second line of $ARGUMENTS", prompt)
	}
	for _, frag := range []string{"previous automated pass", "restate", prevSHA[:12], verdictInstruction} {
		if strings.Contains(prompt, frag) {
			t.Errorf("-p = %q must not carry %q: it would become the slash command's $ARGUMENTS", prompt, frag)
		}
	}
}

// A first pass must invoke claude exactly as it did before this feature: same
// prompt, same system prompt, nothing appended. Empty means "first pass".
func TestFirstPassArgvCarriesNoSecondPassNote(t *testing.T) {
	f := fakeWithReply("no findings")
	rr := New(f, "claude", []string{"--permission-mode", "bypassPermissions"}, true, t.TempDir())

	if _, err := rr.Run(context.Background(), "work", ref, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-p", "/code-review " + ref.URL(),
		"--append-system-prompt", verdictInstruction,
		"--permission-mode", "bypassPermissions",
	}
	if !slices.Equal(f.Calls[0].Args, want) {
		t.Errorf("Args = %q, want %q", f.Calls[0].Args, want)
	}
}

// The -p value must be byte-identical across passes: the dry-run/live
// difference stays exactly --comment, and a pass number is not something the
// slash command has ever been handed.
func TestThePromptIsByteIdenticalAcrossPasses(t *testing.T) {
	for _, dry := range []bool{true, false} {
		first, second := fakeWithReply("x"), fakeWithReply("x")
		if _, err := New(first, "claude", nil, dry, t.TempDir()).
			Run(context.Background(), "work", ref, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := New(second, "claude", nil, dry, t.TempDir()).
			Run(context.Background(), "work", ref, &PreviousPass{HeadSHA: prevSHA}); err != nil {
			t.Fatal(err)
		}
		a := first.Calls[0].Args[slices.Index(first.Calls[0].Args, "-p")+1]
		b := second.Calls[0].Args[slices.Index(second.Calls[0].Args, "-p")+1]
		if a != b {
			t.Errorf("dry=%v: first-pass -p = %q, second-pass -p = %q; they must be identical", dry, a, b)
		}
	}
}

// What the note has to say, and why. Without the "already posted" and "do not
// restate" halves the reviewer has no reason not to repeat every unfixed
// finding on the same line it used last time.
func TestSecondPassNoteNamesThePreviousCommitAndForbidsRestating(t *testing.T) {
	n := secondPassNote(PreviousPass{HeadSHA: prevSHA})
	for _, want := range []string{"previous automated pass", "inline comments", prevSHA[:12]} {
		if !strings.Contains(n, want) {
			t.Errorf("the note must say %q:\n%s", want, n)
		}
	}
	if !strings.Contains(n, "changed since") {
		t.Errorf("the note must point the reviewer at what has changed:\n%s", n)
	}
	if !strings.Contains(n, "Do not restate findings from that pass") {
		t.Errorf("the note must forbid restating the previous pass's findings:\n%s", n)
	}
	if strings.Contains(n, prevSHA) {
		t.Errorf("the note must name the short SHA, not the whole 40 characters:\n%s", n)
	}
}

// The dry-run report is the gate before going live, so a second pass must not
// overwrite the report the first pass left behind: the operator needs both to
// see what changed.
func TestADryRunSecondPassReportDoesNotOverwriteTheFirst(t *testing.T) {
	dir := t.TempDir()

	rr := New(fakeWithReply("first pass findings"), "claude", nil, true, dir)
	first, err := rr.Run(context.Background(), "work", ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	rr = New(fakeWithReply("second pass findings"), "claude", nil, true, dir)
	second, err := rr.Run(context.Background(), "work", ref, &PreviousPass{HeadSHA: prevSHA})
	if err != nil {
		t.Fatal(err)
	}

	// The first pass's name is unchanged, so nothing already on disk moves.
	if got := filepath.Base(first.ReportPath); got != "Example-Org_aex-balances_12.md" {
		t.Errorf("first-pass report = %q, want the name it has always had", got)
	}
	if first.ReportPath == second.ReportPath {
		t.Fatalf("both passes wrote %q; the first pass's report is gone", first.ReportPath)
	}
	body, err := os.ReadFile(first.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "first pass findings") {
		t.Errorf("the first pass's report was overwritten:\n%s", body)
	}
	body, err = os.ReadFile(second.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "second pass findings") {
		t.Errorf("the second pass's report is missing its output:\n%s", body)
	}
}

// A replay is the reason this variant exists. The documented use of `firstpass
// replay` is a needs_attention pull request -- one whose review died part-way
// through posting -- so "a previous pass posted its findings" is not true
// there: some are posted and some are not, and firstpass cannot tell which.
// Sending the reviewer in believing either extreme is how the duplicate
// comment set needs_attention exists to warn about actually happens.
func TestTheNoteForAnIncompletePreviousPassAdmitsTheUncertainty(t *testing.T) {
	inc := secondPassNote(PreviousPass{HeadSHA: prevSHA, Incomplete: true})

	for _, want := range []string{
		"did not finish",
		"may already be posted as inline comments",
		"and some may not",
		"cannot tell how far it got",
		"check whether that comment is already on the pull request",
		prevSHA[:12],
	} {
		if !strings.Contains(inc, want) {
			t.Errorf("the incomplete note must say %q:\n%s", want, inc)
		}
	}
	// It must not make the claim the complete note makes.
	if strings.Contains(inc, "posted its findings as inline comments on it") {
		t.Errorf("the incomplete note must not claim the previous pass finished posting:\n%s", inc)
	}
	if strings.Contains(inc, "Do not restate findings from that pass") {
		t.Errorf("a blanket \"do not restate\" is wrong here: a finding the earlier pass never "+
			"got to must still be raised:\n%s", inc)
	}
	// And it must still tell the reviewer to raise what is genuinely missing,
	// or the uncertainty turns into silence.
	if !strings.Contains(inc, "raise it") {
		t.Errorf("the incomplete note must still ask for findings that are not already posted:\n%s", inc)
	}
	if inc == secondPassNote(PreviousPass{HeadSHA: prevSHA}) {
		t.Error("the two notes must differ; otherwise Incomplete is decoration")
	}
}

// Same wire as the complete note: --append-system-prompt, never the -p value.
func TestTheIncompleteNoteAlsoTravelsAsASystemPromptAndNotInThePrompt(t *testing.T) {
	f := fakeWithReply("posted")
	rr := New(f, "claude", nil, false, t.TempDir())
	pp := &PreviousPass{HeadSHA: prevSHA, Incomplete: true}

	if _, err := rr.Run(context.Background(), "work", ref, pp); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-p", "/code-review " + ref.URL() + " --comment",
		"--append-system-prompt", verdictInstruction + "\n\n" + secondPassNote(*pp),
	}
	if !slices.Equal(f.Calls[0].Args, want) {
		t.Errorf("Args = %q, want %q", f.Calls[0].Args, want)
	}
	prompt := f.Calls[0].Args[slices.Index(f.Calls[0].Args, "-p")+1]
	for _, frag := range []string{"\n", "did not finish", "previous automated pass"} {
		if strings.Contains(prompt, frag) {
			t.Errorf("-p = %q must not carry %q", prompt, frag)
		}
	}
}
