package runner

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOSRunCapturesStdout(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("uses cmd.exe")
	}
	res, err := OS{}.Run(context.Background(), "", "cmd", "/c", "echo hello")
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
	if runtime.GOOS != "windows" {
		t.Skip("uses cmd.exe")
	}
	res, err := OS{}.Run(context.Background(), "", "cmd", "/c", "exit 3")
	if err != nil {
		t.Fatalf("a non-zero exit must not be a Go error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
}

func TestOSRunSurfacesContextDeadline(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("uses cmd.exe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := OS{}.Run(ctx, "", "cmd", "/c", "ping -n 10 127.0.0.1 > NUL")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a timeout must surface as context.DeadlineExceeded, got %v; "+
			"otherwise a killed review looks like a clean run", err)
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
