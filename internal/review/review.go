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

// Run reviews the checkout in dir. A non-zero exit or a cancelled context is an
// error, so the caller never records the PR as reviewed on a failed run.
func (rr *Runner) Run(ctx context.Context, dir string, ref prref.PRRef) (Result, error) {
	args := append([]string{"-p", rr.Prompt(ref)}, rr.extraArgs...)

	res, err := rr.r.Run(ctx, dir, rr.claude, args...)
	out := Result{ExitCode: res.ExitCode, Stdout: res.Stdout}
	if err != nil {
		return out, fmt.Errorf("claude for %s: %w", ref.Key(), err)
	}
	if res.ExitCode != 0 {
		return out, fmt.Errorf("claude for %s exit %d: %s",
			ref.Key(), res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	if rr.dryRun {
		path, werr := rr.writeReport(ref, res.Stdout)
		if werr != nil {
			return out, werr
		}
		out.ReportPath = path
	}
	return out, nil
}

func (rr *Runner) writeReport(ref prref.PRRef, body []byte) (string, error) {
	if err := os.MkdirAll(rr.reportsDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(rr.reportsDir,
		fmt.Sprintf("%s_%s_%d.md", ref.Owner, ref.Repo, ref.Number))

	header := fmt.Sprintf("# Review of %s\n\n%s\n\nGenerated %s — dry run, nothing posted.\n\n---\n\n",
		ref.Key(), ref.URL(), time.Now().UTC().Format(time.RFC3339))

	if err := os.WriteFile(path, append([]byte(header), body...), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
