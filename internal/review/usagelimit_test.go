package review

import (
	"errors"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/runner"
)

// The phrases are the ones the shipped claude binary actually contains, found
// by grepping it rather than invented.
func TestUsageLimitPhraseRecognisesWhatTheCLISays(t *testing.T) {
	for _, out := range []string{
		"You've hit your limit",
		"You've hit your fast limit",
		"You've hit your monthly limit · resets at 3pm",
		"You've hit your monthly spend limit.",
		"Claude usage limit reached",
		"you've hit your limit", // lower-cased by some renderers
	} {
		if UsageLimitPhrase([]byte(out), nil) == "" {
			t.Errorf("must be recognised as a usage limit: %q", out)
		}
		// Either stream: the CLI writes it to stderr when it refuses outright
		// and into stdout when it stops part-way through a session.
		if UsageLimitPhrase(nil, []byte(out)) == "" {
			t.Errorf("must be recognised on stderr too: %q", out)
		}
	}
}

// "rate limit" alone is deliberately not a usage limit. The CLI uses that
// phrase about GitHub throttling and about bash tool concurrency, and
// deferring on either would park a pull request that no amount of waiting
// fixes.
func TestUsageLimitPhraseIgnoresUnrelatedLimits(t *testing.T) {
	for _, out := range []string{
		"This usually means GitHub is rate limiting your requests.",
		"Rate limiting or resource exhaustion issues",
		"Same rate limiting as bash",
		"account device limit reached",
		"panic: runtime error",
		"",
	} {
		if p := UsageLimitPhrase([]byte(out), nil); p != "" {
			t.Errorf("%q must not be read as an account usage limit (matched %q)", out, p)
		}
	}
}

// TestASuccessfulReviewIsNeverAUsageLimit guards the false positive that would
// matter most: a review whose diff or findings happen to quote the phrase.
//
// A pull request that adds an error message, or a test fixture containing one,
// would otherwise be deferred forever -- reviewed successfully every five
// minutes and parked every time.
func TestASuccessfulReviewIsNeverAUsageLimit(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "Review pull request", Result: runner.Result{
			Stdout: []byte("the new error string reads \"You've hit your limit\"\n" +
				VerdictMarker + " findings\n"),
		}},
	}}
	res, err := New(f, "claude", nil, true, t.TempDir()).Run(t.Context(), "work", ref, nil, nil, nil)
	if err != nil {
		t.Fatalf("a successful review must not be an error: %v", err)
	}
	if res.Verdict != VerdictFindings {
		t.Errorf("Verdict = %q, want findings", res.Verdict)
	}
}

func TestAFailedRunCarriesTheUsageLimitError(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "Review pull request", Result: runner.Result{
			ExitCode: 1, Stderr: []byte("You've hit your monthly limit"),
		}},
	}}
	_, err := New(f, "claude", nil, true, t.TempDir()).Run(t.Context(), "work", ref, nil, nil, nil)
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	var limitErr *UsageLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("the caller must be able to recognise a usage limit, got %T: %v", err, err)
	}
	// The underlying exit and stderr survive, so the message still says what
	// happened rather than only how it was classified.
	if !strings.Contains(err.Error(), "exit 1") {
		t.Errorf("the wrapped error must keep the detail: %q", err.Error())
	}
	if limitErr.Matched == "" {
		t.Error("the matched phrase must be recorded, so the log says what was recognised")
	}
}
