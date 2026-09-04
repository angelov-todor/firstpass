// Package review runs a code review over a prepared checkout by driving the
// claude CLI, so the repository's own CLAUDE.md, the dotnet-techne-code-review
// skill and the sonarqube MCP all apply without firstpass knowing about any of
// them.
package review

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/runner"
)

// Result is the outcome of one review run.
type Result struct {
	ExitCode   int
	ReportPath string
	Stdout     []byte
	// Verdict is what the reviewer concluded about severity, parsed out of
	// stdout. It is VerdictUnknown unless the reviewer printed one of the two
	// accepted lines: firstpass never infers a verdict from anything else.
	Verdict Verdict
}

// Verdict is the reviewer's own answer to "does this pull request need a human
// to change something?".
//
// firstpass is deliberately ignorant of the findings themselves —
// /code-review posts them as inline comments and firstpass sees only the exit
// code and stdout — so this one line is the whole channel between the two.
// Deciding the verdict is the reviewer's job; submitting it is firstpass's.
type Verdict string

const (
	// VerdictUnknown is the verdict of a review that printed no accepted
	// verdict line. It is never resolved into one of the other two: firstpass
	// submits nothing rather than guess, because the wrong guess is an
	// approval on a colleague's pull request.
	VerdictUnknown Verdict = "unknown"
	// VerdictApprove means nothing needing change was raised: no findings, or
	// only minor nits.
	VerdictApprove Verdict = "approve"
	// VerdictFindings means something Critical or Important was raised.
	VerdictFindings Verdict = "findings"
)

// VerdictMarker prefixes the one machine-readable line the prompt asks for.
const VerdictMarker = "FIRSTPASS-VERDICT:"

// ParseVerdict reads the verdict out of a review's stdout.
//
// The last marked line wins, and it wins whatever it says. A headless agent
// narrating its own plan can print the marker early ("I will finish with
// FIRSTPASS-VERDICT: approve") and then print the real one after doing the
// work, so an early match cannot be trusted — and an unrecognised last line
// must not fall back to a recognised earlier one either, or this would be
// picking whichever answer it liked best out of the transcript.
//
// The value is compared exactly, after trimming: "APPROVE", "approved" and
// "approve (only nits)" are all unknown. Loose matching is what would turn a
// reviewer that hedged into an approval nobody chose.
func ParseVerdict(stdout []byte) Verdict {
	v := VerdictUnknown
	for _, line := range strings.Split(string(stdout), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), VerdictMarker)
		if !ok {
			continue
		}
		switch strings.TrimSpace(rest) {
		case string(VerdictApprove):
			v = VerdictApprove
		case string(VerdictFindings):
			v = VerdictFindings
		default:
			v = VerdictUnknown
		}
	}
	return v
}

// Runner drives the claude CLI.
type Runner struct {
	r          runner.Runner
	claude     string
	extraArgs  []string
	dryRun     bool
	reportsDir string
}

// New builds a review runner. extraArgs comes from config and normally carries
// --permission-mode bypassPermissions: headless claude cannot run gh to post
// comments under the default mode, and would block on a prompt nobody can
// answer.
func New(r runner.Runner, claude string, extraArgs []string, dryRun bool, reportsDir string) *Runner {
	return &Runner{r: r, claude: claude, extraArgs: extraArgs, dryRun: dryRun, reportsDir: reportsDir}
}

// Prompt is the slash command handed to claude, naming the pull request under
// review. Dry run and live differ by exactly the --comment flag, so a dry-run
// report is what would have been posted.
//
// The ref is load-bearing, not decoration. The checkout Prepare hands over is
// a detached worktree of the PR head inside firstpass's own bare mirror: it has
// no branch, no upstream and no merge-base to compare against, so a bare
// "/code-review" would review an empty diff, and a live one would have no
// pull request to post to. The URL is the only thing that tells the reviewer
// what to look at.
func (rr *Runner) Prompt(ref prref.PRRef) string {
	p := "/code-review " + ref.URL()
	if rr.dryRun {
		return p
	}
	return p + " --comment"
}

