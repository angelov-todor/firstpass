package review

// A second pass reviews the whole pull request again, so the reviewer has to
// be told a pass already happened -- otherwise it restates every finding the
// author has not fixed, on the same lines.

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
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

	if _, err := rr.Run(context.Background(), "work", ref, &PreviousPass{HeadSHA: prevSHA, Posted: true}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("Calls = %+v", f.Calls)
	}
	args := f.Calls[0].Args
	want := []string{
		"-p", "/code-review " + ref.URL() + " --comment",
		"--append-system-prompt", verdictInstruction + "\n\n" + secondPassNote(PreviousPass{HeadSHA: prevSHA, Posted: true}),
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
			Run(context.Background(), "work", ref, &PreviousPass{HeadSHA: prevSHA, Posted: true}); err != nil {
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
	n := secondPassNote(PreviousPass{HeadSHA: prevSHA, Posted: true})
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
	second, err := rr.Run(context.Background(), "work", ref, &PreviousPass{HeadSHA: prevSHA, Posted: true})
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
	// Every shape an incomplete previous pass can take, because the guard has
	// to hold in all of them. It did not: HeadUnchanged was checked first and
	// returned, so a replay of a needs_attention pull request whose head had
	// not moved -- the single most documented replay there is -- was told that
	// pass's findings "were posted as inline comments on it", the exact
	// untruth this variant exists to prevent.
	for _, tc := range []struct {
		name string
		pp   PreviousPass
	}{
		{"the head has moved on", PreviousPass{HeadSHA: prevSHA, Posted: true, Incomplete: true}},
		{"the head has not moved", PreviousPass{
			HeadSHA: prevSHA, Posted: true, Incomplete: true, HeadUnchanged: true,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inc := secondPassNote(tc.pp)
			assertAdmitsUncertainty(t, inc)

			if strings.Contains(inc, "Do not restate findings from that pass") {
				t.Errorf("a blanket \"do not restate\" is wrong here: a finding the earlier pass "+
					"never got to must still be raised:\n%s", inc)
			}
			// And it must still say how to avoid the duplicate, or the
			// uncertainty turns into either a second comment or silence.
			if !strings.Contains(inc, "check whether that comment is already on the pull request") {
				t.Errorf("the reviewer must be told how to avoid duplicating a comment that did "+
					"land:\n%s", inc)
			}
			complete := secondPassNote(PreviousPass{
				HeadSHA: prevSHA, Posted: true, HeadUnchanged: tc.pp.HeadUnchanged,
			})
			if inc == complete {
				t.Error("the note must differ from the one for a pass that finished; otherwise " +
					"Incomplete is decoration")
			}
		})
	}
}

// assertAdmitsUncertainty fails unless the note hedges what the earlier pass
// managed to post.
//
// It asserts the hedge positively and refuses the claim by *pattern*, not by
// one phrasing of it. Grepping for a single sentence is how the unchanged-head
// variant slipped past: it said "its findings were posted as inline comments"
// where the guard was looking for "posted its findings as inline comments on
// it", so the guard held and the claim was made anyway. A future rewording has
// to keep the hedge or trip one of these.
func assertAdmitsUncertainty(t *testing.T, note string) {
	t.Helper()
	for _, want := range []string{
		"did not finish",
		"may already be posted",
		"and some may not",
		"cannot tell how far it got",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("an incomplete pass's note must say %q:\n%s", want, note)
		}
	}
	for _, claim := range []*regexp.Regexp{
		regexp.MustCompile(`(?i)posted its findings`),
		regexp.MustCompile(`(?i)findings (?:were|was|are) posted`),
		regexp.MustCompile(`(?i)findings are (?:already )?on the pull request`),
		regexp.MustCompile(`(?i)they are already on the pull request`),
	} {
		if got := claim.FindString(note); got != "" {
			t.Errorf("the note claims %q of a pass that did not finish; some of its findings are "+
				"on the pull request and some are not, and nothing knows which:\n%s", got, note)
		}
	}
}

