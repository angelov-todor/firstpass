// Package runner is the single seam between firstpass and the external programs
// it drives, so tests can run the decision logic without subprocesses.
package runner

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
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

func (OS) Run(ctx context.Context, dir, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir

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
