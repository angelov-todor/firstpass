package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsAreSafe(t *testing.T) {
	c := Default()
	if !c.DryRun {
		t.Error("DryRun must default to true: the first run must not post to colleagues' PRs")
	}
	if c.MaxReviewsPerSweep <= 0 {
		t.Error("MaxReviewsPerSweep must be positive")
	}
	if c.Interval.D() != 5*time.Minute {
		t.Errorf("Interval = %v, want 5m", c.Interval.D())
	}
	if c.Paths.Python != "python" {
		t.Errorf("Paths.Python = %q, want \"python\" (python3 in git-bash cannot import keyring)", c.Paths.Python)
	}
}

// TestDefaultsLeaveInstallSpecificFieldsUnset is the flip side of
// TestDefaultsAreSafe: a public, freshly cloned build must not ship any other
// team's identifiers. Space, GithubLogin, AllowOwners and Paths.ChatScript
// are install-specific, so Default() must leave every one of them empty
// rather than baking in an example org.
func TestDefaultsLeaveInstallSpecificFieldsUnset(t *testing.T) {
	c := Default()
	if c.Space != "" {
		t.Errorf("Space = %q, want empty: a fresh install must not inherit someone else's chat space", c.Space)
	}
	if c.GithubLogin != "" {
		t.Errorf("GithubLogin = %q, want empty: a fresh install must not inherit someone else's identity", c.GithubLogin)
	}
	if len(c.AllowOwners) != 0 {
		t.Errorf("AllowOwners = %v, want empty: a fresh install must not inherit someone else's org", c.AllowOwners)
	}
	if c.Paths.ChatScript != "" {
		t.Errorf("Paths.ChatScript = %q, want empty: a fresh install must not inherit someone else's path", c.Paths.ChatScript)
	}
}

func TestLoadOverlaysYAMLOntoDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	body := "interval: 90s\ndry_run: false\nallow_owners:\n  - Acme\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval.D() != 90*time.Second {
		t.Errorf("Interval = %v, want 90s", c.Interval.D())
	}
	if c.DryRun {
		t.Error("dry_run: false must be honoured")
	}
	if len(c.AllowOwners) != 1 || c.AllowOwners[0] != "Acme" {
		t.Errorf("AllowOwners = %v, want [Acme]", c.AllowOwners)
	}
	if c.FetchLimit != 50 {
		t.Errorf("FetchLimit = %d, want 50: fields absent from the file must keep their defaults", c.FetchLimit)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("a missing config file must not be an error: %v", err)
	}
	if !c.DryRun {
		t.Error("missing config must fall back to the safe defaults")
	}
}

func TestLoadRejectsBadDuration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("interval: sometimes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("an unparseable duration must be an error, not a silent zero")
	}
}

func TestOwnerAllowedAndRepoDenied(t *testing.T) {
	c := Default()
	c.AllowOwners = []string{"example-org"}
	c.DenyRepos = []string{"example-org/secret-repo"}

	if !c.OwnerAllowed("example-org") {
		t.Error("own org must be allowed")
	}
	if !c.OwnerAllowed("EXAMPLE-ORG") {
		t.Error("owner comparison must be case-insensitive")
	}
	if c.OwnerAllowed("torvalds") {
		t.Error("an outside org must be rejected")
	}
	if !c.RepoDenied("example-org", "secret-repo") {
		t.Error("denied repo must be denied")
	}
	if c.RepoDenied("example-org", "public-repo") {
		t.Error("other repos must not be denied")
	}
}

// withRequired returns c with the four install-specific fields set to
// placeholder values, so a test that mutates one other field can rely on
// Validate() failing for that reason alone rather than for one of these.
func withRequired(c Config) Config {
	c.Space = "spaces/EXAMPLE123"
	c.GithubLogin = "someone"
	c.AllowOwners = []string{"example-org"}
	c.Paths.ChatScript = "chat.py"
	return c
}

