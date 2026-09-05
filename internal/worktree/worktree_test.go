package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/runner"
)

// fixtureOrigin builds a real repository with a refs/pull/1/head ref, which is
// what GitHub exposes for a PR head.
func fixtureOrigin(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	// A developer with commit.gpgsign=true globally would otherwise have these
	// throwaway fixture commits demand a GPG passphrase. With no terminal to
	// prompt on, `git commit` does not fail — it blocks, so the whole package
	// hangs until the test binary's timeout rather than reporting anything
	// useful. A test fixture must not depend on the machine's signing state.
	run("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "base")

	run("checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "feature")
	sha := run("rev-parse", "HEAD")

	run("checkout", "-q", "main")
	run("update-ref", "refs/pull/1/head", sha)
	return dir
}

func newManager(t *testing.T, origin string) *Manager {
	t.Helper()
	base := t.TempDir()
	m := New(runner.OS{}, "git", filepath.Join(base, "repos"), filepath.Join(base, "work"))
	m.URLFor = func(prref.PRRef) string { return filepath.ToSlash(origin) }
	return m
}

var ref = prref.PRRef{Owner: "Example-Org", Repo: "aex-balances", Number: 1}

func TestPrepareChecksOutThePRHead(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real git")
	}
	m := newManager(t, fixtureOrigin(t))

	dir, cleanup, err := m.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if _, err := os.Stat(filepath.Join(dir, "feature.txt")); err != nil {
		t.Errorf("the PR head must be checked out: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "base.txt")); err != nil {
		t.Errorf("the base commit must be present too: %v", err)
	}
}

func TestPrepareIsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real git")
	}
	m := newManager(t, fixtureOrigin(t))

	_, cleanup1, err := m.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	cleanup1()

	dir, cleanup2, err := m.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatalf("a second Prepare must reuse the mirror, not fail: %v", err)
	}
	defer cleanup2()
	if _, err := os.Stat(filepath.Join(dir, "feature.txt")); err != nil {
		t.Errorf("second Prepare produced no checkout: %v", err)
	}
}

func TestPrepareRecoversFromALeftoverWorktree(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real git")
	}
	m := newManager(t, fixtureOrigin(t))

	dir, _, err := m.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a killed run: the worktree is still registered and on disk.
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}

	dir2, cleanup, err := m.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatalf("a worktree left behind by a killed run must not wedge the next review: %v", err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(dir2, "feature.txt")); err != nil {
		t.Errorf("recovery produced no checkout: %v", err)
	}
}

func TestCleanupRemovesTheWorktree(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real git")
	}
	m := newManager(t, fixtureOrigin(t))

	dir, cleanup, err := m.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("cleanup must remove the worktree, stat err = %v", err)
	}
}

func TestPrepareReportsCloneFailure(t *testing.T) {
	base := t.TempDir()
	m := New(runner.OS{}, "git", filepath.Join(base, "repos"), filepath.Join(base, "work"))
	m.URLFor = func(prref.PRRef) string { return filepath.Join(base, "does-not-exist") }

	if _, _, err := m.Prepare(context.Background(), ref); err == nil {
		t.Error("a clone failure must be reported")
	}
}

func TestDefaultURLIsGitHub(t *testing.T) {
	m := New(runner.OS{}, "git", "repos", "work")
	if got := m.url(ref); got != "https://github.com/Example-Org/aex-balances.git" {
		t.Errorf("url() = %q", got)
	}
}

// gitIn runs git inside the prepared worktree and reports stdout plus whether
// the command succeeded.
func gitIn(t *testing.T, dir string, args ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err == nil
}

// TestPrepareGivesTheWorktreeAResolvableDiffBase is the assertion that would
// have caught C1: Prepare used to fetch only refs/pull/N/head, leaving the
// worktree with no remote-tracking refs at all. `git rev-parse origin/main`
// failed inside it, so a review had no base to diff against and would have
// reported on an empty diff -- and a dry-run report of nothing is worse than
// a failure, because reading dry-run reports is the gate before going live.
func TestPrepareGivesTheWorktreeAResolvableDiffBase(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real git")
	}
	m := newManager(t, fixtureOrigin(t))

	dir, cleanup, err := m.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if out, ok := gitIn(t, dir, "rev-parse", "refs/remotes/origin/main"); !ok {
		t.Fatalf("refs/remotes/origin/main must resolve inside the worktree: %s", out)
	}
	if out, ok := gitIn(t, dir, "rev-parse", "origin/main"); !ok {
		t.Errorf("the short form origin/main must resolve too: %s", out)
	}

	out, ok := gitIn(t, dir, "diff", "--name-only", "refs/remotes/origin/main")
	if !ok {
		t.Fatalf("diff against the base must work: %s", out)
	}
	if !strings.Contains(out, "feature.txt") {
		t.Errorf("diff against the base must be non-empty for the PR head, got %q", out)
	}
}

