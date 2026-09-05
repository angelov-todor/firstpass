// Package config loads firstpass's configuration, defaulting to values safe
// enough to run unattended.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so YAML can carry "5m" style values. yaml.v3
// will not decode a string into a time.Duration on its own, and the failure is
// silent: every duration would become zero.
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// D returns the wrapped duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Paths names the external programs firstpass drives.
type Paths struct {
	Python     string `yaml:"python"`
	ChatScript string `yaml:"chat_script"`
	Claude     string `yaml:"claude"`
	Git        string `yaml:"git"`
	GH         string `yaml:"gh"`
}

// Config is the whole of firstpass's configuration.
type Config struct {
	Space              string   `yaml:"space"`
	GithubLogin        string   `yaml:"github_login"`
	Interval           Duration `yaml:"interval"`
	DryRun             bool     `yaml:"dry_run"`
	MaxReviewsPerSweep int      `yaml:"max_reviews_per_sweep"`
	// ReviewConcurrency is how many reviews may run at once. One is serial,
	// which is what firstpass did before this existed and remains the default:
	// a tool that writes comments on colleagues' pull requests should not
	// change how much it does at once because somebody upgraded.
	//
	// The practical ceiling is unlikely to be this machine. Each review is a
	// `claude -p` session doing tool calls, and they share one account's rate
	// limits, so raising this past a small number tends to buy queueing rather
	// than throughput.
	ReviewConcurrency int      `yaml:"review_concurrency"`
	ReviewTimeout     Duration `yaml:"review_timeout"`
	// ChatTimeout, GHTimeout and CloneTimeout bound the three subprocesses
	// that are not claude. Without them an unattended daemon has no bound on
	// chat.py, gh, or a `git clone --bare` of a whole repository -- and on
	// Windows a private-repo HTTPS clone can raise a Git Credential Manager
	// dialog and wait for a human who is not there.
	ChatTimeout        Duration `yaml:"chat_timeout"`
	GHTimeout          Duration `yaml:"gh_timeout"`
	CloneTimeout       Duration `yaml:"clone_timeout"`
	PendingMaxAttempts int      `yaml:"pending_max_attempts"`
	PendingMaxAge      Duration `yaml:"pending_max_age"`
	FetchLimit         int      `yaml:"fetch_limit"`
	AllowOwners        []string `yaml:"allow_owners"`
	DenyRepos          []string `yaml:"deny_repos"`
	ClaudeArgs         []string `yaml:"claude_args"`
	StateDir           string   `yaml:"state_dir"`
	Paths              Paths    `yaml:"paths"`
}

// Default is the shipped configuration: safe, but not yet usable. Space,
// GithubLogin, AllowOwners and Paths.ChatScript are deliberately left unset
// so a fresh install cannot act until it has been configured — see
// config.yaml.example. Every other field is a safe operational default.
func Default() Config {
	return Config{
		Interval:           Duration(5 * time.Minute),
		DryRun:             true,
		MaxReviewsPerSweep: 3,
		ReviewConcurrency:  1,
		ReviewTimeout:      Duration(30 * time.Minute),
		ChatTimeout:        Duration(2 * time.Minute),
		GHTimeout:          Duration(time.Minute),
		CloneTimeout:       Duration(15 * time.Minute),
		PendingMaxAttempts: 20,
		PendingMaxAge:      Duration(168 * time.Hour),
		FetchLimit:         50,
		ClaudeArgs:         []string{"--permission-mode", "bypassPermissions"},
		StateDir:           DefaultStateDir(),
		Paths: Paths{
			// python, not python3: on some systems the python3 on PATH cannot
			// import the keyring module a chat script is likely to need.
			Python: "python",
			Claude: "claude",
			Git:    "git",
			GH:     "gh",
			// ChatScript is left unset: it points at a chat-reading script that is
			// specific to each install. See config.yaml.example.
		},
	}
}

// DefaultStateDir is where the database, reports, mirrors and worktrees live.
func DefaultStateDir() string {
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "firstpass")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".firstpass")
}

