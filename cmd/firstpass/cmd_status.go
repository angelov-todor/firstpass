package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/store"
)

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := openApp(*cfgPath, false, false)
	if err != nil {
		return err
	}
	defer a.Close()

	reviews, err := a.store.Reviews()
	if err != nil {
		return err
	}
	pending, err := a.store.AllPending()
	if err != nil {
		return err
	}
	wm, hasWM, err := a.store.Watermark()
	if err != nil {
		return err
	}

	_, statErr := os.Stat(a.cfg.PauseFile())
	renderStatus(os.Stdout, reviews, pending, wm, hasWM, statErr == nil, a.cfg.DryRun)
	return nil
}

// outcomeCell is the OUTCOME column: the outcome, and for a review the
// verdict firstpass submitted with it.
//
// A bare "reviewed" is what the operator was reading for a clean pull request
// that said nothing at all, so every verdict state gets its own string. In
// particular "reviewed / approved" (submitted) must not be confusable with
// "reviewed / no verdict" (a dry run, or a submission that failed -- Detail
// says which) or "reviewed / verdict unknown" (the reviewer never said).
//
// A review that was not the first pass over its pull request says so as well.
// Comments arriving twice on one pull request is the thing the operator will
// want explained, and this is the answer. Only a later pass is annotated:
// almost every row is a first pass, and saying "pass 1" on all of them would
// bury the one row that matters. A pre-existing row carries no pass number at
// all and reads as the first pass it was, never as "pass 0" -- see
// store.Review.PassNumber.
func outcomeCell(r store.Review) string {
	cell := verdictCell(r)
	if r.Outcome == store.OutcomeReviewed && r.PassNumber() > 1 {
		// Only a review has a pass: a skipped or expired row was never
		// reviewed at all.
		cell += fmt.Sprintf(" (pass %d)", r.PassNumber())
	}
	return cell
}

func verdictCell(r store.Review) string {
	switch {
	case r.Verdict == store.VerdictApproved:
		return string(r.Outcome) + " / approved"
	case r.Verdict == store.VerdictFindings:
		return string(r.Outcome) + " / findings"
	case r.Verdict == store.VerdictUnknown:
		return string(r.Outcome) + " / verdict unknown"
	case r.Outcome == store.OutcomeReviewed:
		// Only reviews carry a verdict, so only a review's absence of one is
		// worth a word. A skipped or deferred row is left exactly as it was.
		return string(r.Outcome) + " / no verdict"
	default:
		return string(r.Outcome)
	}
}

func renderStatus(w io.Writer, reviews []store.Review, pending []store.Pending,
	wm store.Watermark, hasWM, paused, dryRun bool) {

	mode := "live — comments are posted to GitHub"
	if dryRun {
		mode = "dry run — nothing is posted"
	}
	fmt.Fprintf(w, "mode: %s\n", mode)
	if paused {
		fmt.Fprintln(w, "state: PAUSED — nothing will be reviewed or posted until `firstpass resume`")
	}
	if hasWM {
		fmt.Fprintf(w, "watermark: %s (%s)\n", wm.MessageName, wm.CreateTime.Format(time.RFC3339))
	} else {
		fmt.Fprintln(w, "no watermark yet — the next run is a cold start and will review nothing")
	}

	sort.Slice(reviews, func(i, j int) bool { return reviews[i].DecidedAt.After(reviews[j].DecidedAt) })

	fmt.Fprintf(w, "\nreviews (%d)\n", len(reviews))
	if len(reviews) > 0 {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "PR\tOUTCOME\tDECIDED\tTOOK\tEXIT\tDETAIL")
		actionable, inFlight := 0, 0
		for _, r := range reviews {
			decided := "-"
			if !r.DecidedAt.IsZero() {
				decided = r.DecidedAt.Format("2006-01-02 15:04")
			}
			took := "-"
			if r.DurationMS > 0 {
				took = (time.Duration(r.DurationMS) * time.Millisecond).Round(time.Second).String()
			}
			// A killed review carries no exit status at all, and rendering the
			// persisted sentinel as a number would read as an ordinary
			// failure. A clean run's zero earns no entry.
			exit := "-"
			switch {
			case r.ExitCode < 0:
				exit = "killed"
			case r.ExitCode > 0:
				exit = strconv.Itoa(r.ExitCode)
			}
			// in_flight counts as actionable: it is an orphan from a run that
			// died, so comments may already be on the PR. Left out, the one
			// outcome meaning "a run vanished mid-post" would be the one
			// outcome the summary stayed silent about.
			if r.Outcome == store.OutcomeNeedsAttention || r.Outcome == store.OutcomeInFlight {
				actionable++
			}
			if r.Outcome == store.OutcomeInFlight {
				inFlight++
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Key, outcomeCell(r), decided, took, exit, r.Detail)
		}
		tw.Flush()
		if actionable > 0 {
			fmt.Fprintf(w, "\n%d need attention. Each may already carry partial comments; "+
				"run `firstpass replay <pr-url>` to review one again deliberately.\n", actionable)
		}
		if inFlight > 0 {
			fmt.Fprintf(w, "%d still marked in_flight: a run died mid-review and never recorded an "+
				"outcome. The next sweep converts these to needs_attention.\n", inFlight)
		}
	}

	fmt.Fprintf(w, "\npending (%d)\n", len(pending))
	if len(pending) > 0 {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "PR\tATTEMPTS\tLAST REASON")
		for _, p := range pending {
			fmt.Fprintf(tw, "%s\t%d\t%s\n", p.Key, p.Attempts, p.LastReason)
		}
		tw.Flush()
	}
}
