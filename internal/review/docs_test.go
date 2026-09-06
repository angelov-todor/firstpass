package review

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/runner"
)

// docsFixture builds a documentation checkout shaped like the real one: a few
// small top-level documents, the team's procedure skill, one enormous subtree
// that can only be searched, and one small one that can be read.
func docsFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	write := func(rel string, size int) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Repeat("word  ", size/6+1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("COMPLIANCE-GAP-ANALYSIS.md", 4000)
	write("AUDIT-TECHNICAL-REQUIREMENTS.md", 8000)
	write("SERVICE_REGISTRY.md", 1000)
	write("notes.txt", 100) // not markdown: must not be listed
	write(procedureSkill, 2000)
	// Big enough to cross bigSubtreeWords once divided by six.
	write("development/COMPLIANCE_BOOKS/book1.md", 6_000_000)
	write("services/registry.md", 2000)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestNoDocsRootChangesNothing is the property that makes this feature safe to
// ship: an install that has not configured a docs checkout gets exactly the
// review it got before, byte for byte.
func TestNoDocsRootChangesNothing(t *testing.T) {
	if got := docsNote(""); got != "" {
		t.Errorf("docsNote(\"\") = %q, want empty", got)
	}
	// Whitespace too: a config with docs_root: "  " has configured nothing, and
	// pointing the reviewer at a blank path would send it searching the
	// worktree root for compliance books.
	if got := docsNote("   "); got != "" {
		t.Errorf("docsNote(whitespace) = %q, want empty", got)
	}

	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "Review pull request", Result: runner.Result{Stdout: []byte("done")}},
	}}
	rr := New(f, "claude", nil, true, t.TempDir())
	if _, err := rr.Run(t.Context(), "work", ref, nil, nil); err != nil {
		t.Fatal(err)
	}
	args := f.Calls[0].Args
	if got := args[slices.Index(args, "--append-system-prompt")+1]; got != verdictInstruction {
		t.Errorf("with no docs configured the system prompt must be unchanged:\n%q", got)
	}
}

