package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/runner"
)

type check struct {
	name   string
	ok     bool
	detail string
}

const (
	// doctorCheckTimeout bounds one external dependency, not the whole
	// command, so a slow one cannot mask the others.
	//
	// 30s, not 20s: `claude --version` and `gh auth status` are node and Go
	// binaries that hit the network on a cold Windows box, behind a virus
	// scanner that sees each of them for the first time. Slow-but-working is
	// the common case there, and reporting a false failure from doctor -- the
	// one command whose whole job is to say whether the dependencies are
	// healthy -- is worse than waiting another ten seconds for the truth.
	doctorCheckTimeout = 30 * time.Second
	// doctorOverallTimeout keeps the command itself bounded: four sequential
	// checks at doctorCheckTimeout each (4 x 30s) fit inside it with room to
	// spare, and nothing can run longer than this in total.
	doctorOverallTimeout = 2 * time.Minute
)

// withCheckTimeout runs one doctor check under its own deadline, derived from
// the command's overall deadline so both bounds apply and the shorter wins.
func withCheckTimeout(parent context.Context, d time.Duration, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()
	return fn(ctx)
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// One deadline per check, not one for the command. Shared, a slow `gh auth
	// status` could consume the whole budget and the Google Chat check — the
	// one fatalChatBanner explicitly sends the operator to run — would fail on
	// a timeout rather than on its own merits, sending them to
	// re-authenticate the wrong thing entirely.
	overall, cancelAll := context.WithTimeout(context.Background(), doctorOverallTimeout)
	defer cancelAll()

	var checks []check
	add := func(name string, err error, detail string) {
		if err != nil {
			checks = append(checks, check{name: name, ok: false, detail: err.Error()})
			return
		}
		checks = append(checks, check{name: name, ok: true, detail: detail})
	}

	cfg, err := config.Load(*cfgPath)
	add("config loads", err, *cfgPath)
	if err == nil {
		add("config valid", explainValidate(cfg, *cfgPath), fmt.Sprintf("dry_run=%v allow_owners=%v", cfg.DryRun, cfg.AllowOwners))
		add("state dir writable", writable(cfg.StateDir), cfg.StateDir)
		add("chat.py present", exists(cfg.Paths.ChatScript), cfg.Paths.ChatScript)

		r := runner.OS{}
		bounded := func(fn func(context.Context) error) error {
			return withCheckTimeout(overall, doctorCheckTimeout, fn)
		}
		add("git works", bounded(func(ctx context.Context) error {
			return version(ctx, r, cfg.Paths.Git, "--version")
		}), cfg.Paths.Git)
		add("claude works", bounded(func(ctx context.Context) error {
			return version(ctx, r, cfg.Paths.Claude, "--version")
		}), cfg.Paths.Claude)
		add("gh authenticated", bounded(func(ctx context.Context) error {
			return ghAuth(ctx, r, cfg.Paths.GH)
		}), cfg.Paths.GH)

		ch := chat.New(r, cfg.Paths.Python, cfg.Paths.ChatScript, cfg.Space)
		var named bool
		nerr := bounded(func(ctx context.Context) error {
			var err error
			named, err = ch.HasNamedRooms(ctx)
			return err
		})
		switch {
		case nerr != nil:
			add("google chat reachable", nerr, "")
		case !named:
			add("google chat account", errors.New(
				"this account can see no named spaces — two Google accounts exist on this machine "+
					"and the personal one lists none; re-run `python auth.py login` as the work account"), "")
		default:
			add("google chat account", nil, "named spaces visible")
		}
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	failed := 0
	for _, c := range checks {
		mark := "PASS"
		if !c.ok {
			mark, failed = "FAIL", failed+1
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", mark, c.name, c.detail)
	}
	tw.Flush()

	if failed > 0 {
		return fmt.Errorf("%d of %d checks failed", failed, len(checks))
	}
	fmt.Printf("\nall %d checks passed\n", len(checks))
	return nil
}

// explainValidate wraps cfg.Validate() with an explicit note when the
// underlying cause is that no config file was found at all, so a fresh
// clone's first `doctor` run does not read as a mysterious failure: it
// should say plainly that a config file is required and name the example.
func explainValidate(cfg config.Config, cfgPath string) error {
	verr := cfg.Validate()
	if verr == nil {
		return nil
	}
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no config file found at %s: copy config.yaml.example there and fill in "+
			"the required fields — %w", cfgPath, verr)
	}
	return verr
}

func exists(path string) error {
	if path == "" {
		return errors.New("not configured")
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}
	return nil
}

func writable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".firstpass-write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return err
	}
	return os.Remove(probe)
}

func version(ctx context.Context, r runner.Runner, bin string, args ...string) error {
	res, err := r.Run(ctx, "", bin, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s exit %d", bin, res.ExitCode)
	}
	return nil
}

func ghAuth(ctx context.Context, r runner.Runner, gh string) error {
	res, err := r.Run(ctx, "", gh, "auth", "status")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("gh auth status exit %d: run `gh auth login`", res.ExitCode)
	}
	return nil
}