// verdictInstruction asks for the one line ParseVerdict reads. It travels as
// --append-system-prompt, never inside the -p prompt.
//
// That distinction is load-bearing. Everything after "/code-review" in the -p
// value becomes the slash command's $ARGUMENTS, so an instruction appended
// there would be handed to a skill that parses its arguments for an effort
// level, a --comment flag and a target: at best ignored, leaving no verdict
// line at all, and at worst perturbing the target parsing -- and firstpass
// would not find out until it silently failed in production. As a system
// prompt the instruction reaches the reviewing agent directly and the prompt
// stays byte-identical to the form that has actually been exercised against
// real claude.
//
// It is passed identically in both modes, so a dry-run report's "the verdict
// would have been" is a genuine preview: the reviewer was asked exactly the
// question a live run asks. The dry-run and live prompts still differ by
// exactly --comment.
//
// It has to stand on its own, with no command adjacent to lean on, and it
// states the severity rule because firstpass cannot apply it: firstpass never
// sees a finding, only this line.
const verdictInstruction = "You are running as firstpass: an automated first pass over a pull " +
	"request, before any human has looked at it.\n\n" +
	"When your review is complete, print exactly one of these two lines, verbatim, as the very " +
	"last line of your output:\n" +
	VerdictMarker + " approve\n" +
	VerdictMarker + " findings\n\n" +
	"Print `findings` if the review raised anything Critical or Important. Print `approve` if " +
	"the review raised nothing at all, or only minor nits. Print nothing after that line, and " +
	"no other line starting with " + VerdictMarker + "\n\n" +
	"firstpass reads that line to decide whether to submit an approving review on the pull " +
	"request or to leave it in the team's human review queue. It is the only thing firstpass " +
	"can see about what you found: if the line is missing or reworded, firstpass submits no " +
	"verdict at all."

// shortSHA is how a commit is named to a human -- or to a reviewing agent.
// Twelve characters, because the whole forty are noise in prose and the
// reviewer never has to type it back.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// secondPassNote tells the reviewer that a pass has already been here. It is
// appended to verdictInstruction and travels as --append-system-prompt for
// exactly the reasons that instruction does: prose in the -p value becomes
// /code-review's own $ARGUMENTS. Prompt() stays byte-identical across passes.
//
// Without it a second pass restates every finding the author has not yet
// fixed, on the same lines, putting a second copy of each comment on a
// colleague's pull request -- which is the whole cost this feature could
// impose and the only one it cannot detect for itself.
//
// It asks for attention on what changed rather than for an incremental diff,
// and says why: the mirror force-updates refs/firstpass/N, so the
// previously-reviewed commit can be unreachable in the checkout by now. A
// reviewer told to "diff against <sha>" would find nothing there and either
// invent an answer or review nothing at all.
func secondPassNote(previousHeadSHA string) string {
	short := shortSHA(previousHeadSHA)
	return "A previous automated pass already reviewed this pull request at commit " + short +
		" and posted its findings as inline comments on it.\n\n" +
		"Concentrate on what has changed since " + short + ". Do not restate findings from that " +
		"pass: they are already on the pull request, and repeating one puts a second copy of the " +
		"same comment on the same line, which spends the author's time twice on one point.\n\n" +
		"Review the pull request as it now stands, not a diff against " + short + ": firstpass " +
		"force-updates the mirror ref it checks out, so " + short + " may no longer be reachable " +
		"from the checkout you are looking at. Work out what has changed from the pull request " +
		"itself."
}

// ReportError reports that the review itself finished but its dry-run report
// could not be written.
//
// It exists so the caller can tell the two apart. A dry run posts nothing, so
// a failed report write must never be recorded the way a killed review is —
// with no exit status ("killed" in `firstpass status`) and a detail warning
// that comments may already be partially posted. Neither is true here: claude
// exited cleanly and nothing was sent to GitHub.
type ReportError struct{ Err error }

func (e *ReportError) Error() string { return "write dry-run report: " + e.Err.Error() }
func (e *ReportError) Unwrap() error { return e.Err }

