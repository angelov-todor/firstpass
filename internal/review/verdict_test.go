package review

// The verdict: /code-review posts its findings itself, so firstpass sees only
// the exit code and stdout. One machine-readable line is the whole channel
// between what the reviewer concluded and what firstpass submits, which is why
// its parsing is strict and never guesses.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/runner"
)

func TestParseVerdictReadsApprove(t *testing.T) {
	if got := ParseVerdict([]byte("looked at everything\nFIRSTPASS-VERDICT: approve\n")); got != VerdictApprove {
		t.Errorf("ParseVerdict() = %q, want %q", got, VerdictApprove)
	}
}

func TestParseVerdictReadsFindings(t *testing.T) {
	if got := ParseVerdict([]byte("posted 3 comments\nFIRSTPASS-VERDICT: findings")); got != VerdictFindings {
		t.Errorf("ParseVerdict() = %q, want %q", got, VerdictFindings)
	}
}

func TestParseVerdictToleratesSurroundingWhitespace(t *testing.T) {
	for _, in := range []string{
		"  FIRSTPASS-VERDICT: approve  \n",
		"\tFIRSTPASS-VERDICT:approve\r\n",
		"FIRSTPASS-VERDICT:   approve",
	} {
		if got := ParseVerdict([]byte(in)); got != VerdictApprove {
			t.Errorf("ParseVerdict(%q) = %q, want %q", in, got, VerdictApprove)
		}
	}
}

// No line at all is the case that actually happened: /code-review found
// nothing and said nothing. It must not become an approval by default.
func TestParseVerdictWithNoLineIsUnknown(t *testing.T) {
	if got := ParseVerdict([]byte("I reviewed the diff and found nothing.\n")); got != VerdictUnknown {
		t.Errorf("ParseVerdict() = %q, want %q", got, VerdictUnknown)
	}
}

func TestParseVerdictWithNoOutputAtAllIsUnknown(t *testing.T) {
	if got := ParseVerdict(nil); got != VerdictUnknown {
		t.Errorf("ParseVerdict(nil) = %q, want %q", got, VerdictUnknown)
	}
}

// Anything outside the two accepted words is unknown, never a guess: an
// "lgtm" or an "approve with nits" that firstpass rounded to approve would
// approve a colleague's pull request on the strength of a string it does not
// actually understand.
func TestParseVerdictIsStrictAboutTheValue(t *testing.T) {
	for _, in := range []string{
		"FIRSTPASS-VERDICT: lgtm",
		"FIRSTPASS-VERDICT: APPROVE",
		"FIRSTPASS-VERDICT: approve (only nits)",
		"FIRSTPASS-VERDICT: approved",
		"FIRSTPASS-VERDICT:",
		"the answer is FIRSTPASS-VERDICT: approve",
	} {
		if got := ParseVerdict([]byte(in)); got != VerdictUnknown {
			t.Errorf("ParseVerdict(%q) = %q, want %q", in, got, VerdictUnknown)
		}
	}
}

// The reviewer is asked for the line last, but a headless agent narrating its
// own plan can mention it early and then print the real one. The last
// matching line is the verdict; an earlier one is chatter.
func TestParseVerdictTakesTheLastMatchingLine(t *testing.T) {
	out := "I will finish with FIRSTPASS-VERDICT: approve\n" +
		"FIRSTPASS-VERDICT: approve\n" +
		"wait, one more file\n" +
		"posted a comment\n" +
		"FIRSTPASS-VERDICT: findings\n" +
		"done.\n"
	if got := ParseVerdict([]byte(out)); got != VerdictFindings {
		t.Errorf("ParseVerdict() = %q, want %q: the last verdict line is the verdict", got, VerdictFindings)
	}
}

// The reverse direction of the same rule: an unrecognised last line does not
// fall back to a recognised earlier one.
func TestParseVerdictDoesNotFallBackToAnEarlierLine(t *testing.T) {
	out := "FIRSTPASS-VERDICT: findings\nFIRSTPASS-VERDICT: maybe\n"
	if got := ParseVerdict([]byte(out)); got != VerdictUnknown {
		t.Errorf("ParseVerdict() = %q, want %q", got, VerdictUnknown)
	}
}

func TestRunReturnsTheParsedVerdict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout string
		want   Verdict
	}{
		{"approve", "all good\nFIRSTPASS-VERDICT: approve\n", VerdictApprove},
		{"findings", "posted\nFIRSTPASS-VERDICT: findings\n", VerdictFindings},
		{"silent", "found nothing worth saying\n", VerdictUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &runner.Fake{Replies: []runner.Reply{
				{Match: "code-review", Result: runner.Result{Stdout: []byte(tc.stdout)}},
			}}
			res, err := New(f, "claude", nil, false, t.TempDir()).Run(context.Background(), "work", ref)
			if err != nil {
				t.Fatal(err)
			}
			if res.Verdict != tc.want {
				t.Errorf("Result.Verdict = %q, want %q", res.Verdict, tc.want)
			}
		})
	}
}

// The prompt is the only place the verdict protocol is stated, and it must be
// stated identically in both modes: the dry-run report's would-be verdict is
// only a preview if the reviewer was asked the same question.
func TestPromptAsksForTheVerdictLineInBothModes(t *testing.T) {
	for _, dry := range []bool{true, false} {
		got := New(&runner.Fake{}, "claude", nil, dry, t.TempDir()).Prompt(ref)
		for _, want := range []string{
			"FIRSTPASS-VERDICT: approve",
			"FIRSTPASS-VERDICT: findings",
			"Critical or Important",
			"nits",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("dryRun=%v prompt missing %q:\n%s", dry, want, got)
			}
		}
	}
}

// A dry run submits no verdict, so the report is the only place the operator
// can watch it decide before it decides for real.
func TestDryRunReportStatesTheWouldBeVerdict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout string
		want   string
	}{
		{"approve", "FIRSTPASS-VERDICT: approve\n", "would have been approve"},
		{"findings", "FIRSTPASS-VERDICT: findings\n", "would have been findings"},
		{"missing", "nothing to report\n", "no verdict line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &runner.Fake{Replies: []runner.Reply{
				{Match: "code-review", Result: runner.Result{Stdout: []byte(tc.stdout)}},
			}}
			res, err := New(f, "claude", nil, true, t.TempDir()).Run(context.Background(), "work", ref)
			if err != nil {
				t.Fatal(err)
			}
			body, rerr := os.ReadFile(res.ReportPath)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("report must say what the verdict would have been, missing %q:\n%s", tc.want, body)
			}
		})
	}
}