// Same wire as the complete note: --append-system-prompt, never the -p value.
func TestTheIncompleteNoteAlsoTravelsAsASystemPromptAndNotInThePrompt(t *testing.T) {
	f := fakeWithReply("posted")
	rr := New(f, "claude", nil, false, t.TempDir())
	pp := &PreviousPass{HeadSHA: prevSHA, Posted: true, Incomplete: true}

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

// A dry run records `reviewed` but withholds --comment, so its findings went
// to a local report and never reached the pull request. The complete note
// would then be flatly untrue on both of its claims -- "posted its findings as
// inline comments on it" and "they are already on the pull request" -- and its
// "do not restate" would suppress every finding for no reason at all. Since
// nothing is on the pull request there is nothing to warn about, so nothing is
// said.
func TestAPreviousPassThatPostedNothingSendsNoNote(t *testing.T) {
	f := fakeWithReply("posted")
	rr := New(f, "claude", []string{"--permission-mode", "bypassPermissions"}, false, t.TempDir())

	if _, err := rr.Run(context.Background(), "work", ref,
		&PreviousPass{HeadSHA: prevSHA, Posted: false}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-p", "/code-review " + ref.URL() + " --comment",
		"--append-system-prompt", verdictInstruction,
		"--permission-mode", "bypassPermissions",
	}
	if !slices.Equal(f.Calls[0].Args, want) {
		t.Errorf("Args = %q, want the first-pass argv: there is nothing on the pull request to "+
			"tell the reviewer about", f.Calls[0].Args)
	}
}

// The report still goes to its own filename, so a dry-run pass following a
// dry-run pass does not overwrite the report the operator is about to read.
// That is why the previous pass is still passed in at all.
func TestAPassAfterAnUnpostedOneStillGetsItsOwnReportName(t *testing.T) {
	dir := t.TempDir()
	first, err := New(fakeWithReply("pass one"), "claude", nil, true, dir).
		Run(context.Background(), "work", ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(fakeWithReply("pass two"), "claude", nil, true, dir).
		Run(context.Background(), "work", ref, &PreviousPass{HeadSHA: prevSHA, Posted: false})
	if err != nil {
		t.Fatal(err)
	}
	if first.ReportPath == second.ReportPath {
		t.Fatalf("both passes wrote %q; the report the operator is reading is gone", first.ReportPath)
	}
}

// A replay at the very commit the earlier pass reviewed. "Concentrate on what
// has changed" names changes that do not exist, and a blanket "do not restate"
// tells the reviewer not to report the findings it is being asked to find --
// close to neutering the command, and replay is exactly what someone runs when
// pass 1 was unsatisfying or when they are flipping dry run to live.
//
// Both previous-pass shapes get the same instruction, because the useful one
// is the same either way: review in full, and check before posting a comment
// that may already be there.
func TestTheNoteForAnUnchangedHeadAsksForAFullReview(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		pp := PreviousPass{HeadSHA: prevSHA, Posted: true, HeadUnchanged: true, Incomplete: incomplete}
		n := secondPassNote(pp)

		if strings.Contains(n, "Concentrate on what has changed") {
			t.Errorf("incomplete=%v: nothing has changed, so this asks for the impossible:\n%s",
				incomplete, n)
		}
		if strings.Contains(n, "Do not restate findings from that pass") {
			t.Errorf("incomplete=%v: a blanket refusal here suppresses the findings the review "+
				"is being asked for:\n%s", incomplete, n)
		}
		if !strings.Contains(n, "review it in full") {
			t.Errorf("incomplete=%v: the reviewer must be asked for a full review:\n%s", incomplete, n)
		}
		if !strings.Contains(n, "already on the pull request") {
			t.Errorf("incomplete=%v: the duplicate-comment hazard is the one thing still worth "+
				"saying:\n%s", incomplete, n)
		}
		// The "not a diff against X" paragraph is about a commit that may have
		// become unreachable. The head *is* that commit here.
		if strings.Contains(n, "may no longer be reachable") {
			t.Errorf("incomplete=%v: the pull request is at that commit, so this is nonsense:\n%s",
				incomplete, n)
		}
	}
}

// And the moved-head notes keep saying what they said, so the unchanged-head
// variant cannot be implemented by weakening the common case.
func TestTheMovedHeadNotesStillAskToFocusOnWhatChanged(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		n := secondPassNote(PreviousPass{HeadSHA: prevSHA, Posted: true, Incomplete: incomplete})
		if !strings.Contains(n, "Concentrate on what has changed since "+prevSHA[:12]) {
			t.Errorf("incomplete=%v: the moved-head note must still point at what changed:\n%s",
				incomplete, n)
		}
		if !strings.Contains(n, "may no longer be reachable") {
			t.Errorf("incomplete=%v: the moved-head note must still explain why it is not a diff:\n%s",
				incomplete, n)
		}
	}
}
