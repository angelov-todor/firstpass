package pipeline

// Condition 2 has to establish "this post came after we reviewed it", not
// merely "this message name differs from the recorded one".
//
// Two routine things re-offer messages that are older than the review:
// a watermark gap -- chat.Client returns the whole window when the watermark
// has fallen out of it, and Sweep deliberately processes all of it -- and
// `scan -backfill N`, which ignores the watermark by design. candidates()
// takes the *oldest* message carrying a ref while the record holds the newest
// one seen, so the two names differ, and if the head has moved for any reason
// -- an ordinary push, nobody re-posted anything -- a name-only check
// manufactures a second pass: a full comment set and a submitted verdict on a
// colleague's pull request that nobody asked for. And invisibly, because the
// older message's reaction latches are already set, so no 👀 and no result
// reaction appear either.

import (
	"context"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/store"
)

// The two posts that carried this pull request, both older than the review
// they triggered. m1 is the oldest, which is the one candidates() picks.
var (
	firstPostAt  = time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
	secondPostAt = time.Date(2026, 9, 2, 9, 30, 0, 0, time.UTC)
)

// reviewedAfterTwoPosts is a pull request reviewed once, triggered by the
// newer of the two posts that carried it, and decided after both.
func reviewedAfterTwoPosts() store.Review {
	r := firstPassRecord(store.OutcomeReviewed, oldSHA, secondPost)
	r.StartedAt = secondPostAt.Add(time.Minute)
	r.DecidedAt = decidedAt
	return r
}

// windowWithBothPosts is the fetch window a gap or a backfill produces:
// newest-first, and carrying both posts.
func windowWithBothPosts() []chat.Message {
	url := prURL("aex-balances", 12)
	return []chat.Message{
		postAt(secondPost, "bumping "+url, secondPostAt),
		postAt(firstPost, "please review "+url, firstPostAt),
	}
}

const firstPost = firstTrigger

