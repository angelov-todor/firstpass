package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/pipeline"
)

func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "config file")
	live := fs.Bool("live", false, "post comments to GitHub even if dry_run is set in config")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Loaded once so a bad config fails immediately rather than on the first
	// tick, and so the startup line can name the interval. The store is not
	// opened here: watchSweep opens and closes one per sweep.
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *live {
		cfg.DryRun = false
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	log := newLogger()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	log.Info("watching",
		"space", cfg.Space, "interval", cfg.Interval.D(), "dry_run", cfg.DryRun)

	ticker := time.NewTicker(cfg.Interval.D())
	defer ticker.Stop()

	for {
		err := watchSweep(ctx, *cfgPath, *live, log)
		switch {
		case err == nil:
		case errors.Is(err, context.Canceled):
			log.Info("stopping")
			return nil
		default:
			// A missing scope or a revoked grant cannot be fixed by waiting, so
			// stop rather than log the same failure every interval forever.
			var apiErr *chat.APIError
			if errors.As(err, &apiErr) && apiErr.Fatal() {
				fmt.Fprint(os.Stderr, fatalChatBanner(apiErr))
				return fmt.Errorf("unrecoverable Google Chat error: %w", err)
			}
			log.Error("sweep failed", "err", err)
		}

		select {
		case <-ctx.Done():
			log.Info("stopping")
			return nil
		case <-ticker.C:
		}
	}
}

// watchSweep runs exactly one sweep, with the store open only for its
// duration.
//
// bbolt takes an exclusive lock on the database file for as long as it is
// open. A daemon that held the store across its interval would make `firstpass
// status`, `firstpass scan` and `firstpass replay` fail with a five-second lock
// timeout for its entire lifetime — the operator's only window into the
// daemon, unavailable exactly when they need it. The interval is minutes, so
// releasing the lock between ticks buys real observability for nothing.
//
// Every exit path closes the store: the deferred Close covers the sweep
// failing, the config failing to reload, and an interrupt arriving mid-sweep.
func watchSweep(ctx context.Context, cfgPath string, live bool, log *slog.Logger) error {
	a, err := openApp(cfgPath, live, true)
	if err != nil {
		return err
	}
	defer a.Close()

	// watch has no plain-text progress renderer and no heartbeat goroutine:
	// its output is a structured log an operator is already watching, not a
	// terminal or a redirected result table, so the same events are routed
	// through slog at INFO instead -- see slogProgressHandler.
	a.pipe.Progress = slogProgressHandler(a.log)

	rep, err := a.pipe.Sweep(ctx, pipeline.Options{})
	if err != nil {
		return err
	}

	log.Info("sweep done",
		"messages", rep.MessagesScanned, "reviewed", rep.Reviewed,
		"decisions", len(rep.Decisions), "paused", rep.Paused)
	if rep.WatermarkGap {
		log.Warn("fetch window too small: messages were not scanned and the watermark was held; " +
			"raise fetch_limit in the config, or run `firstpass scan -backfill N` to cover the gap")
	}
	for _, d := range rep.Decisions {
		if d.Action == pipeline.ActionNeedsAttention {
			log.Warn("needs attention", "pr", d.Ref.URL(), "reason", d.Reason)
		}
	}
	return nil
}
