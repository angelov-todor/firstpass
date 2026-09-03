package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/pipeline"
)

func cmdScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "config file")
	printOnly := fs.Bool("print-only", false, "decide and print, changing no state")
	live := fs.Bool("live", false, "post comments to GitHub even if dry_run is set in config")
	backfill := fs.Int("backfill", 0, "take the last N messages, ignoring the watermark")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateBackfill(*backfill); err != nil {
		return err
	}

	a, err := openApp(*cfgPath, *live, !*printOnly)
	if err != nil {
		return err
	}
	defer a.Close()

	rep, err := a.pipe.Sweep(context.Background(), pipeline.Options{
		PrintOnly: *printOnly,
		Backfill:  *backfill,
	})
	if err != nil {
		// scan is the Task Scheduler entry point, so its stderr is a log file
		// nobody reads. A fatal chat error means no sweep happened at all, and
		// the likeliest cause looks exactly like a quiet week — say so loudly
		// enough that a glance at the log cannot miss it.
		var apiErr *chat.APIError
		if errors.As(err, &apiErr) && apiErr.Fatal() {
			fmt.Fprint(os.Stderr, fatalChatBanner(apiErr))
			return fmt.Errorf("unrecoverable Google Chat error: %w", err)
		}
		return err
	}
	renderSweep(os.Stdout, rep, *printOnly, a.cfg.DryRun)
	return nil
}

// validateBackfill rejects a negative -backfill.
//
// A negative window is meaningless, and it used to be actively dangerous: the
// pipeline's cold-start guard tested `Backfill == 0` while its window
// selection tested `Backfill > 0`, so `scan -live -backfill -1` on a fresh
// install fell between the two, skipped launch day's protection, posted on the
// whole fetch_limit window and advanced the watermark. The pipeline's guard
// now tests `<= 0` as well; this rejects the flag outright, so the operator is
// told rather than silently given a plain sweep.
func validateBackfill(n int) error {
	if n < 0 {
		return fmt.Errorf("-backfill must not be negative, got %d: pass a positive count of "+
			"messages to re-scan, or omit the flag to sweep from the watermark", n)
	}
	return nil
}

// fatalChatBanner is the loud, multi-line stderr block printed when Google
// Chat rejects the request in a way waiting cannot fix.
//
// A one-line error is not enough here. Two Google accounts exist on this
// machine and the personal one can see none of the team's spaces, so being
// authenticated as the wrong one produces PERMISSION_DENIED on get-messages —
// and an operator skimming a scheduled task's log needs to know instantly
// that nothing was swept, rather than reading "0 messages" as a quiet week.
func fatalChatBanner(err error) string {
	const rule = "****************************************************************"
	var b strings.Builder
	b.WriteString("\n" + rule + "\n")
	b.WriteString("firstpass scan REFUSED TO SWEEP - Google Chat rejected the request\n")
	b.WriteString(rule + "\n")
	b.WriteString("  " + err.Error() + "\n\n")
	b.WriteString("  Nothing was scanned, nothing was reviewed, and the watermark\n")
	b.WriteString("  did not move. This is NOT a quiet week.\n\n")
	b.WriteString("  Most likely cause: firstpass is authenticated as the wrong\n")
	b.WriteString("  Google account. Two accounts exist on this machine and the\n")
	b.WriteString("  personal one can see none of the team's spaces, which looks\n")
	b.WriteString("  exactly like \"nobody posted a PR\".\n\n")
	b.WriteString("  Remedy: run `firstpass doctor`. If the Google Chat account\n")
	b.WriteString("  check fails, re-run `python auth.py login` as the work\n")
	b.WriteString("  account, then `firstpass doctor` again.\n")
	b.WriteString(rule + "\n\n")
	return b.String()
}

// renderSweep prints a sweep so a human can audit the decisions.
//
// The mode line must never claim comments were posted when they were not.
// printOnly takes priority over dryRun: cmdScan passes whatever -print-only
// was set to, so a run can have dry_run: false in its config (mid-
// configuration, or simply misconfigured) while -print-only is also set,
// writing no state and posting nothing — that combination must say
// "print-only", never "live". Print-only does still query GitHub, to know
// each PR's state and reason, so its wording must not claim to touch
// nothing at all.
func renderSweep(w io.Writer, rep pipeline.SweepReport, printOnly, dryRun bool) {
	mode := "live — comments are posted to GitHub"
	switch {
	case printOnly:
		mode = "print-only — nothing written or posted, though GitHub was queried to decide"
	case dryRun:
		mode = "dry run — nothing is posted"
	}
	fmt.Fprintf(w, "%d messages scanned (%s)\n", rep.MessagesScanned, mode)

	if rep.WatermarkGap {
		fmt.Fprintln(w, "WARNING: the fetch window was too small to reach the watermark, so the messages")
		fmt.Fprintln(w, "between it and the oldest message fetched were never scanned. The watermark was")
		fmt.Fprintln(w, "not advanced, so the next sweep re-scans this window — but the older messages")
		fmt.Fprintln(w, "stay out of reach. Raise fetch_limit in the config, and run with -backfill N to")
		fmt.Fprintln(w, "cover the gap now.")
	}
	if rep.Paused {
		fmt.Fprintln(w, "sweep is paused: refs were queued, nothing was reviewed or posted")
	}
	if rep.ColdStart {
		fmt.Fprintln(w, "cold start: the watermark was set to the newest message and nothing was reviewed.")
		fmt.Fprintln(w, "to review history on purpose, run with --backfill N")
		return
	}
	if len(rep.Decisions) == 0 {
		fmt.Fprintln(w, "no PR references found")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PR\tACTION\tREASON")
	for _, d := range rep.Decisions {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", d.Ref.Key(), d.Action, d.Reason)
	}
	tw.Flush()
	fmt.Fprintf(w, "\n%d reviewed\n", rep.Reviewed)
}