// A watermark gap, driven through the real path: the watermark names a message
// that is no longer in the window, so chat reports it missing and Sweep scans
// everything. The head has moved since the review -- an ordinary push -- and
// nobody re-posted anything.
func TestAWatermarkGapDoesNotManufactureASecondPass(t *testing.T) {
	h := newHarness(t, windowWithBothPosts())
	// A watermark whose message has scrolled out of the window entirely.
	if err := h.st.SetWatermark(store.Watermark{
		MessageName: "spaces/A/messages/gone",
		CreateTime:  time.Date(2026, 9, 1, 6, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	seeded := reviewedAfterTwoPosts()
	seedFirstPass(t, h, seeded, newSHA)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.WatermarkGap {
		t.Fatal("this test is meant to exercise the gap path and did not reach it")
	}
	d, _ := decisionFor(rep, secondPassKey)
	if d.Action != ActionSkip {
		t.Fatalf("Action = %q (%s), want skip: no one re-posted this pull request; the sweep is "+
			"re-reading posts that are older than the review itself", d.Action, d.Reason)
	}
	if len(h.rev.ran) != 0 {
		t.Fatalf("ran = %v; a re-scan is not a request for another review", h.rev.ran)
	}
	if len(h.prs.submitted) != 0 {
		t.Errorf("submitted = %+v; nothing was reviewed, so no verdict may be submitted",
			h.prs.submitted)
	}
	if got, _, _ := h.st.Review(secondPassKey); !reviewsEqual(got, seeded) {
		t.Errorf("record =\n %+v\nwant untouched:\n %+v", got, seeded)
	}
}

// `scan -backfill N` ignores the watermark by design, so it offers the same
// older posts on purpose. It must not review anything either.
func TestABackfillDoesNotManufactureASecondPass(t *testing.T) {
	h := newHarness(t, windowWithBothPosts())
	h.seedWatermark(t)
	seeded := reviewedAfterTwoPosts()
	seedFirstPass(t, h, seeded, newSHA)

	rep, err := h.p.Sweep(context.Background(), Options{Backfill: 50})
	if err != nil {
		t.Fatal(err)
	}
	if h.ch.lastSince != "" || h.ch.lastLimit != 50 {
		t.Fatalf("this test is meant to exercise the backfill path: since=%q limit=%d",
			h.ch.lastSince, h.ch.lastLimit)
	}
	d, _ := decisionFor(rep, secondPassKey)
	if d.Action != ActionSkip {
		t.Fatalf("Action = %q (%s), want skip: a backfill re-reads old posts by design",
			d.Action, d.Reason)
	}
	if len(h.rev.ran) != 0 {
		t.Fatalf("ran = %v", h.rev.ran)
	}
	if got, _, _ := h.st.Review(secondPassKey); !reviewsEqual(got, seeded) {
		t.Errorf("record =\n %+v\nwant untouched:\n %+v", got, seeded)
	}
}

// The unit of the fix, stated on its own: a post that predates the review is
// not a re-post, whatever its name says and however far the head has moved.
func TestAPostOlderThanTheReviewIsNotARepost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		postAt time.Time
		want   Action
	}{
		{"posted before the review finished", decidedAt.Add(-time.Minute), ActionSkip},
		// Exactly equal is not after. A post landing as the review finished
		// cannot have been prompted by its result, and the fail-safe reading
		// costs nothing: the next re-post is newer and does trigger.
		{"posted at the very moment it finished", decidedAt, ActionSkip},
		{"posted after the review finished", decidedAt.Add(time.Minute), ActionReview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, []chat.Message{
				postAt(thirdPost, "another look "+prURL("aex-balances", 12), tc.postAt),
			})
			h.seedWatermark(t)
			seeded := reviewedAfterTwoPosts()
			seedFirstPass(t, h, seeded, newSHA)

			rep, err := h.p.Sweep(context.Background(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			d, _ := decisionFor(rep, secondPassKey)
			if d.Action != tc.want {
				t.Fatalf("Action = %q (%s), want %q", d.Action, d.Reason, tc.want)
			}
			if tc.want == ActionSkip {
				if len(h.rev.ran) != 0 {
					t.Errorf("ran = %v", h.rev.ran)
				}
				if got, _, _ := h.st.Review(secondPassKey); !reviewsEqual(got, seeded) {
					t.Errorf("record =\n %+v\nwant untouched:\n %+v", got, seeded)
				}
				return
			}
			if len(h.rev.ran) != 1 {
				t.Errorf("ran = %v, want one review", h.rev.ran)
			}
			if rec, _, _ := h.st.Review(secondPassKey); rec.Pass != 2 || rec.TriggerMessage != thirdPost {
				t.Errorf("record = %+v, want pass 2 triggered by the new post", rec)
			}
		})
	}
}

// A record with no decision time cannot establish the ordering condition 2
// needs, so it is refused -- the same reasoning as a reviewed record with no
// head SHA. Belt and braces for rows written by anything that forgets to set
// it, and the safe direction if that ever happens.
func TestAReviewedRecordWithNoDecisionTimeIsNeverSecondPassed(t *testing.T) {
	h := newHarness(t, []chat.Message{
		postAt(thirdPost, "another look "+prURL("aex-balances", 12), decidedAt.Add(time.Hour)),
	})
	h.seedWatermark(t)
	seeded := reviewedAfterTwoPosts()
	seeded.DecidedAt = time.Time{}
	seedFirstPass(t, h, seeded, newSHA)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, secondPassKey)
	if d.Action != ActionSkip {
		t.Fatalf("Action = %q (%s), want skip: nothing says when this was reviewed, so nothing "+
			"can say the post came after it", d.Action, d.Reason)
	}
	if len(h.prs.inspected) != 0 {
		t.Errorf("Inspect called for %v; this is decidable at the record gate", h.prs.inspected)
	}
}

// ---- the two clocks ----