// TestPrepareSetsRemoteFetchOnTheMirror keeps the mirror self-describing, so
// a later plain `git fetch` in it brings the branches down as well.
func TestPrepareSetsRemoteFetchOnTheMirror(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real git")
	}
	m := newManager(t, fixtureOrigin(t))

	_, cleanup, err := m.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	out, ok := gitIn(t, m.mirrorPath(ref), "config", "--get-all", "remote.origin.fetch")
	if !ok {
		t.Fatalf("remote.origin.fetch must be configured on the mirror: %s", out)
	}
	if !strings.Contains(out, "refs/remotes/origin/*") {
		t.Errorf("remote.origin.fetch = %q, want the branch refspec", out)
	}
}

// M22: a clone killed after `git init` but before `origin` was configured
// leaves a directory that has HEAD and nothing else. The old existence check
// accepted it, so every later `fetch origin` failed and the mirror was wedged
// permanently.
func TestPrepareRecoversFromAMirrorWithNoRemote(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real git")
	}
	m := newManager(t, fixtureOrigin(t))

	mirror := m.mirrorPath(ref)
	if err := os.MkdirAll(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	head := "ref: refs/heads/main" + "\n"
	if err := os.WriteFile(filepath.Join(mirror, "HEAD"), []byte(head), 0o600); err != nil {
		t.Fatal(err)
	}

	dir, cleanup, err := m.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatalf("a half-written mirror must be re-cloned, not wedge every future review: %v", err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(dir, "feature.txt")); err != nil {
		t.Errorf("recovery produced no checkout: %v", err)
	}
}

// I5: git must never stop and wait for a human. On Windows a private-repo
// HTTPS clone can raise a Git Credential Manager dialog and block forever,
// and firstpass clones by plain https URL with no credential configuration.
func TestGitInvocationsAreNonInteractive(t *testing.T) {
	f := &runner.Fake{}
	m := New(f, "git", filepath.Join(t.TempDir(), "repos"), filepath.Join(t.TempDir(), "work"))
	m.URLFor = func(prref.PRRef) string { return "https://github.com/Example-Org/aex-balances.git" }

	// Every call errors (the fake has no replies), which is enough: the first
	// git invocation is recorded before Prepare gives up.
	_, _, _ = m.Prepare(context.Background(), ref)
	if len(f.Calls) == 0 {
		t.Fatal("no git call was recorded")
	}
	for _, c := range f.Calls {
		line := c.String()
		for _, want := range []string{"-c credential.interactive=false", "-c core.askPass="} {
			if !strings.Contains(line, want) {
				t.Errorf("git call %q must carry %q so it cannot block on a prompt", line, want)
			}
		}
	}
}

// TestPrepareGivesUpOnTheMirrorWhenItsContextEnds is why the gate is a channel
// and not a sync.Mutex.
//
// handle builds the CloneTimeout deadline before it calls Prepare, so a
// candidate queued behind a cold clone of the same repository spends its own
// clone budget waiting. On a Mutex it would go on waiting past that deadline
// and only fail later, having run a git command against an already-expired
// context -- and a Ctrl-C could not free it either, because a worker parked on
// Lock() is unreachable, so shutdown would have to wait out the clone.
//
// Bounded well below the wait a Mutex would impose, so a regression fails here
// rather than hanging the suite.
func TestPrepareGivesUpOnTheMirrorWhenItsContextEnds(t *testing.T) {
	base := t.TempDir()
	m := New(runner.OS{}, "git", filepath.Join(base, "repos"), filepath.Join(base, "work"))
	m.URLFor = func(prref.PRRef) string { return "https://example.invalid/x.git" }

	// Stand in for a sibling review holding the mirror through a long clone.
	held := prref.PRRef{Owner: "Example-Org", Repo: "busy", Number: 1}
	mirror := m.mirrorPath(held)
	if err := m.lockMirror(context.Background(), mirror); err != nil {
		t.Fatal(err)
	}
	defer m.unlockMirror(mirror)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	type outcome struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan outcome, 1)
	go func() {
		start := time.Now()
		_, _, err := m.Prepare(ctx, prref.PRRef{Owner: "Example-Org", Repo: "busy", Number: 2})
		done <- outcome{err, time.Since(start)}
	}()

	select {
	case got := <-done:
		if !errors.Is(got.err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want context.DeadlineExceeded", got.err)
		}
		if got.elapsed > 3*time.Second {
			t.Errorf("Prepare took %s to give up on a 50ms deadline", got.elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Prepare never returned: the wait for the mirror ignores its context, " +
			"so a queued review cannot be interrupted")
	}
}