// DefaultConfigPath is the config file consulted when no -config flag is given.
func DefaultConfigPath() string {
	if d := os.Getenv("APPDATA"); d != "" {
		return filepath.Join(d, "firstpass", "config.yaml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".firstpass", "config.yaml")
}

// Load starts from Default and overlays whatever the YAML file specifies. A
// missing file is not an error: the defaults are the shipped configuration.
func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

// Validate rejects configurations that would be unsafe or useless to run.
func (c Config) Validate() error {
	if c.Space == "" {
		return errors.New("space is required: set \"space\" in your config file to the Google Chat " +
			"space to watch (see config.yaml.example)")
	}
	if c.GithubLogin == "" {
		return errors.New("github_login is required: without it firstpass would review your own PRs; " +
			"set \"github_login\" in your config file (see config.yaml.example)")
	}
	if len(c.AllowOwners) == 0 {
		return errors.New("allow_owners must not be empty: an empty list would permit review comments " +
			"on any org's PRs; set \"allow_owners\" in your config file (see config.yaml.example)")
	}
	if c.Paths.ChatScript == "" {
		return errors.New("paths.chat_script is required: it must point at a script that can read your " +
			"Google Chat space (see config.yaml.example)")
	}
	if c.MaxReviewsPerSweep <= 0 {
		return errors.New("max_reviews_per_sweep must be positive")
	}
	if c.ReviewConcurrency <= 0 {
		return errors.New("review_concurrency must be positive (1 is serial)")
	}
	// A concurrency above the cap would let several candidates each claim the
	// last free slot's worth of preparatory work and then be turned away after
	// paying for a clone. Refusing the combination is clearer than quietly
	// clamping one to the other, because either value could be the one the
	// operator meant.
	if c.ReviewConcurrency > c.MaxReviewsPerSweep {
		return fmt.Errorf("review_concurrency (%d) must not exceed max_reviews_per_sweep (%d): "+
			"reviews beyond the cap would prepare a worktree and then be turned away",
			c.ReviewConcurrency, c.MaxReviewsPerSweep)
	}
	if c.ReviewTimeout.D() <= 0 {
		return errors.New("review_timeout must be positive")
	}
	if c.Interval.D() <= 0 {
		return errors.New("interval must be positive")
	}
	if c.FetchLimit <= 0 {
		return errors.New("fetch_limit must be positive")
	}
	if c.ChatTimeout.D() <= 0 {
		return errors.New("chat_timeout must be positive: chat.py would otherwise run unbounded")
	}
	if c.GHTimeout.D() <= 0 {
		return errors.New("gh_timeout must be positive: gh would otherwise run unbounded")
	}
	if c.CloneTimeout.D() <= 0 {
		return errors.New("clone_timeout must be positive: a git clone would otherwise run unbounded, " +
			"and a credential prompt would block the daemon forever")
	}
	if c.PendingMaxAttempts <= 0 {
		return errors.New("pending_max_attempts must be positive: zero makes every parked PR expire " +
			"terminally on its next sweep, so nothing deferred is ever reviewed")
	}
	if c.PendingMaxAge.D() <= 0 {
		return errors.New("pending_max_age must be positive: zero makes every parked PR expire " +
			"terminally on its next sweep, so nothing deferred is ever reviewed")
	}
	if c.StateDir == "" {
		return errors.New("state_dir is required")
	}
	if !filepath.IsAbs(c.StateDir) {
		// A relative state_dir is resolved differently by the two consumers
		// that matter: `git worktree add` resolves it against the mirror,
		// os.RemoveAll against firstpass's working directory. Launched from
		// inside one of the user's own clones, that RemoveAll would run in
		// their working copy.
		return fmt.Errorf("state_dir must be an absolute path, got %q: a relative path can resolve "+
			"inside whatever directory firstpass was launched from", c.StateDir)
	}
	return nil
}

// OwnerAllowed reports whether PRs under this owner may be acted on.
func (c Config) OwnerAllowed(owner string) bool {
	for _, o := range c.AllowOwners {
		if strings.EqualFold(o, owner) {
			return true
		}
	}
	return false
}

// RepoDenied reports whether this specific repository is carved out.
func (c Config) RepoDenied(owner, repo string) bool {
	full := owner + "/" + repo
	for _, d := range c.DenyRepos {
		if strings.EqualFold(d, full) {
			return true
		}
	}
	return false
}

func (c Config) DBPath() string     { return filepath.Join(c.StateDir, "state.db") }
func (c Config) PauseFile() string  { return filepath.Join(c.StateDir, "PAUSE") }
func (c Config) ReportsDir() string { return filepath.Join(c.StateDir, "reports") }
func (c Config) ReposDir() string   { return filepath.Join(c.StateDir, "repos") }
func (c Config) WorkDir() string    { return filepath.Join(c.StateDir, "work") }
