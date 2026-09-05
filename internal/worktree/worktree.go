// Package worktree puts a pull request's head on disk in a throwaway checkout,
// backed by a bare mirror in firstpass's own cache.
//
// It never opens the user's own clones. A background review that ran git in a
// repository the user was working in is how uncommitted work gets lost.
package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/runner"
)

// branchSpec brings the repository's branches down as remote-tracking refs, so
// a base like origin/main resolves inside the worktree.
const branchSpec = "+refs/heads/*:refs/remotes/origin/*"

// nonInteractive returns the git-level options that stop git waiting for a
// human. On this machine a private-repo HTTPS clone can raise a Git
// Credential Manager dialog, and an unattended daemon would sit on that
// dialog until somebody noticed.
//
// These are passed as git -c options rather than through the environment
// because runner.Runner deliberately has no environment parameter; the
// process-wide GIT_TERMINAL_PROMPT=0 that complements them is set once in
// main.
func nonInteractive() []string {
	return []string{"-c", "credential.interactive=false", "-c", "core.askPass="}
}

// Manager owns the mirror cache and the worktree directory.
type Manager struct {
	r        runner.Runner
	git      string
	reposDir string
	workDir  string

	// URLFor overrides the clone URL. Left nil it builds the GitHub HTTPS URL;
	// tests point it at a local fixture repository.
	URLFor func(prref.PRRef) string

	// mu guards mirrorGates, which holds one gate per mirror.
	mu          sync.Mutex
	mirrorGates map[string]chan struct{}
}

// cleanupLockWait bounds how long cleanup waits for the mirror before taking
// the fallback path. The review is over by then, so the only thing waiting
// longer would achieve is holding this worker's slot -- and its review slot,
// since cleanup's defer runs before the slot is released -- while a sibling
// finishes a clone that may take a quarter of an hour.
const cleanupLockWait = 2 * time.Minute

// mirrorLock returns the mutex guarding one mirror, creating it on first use.
//
// Worktrees are per pull request but mirrors are per repository, so two pull
// requests from the same service share one bare repository on disk -- and a
// team that posts several pull requests from one service in a single message
// is the ordinary case, not a corner. Every step of Prepare mutates that
// shared repository: it may `RemoveAll` and re-clone it, it fetches into it,
// and it runs `worktree prune`, which removes worktree registrations another
// review is in the middle of creating. Concurrent Prepares for one repository
// would range from git's "cannot lock ref" failures to one review deleting the
// mirror another is reading.
//
// The lock is per mirror rather than global so reviews of different
// repositories still run in parallel, which is the point of the exercise. It
// is held for the whole of Prepare, including the clone: a first clone of a
// large repository is slow, but a second review of the same repository has
// nothing to do until that clone exists anyway.
//
// What the lock does NOT cover is a review already running out of the mirror.
// Worktrees share the mirror's refs, so a sibling's fetch moves
// refs/remotes/origin/* under a live checkout. Measured in a fixture: a
// two-dot `git diff origin/main` inside a running worktree gained a file the
// pull request never touched. The three-dot form the reviewer actually uses,
// origin/main...HEAD, is immune, because the merge base stays at the branch
// point. Closing the gap properly would mean a clone per pull request, or
// holding the lock for the whole review and thereby serialising exactly the
// same-repository case that is most worth parallelising. It is documented in
// README instead.
//
// A gate rather than a sync.Mutex because the wait has to be interruptible;
// see lockMirror.
func (m *Manager) mirrorGate(mirror string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mirrorGates == nil {
		m.mirrorGates = map[string]chan struct{}{}
	}
	g, ok := m.mirrorGates[mirror]
	if !ok {
		g = make(chan struct{}, 1)
		m.mirrorGates[mirror] = g
	}
	return g
}

