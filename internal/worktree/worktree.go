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

	// mu guards mirrorLocks, which holds one mutex per mirror.
	mu          sync.Mutex
	mirrorLocks map[string]*sync.Mutex
}

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
func (m *Manager) mirrorLock(mirror string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mirrorLocks == nil {
		m.mirrorLocks = map[string]*sync.Mutex{}
	}
	l, ok := m.mirrorLocks[mirror]
	if !ok {
		l = &sync.Mutex{}
		m.mirrorLocks[mirror] = l
	}
	return l
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

	lock := m.mirrorLock(mirror)
	lock.Lock()
	defer lock.Unlock()

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
		// The same mirror lock as Prepare: `worktree remove` rewrites the
		// mirror's worktree registrations, so a review finishing must not run
		// it while a sibling review of the same repository is inside Prepare.
		// It is taken here rather than held from Prepare because cleanup runs
		// after the review, which is the whole half-hour this lock must not
		// span.
		lock := m.mirrorLock(mirror)
		lock.Lock()
		defer lock.Unlock()

		// context.Background: cleanup must still run when the review's deadline
		// has already fired, or the worktree leaks.
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
