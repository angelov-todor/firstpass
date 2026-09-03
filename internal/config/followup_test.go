package config

// Finding 4: config.yaml.example could not be used as documented. An explicit
// empty string overrides a default rather than falling back to it, so
// `state_dir: ""` in the example meant that copying it and filling in only the
// four REQUIRED fields produced "state_dir is required" -- on the onboarding
// path of a public repository.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exampleConfigPath is config.yaml.example at the repository root. A test's
// working directory is its own package directory, so the path is relative to
// internal/config.
func exampleConfigPath() string { return filepath.Join("..", "..", "config.yaml.example") }

// fillRequired is the onboarding path README documents: copy the example and
// fill in only the fields it marks REQUIRED. Every replacement must find its
// target, so a change to the example's shape fails this test loudly rather
// than leaving it silently testing nothing.
func fillRequired(t *testing.T, body string) string {
	t.Helper()
	for _, r := range []struct{ old, new string }{
		{`space: ""`, `space: "spaces/EXAMPLE123"`},
		{`github_login: ""`, `github_login: "someone"`},
		{`allow_owners: []`, "allow_owners:\n  - example-org"},
		{`chat_script: ""`, `chat_script: "C:/tools/chat.py"`},
	} {
		if !strings.Contains(body, r.old) {
			t.Fatalf("config.yaml.example no longer contains %q, so this test cannot fill it in", r.old)
		}
		body = strings.Replace(body, r.old, r.new, 1)
	}
	return body
}

func TestExampleConfigValidatesWithOnlyTheRequiredFieldsFilledIn(t *testing.T) {
	// Default() reads LOCALAPPDATA, so pin it: the assertion below is about
	// state_dir falling back to the default, not about this machine's layout.
	t.Setenv("LOCALAPPDATA", t.TempDir())

	raw, err := os.ReadFile(exampleConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(fillRequired(t, string(raw))), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a copy of config.yaml.example with only the four REQUIRED fields filled in must "+
			"validate, or the documented onboarding path is broken: %v", err)
	}

	// Every other key in the example must still leave its default in place: a
	// key present-but-empty whose zero value is meaningful is the same trap.
	def := Default()
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"state_dir", cfg.StateDir, def.StateDir},
		{"dry_run", cfg.DryRun, def.DryRun},
		{"interval", cfg.Interval, def.Interval},
		{"max_reviews_per_sweep", cfg.MaxReviewsPerSweep, def.MaxReviewsPerSweep},
		{"review_timeout", cfg.ReviewTimeout, def.ReviewTimeout},
		{"chat_timeout", cfg.ChatTimeout, def.ChatTimeout},
		{"gh_timeout", cfg.GHTimeout, def.GHTimeout},
		{"clone_timeout", cfg.CloneTimeout, def.CloneTimeout},
		{"pending_max_attempts", cfg.PendingMaxAttempts, def.PendingMaxAttempts},
		{"pending_max_age", cfg.PendingMaxAge, def.PendingMaxAge},
		{"fetch_limit", cfg.FetchLimit, def.FetchLimit},
		{"paths.python", cfg.Paths.Python, def.Paths.Python},
		{"paths.claude", cfg.Paths.Claude, def.Paths.Claude},
		{"paths.git", cfg.Paths.Git, def.Paths.Git},
		{"paths.gh", cfg.Paths.GH, def.Paths.GH},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want the default %v: the example must not override a "+
				"meaningful default with an empty value", c.name, c.got, c.want)
		}
	}
	if strings.Join(cfg.ClaudeArgs, " ") != strings.Join(def.ClaudeArgs, " ") {
		t.Errorf("claude_args = %q, want the default %q", cfg.ClaudeArgs, def.ClaudeArgs)
	}
	if len(cfg.DenyRepos) != 0 {
		t.Errorf("deny_repos = %q, want empty", cfg.DenyRepos)
	}
}

// The trap itself, stated directly: an explicit empty state_dir must still be
// rejected. Commenting the key out in the example is the fix; loosening
// Validate would let a genuinely empty state_dir through.
func TestExplicitlyEmptyStateDirIsStillRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "space: spaces/A\ngithub_login: someone\nallow_owners:\n  - example-org\n" +
		"paths:\n  chat_script: chat.py\nstate_dir: \"\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil {
		t.Error("an explicitly empty state_dir must be an error: it is what the example used to ship")
	}
}