func TestValidate(t *testing.T) {
	t.Run("empty allow_owners is an error", func(t *testing.T) {
		c := withRequired(Default())
		c.AllowOwners = nil
		if err := c.Validate(); err == nil {
			t.Error("empty allow_owners must fail validation, not become an implicit allow-all")
		}
	})
	t.Run("defaults alone are invalid", func(t *testing.T) {
		// A fresh install must be configured before it can run: space,
		// github_login, allow_owners and paths.chat_script are all
		// install-specific and Default() deliberately leaves them empty.
		if err := Default().Validate(); err == nil {
			t.Error("Default() must be invalid until space, github_login, allow_owners and " +
				"paths.chat_script are configured")
		}
	})
	t.Run("defaults plus the four required fields validate", func(t *testing.T) {
		if err := withRequired(Default()).Validate(); err != nil {
			t.Errorf("Default() with space, github_login, allow_owners and paths.chat_script set must be valid: %v", err)
		}
	})
	t.Run("empty space is an error", func(t *testing.T) {
		c := withRequired(Default())
		c.Space = ""
		if err := c.Validate(); err == nil {
			t.Error("space is required")
		}
	})
	t.Run("empty github_login is an error", func(t *testing.T) {
		c := withRequired(Default())
		c.GithubLogin = ""
		if err := c.Validate(); err == nil {
			t.Error("github_login is required")
		}
	})
	t.Run("empty paths.chat_script is an error", func(t *testing.T) {
		c := withRequired(Default())
		c.Paths.ChatScript = ""
		if err := c.Validate(); err == nil {
			t.Error("paths.chat_script is required")
		}
	})
	t.Run("zero review timeout is an error", func(t *testing.T) {
		c := withRequired(Default())
		c.ReviewTimeout = 0
		if err := c.Validate(); err == nil {
			t.Error("review_timeout must be positive")
		}
	})

	// I5: an unattended daemon needs a bound on every subprocess, not just
	// claude. A zero here would restore the unbounded context these replace.
	t.Run("zero subprocess timeouts are errors", func(t *testing.T) {
		for name, mutate := range map[string]func(*Config){
			"chat_timeout":  func(c *Config) { c.ChatTimeout = 0 },
			"gh_timeout":    func(c *Config) { c.GHTimeout = 0 },
			"clone_timeout": func(c *Config) { c.CloneTimeout = 0 },
		} {
			c := withRequired(Default())
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("%s must be positive", name)
			}
		}
	})

	// I10: pending_max_attempts: 0 makes "Attempts >= max" true immediately
	// and pending_max_age: 0 makes "age > max" true immediately, so every
	// parked PR expires terminally on its second sweep and is never reviewed.
	t.Run("zero pending limits are errors", func(t *testing.T) {
		c := withRequired(Default())
		c.PendingMaxAttempts = 0
		if err := c.Validate(); err == nil {
			t.Error("pending_max_attempts must be positive: zero expires the whole backlog on sight")
		}
		c = withRequired(Default())
		c.PendingMaxAge = 0
		if err := c.Validate(); err == nil {
			t.Error("pending_max_age must be positive: zero expires the whole backlog on sight")
		}
	})

	// I9: worktree.Prepare passes state_dir-derived paths to `git worktree
	// add` (resolved relative to the mirror) and to os.RemoveAll (resolved
	// relative to firstpass's cwd). With a relative state_dir those two
	// disagree, and a firstpass launched from inside one of the operator's own
	// clones would run that RemoveAll inside their working copy.
	t.Run("relative state_dir is an error", func(t *testing.T) {
		c := withRequired(Default())
		c.StateDir = filepath.Join("state", "firstpass")
		if err := c.Validate(); err == nil {
			t.Error("a relative state_dir must fail validation: it can resolve inside the user's own clone")
		}
	})
	t.Run("empty state_dir is an error", func(t *testing.T) {
		c := withRequired(Default())
		c.StateDir = ""
		if err := c.Validate(); err == nil {
			t.Error("state_dir is required")
		}
	})
}

func TestSubprocessTimeoutDefaults(t *testing.T) {
	c := Default()
	for name, got := range map[string]struct{ have, want time.Duration }{
		"ChatTimeout":  {c.ChatTimeout.D(), 2 * time.Minute},
		"GHTimeout":    {c.GHTimeout.D(), time.Minute},
		"CloneTimeout": {c.CloneTimeout.D(), 15 * time.Minute},
	} {
		if got.have != got.want {
			t.Errorf("%s = %v, want %v", name, got.have, got.want)
		}
	}
}

func TestPathHelpersLiveUnderStateDir(t *testing.T) {
	c := Default()
	c.StateDir = filepath.Join("X:", "state")
	for name, got := range map[string]string{
		"DBPath":     c.DBPath(),
		"PauseFile":  c.PauseFile(),
		"ReportsDir": c.ReportsDir(),
		"ReposDir":   c.ReposDir(),
		"WorkDir":    c.WorkDir(),
	} {
		if filepath.Dir(got) != c.StateDir {
			t.Errorf("%s() = %q, want a child of %q", name, got, c.StateDir)
		}
	}
}
