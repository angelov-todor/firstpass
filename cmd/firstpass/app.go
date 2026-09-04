package main

import (
	"log/slog"
	"os"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/pipeline"
	"github.com/angelov-todor/firstpass/internal/review"
	"github.com/angelov-todor/firstpass/internal/runner"
	"github.com/angelov-todor/firstpass/internal/store"
	"github.com/angelov-todor/firstpass/internal/worktree"
)

type app struct {
	cfg   config.Config
	store *store.Store
	log   *slog.Logger
	pipe  *pipeline.Pipeline
}

// newLogger is the one log handler every command uses.
func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// openApp loads the config, opens the store, and wires a pipeline.
//
// withReview controls whether the worktree manager and review runner are
// wired into the pipeline. Callers that only want to audit URL extraction —
// or that must never post — pass false, leaving pipe.WTs and pipe.Rev nil.
//
// The store this returns holds an exclusive bbolt file lock until Close, so
// every caller must release it promptly; cmdWatch opens one app per sweep for
// exactly that reason.
func openApp(configPath string, live bool, withReview bool) (*app, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	if live {
		cfg.DryRun = false
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, err
	}

	log := newLogger()
	r := runner.OS{}

	cc := chat.New(r, cfg.Paths.Python, cfg.Paths.ChatScript, cfg.Space)
	p := &pipeline.Pipeline{
		Cfg:   cfg,
		Store: st,
		Chat:  cc,
		PRs:   ghpr.New(r, cfg.Paths.GH),
		Log:   log,
	}
	if withReview {
		p.WTs = worktree.New(r, cfg.Paths.Git, cfg.ReposDir(), cfg.WorkDir())
		p.Rev = review.New(r, cfg.Paths.Claude, cfg.ClaudeArgs, cfg.DryRun, cfg.ReportsDir())
	}
	// Reactions are outward-facing, so a dry run gets no reactor at all. The
	// pipeline refuses to react in a dry run on its own account too; two
	// independent guards, because dry_run is the switch the operator trusts
	// with "this cannot touch anybody else's chat or pull request".
	if withReview && !cfg.DryRun {
		p.React = cc
	}

	return &app{cfg: cfg, store: st, log: log, pipe: p}, nil
}

func (a *app) Close() error { return a.store.Close() }