// Run reviews the checkout in dir. A non-zero exit or a cancelled context is an
// error, so the caller never records the PR as reviewed on a failed run.
//
// In dry-run mode a report is written whether or not the review succeeded. A
// failed dry run is exactly when the operator most wants to read what
// happened, and reading a dry-run report is the gate before ever going live,
// so discarding the captured output on the failure path left nothing to read
// at the worst possible moment. The report names the failure itself.
//
// previousHeadSHA is the commit an earlier pass over this pull request
// reviewed, and empty means this is the first pass. It changes only the
// system prompt and the dry-run report's filename: the -p value is
// byte-identical across passes, because everything in it is /code-review's
// own arguments.
func (rr *Runner) Run(ctx context.Context, dir string, ref prref.PRRef, previousHeadSHA string) (Result, error) {
	system := verdictInstruction
	if previousHeadSHA != "" {
		system += "\n\n" + secondPassNote(previousHeadSHA)
	}
	// extraArgs stays last: it is operator-controlled config, so it must keep
	// being able to override anything firstpass sets for itself.
	args := append([]string{
		"-p", rr.Prompt(ref),
		"--append-system-prompt", system,
	}, rr.extraArgs...)

	res, err := rr.r.Run(ctx, dir, rr.claude, args...)
	out := Result{
		ExitCode: res.ExitCode,
		Stdout:   res.Stdout,
		Verdict:  ParseVerdict(res.Stdout),
	}

	// runErr is the review's own failure, kept separate from any failure to
	// write the report about it.
	var runErr error
	switch {
	case err != nil:
		runErr = fmt.Errorf("claude for %s: %w", ref.Key(), err)
	case res.ExitCode != 0:
		runErr = fmt.Errorf("claude for %s exit %d: %s",
			ref.Key(), res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	if !rr.dryRun {
		// A live run's findings live on the pull request; there is no report.
		return out, runErr
	}

	path, werr := rr.writeReport(ref, previousHeadSHA, res.Stdout, out.Verdict, runErr)
	if werr != nil {
		if runErr != nil {
			// The review failed and so did the report. The review failure is
			// the one the operator has to act on.
			return out, runErr
		}
		return out, &ReportError{Err: werr}
	}
	out.ReportPath = path
	return out, runErr
}

// verdictNote is the dry-run report's line about the verdict. A dry run
// submits none, so the report is the only place the operator can watch
// firstpass decide before it decides for real.
func verdictNote(v Verdict) string {
	switch v {
	case VerdictApprove:
		return "**Verdict: the verdict would have been approve** — live, firstpass would have " +
			"submitted an approving review on this pull request under your GitHub identity. " +
			"Nothing was submitted here."
	case VerdictFindings:
		return "**Verdict: the verdict would have been findings** — live, firstpass would have " +
			"submitted a COMMENT review, never request-changes, so the pull request stays in the " +
			"team's human review queue. Nothing was submitted here."
	default:
		return "**Verdict: unknown — the reviewer printed no verdict line firstpass recognises.** " +
			"Live, firstpass would have submitted no verdict at all and recorded the review as " +
			"reviewed with an unknown verdict."
	}
}

// writeReport writes the dry-run report. failure, when non-nil, is the
// review's own failure and is recorded in the header, so a report that holds
// only partial output cannot be mistaken for a complete review.
// A later pass over the same pull request gets its own filename, suffixed
// with the commit the pass before it reviewed. Both reports then survive,
// which is what the operator needs to see what the new commits changed --
// and the first pass's name is left exactly as it was, so nothing already on
// disk moves.
func (rr *Runner) writeReport(ref prref.PRRef, previousHeadSHA string, body []byte,
	verdict Verdict, failure error) (string, error) {

	if err := os.MkdirAll(rr.reportsDir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s_%s_%d.md", ref.Owner, ref.Repo, ref.Number)
	if previousHeadSHA != "" {
		name = fmt.Sprintf("%s_%s_%d_after_%s.md",
			ref.Owner, ref.Repo, ref.Number, shortSHA(previousHeadSHA))
	}
	path := filepath.Join(rr.reportsDir, name)

	header := fmt.Sprintf("# Review of %s\n\n%s\n\nGenerated %s — dry run, nothing posted.\n",
		ref.Key(), ref.URL(), time.Now().UTC().Format(time.RFC3339))
	header += "\n" + verdictNote(verdict) + "\n"
	if failure != nil {
		header += fmt.Sprintf("\n**The review did not finish: %s**\n\nWhatever claude had printed "+
			"before it stopped is below, and may be incomplete.\n", failure)
	}
	header += "\n---\n\n"

	if err := os.WriteFile(path, append([]byte(header), body...), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
