package main

// Findings 3 and 6: the CLI accepted a negative -backfill, and `doctor` shared
// one deadline across every check.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- Finding 3: -backfill must not be negative ----

func TestValidateBackfillRejectsNegativeOnly(t *testing.T) {
	for _, n := range []int{-1, -200} {
		if err := validateBackfill(n); err == nil {
			t.Errorf("validateBackfill(%d) = nil; a negative window is meaningless and used to "+
				"bypass the cold-start guard entirely", n)
		}
	}
	for _, n := range []int{0, 1, 200} {
		if err := validateBackfill(n); err != nil {
			t.Errorf("validateBackfill(%d) = %v, want nil", n, err)
		}
	}
}

// The flag must be rejected before anything is opened or swept. The config
// this uses names an executable that does not exist, so even an unfixed build
// cannot reach a real subprocess from here.
func TestScanRejectsANegativeBackfillBeforeSweeping(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := testConfigBody(dir) +
		"  python: " + filepath.ToSlash(filepath.Join(dir, "no-such-python.exe")) + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	err := cmdScan([]string{"-config", cfgPath, "-backfill", "-1"})
	if err == nil {
		t.Fatal("a negative -backfill must be an error")
	}
	if !strings.Contains(err.Error(), "backfill") {
		t.Errorf("the error must name the flag rather than failing somewhere downstream, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "state.db")); serr == nil {
		t.Error("the flag must be rejected before the store is opened")
	}
}

// ---- Finding 6: one slow check must not starve the others ----

// doctor used to build a single 60-second context and share it across every
// check, so a slow `gh auth status` could consume it and the Google Chat check
// -- the one fatalChatBanner explicitly sends the operator to run -- failed on
// a timeout rather than on its own merits.
func TestEachDoctorCheckGetsItsOwnDeadline(t *testing.T) {
	const budget = 50 * time.Millisecond
	parent := context.Background()

	err := withCheckTimeout(parent, budget, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a check that hangs must hit its own deadline, got %v", err)
	}

	var left time.Duration
	if err := withCheckTimeout(parent, budget, func(ctx context.Context) error {
		dl, ok := ctx.Deadline()
		if !ok {
			return errors.New("no deadline")
		}
		left = time.Until(dl)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if left <= budget/2 {
		t.Errorf("the next check had %s left of a %s budget: a slow check must not eat the "+
			"deadline of the ones after it", left, budget)
	}
}

// The command as a whole still has a bound: a parent deadline shorter than the
// per-check budget wins.
func TestDoctorCheckHonoursTheOverallDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := withCheckTimeout(parent, time.Hour, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the overall bound must still apply, got %v", err)
	}
}
