package pipeline

// A third pass, and a head that goes backwards.
//
// Nothing else on this branch exercises more than two passes, which is exactly
// where "never reviewed twice for the same commit" was still breakable:
// condition 3 compared the live head against the *last* pass's commit only, so
// a force-push back to any earlier reviewed commit passed it and earned a
// second comment set on lines that already carried one.

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/store"
)

const (
	thirdSHA   = "abcdef0123456789abcdef0123456789abcdef01"
	secondPost = "spaces/A/messages/m2"
	thirdPost  = "spaces/A/messages/m3"
)

// decidedAt is when the seeded pass finished, and every re-post below is
// posted after it -- which is condition 2's real question.
var decidedAt = time.Date(2026, 9, 2, 10, 12, 0, 0, time.UTC)

// postAt is msg() with an explicit post time, because a re-post has to be
// newer than the review it is asking to repeat.
func postAt(name, text string, at time.Time) chat.Message {
	return chat.Message{Name: name, Text: text, CreateTime: at}
}

// twoPassRecord is a pull request reviewed twice: oldSHA then newSHA.
func twoPassRecord() store.Review {
	return store.Review{
		Key:             secondPassKey,
		Outcome:         store.OutcomeReviewed,
		HeadSHA:         newSHA,
		PreviousHeadSHA: oldSHA,
		ReviewedSHAs:    []string{oldSHA, newSHA},
		TriggerMessage:  secondPost,
		StartedAt:       time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC),
		DecidedAt:       decidedAt,
		DurationMS:      735000,
		Verdict:         store.VerdictFindings,
		Pass:            2,
	}
}

// A genuinely new commit after two passes is a third pass. Without this the
// set check could be implemented as "refuse everything after pass 1" and the
// force-push test below would still pass.
func TestAThirdPassReviewsAGenuinelyNewCommit(t *testing.T) {
	h := newHarness(t, []chat.Message{
		postAt(thirdPost, "once more please "+prURL("aex-balances", 12), decidedAt.Add(time.Hour)),
	})
	h.seedWatermark(t)
	seedFirstPass(t, h, twoPassRecord(), thirdSHA)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, secondPassKey)
	if d.Action != ActionReview {
		t.Fatalf("Action = %q (%s), want review: %q has never been reviewed", d.Action, d.Reason, thirdSHA)
	}
	if !strings.Contains(d.Reason, "pass 3") {
		t.Errorf("Reason = %q, want it to name the third pass", d.Reason)
	}

	rec, _, _ := h.st.Review(secondPassKey)
	if rec.Pass != 3 {
		t.Errorf("Pass = %d, want 3", rec.Pass)
	}
	if rec.HeadSHA != thirdSHA {
		t.Errorf("HeadSHA = %q, want %q", rec.HeadSHA, thirdSHA)
	}
	if rec.PreviousHeadSHA != newSHA {
		t.Errorf("PreviousHeadSHA = %q, want the pass before this one (%q)", rec.PreviousHeadSHA, newSHA)
	}
	got := rec.ReviewedCommits()
	if len(got) != 3 || got[0] != oldSHA || got[1] != newSHA || got[2] != thirdSHA {
		t.Errorf("ReviewedCommits() = %v, want every pass's commit in order", got)
	}
	// The reviewer is told about the pass immediately before it, which is the
	// one whose comments are the freshest on the pull request.
	if pp := onlyPreviousPass(t, h); pp == nil || pp.HeadSHA != newSHA {
		t.Errorf("previous pass = %+v, want one naming %q", pp, newSHA)
	}
}

