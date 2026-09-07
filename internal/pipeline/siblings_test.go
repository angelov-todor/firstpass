package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/review"
)

// The post that prompted this: two links in one message, an API change and its
// frontend. Reviewed apart, neither review can see whether the two halves
// agree with each other.
func siblingHarness(t *testing.T, text string) *harness {
	t.Helper()
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", text)})
	h.seedWatermark(t)
	h.cfg.MaxReviewsPerSweep = 5
	h.apply()
	return h
}

func siblingKeys(sibs []review.Sibling) []string {
	var out []string
	for _, s := range sibs {
		out = append(out, s.Key)
	}
	return out
}

// TestPRsPostedTogetherSeeEachOther is the feature.
//
// A message carrying several links is the team saying those changes belong
// together. Each review still judges its own pull request and posts its own
// comment, but it is handed the others' diffs so it can notice that a field
// renamed on one side was not renamed on the other.
func TestPRsPostedTogetherSeeEachOther(t *testing.T) {
	h := siblingHarness(t, prURL("aex-backoffice", 319)+" "+prURL("aex-backoffice-web", 314))
	h.prs.diffs["example-org/aex-backoffice#319"] = "- string CaseRef\n+ string CaseReference"
	h.prs.diffs["example-org/aex-backoffice-web#314"] = "+ body.caseRef = ref"
	h.apply()

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}

	h.rev.mu.Lock()
	defer h.rev.mu.Unlock()
	if len(h.rev.sibs) != 2 {
		t.Fatalf("both pull requests must be reviewed, got %d reviews", len(h.rev.sibs))
	}
	// Each review sees the other, and only the other.
	for i, ran := range h.rev.ran {
		got := siblingKeys(h.rev.sibs[i])
		if len(got) != 1 {
			t.Fatalf("%s was given %d siblings, want 1: %v", ran, len(got), got)
		}
		if got[0] == ran {
			t.Errorf("%s was given itself as a sibling", ran)
		}
	}
	// And the diff travels, not just the reference: the whole point is that the
	// reviewer can see what the other half did without being told to go and
	// fetch it, which is the shape of instruction this project has repeatedly
	// measured being ignored.
	var sawDiff bool
	for _, sibs := range h.rev.sibs {
		for _, s := range sibs {
			if strings.Contains(s.Diff, "CaseReference") || strings.Contains(s.Diff, "caseRef") {
				sawDiff = true
			}
		}
	}
	if !sawDiff {
		t.Errorf("the sibling's diff must reach the reviewer: %+v", h.rev.sibs)
	}
}

// A lone post costs nothing: no sibling fetch, no context, and the review is
// exactly the review firstpass did before this existed.
func TestALonePostFetchesNoSiblings(t *testing.T) {
	h := siblingHarness(t, prURL("aex-balances", 12))

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	h.prs.mu.Lock()
	fetched := len(h.prs.diffFor)
	h.prs.mu.Unlock()
	if fetched != 0 {
		t.Errorf("a lone pull request must trigger no diff fetches, got %d: %v", fetched, h.prs.diffFor)
	}
	h.rev.mu.Lock()
	defer h.rev.mu.Unlock()
	if len(h.rev.sibs) != 1 || len(h.rev.sibs[0]) != 0 {
		t.Errorf("a lone pull request must be reviewed with no siblings: %+v", h.rev.sibs)
	}
}

// TestASiblingWhoseDiffFailsDoesNotCostTheReview is the direction that matters
// when GitHub misbehaves.
//
// Sibling context improves a review; it is not a precondition for one. A
// deleted branch, a rate limit or a repository the token cannot read must cost
// the review some context and not the review itself. That is deliberately the
// opposite of the feedback fetch, which withholds an approval when it fails --
// the difference being that an approval makes a claim about the feedback,
// while a review makes no claim about its siblings.
func TestASiblingWhoseDiffFailsDoesNotCostTheReview(t *testing.T) {
	h := siblingHarness(t, prURL("aex-backoffice", 319)+" "+prURL("aex-backoffice-web", 314))
	h.prs.diffErr["example-org/aex-backoffice-web#314"] = errors.New("gh: no commits between branches")
	h.apply()

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 2 {
		t.Fatalf("Reviewed = %d, want 2: a failed sibling fetch must not skip a review "+
			"(decisions: %+v)", rep.Reviewed, rep.Decisions)
	}

	h.rev.mu.Lock()
	defer h.rev.mu.Unlock()
	// The review of 319 gets no sibling context, because 314's diff could not
	// be read; the review of 314 still gets 319.
	for i, ran := range h.rev.ran {
		if ran == "example-org/aex-backoffice#319" && len(h.rev.sibs[i]) != 0 {
			t.Errorf("319 should have no sibling context after the fetch failed: %+v", h.rev.sibs[i])
		}
		if ran == "example-org/aex-backoffice-web#314" && len(h.rev.sibs[i]) != 1 {
			t.Errorf("314 should still see 319: %+v", h.rev.sibs[i])
		}
	}
}

// TestSiblingsAreCappedSoOneReviewCannotBeBuried bounds the context.
//
// Each sibling is a diff of up to 40 KB. A post listing eight pull requests
// would otherwise hand the reviewer a third of a context window of other
// people's changes before it reads the one it is meant to be judging.
func TestSiblingsAreCappedSoOneReviewCannotBeBuried(t *testing.T) {
	text := prURL("a", 1) + " " + prURL("b", 2) + " " + prURL("c", 3) + " " +
		prURL("d", 4) + " " + prURL("e", 5)
	h := siblingHarness(t, text)

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	h.rev.mu.Lock()
	defer h.rev.mu.Unlock()
	for i, sibs := range h.rev.sibs {
		if len(sibs) > maxSiblings {
			t.Errorf("%s was given %d siblings, cap is %d", h.rev.ran[i], len(sibs), maxSiblings)
		}
	}
}
