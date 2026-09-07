package review

import (
	"strings"
	"testing"
)

// The note has to say which pull request is under review and that the others
// are not to be commented on.
//
// A reviewer that starts reviewing a sibling posts findings on a pull request
// that is either getting its own review -- so they would arrive twice -- or
// was deliberately excluded from one. Both are worse than saying nothing about
// it, and the note is the only thing that draws the line.
func TestTheSiblingNoteForbidsReviewingTheSiblings(t *testing.T) {
	note := siblingNote("example-org/a#1", []Sibling{
		{Key: "example-org/b#2", URL: "https://example.invalid/b/2", Diff: "+ x", Reviewing: true},
	})
	for _, want := range []string{
		"You are reviewing example-org/a#1",
		"CONTEXT",
		"Do NOT review the others",
		"AS A FINDING ON example-org/a#1",
		"example-org/b#2",
		"https://example.invalid/b/2",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("the note must say %q:\n%s", want, note)
		}
	}
}

// Whether a sibling is being reviewed too changes what the reviewer should do
// about a problem it spots there: one that gets its own review will hear about
// it separately, while one that does not gets no other look at all. Saying the
// wrong one leaves a real problem unreported or reported twice.
func TestTheNoteSaysWhetherASiblingIsBeingReviewed(t *testing.T) {
	reviewed := siblingNote("a#1", []Sibling{{Key: "b#2", Diff: "x", Reviewing: true}})
	if !strings.Contains(reviewed, "reviewing this one separately") {
		t.Errorf("a sibling under review must be marked as such:\n%s", reviewed)
	}

	not := siblingNote("a#1", []Sibling{{Key: "b#2", Diff: "x", Reviewing: false}})
	if !strings.Contains(not, "NOT reviewing this one") {
		t.Errorf("a sibling nobody will review must be marked as such:\n%s", not)
	}
	if !strings.Contains(not, "Nothing else will look at it") {
		t.Errorf("the note must say plainly that nothing else will look at it:\n%s", not)
	}
}

// A cut diff must say so. A reviewer that believes it saw the whole change
// will read the absence of a rename as evidence the rename was not needed --
// which is exactly the cross-repo finding this feature exists to produce, got
// backwards.
func TestATruncatedSiblingDiffSaysSo(t *testing.T) {
	note := siblingNote("a#1", []Sibling{{Key: "b#2", Diff: "x", Truncated: true}})
	if !strings.Contains(note, "diff truncated") {
		t.Errorf("a cut diff must be declared:\n%s", note)
	}
	if !strings.Contains(note, "absence of a change here as evidence there was none") {
		t.Errorf("the note must warn against reading absence as evidence:\n%s", note)
	}
	if strings.Contains(siblingNote("a#1", []Sibling{{Key: "b#2", Diff: "x"}}), "truncated") {
		t.Error("a complete diff must not claim to be truncated")
	}
}

// No siblings, no note: a lone pull request must ask exactly what it asked
// before this feature existed.
func TestNoSiblingsMeansNoNote(t *testing.T) {
	if got := siblingNote("a#1", nil); got != "" {
		t.Errorf("siblingNote with no siblings = %q, want empty", got)
	}
	if got := siblingNote("a#1", []Sibling{}); got != "" {
		t.Errorf("siblingNote with an empty slice = %q, want empty", got)
	}
}