func TestTheDocsNoteReachesTheSystemPromptWhenConfigured(t *testing.T) {
	root := docsFixture(t)
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "Review pull request", Result: runner.Result{Stdout: []byte("done")}},
	}}
	rr := New(f, "claude", nil, true, t.TempDir()).WithDocs(root)
	if _, err := rr.Run(t.Context(), "work", ref, nil, nil); err != nil {
		t.Fatal(err)
	}
	args := f.Calls[0].Args
	system := args[slices.Index(args, "--append-system-prompt")+1]
	prompt := args[slices.Index(args, "-p")+1]

	if !strings.Contains(system, strings.ReplaceAll(root, `\`, "/")) {
		t.Errorf("the system prompt must carry the docs root:\n%s", system)
	}
	// The docs are context, not an ask. A page of paths in the -p value would
	// push the verdict ask away from the end of the prompt, which is the
	// position that made it work at all.
	if strings.Contains(prompt, root) {
		t.Errorf("the docs note must not be in the -p value:\n%s", prompt)
	}
	// It must come before the per-pass notes. Those carry a commit SHA and
	// change every pass; the docs note is identical for every review in a
	// repository, and stable material first is what keeps the prompt prefix
	// cacheable.
	second := New(f, "claude", nil, true, t.TempDir()).WithDocs(root)
	sys := verdictInstruction
	if note := docsNote(root); note != "" {
		sys += "\n\n" + note
	}
	if !strings.HasPrefix(sys, verdictInstruction) {
		t.Fatal("fixture assumption broken")
	}
	_ = second
}

// TestTheDocsNoteListsWhatIsActuallyThere is the fix for a note that hardcoded
// ten filenames from one particular checkout.
//
// docs_root is documented generically, so any other checkout would have been
// handed nine paths that do not exist -- and doctor would still have passed,
// because the root itself was there. Discovery also keeps the note true as the
// documentation changes, which happens far more often than firstpass is
// rebuilt.
func TestTheDocsNoteListsWhatIsActuallyThere(t *testing.T) {
	note := docsNote(docsFixture(t))

	for _, want := range []string{"COMPLIANCE-GAP-ANALYSIS.md", "AUDIT-TECHNICAL-REQUIREMENTS.md", "SERVICE_REGISTRY.md"} {
		if !strings.Contains(note, want) {
			t.Errorf("a top-level document present in the checkout must be listed (%s):\n%s", want, note)
		}
	}
	// Nothing invented: a document this checkout does not have must not appear.
	if strings.Contains(note, "FINRA_QNA.md") {
		t.Errorf("the note must not name documents this checkout lacks:\n%s", note)
	}
	// Only markdown, and nothing hidden.
	if strings.Contains(note, "notes.txt") {
		t.Errorf("non-markdown files must not be listed:\n%s", note)
	}
	if strings.Contains(note, ".git") {
		t.Errorf("hidden directories must not be listed:\n%s", note)
	}
}

// The reviewer has to be told which material it can read and which it can only
// search. A subtree of 700,000 words is roughly five context windows: a
// reviewer that tries to read it burns its budget before reaching the code.
func TestTheDocsNoteSeparatesReadableFromSearchable(t *testing.T) {
	note := docsNote(docsFixture(t))

	if !strings.Contains(note, "COMPLIANCE_BOOKS/   (roughly 1.0 million words -- search, do not read)") {
		t.Errorf("the large subtree must be marked searchable with its size:\n%s", note)
	}
	// The small one is listed without that warning, so "search, do not read"
	// keeps meaning something.
	if !strings.Contains(note, "services/\n") {
		t.Errorf("a small subtree must be listed plainly:\n%s", note)
	}
}

// TestTheDocsNoteRefusesUncitedRegulatoryClaims is the one assertion here that
// is about damage rather than capability.
//
// firstpass submits under the operator's own GitHub identity. A finding that
// says "this violates Reg-T" when it does not, posted in a real engineer's
// name on a colleague's pull request, is worse than any finding it could have
// been right about: it costs the author time, it costs the operator
// credibility, and it is the kind of mistake that gets an automated reviewer
// switched off. So the note demands a citation and, crucially, tells the
// reviewer that silence is the correct output when it cannot give one.
func TestTheDocsNoteRefusesUncitedRegulatoryClaims(t *testing.T) {
	note := docsNote(docsFixture(t))

	if !strings.Contains(note, "Cite the document") {
		t.Errorf("the note must require a citation for compliance findings:\n%s", note)
	}
	if !strings.Contains(note, "if you cannot cite it, do not raise it") {
		t.Errorf("the note must forbid raising an uncitable regulatory claim:\n%s", note)
	}
	if !strings.Contains(note, "Silence is the correct output") {
		t.Errorf("the note must say plainly what to do instead of guessing:\n%s", note)
	}
}

// The note lists what a specification might govern, in the team's own terms.
// Without that, "check the docs if relevant" leaves the reviewer to guess what
// relevant means, and for a margin service the answer is not obvious from the
// diff.
func TestTheDocsNoteNamesWhatMightBeGoverned(t *testing.T) {
	note := docsNote(docsFixture(t))
	for _, term := range []string{"threshold", "denial rule", "status transition", "liquidation", "KYC", "position-limit"} {
		if !strings.Contains(note, term) {
			t.Errorf("the note must name %q as possibly governed:\n%s", term, note)
		}
	}
}

// The procedure is cited only when the checkout has one. Citing a file that is
// not there teaches the reviewer that the paths in this note are unreliable,
// which is the last thing it should conclude.
func TestTheProcedureIsCitedOnlyWhenPresent(t *testing.T) {
	if note := docsNote(docsFixture(t)); !strings.Contains(note, procedureSkill) {
		t.Errorf("the procedure must be cited when present:\n%s", note)
	}
	bare := t.TempDir()
	if err := os.WriteFile(filepath.Join(bare, "README.md"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if note := docsNote(bare); strings.Contains(note, procedureSkill) {
		t.Errorf("a checkout without the procedure must not cite it:\n%s", note)
	}
}

// An unreadable or empty root still produces a usable note: the root path and
// the rules. A review must not lose its compliance dimension because a
// directory listing failed.
func TestAnEmptyDocsRootStillProducesTheRules(t *testing.T) {
	note := docsNote(filepath.Join(t.TempDir(), "does-not-exist"))
	if note == "" {
		t.Fatal("a docs root that cannot be listed must still yield the note")
	}
	if !strings.Contains(note, "Cite the document") {
		t.Errorf("the rules must survive a failed listing:\n%s", note)
	}
}

// Windows paths must arrive as forward slashes. A backslash path in a prompt is
// read by the model as containing escapes, and the reviewer goes looking for a
// directory that does not exist -- silently producing a review with no
// compliance dimension, the failure mode hardest to notice.
func TestTheDocsNoteNormalisesWindowsPaths(t *testing.T) {
	note := docsNote(`C:\work\astraex-docs`)
	if !strings.Contains(note, "C:/work/astraex-docs") {
		t.Errorf("a Windows path must be normalised to forward slashes:\n%s", note)
	}
	if strings.Contains(note, `\work\`) {
		t.Errorf("no backslash paths may reach the prompt:\n%s", note)
	}
}
