package ghpr

import (
	"context"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/runner"
)

const searchJSON = `{"total_count":3,"items":[
{"number":213,"repository_url":"https://api.github.com/repos/AstraBit-CPT/aex-trade-terminal",
 "updated_at":"2026-09-07T08:30:00Z","draft":false,
 "user":{"login":"varbanvatralov-arch","type":"User"},"pull_request":{}},
{"number":35,"repository_url":"https://api.github.com/repos/AstraBit-CPT/aex-risk-guard-service",
 "updated_at":"2026-09-06T11:00:00Z","draft":false,
 "user":{"login":"dependabot[bot]","type":"Bot"},"pull_request":{}},
{"number":900,"repository_url":"https://api.github.com/repos/AstraBit-CPT/aex-trade-terminal",
 "updated_at":"2026-09-05T10:00:00Z","user":{"login":"someone","type":"User"}}
]}`

func discoverFake(out string) (*Client, *runner.Fake) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "search/issues", Result: runner.Result{Stdout: []byte(out)}},
	}}
	return New(f, "gh"), f
}

// TestDiscoverAsksForWhatItSaysItAsksFor pins the query, because every safety
// property of this source lives in it: the organisation, the review request,
// and the author exclusions that keep it to one page.
func TestDiscoverAsksForWhatItSaysItAsksFor(t *testing.T) {
	c, f := discoverFake(searchJSON)
	_, err := c.Discover(context.Background(), Query{
		Owner:              "AstraBit-CPT",
		ReviewRequestedFor: "angelov-todor",
		ExcludeAuthors:     []string{"app/dependabot", "app/astraex-pipeline-bot"},
	})
	if err != nil {
		t.Fatal(err)
	}
	line := f.Calls[0].String()
	for _, want := range []string{
		"org:AstraBit-CPT",
		"is:pr",
		"state:open",
		"draft:false",
		// The login, never "@me": that is a gh CLI convenience and means
		// nothing to the API this calls, which would silently match nobody.
		"review-requested:angelov-todor",
		"-author:app/dependabot",
		"-author:app/astraex-pipeline-bot",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the query must contain %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "@me") {
		t.Errorf("@me is a gh CLI convenience and matches nothing here:\n%s", line)
	}
}

// The query without a review request is the every-open-pull-request query --
// over five hundred on this operator's organisation, each one a clone, a
// claude run and a comment nobody asked for. It must be unreachable, not
// merely undocumented.
func TestDiscoverRefusesToSearchWithoutAReviewRequest(t *testing.T) {
	c, f := discoverFake(searchJSON)
	if _, err := c.Discover(context.Background(), Query{Owner: "AstraBit-CPT"}); err == nil {
		t.Fatal("a query with no login to require a review request from must be refused")
	}
	if len(f.Calls) != 0 {
		t.Errorf("nothing should have been asked of GitHub, got %v", f.Calls)
	}
	if _, err := c.Discover(context.Background(), Query{ReviewRequestedFor: "x"}); err == nil {
		t.Error("a query with no owner must be refused")
	}
}

func TestDiscoverReadsTheResults(t *testing.T) {
	c, _ := discoverFake(searchJSON)
	page, err := c.Discover(context.Background(), Query{
		Owner: "AstraBit-CPT", ReviewRequestedFor: "angelov-todor",
	})
	if err != nil {
		t.Fatal(err)
	}
	found := page.Found
	// Two of the three items: search/issues returns issues and pull requests
	// together, and the third carries no pull_request key.
	if len(found) != 2 {
		t.Fatalf("want the two pull requests, got %d: %+v", len(found), found)
	}
	got := found[0]
	// Folded, because that is what Key and the store assume. GitHub answers
	// "AstraBit-CPT" where a chat link says "astrabit-cpt", and an unfolded
	// ref would be a second key for a pull request firstpass already has one
	// for -- which reads through as reviewing it twice.
	if got.Ref.Owner != "astrabit-cpt" || got.Ref.Repo != "aex-trade-terminal" || got.Ref.Number != 213 {
		t.Errorf("the repository is only in repository_url, and it decoded wrong: %+v", got.Ref)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is the whole second-pass prompt for this source; it must decode")
	}
	if got.IsBot {
		t.Error("a User must not be reported as a bot")
	}
	if !found[1].IsBot {
		t.Error("a Bot must be reported as one, or exclude_bots can never work")
	}
}