// Condition 2 asks "did this post come after the one we reviewed for". Asking
// it of a post time and a *decision* time crosses clock domains: Google
// timestamps the message, this machine timestamps the decision. A laptop clock
// an hour behind Google's is enough to make a post that genuinely predates a
// review compare as later than it -- and a watermark gap holds the watermark,
// so the whole window is re-offered every sweep until the gap clears.
//
// Both cases run under the same skew, through the same gap, and differ only in
// when the re-offered post was made. That pairing is the point: a fix that
// refused everything under skew would fail the second case.
func TestAGapUnderASkewedLocalClockDoesNotManufactureASecondPass(t *testing.T) {
	const (
		reviewedPost = "spaces/A/messages/reviewed-for"
		olderPost    = "spaces/A/messages/older"
		newerPost    = "spaces/A/messages/newer"
	)
	// Google's clock. The post firstpass reviewed for, one older than it that
	// was never processed, and one genuinely newer.
	var (
		reviewedPostAt = time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
		olderPostAt    = time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
		newerPostAt    = time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
		// This machine's clock, two hours behind Google's -- so the review is
		// stamped *earlier* than the post that triggered it.
		localNow = time.Date(2026, 9, 2, 7, 0, 0, 0, time.UTC)
	)

	for _, tc := range []struct {
		name   string
		post   string
		postAt time.Time
		want   Action
	}{
		{"a post older than the review", olderPost, olderPostAt, ActionSkip},
		{"a post newer than the review", newerPost, newerPostAt, ActionReview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := prURL("aex-balances", 12)
			h := newHarness(t, []chat.Message{
				postAt(reviewedPost, "please review "+url, reviewedPostAt),
				postAt("spaces/A/messages/wm", "the watermarked message", localNow),
			})
			h.p.Now = func() time.Time { return localNow }
			if err := h.st.SetWatermark(store.Watermark{
				MessageName: "spaces/A/messages/wm", CreateTime: localNow,
			}); err != nil {
				t.Fatal(err)
			}
			h.prs.info[secondPassKey] = ghprOpen(oldSHA)

			// The first pass, recorded by the pipeline itself so the skew is
			// baked in exactly as production would bake it.
			if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
				t.Fatal(err)
			}
			rec, ok, _ := h.st.Review(secondPassKey)
			if !ok || rec.Outcome != store.OutcomeReviewed {
				t.Fatalf("the first pass did not happen: %+v", rec)
			}
			if !rec.DecidedAt.Before(rec.TriggerTime) {
				t.Fatalf("this test needs the local clock behind Google's: decided %s, posted %s",
					rec.DecidedAt, rec.TriggerTime)
			}

			// The head moves -- an ordinary push, nobody re-posted anything --
			// and the watermarked message is gone from the window, so the
			// sweep re-reads everything it can see.
			h.prs.info[secondPassKey] = ghprOpen(newSHA)
			h.ch.msgs = []chat.Message{postAt(tc.post, "look at "+url, tc.postAt)}

			rep, err := h.p.Sweep(context.Background(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if !rep.WatermarkGap {
				t.Fatal("this test is meant to exercise the gap path and did not reach it")
			}
			d, _ := decisionFor(rep, secondPassKey)
			if d.Action != tc.want {
				t.Fatalf("Action = %q (%s), want %q", d.Action, d.Reason, tc.want)
			}
			if tc.want == ActionSkip {
				if len(h.rev.ran) != 1 {
					t.Errorf("ran = %v, want only the first pass: a re-scan of a post older "+
						"than the review is not a request for another one, whatever the "+
						"machine's clock says", h.rev.ran)
				}
				if len(h.prs.submitted) != 0 {
					t.Errorf("submitted = %+v; a dry run submits nothing", h.prs.submitted)
				}
				return
			}
			if len(h.rev.ran) != 2 {
				t.Errorf("ran = %v, want two passes: this post really is newer than the review, "+
					"and skew must not suppress a genuine re-post either", h.rev.ran)
			}
		})
	}
}

// A row written before the post time was recorded has nothing to compare, so
// it falls back to the decision-time check and behaves exactly as it did
// before -- neither better nor worse, which is the conservative direction for
// rows already on disk.
func TestAReviewedRowWithNoRecordedPostTimeFallsBackToTheDecisionTime(t *testing.T) {
	for _, tc := range []struct {
		name   string
		postAt time.Time
		want   Action
	}{
		{"a post after the recorded decision", decidedAt.Add(time.Hour), ActionReview},
		{"a post before it", decidedAt.Add(-time.Hour), ActionSkip},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, []chat.Message{
				postAt(thirdPost, "another look "+prURL("aex-balances", 12), tc.postAt),
			})
			h.seedWatermark(t)
			seeded := reviewedAfterTwoPosts()
			seeded.TriggerTime = time.Time{} // as a pre-existing row decodes
			seedFirstPass(t, h, seeded, newSHA)

			rep, err := h.p.Sweep(context.Background(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if d, _ := decisionFor(rep, secondPassKey); d.Action != tc.want {
				t.Fatalf("Action = %q (%s), want %q", d.Action, d.Reason, tc.want)
			}
		})
	}
}
