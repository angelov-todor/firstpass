package review

import (
	"slices"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/runner"
)

const docsRoot = "c:/work/astraex-docs"

// TestNoDocsRootChangesNothing is the property that makes this feature safe to
// ship: an install that has not configured a docs checkout gets exactly the
// review it got before, byte for byte.
func TestNoDocsRootChangesNothing(t *testing.T) {
	if got := docsNote(""); got != "" {
		t.Errorf("docsNote(\"\") = %q, want empty", got)
	}
	// Whitespace too: a config with docs_root: "  " has configured nothing,
	// and pointing the reviewer at a blank path would have it searching the
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
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "Review pull request", Result: runner.Result{Stdout: []byte("done")}},
	}}
	rr := New(f, "claude", nil, true, t.TempDir()).WithDocs(docsRoot)
	if _, err := rr.Run(t.Context(), "work", ref, nil, nil); err != nil {
		t.Fatal(err)
	}
	args := f.Calls[0].Args
	system := args[slices.Index(args, "--append-system-prompt")+1]
	prompt := args[slices.Index(args, "-p")+1]

	if !strings.Contains(system, docsRoot) {
		t.Errorf("the system prompt must carry the docs root:\n%s", system)
	}
	// The docs are context, not an ask. Putting a page of paths in the -p value
	// would push the verdict ask away from the end of the prompt, which is the
	// position that made it work at all.
	if strings.Contains(prompt, docsRoot) {
		t.Errorf("the docs note must not be in the -p value:\n%s", prompt)
	}
}

// TestTheDocsNoteRefusesUncitedRegulatoryClaims is the one assertion here that
// is about damage rather than about capability.
//
// firstpass submits under the operator's own GitHub identity. A finding that
// says "this violates Reg-T" when it does not, posted in a real engineer's
// name on a colleague's pull request, is worse than any finding it could have
// been right about -- it costs the author time, it costs the operator
// credibility, and it is the kind of mistake that gets an automated reviewer
// switched off. So the note has to demand a citation and, crucially, tell the
// reviewer that silence is the correct output when it cannot give one.
func TestTheDocsNoteRefusesUncitedRegulatoryClaims(t *testing.T) {
	note := docsNote(docsRoot)

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

// The note has to distinguish material worth reading from material that can
// only be searched. The compliance books are around 700,000 words -- roughly
// five context windows -- so a reviewer that tries to read them will burn its
// budget and its context before it reaches the code.
func TestTheDocsNoteSeparatesReadableFromSearchable(t *testing.T) {
	note := docsNote(docsRoot)

	for _, small := range []string{"COMPLIANCE-GAP-ANALYSIS.md", "AUDIT-TECHNICAL-REQUIREMENTS.md", "SERVICE_REGISTRY.md"} {
		if !strings.Contains(note, small) {
			t.Errorf("the note must name %s among the documents worth opening:\n%s", small, note)
		}
	}
	if !strings.Contains(note, "far too large to read whole -- search them") {
		t.Errorf("the note must say the books are for searching:\n%s", note)
	}
	if !strings.Contains(note, "COMPLIANCE_BOOKS") {
		t.Errorf("the note must name the books:\n%s", note)
	}
	// The team's own retrieval procedure, named rather than left to be
	// discovered: the skill that describes it lives outside the directory
	// Claude Code searches for skills, so nothing loads it automatically.
	if !strings.Contains(note, "checking-astraex-docs-before-implementing/SKILL.md") {
		t.Errorf("the note must point at the team's own procedure:\n%s", note)
	}
}

// The note lists what a specification might govern, in the team's own terms.
// Without that, "check the docs if relevant" leaves the reviewer to guess what
// relevant means, and the answer for a margin service is not obvious from the
// diff alone.
func TestTheDocsNoteNamesWhatMightBeGoverned(t *testing.T) {
	note := docsNote(docsRoot)
	for _, term := range []string{"threshold", "denial rule", "status transition", "liquidation", "KYC", "position-limit"} {
		if !strings.Contains(note, term) {
			t.Errorf("the note must name %q as possibly governed:\n%s", term, note)
		}
	}
}

// Windows paths must arrive as forward slashes. A backslash path in a prompt
// is read by the model as containing escapes, and the reviewer would go
// looking for a directory that does not exist -- silently producing a review
// with no compliance dimension at all, which is the failure mode hardest to
// notice.
func TestTheDocsNoteNormalisesWindowsPaths(t *testing.T) {
	note := docsNote(`C:\work\astraex-docs`)
	if !strings.Contains(note, "C:/work/astraex-docs") {
		t.Errorf("a Windows path must be normalised to forward slashes:\n%s", note)
	}
	if strings.Contains(note, `\work\`) {
		t.Errorf("no backslash paths may reach the prompt:\n%s", note)
	}
}