// lockMirror takes the mirror's gate, giving up if ctx ends first.
//
// sync.Mutex cannot do that, and the difference is not theoretical. handle
// builds the CloneTimeout deadline before it calls Prepare, so a candidate
// queued behind a cold clone spends its own clone budget waiting; on a
// Mutex it would then keep waiting past its deadline, and only fail once it
// got the lock and ran a git command against a context that had already
// expired. Worse, a Ctrl-C would not free it: a worker parked on Lock() is
// unreachable, so shutdown would wait out the clone.
//
// Giving up returns ctx.Err(), which handle defers and retries on a later
// sweep -- and by then the clone this candidate was waiting for has finished,
// so the retry is fast.
func (m *Manager) lockMirror(ctx context.Context, mirror string) error {
	gate := m.mirrorGate(mirror)
	select {
	case gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) unlockMirror(mirror string) {
	<-m.mirrorGate(mirror)
}

func New(r runner.Runner, git, reposDir, workDir string) *Manager {
	return &Manager{r: r, git: git, reposDir: reposDir, workDir: workDir}
}

func (m *Manager) url(ref prref.PRRef) string {
	if m.URLFor != nil {
		return m.URLFor(ref)
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", ref.Owner, ref.Repo)
}

func (m *Manager) mirrorPath(ref prref.PRRef) string {
	return filepath.Join(m.reposDir, fmt.Sprintf("%s_%s.git", ref.Owner, ref.Repo))
}

func (m *Manager) workPath(ref prref.PRRef) string {
	return filepath.Join(m.workDir, fmt.Sprintf("%s_%s_%d", ref.Owner, ref.Repo, ref.Number))
}

// Prepare returns a directory holding the PR head, plus a cleanup func the
// caller must always invoke.
func (m *Manager) Prepare(ctx context.Context, ref prref.PRRef) (string, func(), error) {
	noop := func() {}
	mirror := m.mirrorPath(ref)
	work := m.workPath(ref)

	if err := m.lockMirror(ctx, mirror); err != nil {
		return "", noop, fmt.Errorf("waiting for the mirror of %s: %w", ref.Key(), err)
	}
	defer m.unlockMirror(mirror)

	for _, d := range []string{m.reposDir, m.workDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", noop, err
		}
	}

	usable, err := m.mirrorUsable(ctx, mirror)
	if err != nil {
		return "", noop, err
	}
	if !usable {
		// A clone killed between `git init` and configuring origin leaves a
		// directory with a HEAD file and no remote. It passes an existence
		// check but every later `fetch origin` in it fails, so the mirror
		// would be wedged for good; start over instead.
		if err := os.RemoveAll(mirror); err != nil {
			return "", noop, err
		}
		if err := m.git0(ctx, "clone", "--bare", m.url(ref), mirror); err != nil {
			return "", noop, fmt.Errorf("clone mirror for %s: %w", ref.Key(), err)
		}
		// A bare clone configures no fetch refspec at all, so the mirror
		// cannot say what it tracks. Record it, so a plain `git fetch` in the
		// mirror -- by firstpass or by hand -- brings the branches down.
		if err := m.git0(ctx, "-C", mirror, "config", "remote.origin.fetch", branchSpec); err != nil {
			return "", noop, fmt.Errorf("configure mirror for %s: %w", ref.Key(), err)
		}
	}

	prRef := "refs/firstpass/" + strconv.Itoa(ref.Number)
	spec := fmt.Sprintf("+refs/pull/%d/head:%s", ref.Number, prRef)
	// The branch refspec is fetched alongside the pull ref so the worktree has
	// something to diff against. Without it the checkout is a detached head
	// with no remote-tracking refs whatsoever: `git rev-parse origin/main`
	// fails inside it and a review sees an empty diff.
	if err := m.git0(ctx, "-C", mirror, "fetch", "origin", spec, branchSpec); err != nil {
		return "", noop, fmt.Errorf("fetch head of %s: %w", ref.Key(), err)
	}

	// A worktree left registered by a killed run would make `worktree add`
	// fail, wedging every future review of this PR. Clear it first; both of
	// these are expected to fail on the happy path.
	_ = m.git0(ctx, "-C", mirror, "worktree", "remove", "--force", work)
	if err := os.RemoveAll(work); err != nil {
		return "", noop, err
	}
	_ = m.git0(ctx, "-C", mirror, "worktree", "prune")

	if err := m.git0(ctx, "-C", mirror, "worktree", "add", "--detach", work, prRef); err != nil {
		return "", noop, fmt.Errorf("worktree add for %s: %w", ref.Key(), err)
	}

	cleanup := func() {
		// The same mirror gate as Prepare: `worktree remove` rewrites the
		// mirror's worktree registrations, so a review finishing must not run
		// it while a sibling review of the same repository is inside Prepare.
		// Taken here rather than held from Prepare because cleanup runs after
		// the review, which is the half-hour this lock must not span.
		//
		// context.Background for the git call: cleanup must still run when the
		// review's own deadline has already fired, or the worktree leaks.
		lockCtx, cancel := context.WithTimeout(context.Background(), cleanupLockWait)
		defer cancel()
		if err := m.lockMirror(lockCtx, mirror); err != nil {
			// A sibling is holding the mirror -- cloning, most likely. Remove
			// the checkout anyway, because leaving it behind is what wedges
			// the next review of this pull request, and let the next Prepare's
			// `worktree prune` drop the stale registration. That recovery path
			// already exists and is tested; see
			// TestPrepareRecoversFromALeftoverWorktree.
			_ = os.RemoveAll(work)
			return
		}
		defer m.unlockMirror(mirror)

		_ = m.git0(context.Background(), "-C", mirror, "worktree", "remove", "--force", work)
		_ = os.RemoveAll(work)
	}
	return work, cleanup, nil
}

// mirrorUsable reports whether the mirror on disk can be fetched into. Both a
// HEAD file and a configured origin are required: either alone is a partial
// clone left by a killed run.
//
// "git ran and reported the key is absent" and "git could not be run at all"
// must not be conflated. Only the first says anything about the mirror; the
// second is an AV hold, a transient EPERM or a PATH blip, and treating it as
// "not usable" made Prepare delete a possibly multi-gigabyte cache and then
// fail the re-clone for the very same reason. An error here defers the ref for
// a later sweep instead, which costs nothing but a retry.
func (m *Manager) mirrorUsable(ctx context.Context, mirror string) (bool, error) {
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	code, _, err := m.gitExit(ctx, "-C", mirror, "config", "--get", "remote.origin.url")
	if err != nil {
		return false, fmt.Errorf("check mirror %s: %w", mirror, err)
	}
	// `git config --get` exits 1 when the key is not set, and non-zero for
	// every other way of not having a single usable origin (not a repository,
	// unreadable config). All of those are the partial-clone signal.
	return code == 0, nil
}

// gitExit runs git and reports its exit code. The error is reserved for git
// not having run at all, which is why mirrorUsable can tell the two apart.
func (m *Manager) gitExit(ctx context.Context, args ...string) (int, []byte, error) {
	res, err := m.r.Run(ctx, "", m.git, append(nonInteractive(), args...)...)
	if err != nil {
		return 0, res.Stderr, err
	}
	return res.ExitCode, res.Stderr, nil
}

// git0 runs git and treats any non-zero exit as a failure.
func (m *Manager) git0(ctx context.Context, args ...string) error {
	code, stderr, err := m.gitExit(ctx, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("git %s exit %d: %s",
			strings.Join(args, " "), code, strings.TrimSpace(string(stderr)))
	}
	return nil
}
