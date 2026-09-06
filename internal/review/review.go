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

// promptVerdictAsk is the verdict requirement as it appears in the -p value,
// after the command and its arguments.
//
// It duplicates what verdictInstruction already says in the system prompt, and
// the duplication is the point. The system prompt alone did not work: fourteen
// consecutive production reviews finished, posted their comments, and printed
// no verdict line, so every one of them submitted nothing.
//
// Three measurements established why, rather than one theory:
//
//   - Asked in --append-system-prompt for a trivial -p prompt, the line is
//     printed exactly as specified. The mechanism is sound.
//   - Asked in --append-system-prompt alongside "/code-review <url>", an
//     instruction as blunt as "do not perform any review, do not use any
//     tools" was ignored and the reviewer worked for over three minutes. The
//     task's own instructions dominate an appended system prompt.
//   - Reproduced against a throwaway pull request: a complete review of all
//     three seeded defects, ending in prose, verdict=unknown.
//
// /code-review carries its own explicit output contract, and by the time the
// review is written the appended request is thousands of tokens behind the
// instructions actually being followed. Restating it in the user turn puts it
// where it is the last thing asked, next to the deliverable it constrains.
//
// It is safe to put here despite everything after the command becoming that
// command's $ARGUMENTS: the command does not reference $ARGUMENTS at all, so
// its arguments reach the reviewer as trailing prompt text rather than being
// parsed. That was checked in the shipped command definition, not assumed.
const promptVerdictAsk = "\n\nWhen the review is complete, print exactly one of these two lines, " +
	"verbatim, as the very last line of your output:\n" +
	VerdictMarker + " approve\n" +
	VerdictMarker + " findings\n" +
	"Print `findings` if the review raised anything Critical or Important, and `approve` if it " +
	"raised nothing or only minor nits. This line is the only thing firstpass can see about what " +
	"you found.\n" +
	// ParseVerdict takes the *last* marked line, so this constraint is not
	// politeness: a recap or a sentence quoting the marker after the real
	// verdict is read as the verdict. "findings" followed by prose mentioning
	// approve resolves to approve -- an approving review on a colleague's pull
	// request that nobody chose, which is the single outcome this feature
	// exists to prevent. The system prompt has said this all along, and this
	// commit is the evidence that the system prompt alone is not obeyed.
	"Print nothing at all after that line, and no other line beginning with " + VerdictMarker

// Prompt is what claude is asked to do: the slash command naming the pull
// request under review, and the verdict line firstpass reads back. Dry run and
// live differ by exactly the --comment flag, so a dry-run report is what would
// have been posted.
//
// The ref is load-bearing, not decoration. The checkout Prepare hands over is
// a detached worktree of the PR head inside firstpass's own bare mirror: it has
// no branch, no upstream and no merge-base to compare against, so a bare
// "/code-review" would review an empty diff, and a live one would have no
// pull request to post to. The URL is the only thing that tells the reviewer
// what to look at.
//
// The command and its flag come first and are unchanged, so the form that has
// been exercised against real claude is still exactly what is asked; see
// promptVerdictAsk for why the verdict requirement is repeated here.
func (rr *Runner) Prompt(ref prref.PRRef) string {
	p := "/code-review " + ref.URL()
	if !rr.dryRun {
		p += " --comment"
	}
	return p + promptVerdictAsk
}

