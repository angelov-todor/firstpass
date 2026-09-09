package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/runner"
	"github.com/angelov-todor/firstpass/internal/store"
)

// cmdCatchup moves the chat watermark to the newest message in the space
// without reviewing anything in between.
//
// The gap it fills: firstpass off for a day comes back to a day of messages
// and reviews all of them. Usually that is exactly right -- it is why the
// watermark exists -- but sometimes the operator has already dealt with that
// window by hand, or simply does not want it, and the only way to say so was
// to let the sweep run and then live with the comments it posted.
//
// It reviews nothing and posts nothing. What it does is destructive in one
// specific way: the pull requests in the skipped window will not be reviewed
// unless somebody posts them again, so it prints what it is skipping.
func cmdCatchup(args []string) error {
	fs := flag.NewFlagSet("catchup", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "config file")
	printOnly := fs.Bool("print-only", false,
		"show what would be skipped and leave the watermark where it is")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: firstpass catchup [-print-only]")
	}

	a, err := openApp(*cfgPath, false, false)
	if err != nil {
		return err
	}
	defer a.Close()

	wm, hasWM, err := a.store.Watermark()
	if err != nil {
		return err
	}
	since := wm.MessageName
	if !hasWM {
		// No watermark is the cold start, which already reviews nothing. The
		// useful thing to do is still to set one, so the first real sweep
		// starts from now rather than from whatever the fetch window holds.
		since = ""
	}

	cc := chat.New(runner.OS{}, a.cfg.Paths.Python, a.cfg.Paths.ChatScript, a.cfg.Space)
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.ChatTimeout.D())
	defer cancel()
	msgs, foundSince, err := cc.Fetch(ctx, since, a.cfg.FetchLimit)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		fmt.Println("nothing to skip: no messages in the window")
		return nil
	}

	// Reported rather than assumed away: when the watermark has fallen out of
	// the window, the fetch returned everything it had, so the count below is
	// the window and not the gap. The operator is about to skip whatever is in
	// it either way, and should know which of the two they are looking at.
	if hasWM && !foundSince {
		fmt.Println("note: the watermark is older than the fetch window, so this is the " +
			"whole window rather than only the gap")
	}

	refs, decided := skippedRefs(a.store, msgs)
	newest := msgs[0]

	fmt.Printf("%d messages in the window, carrying %d pull requests (%d already decided)\n",
		len(msgs), len(refs), decided)
	for _, ref := range refs {
		fmt.Println("  ", ref.URL())
	}
	fmt.Printf("newest message: %s (%s)\n", newest.Name, newest.CreateTime.Format("2006-01-02 15:04"))

	if *printOnly {
		fmt.Println("print-only: the watermark has not moved")
		return nil
	}
	if err := a.store.SetWatermark(store.Watermark{
		MessageName: newest.Name, CreateTime: newest.CreateTime,
	}); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "watermark moved; the next sweep starts after that message")
	// Said plainly, because it is the consequence people forget: nothing here
	// is queued for later. A pull request in the skipped window is reviewed
	// only if somebody posts it again, or with `firstpass replay`.
	fmt.Fprintln(os.Stdout, "the pull requests above will not be reviewed unless re-posted "+
		"or replayed")
	return nil
}

// skippedRefs lists the distinct pull requests in the messages about to be
// skipped, and how many of them firstpass had already decided about.
//
// The already-decided count matters to the operator's judgement: a window of
// twenty messages whose pull requests are all reviewed is nothing to think
// about, and one carrying three untouched pull requests is.
func skippedRefs(st *store.Store, msgs []chat.Message) ([]prref.PRRef, int) {
	var refs []prref.PRRef
	seen := map[string]bool{}
	decided := 0
	for _, m := range msgs {
		for _, ref := range prref.Extract(m.Text) {
			if seen[ref.Key()] {
				continue
			}
			seen[ref.Key()] = true
			refs = append(refs, ref)
			if rec, found, err := st.Review(ref.Key()); err == nil && found &&
				rec.Outcome.Terminal() {
				decided++
			}
		}
	}
	return refs, decided
}
