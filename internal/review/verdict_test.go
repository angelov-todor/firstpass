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
				{Match: "Review pull request", Result: runner.Result{Stdout: []byte(tc.stdout)}},
			}}
			res, err := New(f, "claude", nil, false, t.TempDir()).Run(context.Background(), "work", ref, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.Verdict != tc.want {
				t.Errorf("Result.Verdict = %q, want %q", res.Verdict, tc.want)
			}
		})
	}
}

// The instruction has to stand on its own: it reaches the reviewer as a
// system prompt, with no command next to it to lean on. It must name both
// accepted lines, say the last line printed is the verdict, and state the
// severity rule -- firstpass cannot apply that rule itself, because it never
// sees a finding.
func TestVerdictInstructionStatesTheProtocolAndTheSeverityRule(t *testing.T) {
	for _, want := range []string{
		"FIRSTPASS-VERDICT: approve",
		"FIRSTPASS-VERDICT: findings",
		"last line of your output",
		"Critical or Important",
		"nits",
	} {
		if !strings.Contains(verdictInstruction, want) {
			t.Errorf("the verdict instruction is missing %q:\n%s", want, verdictInstruction)
		}
	}
}

// TestThePromptCarriesTheVerdictAsk is the regression guard for the bug that
// made this whole feature inert in production, and it is the exact inverse of
// the test that used to stand here.
//
// That test asserted the -p value must NOT mention the verdict, reasoning that
// anything after "/code-review" becomes the slash command's $ARGUMENTS and
// would therefore be "at best ignored and no verdict line ever comes back".
// The reasoning was confident and wrong in the decisive direction: leaving the
// ask out of the prompt is what meant no verdict line ever came back.
// Fourteen consecutive production reviews finished, posted their comments and
// submitted nothing, and the test read as protection the whole time.
//
// What was actually measured, once live stdout was captured:
//
//   - the ask in --append-system-prompt alone, with a trivial -p prompt: the
//     line is printed exactly as asked. The mechanism was never the problem.
//   - the same ask alongside "/code-review <url>": ignored, and so was a far
//     blunter "do not perform any review" -- the reviewer worked for over
//     three minutes. The task's own instructions dominate.
//   - the ask added to the -p value: the reviewer's last line was
//     "FIRSTPASS-VERDICT: findings", parsed and recorded. One variable
//     changed between that run and the failing one.
//
// The $ARGUMENTS worry was real but harmless: the command definition
// referenced $ARGUMENTS nowhere, so its arguments reached the reviewer as
// trailing prompt text rather than being parsed for flags. The slash command
// is gone now in favour of a general instruction, which removes the concern
// entirely -- but the measurement it produced is why the ask is still stated
// in the -p value and not left to the system prompt alone.
func TestThePromptCarriesTheVerdictAsk(t *testing.T) {
	for _, dry := range []bool{true, false} {
		got := New(&runner.Fake{}, "claude", nil, dry, t.TempDir()).Prompt(ref)
		if !strings.Contains(got, VerdictMarker+" approve") ||
			!strings.Contains(got, VerdictMarker+" findings") {
			t.Errorf("dryRun=%v: the prompt must ask for both verdict lines, or the reviewer "+
				"finishes its review and prints nothing firstpass can read: %q", dry, got)
		}
		if !strings.HasPrefix(got, "Review pull request ") {
			t.Errorf("dryRun=%v: the prompt must still open by naming the pull request: %q", dry, got)
		}
		// ParseVerdict takes the LAST marked line, so "print nothing after it"
		// is what stops a recap or a sentence quoting the marker from becoming
		// the verdict. "findings" followed by prose mentioning approve
		// resolves to approve -- an approval nobody chose, on a colleague's
		// pull request. Saying so only in the system prompt is what this whole
		// commit demonstrates is not enough.
		if !strings.Contains(got, "Print nothing at all after that line") {
			t.Errorf("dryRun=%v: the prompt must forbid output after the verdict line, or a "+
				"trailing recap is read as the verdict: %q", dry, got)
		}
	}
}

// And the instruction must still reach claude, in both modes, byte for byte:
// the dry-run report's "the verdict would have been" is only a preview if the
// reviewer was asked exactly the question a live run asks.
func TestBothModesPassTheSameVerdictInstructionToClaude(t *testing.T) {
	arg := func(dry bool) string {
		t.Helper()
		f := &runner.Fake{Replies: []runner.Reply{
			{Match: "Review pull request", Result: runner.Result{Stdout: []byte("x")}},
		}}
		if _, err := New(f, "claude", nil, dry, t.TempDir()).Run(context.Background(), "work", ref, nil, nil); err != nil {
			t.Fatal(err)
		}
		args := f.Calls[0].Args
		for i, a := range args {
			if a == "--append-system-prompt" && i+1 < len(args) {
				return args[i+1]
			}
		}
		t.Fatalf("dryRun=%v: no --append-system-prompt in %q", dry, args)
		return ""
	}
	dry, live := arg(true), arg(false)
	if dry != verdictInstruction {
		t.Errorf("dry run passed %q, want the verdict instruction", dry)
	}
	if dry != live {
		t.Errorf("the instruction must be identical in both modes:\ndry  = %q\nlive = %q", dry, live)
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
				{Match: "Review pull request", Result: runner.Result{Stdout: []byte(tc.stdout)}},
			}}
			res, err := New(f, "claude", nil, true, t.TempDir()).Run(context.Background(), "work", ref, nil, nil)
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