// verdictInstruction asks for the one line ParseVerdict reads, and explains
// what firstpass does with it. It travels as --append-system-prompt.
//
// It used to be the only place the line was asked for, on the reasoning that
// anything after "/code-review" in the -p value becomes that command's
// $ARGUMENTS and so would be "at best ignored, leaving no verdict line at
// all". The prediction was exactly inverted: asking only here is what left no
// verdict line at all, through fourteen consecutive production reviews. The
// task's own instructions dominate an appended system prompt over the course
// of a long agentic run. promptVerdictAsk records what was measured.
//
// It stays, in full, for two reasons. It is the longer statement -- it says
// why the line matters, which the prompt's shorter restatement does not -- and
// it is passed identically in both modes, so a dry-run report's "the verdict
// would have been" is a genuine preview of the live question.
//
// It is passed identically in both modes, so a dry-run report's "the verdict
// would have been" is a genuine preview: the reviewer was asked exactly the
// question a live run asks. The dry-run and live prompts still differ by
// exactly --comment.
//
// It has to stand on its own, with no command adjacent to lean on, and it
// states the severity rule because firstpass cannot apply it: firstpass never
// sees a finding, only this line.
// "an automated review pass", not "an automated first pass ... before any
// human has looked at it". The old wording was told to the reviewer on every
// pass, including the fifth, where both halves are false: it is not the first
// pass, and a human may well have reviewed the pull request by then. What the
// reviewer needs to know about history it is told precisely, by
// secondPassNote, which only appears when there actually was a previous pass.
const verdictInstruction = "You are running as firstpass: an automated review pass over a pull " +
	"request, ahead of human review.\n\n" +
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