func TestDiscoverKeepsOnlyTheRepositoriesAskedFor(t *testing.T) {
	c, _ := discoverFake(searchJSON)
	page, err := c.Discover(context.Background(), Query{
		Owner: "AstraBit-CPT", ReviewRequestedFor: "angelov-todor",
		RepoPrefixes: []string{"aex-trade"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Found) != 1 || page.Found[0].Ref.Repo != "aex-trade-terminal" {
		t.Fatalf("the prefix filter kept the wrong set: %+v", page.Found)
	}
	// Scanned counts what GitHub returned, not what survived the filter. It is
	// what makes the truncation warning mean anything under a narrow prefix.
	if page.Scanned != 3 {
		t.Errorf("Scanned = %d, want every item GitHub returned", page.Scanned)
	}

	// No prefixes means every repository under the owner, which is the
	// documented default and not an empty result.
	all, err := c.Discover(context.Background(), Query{
		Owner: "AstraBit-CPT", ReviewRequestedFor: "angelov-todor",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Found) != 2 {
		t.Errorf("no prefixes must mean no filtering, got %d", len(all.Found))
	}
}

// A failed search must be an error rather than an empty result. Silently
// returning nothing would make a broken query look exactly like a quiet day.
func TestDiscoverReportsFailure(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "search/issues", Result: runner.Result{
			ExitCode: 1, Stderr: []byte("HTTP 403: rate limit exceeded"),
		}},
	}}
	if _, err := New(f, "gh").Discover(context.Background(), Query{
		Owner: "o", ReviewRequestedFor: "l",
	}); err == nil {
		t.Fatal("a non-zero exit must be an error")
	} else if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("the error must carry what GitHub said: %v", err)
	}
}

// TestTruncationIsCountedBeforeFiltering is the bug this had at first.
//
// The warning exists to tell the operator that bots are crowding real pull
// requests off the page, and the configuration where that bites hardest is a
// narrow repo_prefixes over a queue full of dependabot -- where the filtered
// result is tiny precisely because the page was full of things it dropped.
// Derived from the filtered count, the warning was silent exactly there.
func TestTruncationIsCountedBeforeFiltering(t *testing.T) {
	c, _ := discoverFake(searchJSON)
	page, err := c.Discover(context.Background(), Query{
		Owner: "AstraBit-CPT", ReviewRequestedFor: "angelov-todor",
		// A prefix nothing matches, over a full page.
		RepoPrefixes: []string{"nothing-matches-"},
		Limit:        3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Found) != 0 {
		t.Fatalf("the prefix matches nothing, so nothing should survive: %+v", page.Found)
	}
	if !page.Truncated {
		t.Error("three items against a limit of three is a full page, whatever the filter kept")
	}
	if page.Scanned != 3 {
		t.Errorf("Scanned = %d, want 3", page.Scanned)
	}
}

// GitHub's incomplete_results is not truncation, and the distinction is the
// point: the two call for opposite responses.
//
// A full page is the operator's to fix -- exclude the noisiest authors. A
// partial search is GitHub giving up part way, and no configuration change
// clears it: measured against a real organisation, three identical calls
// returned 6, 8 and 9 of a reported 9, with the flag set every time. Folding
// it into Truncated made doctor fail permanently while advising a fix that
// does nothing.
func TestPartialResultsAreNotTruncation(t *testing.T) {
	c, _ := discoverFake(`{"total_count":9,"incomplete_results":true,"items":[
{"number":1,"repository_url":"https://api.github.com/repos/o/r","updated_at":"2026-09-07T08:30:00Z",
 "user":{"login":"u","type":"User"},"pull_request":{}}]}`)
	page, err := c.Discover(context.Background(), Query{Owner: "o", ReviewRequestedFor: "l"})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Partial {
		t.Error("incomplete_results must be reported")
	}
	if page.Truncated {
		t.Error("one item against a limit of a hundred is not a full page, whatever " +
			"GitHub says about completeness")
	}
	// And the pull requests it did return are still used: a partial answer is
	// most of an answer, and the next sweep asks again.
	if len(page.Found) != 1 {
		t.Errorf("a partial search still yields what it found, got %d", len(page.Found))
	}
}
