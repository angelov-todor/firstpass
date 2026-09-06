package ghpr

import (
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/runner"
)

var fbRef = prref.PRRef{Owner: "example-org", Repo: "aex-balances", Number: 12}

// feedbackFixture is shaped from real GitHub responses and sanitised for
// content.
//
// The shape was taken from two live pull requests and the parser was run
// against both before this fixture was written: one carried two reviews and
// six general comments and no inline threads at all, the other three inline
// threads. Every characteristic below is one of theirs -- a CI bot comment, a
// review whose body is empty, a thread with a null line, an unresolved thread
// that is outdated, and a resolved thread.
//
// It is sanitised because this repository is public and the real payloads are
// colleagues' review discussion. Inventing the shape would have been the
// mistake that shipped the list-spaces defect; copying the content would have
// been a worse one.
const feedbackFixture = `{"data":{"repository":{"pullRequest":{
 "reviewDecision":"REVIEW_REQUIRED",
 "reviewThreads":{"totalCount":3,"nodes":[
   {"isResolved":false,"isOutdated":true,"path":"src/A.cs","line":null,
    "comments":{"nodes":[{"author":{"login":"reviewer-one","__typename":"User"},
      "body":"Null check missing here.\nSecond line ignored.","url":"https://example.invalid/t1"}]}},
   {"isResolved":false,"isOutdated":false,"path":"src/B.cs","line":42,
    "comments":{"nodes":[{"author":{"login":"reviewer-two","__typename":"User"},
      "body":"This allocates on every call.","url":"https://example.invalid/t2"}]}},
   {"isResolved":true,"isOutdated":false,"path":"src/C.cs","line":7,
    "comments":{"nodes":[{"author":{"login":"reviewer-one","__typename":"User"},
      "body":"Naming.","url":"https://example.invalid/t3"}]}}]},
 "reviews":{"totalCount":2,"nodes":[
   {"author":{"login":"reviewer-two","__typename":"User"},"state":"COMMENTED",
    "body":"## Overall\nLooks close.","url":"https://example.invalid/r1"},
   {"author":{"login":"reviewer-three","__typename":"User"},"state":"APPROVED",
    "body":"","url":"https://example.invalid/r2"}]},
 "comments":{"totalCount":2,"nodes":[
   {"author":{"login":"github-actions","__typename":"Bot"},
    "body":"# Summary\nBuild passed.","url":"https://example.invalid/c1"},
   {"author":{"login":"reviewer-one","__typename":"User"},
    "body":"Please also update the changelog.","url":"https://example.invalid/c2"}]}}}}}`

func parseFixture(t *testing.T, body string) Feedback {
	t.Helper()
	f, err := parseFeedback([]byte(body), fbRef)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFeedbackReadsAllThreeSurfaces(t *testing.T) {
	f := parseFixture(t, feedbackFixture)

	var threads, reviews, comments int
	for _, it := range f.Items {
		switch it.Surface {
		case SurfaceThread:
			threads++
		case SurfaceReview:
			reviews++
		case SurfaceComment:
			comments++
		}
	}
	// Three surfaces, because the team uses all three: reading one would miss
	// most of the feedback on a given pull request.
	if threads != 3 || comments != 2 {
		t.Errorf("threads=%d comments=%d, want 3 and 2", threads, comments)
	}
	// One of the two reviews has an empty body and carries no feedback of its
	// own; its state is already in ReviewDecision. Listing it would pad the
	// index with a line the reviewer can do nothing about.
	if reviews != 1 {
		t.Errorf("reviews=%d, want 1: the empty-bodied review carries nothing to address", reviews)
	}
	if f.ReviewDecision != "REVIEW_REQUIRED" {
		t.Errorf("ReviewDecision = %q", f.ReviewDecision)
	}
	if f.Truncated {
		t.Error("nothing was truncated")
	}
}

// A CI summary is not review feedback. Unmarked, a build report reads as a
// point that must be addressed before an approval -- and one of the six
// comments on the pull request this was designed against is exactly that.
func TestFeedbackMarksBots(t *testing.T) {
	f := parseFixture(t, feedbackFixture)
	bots := 0
	for _, it := range f.Items {
		if it.IsBot {
			bots++
			if it.Author != "github-actions" {
				t.Errorf("unexpected bot %q", it.Author)
			}
		}
	}
	if bots != 1 {
		t.Errorf("bots=%d, want 1", bots)
	}
}

// Resolved and outdated are carried through and not interpreted. "outdated"
// especially: all three threads on the real pull request this came from are
// unresolved and outdated, which may mean fixed or may mean the lines merely
// moved. Only the code can tell, so the flag is shown to the reviewer rather
// than resolved by firstpass.
func TestFeedbackCarriesThreadStateWithoutInterpretingIt(t *testing.T) {
	f := parseFixture(t, feedbackFixture)
	var resolved, outdated, nullLine int
	for _, it := range f.Items {
		if it.Surface != SurfaceThread {
			continue
		}
		if it.Resolved {
			resolved++
		}
		if it.Outdated {
			outdated++
		}
		if it.Line == 0 {
			nullLine++
		}
	}
	if resolved != 1 || outdated != 1 {
		t.Errorf("resolved=%d outdated=%d, want 1 and 1", resolved, outdated)
	}
	// A null line is what GitHub returns for a thread whose anchor is gone;
	// decoding it as a plain int would fail the whole fetch and, through the
	// gate, silently cost every approval on that pull request.
	if nullLine != 1 {
		t.Errorf("nullLine=%d, want 1: a thread with a null line must not break the fetch", nullLine)
	}
}

func TestFeedbackExcerptIsTheFirstLineOnly(t *testing.T) {
	f := parseFixture(t, feedbackFixture)
	for _, it := range f.Items {
		if strings.Contains(it.Excerpt, "\n") {
			t.Errorf("excerpt spans lines: %q", it.Excerpt)
		}
		if strings.Contains(it.Excerpt, "Second line ignored") {
			t.Errorf("excerpt took more than the first line: %q", it.Excerpt)
		}
	}
}

// A count higher than the nodes returned means GitHub had more to give. The
// index must say so: "there is more you have not been shown" cannot support
// "everything raised here has been addressed", and Usable is what the approval
// gate reads.
func TestFeedbackReportsTruncation(t *testing.T) {
	body := strings.Replace(feedbackFixture, `"comments":{"totalCount":2`, `"comments":{"totalCount":99`, 1)
	f := parseFixture(t, body)
	if !f.Truncated {
		t.Error("a totalCount above the nodes returned must set Truncated")
	}
	if f.Usable() {
		t.Error("a truncated list must not be usable for an approval")
	}
}

func TestFeedbackChangesRequested(t *testing.T) {
	f := parseFixture(t, strings.Replace(feedbackFixture,
		`"reviewDecision":"REVIEW_REQUIRED"`, `"reviewDecision":"CHANGES_REQUESTED"`, 1))
	if !f.ChangesRequested() {
		t.Error("CHANGES_REQUESTED must be reported")
	}
	if !parseFixture(t, feedbackFixture).Usable() {
		t.Error("an untruncated list is usable")
	}
	if parseFixture(t, feedbackFixture).ChangesRequested() {
		t.Error("REVIEW_REQUIRED is not a request for changes")
	}
}

func TestFetchFeedbackReportsAFailedCall(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "graphql", Result: runner.Result{ExitCode: 1, Stderr: []byte("gh: not found")}},
	}}
	if _, err := New(f, "gh").FetchFeedback(t.Context(), fbRef); err == nil {
		t.Error("a non-zero gh exit must be an error: the caller withholds approval on it")
	}
}
