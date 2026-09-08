package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/store"
)

// cmdClear marks one pull request's record handled.
//
// The gap it fills: a review that did not finish is recorded needs_attention,
// which is terminal and correct -- a human has to look, because comments may
// be half posted. But once the human has looked, there was no way to say so.
// The record sat in `firstpass status` as an outstanding item forever, and the
// only way to move it was `firstpass replay`, which re-reviews a pull request
// that may well be merged and closed by then.
//
// Marked, not deleted. Deleting the row would take away the dedupe record too,
// so a link to the same pull request posted again in chat -- a colleague
// bumping an old thread -- would review it from scratch. A cleared record is
// terminal, so it stays a decision firstpass has already made; it simply stops
// asking for attention.
func cmdClear(args []string) error {
	fs := flag.NewFlagSet("clear", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "config file")
	note := fs.String("note", "", "why it is cleared; recorded in the detail column")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: firstpass clear [-note text] <pr-url | owner/repo#n>")
	}
	refs := prref.Extract(fs.Arg(0))
	if len(refs) != 1 {
		return fmt.Errorf("could not read exactly one PR reference from %q", fs.Arg(0))
	}
	key := refs[0].Key()

	a, err := openApp(*cfgPath, false, false)
	if err != nil {
		return err
	}
	defer a.Close()

	was, unparked, err := clearRecord(a.store, key, *note, time.Now())
	if err != nil {
		return err
	}
	if unparked {
		fmt.Fprintln(os.Stdout, "removed from the backlog:", key)
	}
	fmt.Fprintf(os.Stdout, "cleared %s (was %s)\n", key, was)
	return nil
}

// clearRecord is the whole of what clear does, split from the command so it
// can be tested against a real store without a config file and an app.
//
// Returns the outcome that was replaced, and whether a backlog entry was
// removed as well.
func clearRecord(st *store.Store, key, note string, now time.Time) (
	was store.Outcome, unparked bool, err error) {

	rec, found, err := st.Review(key)
	if err != nil {
		return "", false, err
	}
	// A typo must not look like success. Without this, `firstpass clear` on a
	// mistyped URL prints nothing wrong and leaves the record it was aimed at
	// exactly where it was.
	if !found {
		return "", false, fmt.Errorf("no record for %s; `firstpass status` lists the keys", key)
	}

	// Only the two outcomes that ask for attention can be cleared. The others
	// are already settled, and clearing one would only lose information: the
	// detail of why a pull request was skipped, or that a review completed and
	// what verdict it submitted.
	if rec.Outcome != store.OutcomeNeedsAttention && rec.Outcome != store.OutcomeInFlight {
		return "", false, fmt.Errorf("%s is recorded %q, which is not asking for attention; "+
			"nothing to clear", key, rec.Outcome)
	}

	was = rec.Outcome
	// The original detail is kept rather than replaced. It is the only account
	// of what went wrong, it exists nowhere else once the log rotates, and the
	// point of clearing is that a human dealt with it -- not that it never
	// happened.
	rec.Outcome = store.OutcomeCleared
	rec.Detail = "cleared by the operator" + noteSuffix(note) + " (was " +
		string(was) + ": " + rec.Detail + ")"
	rec.DecidedAt = now
	if err := st.PutReview(rec); err != nil {
		return "", false, err
	}

	// A cleared pull request must not still be in the backlog: leaving the
	// pending entry would re-offer it on the next sweep and review the very
	// thing just declared handled.
	_, parked, err := st.Pending(key)
	if err != nil {
		return "", false, err
	}
	if parked {
		if err := st.DeletePending(key); err != nil {
			return "", false, err
		}
	}
	return was, parked, nil
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return ": " + note
}