// overlapRunner records the greatest number of git invocations that were ever
// in flight at the same moment.
type overlapRunner struct {
	mu     sync.Mutex
	active int
	max    int
}

func (o *overlapRunner) Run(ctx context.Context, dir, name string, args ...string) (runner.Result, error) {
	o.mu.Lock()
	o.active++
	if o.active > o.max {
		o.max = o.active
	}
	o.mu.Unlock()

	// Wide enough that two unsynchronised Prepares would certainly overlap:
	// each makes several git calls, so the windows are hundreds of
	// milliseconds long against a scheduling gap of microseconds.
	time.Sleep(50 * time.Millisecond)

	o.mu.Lock()
	o.active--
	o.mu.Unlock()
	return runner.Result{}, nil
}

func (o *overlapRunner) peak() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.max
}

// TestPreparesForOneRepositoryDoNotOverlap is the invariant that makes
// concurrent reviews safe at all.
//
// Worktrees are per pull request; mirrors are per repository. Two pull requests
// from one service -- which is how the team posts them -- share a bare
// repository, and Prepare rewrites it throughout: it can RemoveAll and
// re-clone it, it fetches into it, and it runs `worktree prune`, which removes
// registrations another Prepare is in the middle of making.
func TestPreparesForOneRepositoryDoNotOverlap(t *testing.T) {
	o := &overlapRunner{}
	base := t.TempDir()
	m := New(o, "git", filepath.Join(base, "repos"), filepath.Join(base, "work"))
	m.URLFor = func(prref.PRRef) string { return "https://example.invalid/x.git" }

	var wg sync.WaitGroup
	for _, n := range []int{1, 2} {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r := prref.PRRef{Owner: "Example-Org", Repo: "same-repo", Number: n}
			_, cleanup, err := m.Prepare(context.Background(), r)
			if err != nil {
				t.Error(err)
				return
			}
			cleanup()
		}(n)
	}
	wg.Wait()

	if peak := o.peak(); peak != 1 {
		t.Errorf("%d git commands ran against one mirror at once, want 1; "+
			"concurrent Prepares corrupt the shared bare repository", peak)
	}
}

// TestPreparesForDifferentRepositoriesUseDifferentGates pins that the
// serialisation above is per repository and not global. A single lock would
// make this correct but pointless: reviews of unrelated services would queue
// behind each other for no reason, which is the opposite of why concurrency
// was added.
//
// Asserted on lock identity rather than on observed overlap, because "these
// two things did happen at the same time" is a timing assertion and would be
// flaky on a loaded machine.
func TestPreparesForDifferentRepositoriesUseDifferentGates(t *testing.T) {
	base := t.TempDir()
	m := New(runner.OS{}, "git", filepath.Join(base, "repos"), filepath.Join(base, "work"))

	a := m.mirrorPath(prref.PRRef{Owner: "Example-Org", Repo: "alpha", Number: 1})
	b := m.mirrorPath(prref.PRRef{Owner: "Example-Org", Repo: "beta", Number: 1})
	other := m.mirrorPath(prref.PRRef{Owner: "Example-Org", Repo: "alpha", Number: 2})

	if m.mirrorGate(a) == m.mirrorGate(b) {
		t.Error("different repositories must not share a lock")
	}
	if m.mirrorGate(a) != m.mirrorGate(other) {
		t.Error("two pull requests in one repository must share a lock")
	}
}