// ShortSHA is how a commit is named to a human -- or to a reviewing agent.
// Twelve characters, because the whole forty are noise in prose and the
// reviewer never has to type it back.
func ShortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// PreviousPass describes a pass that has already reviewed this pull request.
// A nil *PreviousPass means this is the first pass.
//
// Two fields rather than a bare SHA, because "a pass has been here" and "that
// pass finished" are different facts and the reviewer needs both. A replay --
// whose documented use is a needs_attention pull request -- is exactly the
// case where the second is false.
type PreviousPass struct {
	// HeadSHA is the commit that pass reviewed. It is also the evidence that
	// there was a pass at all: only a record written once claude had started
	// carries one.
	HeadSHA string
	// Posted says that pass's findings actually reached the pull request. A
	// dry run records a review but withholds --comment, so its findings went
	// to a report on disk instead: nothing is on the pull request, there is
	// nothing to duplicate and nothing to hold back, and the reviewer is told
	// nothing at all.
	Posted bool
	// Incomplete says that pass did not finish. It was posting its findings
	// one at a time when it stopped, so some may be on the pull request and
	// some may not, and nothing can tell which.
	Incomplete bool
	// HeadUnchanged says the pull request is still at a commit an earlier pass
	// reviewed, so there is nothing new to concentrate on and no diff to take.
	// Only a replay reaches this: a re-post requires a commit no pass has
	// reviewed.
	HeadUnchanged bool
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
// The incomplete variant is not a hedge for its own sake. Telling the reviewer
// that the earlier pass posted its findings, when that pass in fact died
// part-way through posting them, is wrong in both directions: it invites a
// duplicate of the comments that did land, and it invites silence about the
// findings that never did. So that variant says plainly that nothing knows how
// far the earlier pass got, asks the reviewer to check the pull request before
// posting a comment, and asks it to raise anything that is not already there.
//
// The moved-head variants ask for attention on what changed rather than for an
// incremental diff, and say why: the mirror force-updates refs/firstpass/N, so
// the previously-reviewed commit can be unreachable in the checkout by now. A
// reviewer told to "diff against <sha>" would find nothing there and either
// invent an answer or review nothing at all.
//
// HeadUnchanged replaces all of that framing, because none of it is true when
// the pull request is still at a commit an earlier pass reviewed -- which only
// a replay reaches. "Concentrate on what has changed" then names changes that
// do not exist, and a blanket "do not restate that pass's findings" tells the
// reviewer not to report the findings it is being asked to find, which is close
// to neutering the command. `firstpass replay` is exactly what someone runs
// when the first pass was unsatisfying or when they are flipping dry run to
// live, so it has to come back with a real review. What survives is the one
// thing still true: a comment may already be sitting on that line.
//
// It is deliberately not called at all when the earlier pass posted nothing;
// see Run and PreviousPass.Posted.
func secondPassNote(pp PreviousPass) string {
	short := ShortSHA(pp.HeadSHA)

	if pp.HeadUnchanged {
		// Incomplete has to be honoured here too, and missing that was the
		// whole of a real defect: this branch returns before the one below, so
		// a replay of a needs_attention pull request whose head had not
		// moved -- the most documented replay there is -- was told that pass's
		// findings "were posted as inline comments on it". Some are and some
		// are not, and nothing knows which, which is exactly what the
		// incomplete wording exists to say.
		what := "has already reviewed this pull request at the commit it is still at, and its " +
			"findings were posted as inline comments on it."
		if pp.Incomplete {
			what = "began reviewing this pull request at the commit it is still at, and did not " +
				"finish. Some of its findings may already be posted as inline comments on the " +
				"pull request and some may not: it was posting them one at a time when it " +
				"stopped, and firstpass cannot tell how far it got."
		}
		return "A previous automated pass " + what + " Nothing has changed since, so review it " +
			"in full: this pass was asked for deliberately, and a review that held its findings " +
			"back because an earlier one might have had them would say nothing at all.\n\n" +
			"Before you post a finding, check whether that comment is already on the pull " +
			"request, and do not post it again if it is: a second copy of the same comment on " +
			"the same line spends the author's time twice on one point. Everything else, raise " +
			"as you normally would."
	}

	head := "A previous automated pass already reviewed this pull request at commit " + short +
		" and posted its findings as inline comments on it.\n\n" +
		"Concentrate on what has changed since " + short + ". Do not restate findings from that " +
		"pass: they are already on the pull request, and repeating one puts a second copy of the " +
		"same comment on the same line, which spends the author's time twice on one point."
	if pp.Incomplete {
		head = "A previous automated pass began reviewing this pull request at commit " + short +
			" and did not finish. Some of its findings may already be posted as inline comments " +
			"on the pull request and some may not: it was posting them one at a time when it " +
			"stopped, and firstpass cannot tell how far it got.\n\n" +
			"Concentrate on what has changed since " + short + ". Before you post a finding, " +
			"check whether that comment is already on the pull request, and do not post it again " +
			"if it is: a second copy of the same comment on the same line spends the author's " +
			"time twice on one point. Where a finding is not already there, raise it -- the " +
			"earlier pass may never have got to it."
	}

	return head + "\n\n" +
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
// previous describes a pass that has already reviewed this pull request, and
// nil means this is the first pass. It changes only the system prompt and the
// dry-run report's filename: the -p value is byte-identical across passes,
// because everything in it is /code-review's own arguments.
func (rr *Runner) Run(ctx context.Context, dir string, ref prref.PRRef, previous *PreviousPass) (Result, error) {
	system := verdictInstruction
	// Posted is a gate, not merely a wording input. An earlier pass that
	// posted nothing left nothing on the pull request to duplicate and nothing
	// to hold back, so there is nothing worth telling the reviewer -- and both
	// of the note's claims would be false. previous is still carried in either
	// way, because the report filename depends on it.
	if previous != nil && previous.Posted {
		system += "\n\n" + secondPassNote(*previous)
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
		// A live run's findings live on the pull request, so there is normally
		// no report to write.
		//
		// The exception is a review that finished without a verdict line. That
		// is the one live outcome firstpass cannot explain from its own
		// records: the review worked, the comments are posted, and the only
		// thing missing is the line firstpass needed. Discarding the output
		// there is what let this go unnoticed through fourteen consecutive
		// production reviews -- every one recorded "verdict unknown", and not
		// one of them left anything to read. Diagnosing it in the end took a
		// throwaway pull request and a dry run to reproduce what had already
		// happened fourteen times.
		//
		// A failed write is ignored on purpose: this is diagnostics for an
		// outcome that has already been recorded, and it must not turn a
		// review that succeeded into one that reports an error. It also must
		// not raise ReportError in live mode, whose recorded detail says
		// nothing was posted -- true of a dry run and a dangerous lie here.
		//
		// A review that did not finish qualifies for the same reason the
		// missing verdict does, and more urgently: it was posting comments one
		// at a time when it stopped, firstpass cannot tell how far it got, and
		// the operator is told comments may be half posted. Whatever the
		// reviewer had printed is the only evidence of how far it actually
		// got.
		if runErr != nil || out.Verdict == VerdictUnknown {
			if path, werr := rr.writeReport(ref, previous, res.Stdout, res.Stderr, out.Verdict, runErr); werr == nil {
				out.ReportPath = path
			}
		}
		return out, runErr
	}

	path, werr := rr.writeReport(ref, previous, res.Stdout, res.Stderr, out.Verdict, runErr)
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

// verdictNote is the report's line about the verdict.
//
// It is written differently for the two kinds of report, because they describe
// different facts. A dry-run report is subjunctive: nothing was submitted and
// nothing was posted, so the operator is watching firstpass decide before it
// decides for real. A live report is not: its findings are already on the pull
// request, and saying "nothing was submitted here" about a review whose
// comments are posted would send the operator looking for damage in the wrong
// place -- or, worse, reassure them there is none.
func verdictNote(v Verdict, dryRun bool) string {
	if !dryRun {
		switch v {
		case VerdictApprove, VerdictFindings:
			return fmt.Sprintf("**Verdict: %s** — submitted on the pull request. Its comments "+
				"are posted there too.", v)
		default:
			return "**Verdict: unknown — the reviewer printed no verdict line firstpass " +
				"recognises.** No verdict was submitted and none was guessed. This review's " +
				"comments ARE posted on the pull request; only the verdict is missing. This " +
				"report exists because that outcome cannot be diagnosed from firstpass's own " +
				"records."
		}
	}
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
func (rr *Runner) writeReport(ref prref.PRRef, previous *PreviousPass, body, stderr []byte,
	verdict Verdict, failure error) (string, error) {

	if err := os.MkdirAll(rr.reportsDir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s_%s_%d.md", ref.Owner, ref.Repo, ref.Number)
	if previous != nil {
		name = fmt.Sprintf("%s_%s_%d_after_%s.md",
			ref.Owner, ref.Repo, ref.Number, ShortSHA(previous.HeadSHA))
	}
	path := filepath.Join(rr.reportsDir, name)

	// "dry run, nothing posted" was hardcoded here while the only reports were
	// dry-run reports. Live reports exist now, for reviews that finished
	// without a verdict, and a live report claiming nothing was posted is
	// false about the one thing an operator reads it to establish.
	mode := "dry run, nothing posted"
	if !rr.dryRun {
		mode = "live run — this review's comments are posted on the pull request"
	}
	header := fmt.Sprintf("# Review of %s\n\n%s\n\nGenerated %s — %s.\n",
		ref.Key(), ref.URL(), time.Now().UTC().Format(time.RFC3339), mode)
	header += "\n" + verdictNote(verdict, rr.dryRun) + "\n"
	if failure != nil {
		header += fmt.Sprintf("\n**The review did not finish: %s**\n\nWhatever claude had printed "+
			"before it stopped is below, and may be incomplete.\n", failure)
	}
	header += "\n---\n\n"

	out := append([]byte(header), body...)
	// stderr, when there is any, is where an exit-0 truncation explains
	// itself -- a usage limit, a context compaction, a tool refusal. Those are
	// exactly the explanations for a review that finished and printed no
	// verdict, so a report written to diagnose that outcome must not drop
	// them.
	if len(stderr) > 0 {
		out = append(out, []byte("\n\n---\n\n## stderr\n\n```\n")...)
		out = append(out, stderr...)
		out = append(out, []byte("\n```\n")...)
	}

	if err := os.WriteFile(path, out, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
