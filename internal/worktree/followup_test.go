package worktree

// Finding 8: a transient inability to run git used to destroy a healthy
// mirror. mirrorUsable mapped every failure of `git config --get
// remote.origin.url` to "not usable", and git0 returned an error both for a
// non-zero exit (the key is genuinely absent -- the intended signal) and for a
// failure to run git at all. Prepare then removed a possibly multi-gigabyte
// cache and failed the re-clone for the very same reason.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/runner"
)

// plantMirror builds a mirror directory that looks healthy enough to matter:
// a HEAD file and a pack file standing in for the object cache.
func plantMirror(t *testing.T, mirror string) (head, pack string) {
	t.Helper()
	pack = filepath.Join(mirror, "objects", "pack", "pack-1.pack")
	if err := os.MkdirAll(filepath.Dir(pack), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pack, []byte("objects"), 0o600); err != nil {
		t.Fatal(err)
	}
	head = filepath.Join(mirror, "HEAD")
	if err := os.WriteFile(head, []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return head, pack
}

func TestPrepareKeepsTheMirrorWhenGitCannotBeRun(t *testing.T) {
	base := t.TempDir()
	// A Fake with no replies errors on every call, which is exactly the shape
	// of "git could not be executed": an AV hold, a transient EPERM, a PATH
	// blip. No exit status is produced at all.
	f := &runner.Fake{}
	m := New(f, "git", filepath.Join(base, "repos"), filepath.Join(base, "work"))
	m.URLFor = func(prref.PRRef) string { return "https://github.com/Example-Org/aex-balances.git" }

	mirror := m.mirrorPath(ref)
	if err := os.MkdirAll(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	head, pack := plantMirror(t, mirror)

	if _, _, err := m.Prepare(context.Background(), ref); err == nil {
		t.Fatal("an inability to run git must be reported, so the ref is deferred and retried later")
	}
	if _, err := os.Stat(head); err != nil {
		t.Errorf("the mirror must survive an inability to run git: %v", err)
	}
	if _, err := os.Stat(pack); err != nil {
		t.Errorf("the object cache must survive an inability to run git: %v", err)
	}
}

// The intended signal still works: git ran, reported the key absent, and the
// half-written mirror is discarded. (The real-git version of this is
// TestPrepareRecoversFromAMirrorWithNoRemote; this one also runs under
// -short.)
func TestPrepareDiscardsTheMirrorWhenGitReportsNoOrigin(t *testing.T) {
	base := t.TempDir()
	f := &runner.Fake{Replies: []runner.Reply{
		// `git config --get` exits 1 when the key is not set.
		{Match: "config --get remote.origin.url", Result: runner.Result{ExitCode: 1}},
		{Match: "clone --bare", Result: runner.Result{}},
		{Match: "config remote.origin.fetch", Result: runner.Result{}},
		{Match: "fetch origin", Result: runner.Result{}},
		{Match: "worktree", Result: runner.Result{}},
	}}
	m := New(f, "git", filepath.Join(base, "repos"), filepath.Join(base, "work"))
	m.URLFor = func(prref.PRRef) string { return "https://github.com/Example-Org/aex-balances.git" }

	mirror := m.mirrorPath(ref)
	if err := os.MkdirAll(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	head, _ := plantMirror(t, mirror)

	if _, _, err := m.Prepare(context.Background(), ref); err != nil {
		t.Fatalf("a mirror with no origin must be re-cloned, not wedge every future review: %v", err)
	}
	if _, err := os.Stat(head); !os.IsNotExist(err) {
		t.Errorf("a mirror git says has no origin must be discarded, stat err = %v", err)
	}
}
