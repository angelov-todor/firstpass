package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/pipeline"
	"github.com/angelov-todor/firstpass/internal/prref"
)

func TestRenderSweepListsEveryDecisionWithItsReason(t *testing.T) {
	rep := pipeline.SweepReport{
		MessagesScanned: 12,
		Decisions: []pipeline.Decision{
			{Ref: prref.PRRef{Owner: "Example-Org", Repo: "aex-balances", Number: 12},
				Action: pipeline.ActionWouldReview, Reason: "OPEN, not draft, author colleague"},
			{Ref: prref.PRRef{Owner: "Example-Org", Repo: "aex-margin-service", Number: 3},
				Action: pipeline.ActionSkip, Reason: "own PR"},
		},
	}

	var buf bytes.Buffer
	renderSweep(&buf, rep, true, true)
	out := buf.String()

	for _, want := range []string{
		"12 messages",
		"Example-Org/aex-balances#12",
		"would_review",
		"OPEN, not draft, author colleague",
		"Example-Org/aex-margin-service#3",
		"own PR",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderSweepAnnouncesPrintOnly(t *testing.T) {
	var buf bytes.Buffer
	renderSweep(&buf, pipeline.SweepReport{}, true, true)
	if !strings.Contains(buf.String(), "print-only") {
		t.Errorf("print-only must say so, so nobody assumes state was written or anything was posted:\n%s", buf.String())
	}
}

// TestRenderSweepPrintOnlyNeverClaimsLive is the regression test for the
// falsely-live banner: a misconfigured or transitional dry_run=false, run
// with -print-only, must never claim comments were posted, because
// cmdScan always passes PrintOnly: true and this build cannot post at all.
func TestRenderSweepPrintOnlyNeverClaimsLive(t *testing.T) {
	var buf bytes.Buffer
	renderSweep(&buf, pipeline.SweepReport{}, true, false)
	out := buf.String()
	if strings.Contains(out, "live") {
		t.Errorf("a print-only run must never claim to be live, no matter dry_run:\n%s", out)
	}
}

func TestRenderSweepAnnouncesDryRun(t *testing.T) {
	var buf bytes.Buffer
	renderSweep(&buf, pipeline.SweepReport{}, false, true)
	if !strings.Contains(buf.String(), "dry run") {
		t.Errorf("a dry run must say so, so nobody assumes comments were posted:\n%s", buf.String())
	}
}

func TestRenderSweepAnnouncesLive(t *testing.T) {
	var buf bytes.Buffer
	renderSweep(&buf, pipeline.SweepReport{}, false, false)
	if !strings.Contains(buf.String(), "live") {
		t.Errorf("a live, non-print-only, non-dry-run sweep must say so:\n%s", buf.String())
	}
}

func TestRenderSweepAnnouncesColdStart(t *testing.T) {
	var buf bytes.Buffer
	renderSweep(&buf, pipeline.SweepReport{ColdStart: true, MessagesScanned: 40}, true, true)
	out := buf.String()
	if !strings.Contains(out, "cold start") {
		t.Errorf("a cold start must be explained, not look like a broken sweep:\n%s", out)
	}
	if !strings.Contains(out, "--backfill") {
		t.Errorf("a cold start must point at the way to review history on purpose:\n%s", out)
	}
}

func TestRenderSweepAnnouncesPause(t *testing.T) {
	var buf bytes.Buffer
	renderSweep(&buf, pipeline.SweepReport{Paused: true}, false, false)
	if !strings.Contains(buf.String(), "paused") {
		t.Errorf("a paused sweep must say so:\n%s", buf.String())
	}
}

// testConfigBody is a minimal but Validate-passing config.yaml body: it sets
// state_dir plus the four fields that Default() deliberately leaves empty.
func testConfigBody(stateDir string) string {
	return "state_dir: " + filepath.ToSlash(stateDir) + "\n" +
		"space: spaces/EXAMPLE123\n" +
		"github_login: someone\n" +
		"allow_owners:\n  - example-org\n" +
		"paths:\n  chat_script: chat.py\n"
}

func TestOpenAppWithoutReviewLeavesTheReviewerUnwired(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := testConfigBody(dir)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := openApp(cfgPath, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if a.pipe.Rev != nil || a.pipe.WTs != nil {
		t.Error("withReview=false must leave the reviewer and worktrees nil, so no build can post by accident")
	}
}

func TestOpenAppWithReviewWiresBothAndHonoursLive(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := testConfigBody(dir)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := openApp(cfgPath, false, true)
	if err != nil {
		t.Fatal(err)
	}

	if a.pipe.Rev == nil || a.pipe.WTs == nil {
		t.Fatal("withReview=true must wire both")
	}
	if !a.cfg.DryRun {
		t.Error("dry_run must default to true")
	}
	// Close a's store before opening b's: both configs share the same
	// state_dir, so their state.db is the same file, and bbolt only
	// allows one open handle on a file at a time.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := openApp(cfgPath, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.cfg.DryRun {
		t.Error("-live must switch dry_run off")
	}
}

// The pipeline refuses to react in a dry run on its own account, but dry_run is
// the switch the operator trusts with "this cannot touch anybody else's chat",
// so the wiring declines to hand it a reactor at all. Two independent guards,
// and this pins the outer one -- a pipeline-level test cannot, because it wires
// a reactor itself in order to have something to assert against.
func TestOpenAppWiresNoReactorInADryRun(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(testConfigBody(dir)), 0o600); err != nil {
		t.Fatal(err)
	}

	// Dry run, reviewer wired: the one case where a reactor could plausibly be
	// wanted and must not be.
	a, err := openApp(cfgPath, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !a.cfg.DryRun {
		t.Fatal("dry_run must default to true, or this proves nothing")
	}
	if a.pipe.Rev == nil {
		t.Fatal("the reviewer must be wired, so the nil reactor below is about dry_run alone")
	}
	if a.pipe.React != nil {
		t.Error("a dry run must get no reactor: chat is outward, and dry_run is an absolute " +
			"no-outward-effect switch")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	// Print-only style wiring: no reviewer, so no reactor either.
	b, err := openApp(cfgPath, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if b.pipe.React != nil {
		t.Error("withReview=false must leave the reactor nil too, so no build that cannot review " +
			"can still post to chat")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	// Live and reviewing: the only combination that gets one.
	c, err := openApp(cfgPath, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.cfg.DryRun {
		t.Fatal("-live must switch dry_run off")
	}
	if c.pipe.React == nil {
		t.Error("a live reviewing run must have a reactor, or the feature is wired off entirely")
	}
}
