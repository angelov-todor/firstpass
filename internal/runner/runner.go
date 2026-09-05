// Package runner is the single seam between firstpass and the external programs
// it drives, so tests can run the decision logic without subprocesses.
package runner

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// Result is the outcome of one command.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner executes an external command in dir (empty means the current
// directory).
//
// A non-zero exit status is returned as Result.ExitCode with a nil error:
// callers decide whether it matters. A cancelled or timed-out context is the
// exception and is always returned as an error, so a review killed by its
// deadline cannot be mistaken for a clean run.
type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) (Result, error)
}

// OS runs commands for real.
type OS struct{}

// waitDelay bounds how long Run keeps waiting once the context is done.
//
// Without it a cancelled command is killed but Run does not return: giving
// cmd.Stdout a bytes.Buffer makes os/exec wire an OS pipe, and Wait blocks
// until the write end is closed by every process holding it. CommandContext
// kills the direct child only, so anything that child spawned -- and `claude`
// spawns tool subprocesses constantly -- keeps Run blocked until it finishes
// on its own. Measured: a command cancelled after 50ms returned after 29s.
//
// Five seconds is long enough for a process that is genuinely exiting to flush
// its output, and short enough that Ctrl-C feels like Ctrl-C.
const waitDelay = 5 * time.Second

func (OS) Run(ctx context.Context, dir, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.WaitDelay = waitDelay

	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb

	err := cmd.Run()
	res := Result{Stdout: out.Bytes(), Stderr: errb.Bytes()}

	// Check the context first: killing the process produces an ExitError too,
	// and treating that as a plain non-zero exit would hide the timeout.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, ctxErr
	}

	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	if err != nil {
		return res, err
	}
	return res, nil
}
