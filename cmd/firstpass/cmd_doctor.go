package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// doctorOverallTimeout keeps the command itself bounded, and must leave
	// real headroom over the sum of the checks. Four sequential checks at
	// doctorCheckTimeout were already 4 x 30s = 2m, so a 2m overall budget
	// gave none: the last check to run -- "google chat reachable", the one
	// fatalChatBanner explicitly sends the operator to -- lost exactly the
	// pre-check time and would fail on the context rather than on its merits,
	// reaching a zero deadline in the limit.
	// That is the exact misattribution the per-check deadline exists to
	// prevent, so the overall budget has to exceed the sum, not equal it.
	//
	// Raised from 3m to 4m when the fifth bounded check ("gh can submit
	// reviews") was added: five at 30s is 2m30s, and 3m would have cut the
	// headroom from a minute to thirty seconds.
	doctorOverallTimeout = 4 * time.Minute
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
		// Only when configured: docs_root is optional, and an install without
		// one is not broken, it just gets no compliance dimension.
		//
		// Checked at all because the failure is otherwise invisible. A docs
		// root that does not exist produces reviews that read exactly like
		// reviews that found nothing to say about compliance -- the reviewer is
		// pointed at a missing directory, finds nothing, and says nothing. The
		// operator would have no reason to suspect the feature was off.
		if cfg.DocsRoot != "" {
			// isDir, not exists: a docs_root pointing at a plain file passes an
			// existence check and then every path built under it is missing.
			add("docs root present", isDir(cfg.DocsRoot), cfg.DocsRoot)
		}

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

		// Authenticated is not the same as allowed to write. `gh pr review`
		// is the first writing gh command firstpass runs, and a token that
		// can read pull requests but not review them fails once per PR --
		// which the operator only discovers after a twelve-minute review has
		// already run.
		//
		// The detail is read after the check has run, not inside the add()
		// call: Go does not order a plain variable read against a function
		// call in the same argument list, so passing scopeDetail alongside
		// the call could read it before it was assigned.
		var scopeDetail string
		serr := bounded(func(ctx context.Context) error {
			var err error
			scopeDetail, err = ghReviewScope(ctx, r, cfg.Paths.GH)
			return err
		})
		add("gh can submit reviews", serr, scopeDetail)

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

// isDir is exists() plus the type check. A docs_root pointing at a plain file
// passes an existence check and then every path built under it is missing --
// which yields reviews indistinguishable from reviews with nothing to say
// about compliance.
func isDir(path string) error {
	if path == "" {
		return errors.New("not configured")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is a file, not a directory", path)
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

// ghReviewScope is a read-only preflight for the one writing gh command
// firstpass runs, `gh pr review`. It returns the detail to show on a pass.
//
// `gh api --include user` is a GET: it reads the authenticated user and, for
// a classic token, comes back with an `x-oauth-scopes` response header naming
// the token's scopes. `repo` (or `public_repo` for public repositories only)
// is what submitting a review needs.
//
// The honest limit, and the reason this does not simply pass or fail: a
// fine-grained personal access token or a GitHub App token sends no such
// header, and its permissions cannot be read from a response at all. In that
// case this passes with a detail saying write access could not be determined,
// rather than claiming it is fine. A check that reports a confident PASS it
// has not established would be worse than no check.
func ghReviewScope(ctx context.Context, r runner.Runner, gh string) (string, error) {
	res, err := r.Run(ctx, "", gh, "api", "--include", "user")
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("gh api user exit %d: %s",
			res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	scopes, ok := oauthScopes(res.Stdout)
	if !ok || len(scopes) == 0 {
		return "write access could not be determined: this token sent no x-oauth-scopes header, " +
			"which is normal for a fine-grained or GitHub App token. If it turns out to lack " +
			"write access, each verdict is recorded as a failed submission in `firstpass status` " +
			"and never retried", nil
	}
	for _, s := range scopes {
		if s == "repo" || s == "public_repo" {
			return "x-oauth-scopes: " + strings.Join(scopes, ", "), nil
		}
	}
	return "", fmt.Errorf("the gh token has scopes [%s] but neither `repo` nor `public_repo`, so "+
		"`gh pr review` cannot submit a review verdict: run "+
		"`gh auth refresh -h github.com -s repo`. Dry runs are unaffected — they submit no "+
		"verdict — but every live verdict would fail", strings.Join(scopes, ", "))
}

// oauthScopes reads the x-oauth-scopes response header out of the output of
// `gh api --include`, which prints the status line and headers ahead of the
// body. The second result reports whether the header was present at all,
// which is not the same as it being empty.
func oauthScopes(out []byte) ([]string, bool) {
	for _, line := range strings.Split(string(out), "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "x-oauth-scopes") {
			continue
		}
		var scopes []string
		for _, s := range strings.Split(value, ",") {
			if s = strings.TrimSpace(s); s != "" {
				scopes = append(scopes, s)
			}
		}
		return scopes, true
	}
	return nil, false
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
