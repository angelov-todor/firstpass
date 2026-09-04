package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/pipeline"
	"github.com/angelov-todor/firstpass/internal/prref"
)

func cmdReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "config file")
	live := fs.Bool("live", false, "post comments to GitHub even if dry_run is set in config")
	quiet := fs.Bool("quiet", false, "suppress progress output; the result table still prints")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: firstpass replay [-live] [-quiet] <pr-url | owner/repo#n>")
	}

	refs := prref.Extract(fs.Arg(0))
	if len(refs) != 1 {
		return fmt.Errorf("could not read exactly one PR reference from %q", fs.Arg(0))
	}

	a, err := openApp(*cfgPath, *live, true)
	if err != nil {
		return err
	}
	defer a.Close()

	// See cmdScan for why this is stderr-only and why the deferred stop is
	// belt-and-braces rather than the only thing that stops the heartbeat.
	// replay is exactly the command whose 12-minute silence prompted this
	// feature.
	renderer := wireProgress(a.pipe, *quiet, os.Stderr, a.cfg.ReviewTimeout.D())
	defer func() {
		if renderer != nil {
			renderer.stopHeartbeat()
		}
	}()

	d, err := a.pipe.ReviewOne(context.Background(), refs[0], pipeline.Options{})
	if err != nil {
		return err
	}
	// A replay genuinely can post, so printOnly is always false here -- claiming
	// print-only would be a false statement about side effects.
	renderSweep(os.Stdout, pipeline.SweepReport{
		MessagesScanned: 0,
		Decisions:       []pipeline.Decision{d},
		Reviewed:        reviewedCount(d),
	}, false, a.cfg.DryRun)
	return nil
}

// reviewedCount reports whether ReviewOne's single decision counts as a
// review, so the "N reviewed" line renderSweep prints does not contradict the
// decision table above it -- ReviewOne returns only a Decision, not the
// SweepReport it tracked internally, so the count must be derived here.
func reviewedCount(d pipeline.Decision) int {
	if d.Action == pipeline.ActionReview {
		return 1
	}
	return 0
}
