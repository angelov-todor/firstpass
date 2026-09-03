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
func (rr *Runner) Run(ctx context.Context, dir string, ref prref.PRRef) (Result, error) {
	args := append([]string{"-p", rr.Prompt(ref)}, rr.extraArgs...)

	res, err := rr.r.Run(ctx, dir, rr.claude, args...)
	out := Result{ExitCode: res.ExitCode, Stdout: res.Stdout}

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

	path, werr := rr.writeReport(ref, res.Stdout, runErr)
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

// writeReport writes the dry-run report. failure, when non-nil, is the
// review's own failure and is recorded in the header, so a report that holds
// only partial output cannot be mistaken for a complete review.
func (rr *Runner) writeReport(ref prref.PRRef, body []byte, failure error) (string, error) {
	if err := os.MkdirAll(rr.reportsDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(rr.reportsDir,
		fmt.Sprintf("%s_%s_%d.md", ref.Owner, ref.Repo, ref.Number))

	header := fmt.Sprintf("# Review of %s\n\n%s\n\nGenerated %s — dry run, nothing posted.\n",
		ref.Key(), ref.URL(), time.Now().UTC().Format(time.RFC3339))
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
