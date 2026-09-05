package runner

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

// shell returns an invocation of the platform's shell running script.
//
// These three tests used to hard-code cmd.exe and skip everywhere else. That
// was harmless while the suite only ever ran on Windows, and stopped being
// harmless the moment the race detector moved into a Linux container: the
// tests that skipped there included TestOSRunSurfacesContextDeadline, which
// pins the runner's cancellation contract -- the single behaviour that matters
// most once several reviews can be in flight at once. A net with a hole
// exactly where the risk is concentrated is worse than no net, because it
// reads as coverage.
func shell(script string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", script}
	}
	return "sh", []string{"-c", script}
}

func TestOSRunCapturesStdout(t *testing.T) {
	name, args := shell("echo hello")
	res, err := OS{}.Run(context.Background(), "", name, args...)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(string(res.Stdout), "hello") {
		t.Errorf("Stdout = %q", res.Stdout)
	}
}

func TestOSRunReportsNonZeroExitAsDataNotError(t *testing.T) {
	name, args := shell("exit 3")
	res, err := OS{}.Run(context.Background(), "", name, args...)
	if err != nil {
		t.Fatalf("a non-zero exit must not be a Go error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
}

func TestOSRunSurfacesContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	name, args := shell(map[bool]string{
		true:  "ping -n 10 127.0.0.1 > NUL",
		false: "sleep 10",
	}[runtime.GOOS == "windows"])
	_, err := OS{}.Run(ctx, "", name, args...)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a timeout must surface as context.DeadlineExceeded, got %v; "+
			"otherwise a killed review looks like a clean run", err)
	}
}

// TestOSRunAbandonsAKilledCommandPromptly is the assertion the deadline test
// above should always have carried.
//
// TestOSRunSurfacesContextDeadline checks that a timeout is reported as
// context.DeadlineExceeded, and it passed while Run took the full ten seconds
// of the command it was supposed to have abandoned. The error was right and
// the behaviour was wrong, so the test read as coverage of cancellation while
// proving only that the error value was chosen correctly.
//
// The cause is that Run gives cmd.Stdout a bytes.Buffer, so os/exec wires an
// OS pipe and a copying goroutine, and Wait blocks until the write end is
// closed by everyone holding it. exec.CommandContext kills the direct child
// only; anything that child spawned keeps the pipe open. `claude` spawns tool
// subprocesses constantly.
//
// What that costs firstpass: Ctrl-C during a review does not stop it, the
// operator waits on whatever the reviewer happens to be running; review_timeout
// stops meaning what it says; and once reviews run concurrently, shutdown waits
// on the slowest of them rather than on the deadline.
func TestOSRunAbandonsAKilledCommandPromptly(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real sleeping process")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Thirty seconds so the two outcomes are unmistakable: without a bound on
	// the post-kill wait this takes ~30s, with one it takes about waitDelay.
	name, args := shell(map[bool]string{
		true:  "ping -n 30 127.0.0.1 > NUL",
		false: "sleep 30",
	}[runtime.GOOS == "windows"])

	start := time.Now()
	_, err := OS{}.Run(ctx, "", name, args...)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Run took %s to return from a command cancelled after 50ms; "+
			"a cancelled review must actually stop, not run to completion", elapsed)
	}
}

func TestOSRunReportsMissingBinary(t *testing.T) {
	if _, err := (OS{}).Run(context.Background(), "", "firstpass-no-such-binary"); err == nil {
		t.Error("a missing executable must be an error")
	}
}

func TestFakeMatchesBySubstring(t *testing.T) {
	f := &Fake{Replies: []Reply{
		{Match: "get-messages", Result: Result{Stdout: []byte("{}")}},
	}}

	res, err := f.Run(context.Background(), "", "python", "chat.py", "get-messages", "spaces/A")
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "{}" {
		t.Errorf("Stdout = %q", res.Stdout)
	}
	if len(f.Calls) != 1 || f.Calls[0].Name != "python" {
		t.Errorf("Calls = %+v", f.Calls)
	}
}

func TestFakeErrorsOnUnconfiguredCommand(t *testing.T) {
	f := &Fake{}
	if _, err := f.Run(context.Background(), "", "gh", "pr", "view"); err == nil {
		t.Error("an unconfigured command must fail loudly, not return an empty result")
	}
}