// The bug. A head force-pushed back to a commit an *earlier* pass reviewed is
// unequal to the last pass's commit, so a single-scalar comparison lets it
// through -- onto the very lines pass 1's comments are sitting on.
func TestAHeadForcePushedBackToAnEarlierReviewedCommitIsNotReviewedAgain(t *testing.T) {
	seeded := twoPassRecord()
	for _, tc := range []struct {
		name   string
		record store.Review
	}{
		{"with the recorded set", seeded},
		{
			// A row written before ReviewedSHAs existed. The set has to be
			// derived from HeadSHA and PreviousHeadSHA or this row lets pass
			// 1's commit straight back through.
			name: "derived from a pre-existing row",
			record: func() store.Review {
				r := seeded
				r.ReviewedSHAs = nil
				return r
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, []chat.Message{
				postAt(thirdPost, "reverted, please look again "+prURL("aex-balances", 12),
					decidedAt.Add(time.Hour)),
			})
			h.seedWatermark(t)
			// Force-pushed back to the commit pass 1 reviewed.
			seedFirstPass(t, h, tc.record, oldSHA)

			rep, err := h.p.Sweep(context.Background(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			d, _ := decisionFor(rep, secondPassKey)
			if d.Action != ActionSkip {
				t.Fatalf("Action = %q (%s), want skip: pass 1 already reviewed %q and its "+
					"comments are on those lines", d.Action, d.Reason, oldSHA)
			}
			if !strings.Contains(d.Reason, "no new commits") {
				t.Errorf("Reason = %q, want it to say there is nothing new to review", d.Reason)
			}
			if len(h.rev.ran) != 0 {
				t.Fatalf("ran = %v; a pull request must never be reviewed twice for the same "+
					"commit, whichever pass reviewed it", h.rev.ran)
			}
			if len(h.prs.submitted) != 0 {
				t.Errorf("submitted = %+v; nothing was reviewed", h.prs.submitted)
			}

			// The trigger still moves on, so the same re-post is not
			// re-inspected every sweep; nothing else about the record does.
			want := tc.record
			want.TriggerMessage = thirdPost
			want.TriggerTime = decidedAt.Add(time.Hour)
			if rec, _, _ := h.st.Review(secondPassKey); !reviewsEqual(rec, want) {
				t.Errorf("record =\n %+v\nwant only the trigger changed:\n %+v", rec, want)
			}
		})
	}
}

// reviewsEqual compares two records in full. store.Review carries a slice
// now, so it is no longer comparable with ==, and a helper is better than
// letting each assertion pick its own subset of fields to check.
func reviewsEqual(a, b store.Review) bool { return reflect.DeepEqual(a, b) }

const fourthSHA = "9876543210fedcba9876543210fedcba98765432"

// threePassRecord is a pull request reviewed three times: oldSHA, then
// newSHA, then thirdSHA. The first commit is deliberately *not* the
// last-but-one, which is what twoPassRecord could not express -- there
// PreviousHeadSHA and the earliest reviewed commit are the same string, so a
// test built on it cannot tell "refuses any reviewed commit" from "refuses the
// last two".
func threePassRecord() store.Review {
	r := twoPassRecord()
	r.HeadSHA = thirdSHA
	r.PreviousHeadSHA = newSHA
	r.ReviewedSHAs = []string{oldSHA, newSHA, thirdSHA}
	r.Pass = 3
	return r
}

// The set check, at the call site, with a record deep enough to discriminate.
// A head force-pushed back to the *first* of three reviewed commits is neither
// HeadSHA nor PreviousHeadSHA, so an implementation that checked those two
// scalars would review it again -- blocker 1 exactly, one pass later.
func TestAHeadForcePushedBackPastTheLastTwoCommitsIsNotReviewedAgain(t *testing.T) {
	seeded := threePassRecord()
	for _, tc := range []struct {
		name string
		live string
	}{
		// The case the two-scalar implementation cannot see.
		{"the first of three reviewed commits", oldSHA},
		// The two it can, kept so the test also covers the whole set.
		{"the middle one", newSHA},
		{"the most recent one", thirdSHA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, []chat.Message{
				postAt(thirdPost, "reverted, please look again "+prURL("aex-balances", 12),
					decidedAt.Add(time.Hour)),
			})
			h.seedWatermark(t)
			seedFirstPass(t, h, seeded, tc.live)

			rep, err := h.p.Sweep(context.Background(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			d, _ := decisionFor(rep, secondPassKey)
			if d.Action != ActionSkip {
				t.Fatalf("Action = %q (%s), want skip: %q has already been reviewed and that "+
					"pass's comments are on those lines", d.Action, d.Reason, tc.live)
			}
			if len(h.rev.ran) != 0 {
				t.Fatalf("ran = %v; a pull request must never be reviewed twice for the same "+
					"commit, however many passes ago that was", h.rev.ran)
			}
			if len(h.prs.submitted) != 0 {
				t.Errorf("submitted = %+v; nothing was reviewed", h.prs.submitted)
			}
			want := seeded
			want.TriggerMessage = thirdPost
			want.TriggerTime = decidedAt.Add(time.Hour)
			if rec, _, _ := h.st.Review(secondPassKey); !reviewsEqual(rec, want) {
				t.Errorf("record =\n %+v\nwant only the trigger changed:\n %+v", rec, want)
			}
		})
	}
}

// The positive control for the test above: after three passes a genuinely new
// commit is still reviewed. Without it, "refuse every re-post once a pull
// request has two passes" would satisfy every skip assertion on this branch.
func TestAFourthPassReviewsAGenuinelyNewCommit(t *testing.T) {
	h := newHarness(t, []chat.Message{
		postAt(thirdPost, "once more "+prURL("aex-balances", 12), decidedAt.Add(time.Hour)),
	})
	h.seedWatermark(t)
	seedFirstPass(t, h, threePassRecord(), fourthSHA)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, secondPassKey)
	if d.Action != ActionReview {
		t.Fatalf("Action = %q (%s), want review: %q has never been reviewed", d.Action, d.Reason, fourthSHA)
	}
	rec, _, _ := h.st.Review(secondPassKey)
	if rec.Pass != 4 {
		t.Errorf("Pass = %d, want 4", rec.Pass)
	}
	got := rec.ReviewedCommits()
	if len(got) != 4 || got[0] != oldSHA || got[3] != fourthSHA {
		t.Errorf("ReviewedCommits() = %v, want all four in order", got)
	}
}
