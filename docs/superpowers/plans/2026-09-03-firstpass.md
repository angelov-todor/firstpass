# firstpass Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Go daemon on this machine that watches the `[AstraEx] Team` Google Chat space and, for each newly posted GitHub PR, runs a Claude Code review in an isolated worktree and posts the findings as inline PR comments.

**Architecture:** One binary. A ticker sweeps the chat space via the existing `google-chat` skill's `chat.py`, extracts PR links, filters them against GitHub state and an owner allowlist, then hands each survivor to a serial review worker that prepares a throwaway git worktree and shells out to `claude -p "/code-review --comment"`. All decisions and outcomes live in a local bbolt store so restarts and crashes never re-review a PR.

**Tech Stack:** Go 1.24, `go.etcd.io/bbolt`, `gopkg.in/yaml.v3`, stdlib `flag` and `log/slog`. External processes: `python` (chat.py), `gh`, `git`, `claude`.

**Spec:** `docs/superpowers/specs/2026-09-03-firstpass-design.md`

## Global Constraints

- Module path: `github.com/angelov-todor/firstpass`. Go directive: `go 1.24`.
- Dependencies limited to `go.etcd.io/bbolt` and `gopkg.in/yaml.v3`. No CLI framework — stdlib `flag` with subcommand dispatch.
- Windows-first. Every path built with `path/filepath`. State dir `%LOCALAPPDATA%\firstpass`, config `%APPDATA%\firstpass\config.yaml`.
- `paths.python` must be `python`, **not** `python3`: the git-bash `python3` on this machine cannot import `keyring`, so `chat.py` fails there.
- `dry_run` defaults to `true`. `allow_owners` defaults to `["Example-Org"]`, and an empty `allow_owners` is a validation error, never an implicit allow-all.
- `github_login: angelov-todor`. Space: `spaces/EXAMPLE123`.
- Only `in_flight` is a non-terminal outcome. A review timeout is `needs_attention`, never a retry.
- Every task ends with `gofmt -l .` clean, `go vet ./...` clean, and `go test ./...` passing before the commit.
- Commit messages end with the trailer `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.

## Additions beyond the spec

Four implementation details the spec did not name. Each is called out at its task.

1. **`claude_args`, defaulting to `["--permission-mode", "bypassPermissions"]`.** Headless `claude -p` cannot run `gh` to post comments under the default permission mode — it would block on a prompt no one can answer. Config-driven so it can be tightened without a rebuild. **This is the one addition with a security dimension.** NOTE, CORRECTED AFTER REVIEW: an earlier version of this line claimed the grant was bounded "inside a throwaway worktree". That was wrong. A working directory is not a sandbox: the subprocess inherits the full parent environment, so it can write anywhere the operator can, read `~/.ssh` and `~/.claude`, and use `gh` with the operator's token against any repository — including ones outside `allow_owners`, which sits upstream of a process it cannot confine. The reviewed checkout is also attacker-influenced: `refs/pull/N/head` on a fork PR is code by someone outside the org, and it may carry its own `CLAUDE.md` or `.claude/` config that a headless agent reads as instructions. See docs/superpowers/reviews/2026-09-03-final-review-outcomes.md.
2. **`fetch_limit`, default 50** — messages requested per sweep.
3. **`paths.git` / `paths.gh`** — so `doctor` can check them and tests can inject.
4. **`Manager.URLFor`** — clone-URL override, so the worktree tests can clone a local fixture instead of reaching GitHub.

One thing the spec asks for that this plan **does not** deliver:

5. **Log rotation.** The spec says "stderr and a rotating file". Rotation needs a third dependency (`lumberjack` or equivalent), which the dependency constraint above rules out, and an unrotated append-only log grows without bound. So logging goes to stderr only, and redirection covers the file case: `firstpass watch 2>> %LOCALAPPDATA%\firstpass\firstpass.log`. If a rotating log turns out to matter, it is a small follow-up, not a gap in the design.

## File Structure

| File | Responsibility |
|---|---|
| `go.mod` | Module and the two dependencies |
| `internal/prref/prref.go` | `PRRef`, `Extract`, `ParseKey` — pure, no I/O |
| `internal/config/config.go` | `Config`, `Default`, `Load`, `Validate`, path helpers |
| `internal/store/store.go` | bbolt: watermark, reviews, pending |
| `internal/runner/runner.go` | `Runner` interface + `OS` implementation |
| `internal/runner/fake.go` | `Fake` runner, shared by other packages' tests |
| `internal/chat/chat.go` | `chat.py` wrapper, noise stripping, `APIError` |
| `internal/ghpr/ghpr.go` | `gh pr view` wrapper |
| `internal/worktree/worktree.go` | Bare mirror cache + per-PR worktree |
| `internal/review/review.go` | `claude -p` wrapper, dry-run reports |
| `internal/pipeline/pipeline.go` | One sweep: the whole decision table |
| `cmd/firstpass/main.go` | Subcommand dispatch |
| `cmd/firstpass/app.go` | Config → wired `Pipeline` |
| `cmd/firstpass/cmd_*.go` | One file per subcommand |

---

### Task 1: PR reference extraction

The parser is the component where a bug is silent and expensive: a missed URL means a PR is never reviewed and nothing reports it. It gets the largest test set in the project and no I/O at all.

**Files:**
- Create: `go.mod`
- Create: `internal/prref/prref.go`
- Test: `internal/prref/prref_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type PRRef struct { Owner, Repo string; Number int }`; `func (PRRef) Key() string` → `"owner/repo#5"`; `func (PRRef) URL() string`; `func Extract(text string) []PRRef`; `func ParseKey(s string) (PRRef, error)`.

- [ ] **Step 1: Initialise the module**

```bash
cd firstpass
go mod init github.com/angelov-todor/firstpass
go mod edit -go=1.24
```

- [ ] **Step 2: Write the failing test**

Create `internal/prref/prref_test.go`:

```go
package prref

import "testing"

func TestExtract(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []PRRef
	}{
		{
			name: "single url",
			text: "https://github.com/Example-Org/aex-user-service/pull/91",
			want: []PRRef{{"Example-Org", "aex-user-service", 91}},
		},
		{
			name: "trailing period",
			text: "see https://github.com/Example-Org/aex-balances/pull/7.",
			want: []PRRef{{"Example-Org", "aex-balances", 7}},
		},
		{
			name: "files suffix",
			text: "https://github.com/Example-Org/aex-balances/pull/7/files",
			want: []PRRef{{"Example-Org", "aex-balances", 7}},
		},
		{
			name: "query suffix",
			text: "https://github.com/Example-Org/aex-balances/pull/7?diff=split",
			want: []PRRef{{"Example-Org", "aex-balances", 7}},
		},
		{
			name: "comment fragment yields only the pr",
			text: "https://github.com/Example-Org/aex-balances/pull/7#issuecomment-1",
			want: []PRRef{{"Example-Org", "aex-balances", 7}},
		},
		{
			name: "team batch format",
			text: "Please review the following PRs — margin follow-ups:\n" +
				"• https://github.com/Example-Org/aex-margin-service/pull/1\n" +
				"• https://github.com/Example-Org/aex-balances/pull/2\n",
			want: []PRRef{
				{"Example-Org", "aex-margin-service", 1},
				{"Example-Org", "aex-balances", 2},
			},
		},
		{
			name: "same pr twice is deduped",
			text: "https://github.com/Example-Org/a/pull/1 and https://github.com/Example-Org/a/pull/1",
			want: []PRRef{{"Example-Org", "a", 1}},
		},
		{
			name: "shorthand accepted",
			text: "Example-Org/aex-user-service#91 is ready",
			want: []PRRef{{"Example-Org", "aex-user-service", 91}},
		},
		{
			name: "bare hash is not parsed",
			text: "#91 is ready for review",
			want: nil,
		},
		{
			name: "issue url is not a pr",
			text: "https://github.com/Example-Org/a/issues/5",
			want: nil,
		},
		{
			name: "markdown link",
			text: "[#91](https://github.com/Example-Org/a/pull/91)",
			want: []PRRef{{"Example-Org", "a", 91}},
		},
		{
			name: "empty text",
			text: "",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Extract(tc.text)
			if len(got) != len(tc.want) {
				t.Fatalf("Extract() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Extract()[%d] = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestKeyAndURL(t *testing.T) {
	r := PRRef{Owner: "Example-Org", Repo: "aex-balances", Number: 5}
	if got := r.Key(); got != "Example-Org/aex-balances#5" {
		t.Errorf("Key() = %q", got)
	}
	if got := r.URL(); got != "https://github.com/Example-Org/aex-balances/pull/5" {
		t.Errorf("URL() = %q", got)
	}
}

func TestParseKeyRoundTrip(t *testing.T) {
	want := PRRef{Owner: "Example-Org", Repo: "aex-balances", Number: 5}
	got, err := ParseKey(want.Key())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("ParseKey(%q) = %v, want %v", want.Key(), got, want)
	}
}

func TestParseKeyRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "nope", "o/r", "o/r#", "o/r#x", "https://github.com/o/r/pull/1"} {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("ParseKey(%q) must fail", s)
		}
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/prref/`
Expected: FAIL — `undefined: PRRef`, `undefined: Extract`, `undefined: ParseKey`.

- [ ] **Step 4: Write the implementation**

Create `internal/prref/prref.go`:

```go
// Package prref extracts GitHub pull request references from chat message text.
package prref

import (
	"fmt"
	"regexp"
	"strconv"
)

// PRRef identifies a single GitHub pull request.
type PRRef struct {
	Owner  string
	Repo   string
	Number int
}

// Key is the stable identity used for de-duplication and storage.
func (r PRRef) Key() string { return fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number) }

// URL is the canonical web URL for the pull request.
func (r PRRef) URL() string {
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d", r.Owner, r.Repo, r.Number)
}

const seg = `[A-Za-z0-9._-]+`

var (
	urlRe       = regexp.MustCompile(`https?://github\.com/(` + seg + `)/(` + seg + `)/pull/(\d+)`)
	shorthandRe = regexp.MustCompile(`\b(` + seg + `)/(` + seg + `)#(\d+)\b`)
	keyRe       = regexp.MustCompile(`^(` + seg + `)/(` + seg + `)#(\d+)$`)
)

// Extract pulls every distinct pull request reference out of a chat message.
// URLs are returned in order of appearance, followed by any shorthand
// references.
//
// A bare "#91" is deliberately not recognised. The team's chat rules record it
// as ambiguous — it reads as an issue in whatever repository the reader last
// had open — so resolving it would mean guessing a repository, and a wrong
// guess would review a stranger's PR.
func Extract(text string) []PRRef {
	var out []PRRef
	seen := map[string]bool{}

	add := func(owner, repo, num string) {
		n, err := strconv.Atoi(num)
		if err != nil || n <= 0 {
			return
		}
		ref := PRRef{Owner: owner, Repo: repo, Number: n}
		if seen[ref.Key()] {
			return
		}
		seen[ref.Key()] = true
		out = append(out, ref)
	}

	for _, m := range urlRe.FindAllStringSubmatch(text, -1) {
		add(m[1], m[2], m[3])
	}

	// Blank out the URLs already consumed, so a "#123" fragment hanging off a
	// PR URL cannot be misread as a shorthand reference to a different repo.
	rest := urlRe.ReplaceAllString(text, " ")
	for _, m := range shorthandRe.FindAllStringSubmatch(rest, -1) {
		add(m[1], m[2], m[3])
	}

	return out
}

// ParseKey is the inverse of Key, used when reading refs back out of storage.
func ParseKey(s string) (PRRef, error) {
	m := keyRe.FindStringSubmatch(s)
	if m == nil {
		return PRRef{}, fmt.Errorf("not a PR key: %q", s)
	}
	n, err := strconv.Atoi(m[3])
	if err != nil || n <= 0 {
		return PRRef{}, fmt.Errorf("not a PR key: %q", s)
	}
	return PRRef{Owner: m[1], Repo: m[2], Number: n}, nil
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/prref/ -v`
Expected: PASS, all subtests.

- [ ] **Step 6: Commit**

```bash
gofmt -l . && go vet ./...
git add go.mod internal/prref
git commit -m "$(cat <<'EOF'
feat: extract GitHub PR references from chat text

Full URLs and owner/repo#n shorthand, de-duplicated. Bare #n is
deliberately unparsed: the team chat rules record it as ambiguous, and
resolving it would mean guessing a repository.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Configuration

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Duration time.Duration` with `UnmarshalYAML` and `D() time.Duration`; `type Paths struct{ Python, ChatScript, Claude, Git, GH string }`; `type Config` (fields listed in the implementation); `func Default() Config`; `func Load(path string) (Config, error)`; `func DefaultConfigPath() string`; `func DefaultStateDir() string`; `func (Config) Validate() error`; `func (Config) OwnerAllowed(owner string) bool`; `func (Config) RepoDenied(owner, repo string) bool`; and the path helpers `DBPath`, `PauseFile`, `ReportsDir`, `ReposDir`, `WorkDir`.

> `Duration` exists because `yaml.v3` will not decode the string `"5m"` into a `time.Duration`. Without the wrapper every duration in the config file silently becomes zero, which would make the ticker spin and every review time out instantly.

- [ ] **Step 1: Add the YAML dependency**

```bash
go get gopkg.in/yaml.v3
```

- [ ] **Step 2: Write the failing test**

Create `internal/config/config_test.go`:

```go
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
	if len(c.AllowOwners) == 0 {
		t.Error("AllowOwners must not default to empty; that would allow every org")
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
	if c.Space == "" {
		t.Error("fields absent from the file must keep their defaults")
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
	c.AllowOwners = []string{"Example-Org"}
	c.DenyRepos = []string{"Example-Org/aex-secret"}

	if !c.OwnerAllowed("Example-Org") {
		t.Error("own org must be allowed")
	}
	if !c.OwnerAllowed("example-org") {
		t.Error("owner comparison must be case-insensitive")
	}
	if c.OwnerAllowed("torvalds") {
		t.Error("an outside org must be rejected")
	}
	if !c.RepoDenied("Example-Org", "aex-secret") {
		t.Error("denied repo must be denied")
	}
	if c.RepoDenied("Example-Org", "aex-balances") {
		t.Error("other repos must not be denied")
	}
}

func TestValidate(t *testing.T) {
	t.Run("empty allow_owners is an error", func(t *testing.T) {
		c := Default()
		c.AllowOwners = nil
		if err := c.Validate(); err == nil {
			t.Error("empty allow_owners must fail validation, not become an implicit allow-all")
		}
	})
	t.Run("defaults validate", func(t *testing.T) {
		if err := Default().Validate(); err != nil {
			t.Errorf("Default() must be valid: %v", err)
		}
	})
	t.Run("zero review timeout is an error", func(t *testing.T) {
		c := Default()
		c.ReviewTimeout = 0
		if err := c.Validate(); err == nil {
			t.Error("review_timeout must be positive")
		}
	})
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
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/config/`
Expected: FAIL — `undefined: Default`, `undefined: Load`.

- [ ] **Step 4: Write the implementation**

Create `internal/config/config.go`:

```go
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
	ReviewTimeout      Duration `yaml:"review_timeout"`
	PendingMaxAttempts int      `yaml:"pending_max_attempts"`
	PendingMaxAge      Duration `yaml:"pending_max_age"`
	FetchLimit         int      `yaml:"fetch_limit"`
	AllowOwners        []string `yaml:"allow_owners"`
	DenyRepos          []string `yaml:"deny_repos"`
	ClaudeArgs         []string `yaml:"claude_args"`
	StateDir           string   `yaml:"state_dir"`
	Paths              Paths    `yaml:"paths"`
}

// Default is the shipped configuration: dry-run, this user's org only.
func Default() Config {
	return Config{
		Space:              "spaces/EXAMPLE123",
		GithubLogin:        "angelov-todor",
		Interval:           Duration(5 * time.Minute),
		DryRun:             true,
		MaxReviewsPerSweep: 3,
		ReviewTimeout:      Duration(20 * time.Minute),
		PendingMaxAttempts: 20,
		PendingMaxAge:      Duration(168 * time.Hour),
		FetchLimit:         50,
		AllowOwners:        []string{"Example-Org"},
		ClaudeArgs:         []string{"--permission-mode", "bypassPermissions"},
		StateDir:           DefaultStateDir(),
		Paths: Paths{
			// python, not python3: the git-bash python3 on this machine cannot
			// import keyring, so chat.py fails to start there.
			Python: "python",
			Claude: "claude",
			Git:    "git",
			GH:     "gh",
			ChatScript: filepath.Join(os.Getenv("USERPROFILE"), "projects", "github.com",
				"example-org", ".claude", "skills", "google-chat", "scripts", "chat.py"),
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
		return errors.New("space is required")
	}
	if c.GithubLogin == "" {
		return errors.New("github_login is required: without it firstpass would review your own PRs")
	}
	if len(c.AllowOwners) == 0 {
		return errors.New("allow_owners must not be empty: an empty list would permit review comments on any org's PRs")
	}
	if c.MaxReviewsPerSweep <= 0 {
		return errors.New("max_reviews_per_sweep must be positive")
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
	if c.StateDir == "" {
		return errors.New("state_dir is required")
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
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
gofmt -l . && go vet ./...
git add go.mod go.sum internal/config
git commit -m "$(cat <<'EOF'
feat: configuration with safe defaults

dry_run true and allow_owners [Example-Org] by default; an empty
allow_owners fails validation rather than becoming an allow-all. Duration
values need a wrapper type because yaml.v3 decodes "5m" into a
time.Duration as a silent zero.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: State store

**Files:**
- Create: `internal/store/store.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Outcome string` with constants `OutcomeReviewed`, `OutcomeSkippedAuthor`, `OutcomeSkippedState`, `OutcomeSkippedOwner`, `OutcomeSkippedRepo`, `OutcomeNeedsAttention`, `OutcomeExpired`, `OutcomeInFlight`; `func (Outcome) Terminal() bool`; `type Review`, `type Pending`, `type Watermark`; `func Open(path string) (*Store, error)`; and on `*Store`: `Close`, `Watermark`, `SetWatermark`, `Review`, `PutReview`, `DeleteReview`, `Reviews`, `Pending`, `PutPending`, `DeletePending`, `AllPending`.

- [ ] **Step 1: Add the bbolt dependency**

```bash
go get go.etcd.io/bbolt
```

- [ ] **Step 2: Write the failing test**

Create `internal/store/store_test.go`:

```go
package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openAt(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestWatermarkRoundTrip(t *testing.T) {
	s := openAt(t, t.TempDir())

	if _, ok, err := s.Watermark(); err != nil || ok {
		t.Fatalf("a fresh store must have no watermark (ok=%v err=%v)", ok, err)
	}

	want := Watermark{
		MessageName: "spaces/EXAMPLE123/messages/abc",
		CreateTime:  time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC),
	}
	if err := s.SetWatermark(want); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.Watermark()
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.MessageName != want.MessageName || !got.CreateTime.Equal(want.CreateTime) {
		t.Errorf("Watermark() = %+v, want %+v", got, want)
	}
}

func TestInFlightSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	if err := s.PutReview(Review{
		Key: "o/r#1", Outcome: OutcomeInFlight, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := openAt(t, dir)
	got, ok, err := s2.Review("o/r#1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.Outcome != OutcomeInFlight {
		t.Errorf("Outcome = %q; in_flight must survive a restart or a crashed review is undetectable", got.Outcome)
	}
}

func TestTerminalClassification(t *testing.T) {
	for _, o := range []Outcome{
		OutcomeReviewed, OutcomeSkippedAuthor, OutcomeSkippedState,
		OutcomeSkippedOwner, OutcomeSkippedRepo, OutcomeNeedsAttention, OutcomeExpired,
	} {
		if !o.Terminal() {
			t.Errorf("%q must be terminal", o)
		}
	}
	if OutcomeInFlight.Terminal() {
		t.Error("in_flight must not be terminal: finding one later is how a crash is detected")
	}
}

func TestPendingLifecycle(t *testing.T) {
	s := openAt(t, t.TempDir())

	p := Pending{Key: "o/r#2", FirstSeen: time.Now().UTC(), Attempts: 1, LastReason: "draft"}
	if err := s.PutPending(p); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.Pending("o/r#2")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.Attempts != 1 || got.LastReason != "draft" {
		t.Errorf("Pending() = %+v", got)
	}

	all, err := s.AllPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("AllPending() = %d entries, want 1", len(all))
	}

	if err := s.DeletePending("o/r#2"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Pending("o/r#2"); ok {
		t.Error("a deleted pending entry must be gone")
	}
}

func TestDeletePendingOnAbsentKeyIsNotAnError(t *testing.T) {
	s := openAt(t, t.TempDir())
	if err := s.DeletePending("o/r#404"); err != nil {
		t.Errorf("deleting an absent pending key must be a no-op: %v", err)
	}
}

func TestReviewsAndDeleteReview(t *testing.T) {
	s := openAt(t, t.TempDir())
	if err := s.PutReview(Review{Key: "o/r#1", Outcome: OutcomeReviewed}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutReview(Review{Key: "o/r#2", Outcome: OutcomeSkippedAuthor}); err != nil {
		t.Fatal(err)
	}

	all, err := s.Reviews()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("Reviews() = %d entries, want 2", len(all))
	}

	if err := s.DeleteReview("o/r#1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Review("o/r#1"); ok {
		t.Error("DeleteReview must clear the record so replay can re-review")
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/store/`
Expected: FAIL — `undefined: Open`, `undefined: Review`.

- [ ] **Step 4: Write the implementation**

Create `internal/store/store.go`:

```go
// Package store persists what firstpass has already decided, so a restart or a
// crash never causes a second review of the same pull request.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Outcome is the recorded result for one pull request.
type Outcome string

const (
	OutcomeReviewed       Outcome = "reviewed"
	OutcomeSkippedAuthor  Outcome = "skipped_author"
	OutcomeSkippedState   Outcome = "skipped_state"
	OutcomeSkippedOwner   Outcome = "skipped_owner"
	OutcomeSkippedRepo    Outcome = "skipped_repo"
	OutcomeNeedsAttention Outcome = "needs_attention"
	OutcomeExpired        Outcome = "expired"
	OutcomeInFlight       Outcome = "in_flight"
)

// Terminal reports whether the outcome closes the book on a pull request.
// in_flight is the only non-terminal outcome: finding one on a later sweep is
// how a review that was killed part-way through is detected.
func (o Outcome) Terminal() bool { return o != OutcomeInFlight }

// Review is the record for a pull request firstpass has acted on.
type Review struct {
	Key            string    `json:"key"`
	Outcome        Outcome   `json:"outcome"`
	HeadSHA        string    `json:"head_sha,omitempty"`
	TriggerMessage string    `json:"trigger_message,omitempty"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	DecidedAt      time.Time `json:"decided_at,omitempty"`
	DurationMS     int64     `json:"duration_ms,omitempty"`
	ExitCode       int       `json:"exit_code,omitempty"`
	ReportPath     string    `json:"report_path,omitempty"`
	Detail         string    `json:"detail,omitempty"`
}

// Pending is a pull request deferred to a later sweep.
type Pending struct {
	Key         string    `json:"key"`
	FirstSeen   time.Time `json:"first_seen"`
	Attempts    int       `json:"attempts"`
	LastAttempt time.Time `json:"last_attempt"`
	LastReason  string    `json:"last_reason"`
}

// Watermark is the newest chat message already processed. The message name is
// stable and unique; CreateTime has ties and clock skew, so it is only a coarse
// bound.
type Watermark struct {
	MessageName string    `json:"message_name"`
	CreateTime  time.Time `json:"create_time"`
}

var (
	bucketMeta    = []byte("meta")
	bucketReviews = []byte("reviews")
	bucketPending = []byte("pending")
	keyWatermark  = []byte("watermark")
)

// Store is a bbolt-backed record of past decisions.
type Store struct{ db *bolt.DB }

// Open creates the database and its buckets if they do not exist.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketMeta, bucketReviews, bucketPending} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Watermark() (Watermark, bool, error) {
	var w Watermark
	ok, err := s.get(bucketMeta, keyWatermark, &w)
	return w, ok, err
}

func (s *Store) SetWatermark(w Watermark) error { return s.put(bucketMeta, keyWatermark, w) }

func (s *Store) Review(key string) (Review, bool, error) {
	var r Review
	ok, err := s.get(bucketReviews, []byte(key), &r)
	return r, ok, err
}

func (s *Store) PutReview(r Review) error { return s.put(bucketReviews, []byte(r.Key), r) }

// DeleteReview clears a terminal record so replay can review the PR again.
func (s *Store) DeleteReview(key string) error { return s.del(bucketReviews, key) }

func (s *Store) Reviews() ([]Review, error) {
	var out []Review
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketReviews).ForEach(func(_, v []byte) error {
			var r Review
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			out = append(out, r)
			return nil
		})
	})
	return out, err
}

func (s *Store) Pending(key string) (Pending, bool, error) {
	var p Pending
	ok, err := s.get(bucketPending, []byte(key), &p)
	return p, ok, err
}

func (s *Store) PutPending(p Pending) error { return s.put(bucketPending, []byte(p.Key), p) }

func (s *Store) DeletePending(key string) error { return s.del(bucketPending, key) }

func (s *Store) AllPending() ([]Pending, error) {
	var out []Pending
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPending).ForEach(func(_, v []byte) error {
			var p Pending
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			out = append(out, p)
			return nil
		})
	})
	return out, err
}

func (s *Store) put(bucket, key []byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put(key, b)
	})
}

func (s *Store) del(bucket []byte, key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Delete([]byte(key))
	})
}

func (s *Store) get(bucket, key []byte, dst any) (bool, error) {
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucket).Get(key)
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, dst)
	})
	if err != nil {
		return false, fmt.Errorf("read %s/%s: %w", bucket, key, err)
	}
	return found, nil
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/store/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
gofmt -l . && go vet ./...
git add go.mod go.sum internal/store
git commit -m "$(cat <<'EOF'
feat: bbolt state store for watermark, reviews and pending

in_flight is the sole non-terminal outcome, and it must survive a restart:
finding one on a later sweep is how a review killed mid-post is detected.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Subprocess runner

Every package that shells out takes a `Runner`, which is what lets the pipeline tests run with no subprocesses at all. `Fake` lives in a non-test file because other packages' tests need it.

**Files:**
- Create: `internal/runner/runner.go`
- Create: `internal/runner/fake.go`
- Test: `internal/runner/runner_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Result struct { Stdout, Stderr []byte; ExitCode int }`; `type Runner interface { Run(ctx context.Context, dir, name string, args ...string) (Result, error) }`; `type OS struct{}`; `type Call struct { Dir, Name string; Args []string }` with `String()`; `type Reply struct { Match string; Result Result; Err error }`; `type Fake struct { Calls []Call; Replies []Reply }`.

> The contract that matters: **a non-zero exit is data, not a Go error.** Callers check `Result.ExitCode`. The one exception is a cancelled or timed-out context, which must surface as an error — otherwise a review killed by its deadline would look like a clean run, and the pipeline would record it as reviewed.

- [ ] **Step 1: Write the failing test**

Create `internal/runner/runner_test.go`:

```go
package runner

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOSRunCapturesStdout(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("uses cmd.exe")
	}
	res, err := OS{}.Run(context.Background(), "", "cmd", "/c", "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(string(res.Stdout), "hello") {
		t.Errorf("Stdout = %q", res.Stdout)
	}
}

func TestOSRunReportsNonZeroExitAsDataNotError(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("uses cmd.exe")
	}
	res, err := OS{}.Run(context.Background(), "", "cmd", "/c", "exit 3")
	if err != nil {
		t.Fatalf("a non-zero exit must not be a Go error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
}

func TestOSRunSurfacesContextDeadline(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("uses cmd.exe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := OS{}.Run(ctx, "", "cmd", "/c", "ping -n 10 127.0.0.1 > NUL")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a timeout must surface as context.DeadlineExceeded, got %v; "+
			"otherwise a killed review looks like a clean run", err)
	}
}

func TestOSRunReportsMissingBinary(t *testing.T) {
	if _, err := (OS{}).Run(context.Background(), "", "firstpass-no-such-binary"); err == nil {
		t.Error("a missing executable must be an error")
	}
}

func TestFakeMatchesBySubstring(t *testing.T) {
	f := &Fake{Replies: []Reply{
		{Match: "get-messages", Result: Result{Stdout: []byte("{}")}},
	}}

	res, err := f.Run(context.Background(), "", "python", "chat.py", "get-messages", "spaces/A")
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "{}" {
		t.Errorf("Stdout = %q", res.Stdout)
	}
	if len(f.Calls) != 1 || f.Calls[0].Name != "python" {
		t.Errorf("Calls = %+v", f.Calls)
	}
}

func TestFakeErrorsOnUnconfiguredCommand(t *testing.T) {
	f := &Fake{}
	if _, err := f.Run(context.Background(), "", "gh", "pr", "view"); err == nil {
		t.Error("an unconfigured command must fail loudly, not return an empty result")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/runner/`
Expected: FAIL — `undefined: OS`, `undefined: Fake`.

- [ ] **Step 3: Write the implementation**

Create `internal/runner/runner.go`:

```go
// Package runner is the single seam between firstpass and the external programs
// it drives, so tests can run the decision logic without subprocesses.
package runner

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// Result is the outcome of one command.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner executes an external command in dir (empty means the current
// directory).
//
// A non-zero exit status is returned as Result.ExitCode with a nil error:
// callers decide whether it matters. A cancelled or timed-out context is the
// exception and is always returned as an error, so a review killed by its
// deadline cannot be mistaken for a clean run.
type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) (Result, error)
}

// OS runs commands for real.
type OS struct{}

func (OS) Run(ctx context.Context, dir, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir

	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb

	err := cmd.Run()
	res := Result{Stdout: out.Bytes(), Stderr: errb.Bytes()}

	// Check the context first: killing the process produces an ExitError too,
	// and treating that as a plain non-zero exit would hide the timeout.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, ctxErr
	}

	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	if err != nil {
		return res, err
	}
	return res, nil
}
```

Create `internal/runner/fake.go`:

```go
package runner

import (
	"context"
	"fmt"
	"strings"
)

// Call records one invocation made against a Fake.
type Call struct {
	Dir  string
	Name string
	Args []string
}

func (c Call) String() string {
	return strings.Join(append([]string{c.Name}, c.Args...), " ")
}

// Reply is a canned response, selected when Match is a substring of the
// command line.
type Reply struct {
	Match  string
	Result Result
	Err    error
}

// Fake is a Runner for tests. It records every call and replies from Replies;
// an unmatched command is an error, so a test can never silently exercise a
// path it did not set up.
type Fake struct {
	Calls   []Call
	Replies []Reply
}

func (f *Fake) Run(_ context.Context, dir, name string, args ...string) (Result, error) {
	c := Call{Dir: dir, Name: name, Args: args}
	f.Calls = append(f.Calls, c)

	line := c.String()
	for _, r := range f.Replies {
		if strings.Contains(line, r.Match) {
			return r.Result, r.Err
		}
	}
	return Result{}, fmt.Errorf("fake runner: no reply configured for %q", line)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/runner/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/runner
git commit -m "$(cat <<'EOF'
feat: subprocess runner seam with a recording fake

A non-zero exit is data, not an error; a cancelled context always is one,
so a review killed by its deadline cannot look like a clean run.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---
### Task 5: Google Chat reader

**Files:**
- Create: `internal/chat/chat.go`
- Create: `internal/chat/testdata/messages_noisy.json`
- Create: `internal/chat/testdata/error_scope.json`
- Test: `internal/chat/chat_test.go`

**Interfaces:**
- Consumes: `runner.Runner`, `runner.Fake`, `runner.Result`, `runner.Reply` from Task 4.
- Produces: `type Message struct { Name, Text string; CreateTime time.Time; Sender struct{ Name string } }`; `func New(r runner.Runner, python, script, space string) *Client`; `func (*Client) Fetch(ctx context.Context, sinceName string, limit int) ([]Message, error)`; `func (*Client) HasNamedRooms(ctx context.Context) (bool, error)`; `type APIError struct { Code int; Status, Message string }` with `Error()` and `Fatal() bool`; `func StripLeadingNoise(b []byte) []byte`.

> `Fetch` takes `sinceName string`, not a `store.Watermark`, so this package does not import `store`.
>
> Two documented realities of `chat.py` drive this package. It prints `Access token expired, refreshing...` on stdout *ahead of* the JSON, which breaks a naive decode even though the call succeeded. And two Google accounts exist on this machine — the personal one lists zero named rooms, which is indistinguishable from "nobody posted a PR" unless it is checked for explicitly.

- [ ] **Step 1: Create the test fixtures**

Create `internal/chat/testdata/messages_noisy.json` — note the two noise lines before the JSON, and that messages are newest-first:

```
Access token expired, refreshing...
Token refreshed successfully.
{
  "messages": [
    {
      "name": "spaces/A/messages/m3",
      "text": "Please review the following PRs — margin follow-ups:\nhttps://github.com/Example-Org/aex-balances/pull/12",
      "createTime": "2026-09-03T09:00:00Z",
      "sender": { "name": "users/111" }
    },
    {
      "name": "spaces/A/messages/m2",
      "text": "Example-Org/aex-history-service#48 is ready",
      "createTime": "2026-09-03T08:00:00Z",
      "sender": { "name": "users/222" }
    },
    {
      "name": "spaces/A/messages/m1",
      "text": "standup in 5",
      "createTime": "2026-09-03T07:00:00Z",
      "sender": { "name": "users/222" }
    }
  ]
}
```

Create `internal/chat/testdata/error_scope.json`:

```json
{
  "error": {
    "code": 403,
    "message": "Request had insufficient authentication scopes. ACCESS_TOKEN_SCOPE_INSUFFICIENT",
    "status": "PERMISSION_DENIED"
  }
}
```

- [ ] **Step 2: Write the failing test**

Create `internal/chat/chat_test.go`:

```go
package chat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/angelov-todor/firstpass/internal/runner"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func clientWith(stdout []byte, match string) (*Client, *runner.Fake) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: match, Result: runner.Result{Stdout: stdout}},
	}}
	return New(f, "python", "chat.py", "spaces/A"), f
}

func TestStripLeadingNoise(t *testing.T) {
	in := []byte("Access token expired, refreshing...\nToken refreshed successfully.\n{\"messages\":[]}")
	if got := string(StripLeadingNoise(in)); got != `{"messages":[]}` {
		t.Errorf("StripLeadingNoise() = %q", got)
	}
}

func TestStripLeadingNoiseLeavesCleanJSONAlone(t *testing.T) {
	in := []byte(`{"messages":[]}`)
	if got := string(StripLeadingNoise(in)); got != `{"messages":[]}` {
		t.Errorf("StripLeadingNoise() = %q", got)
	}
}

func TestFetchParsesNoisyOutput(t *testing.T) {
	c, f := clientWith(fixture(t, "messages_noisy.json"), "get-messages")

	msgs, err := c.Fetch(context.Background(), "", 50)
	if err != nil {
		t.Fatalf("the token-refresh preamble must not break decoding: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if msgs[0].Name != "spaces/A/messages/m3" {
		t.Errorf("messages must stay newest-first, got %q", msgs[0].Name)
	}
	if msgs[0].CreateTime.IsZero() {
		t.Error("createTime must decode")
	}
	if len(f.Calls) != 1 {
		t.Fatalf("Calls = %+v", f.Calls)
	}
	line := f.Calls[0].String()
	for _, want := range []string{"chat.py", "get-messages", "spaces/A", "--limit", "50"} {
		if !contains(line, want) {
			t.Errorf("command %q missing %q", line, want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func TestFetchStopsAtWatermark(t *testing.T) {
	c, _ := clientWith(fixture(t, "messages_noisy.json"), "get-messages")

	msgs, err := c.Fetch(context.Background(), "spaces/A/messages/m2", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Name != "spaces/A/messages/m3" {
		t.Fatalf("the watermark must exclude itself and everything older, got %+v", msgs)
	}
}

func TestFetchWithUnknownWatermarkReturnsEverything(t *testing.T) {
	c, _ := clientWith(fixture(t, "messages_noisy.json"), "get-messages")

	msgs, err := c.Fetch(context.Background(), "spaces/A/messages/gone", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("a watermark that has scrolled out of the window must not drop messages, got %d", len(msgs))
	}
}

func TestFetchSurfacesScopeErrorAsFatal(t *testing.T) {
	c, _ := clientWith(fixture(t, "error_scope.json"), "get-messages")

	_, err := c.Fetch(context.Background(), "", 50)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if !apiErr.Fatal() {
		t.Error("an insufficient scope must be fatal; retrying can never fix it")
	}
}

func TestFetchReportsNonZeroExit(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "get-messages", Result: runner.Result{ExitCode: 1, Stderr: []byte("boom")}},
	}}
	c := New(f, "python", "chat.py", "spaces/A")

	if _, err := c.Fetch(context.Background(), "", 50); err == nil {
		t.Error("a non-zero exit from chat.py must be an error")
	}
}

func TestHasNamedRoomsDetectsWrongAccount(t *testing.T) {
	c, _ := clientWith([]byte(`{"spaces":[{"name":"spaces/X","displayName":null}]}`), "list-spaces")

	ok, err := c.HasNamedRooms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("an account listing only unnamed spaces is the wrong account and must report false")
	}
}

func TestHasNamedRoomsAcceptsRealAccount(t *testing.T) {
	c, _ := clientWith([]byte(`{"spaces":[{"name":"spaces/X","displayName":"[AstraEx] Team"}]}`), "list-spaces")

	ok, err := c.HasNamedRooms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("an account that can see a named room must report true")
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/chat/`
Expected: FAIL — `undefined: New`, `undefined: StripLeadingNoise`, `undefined: APIError`.

- [ ] **Step 4: Write the implementation**

Create `internal/chat/chat.go`:

```go
// Package chat reads the team's Google Chat space by driving the google-chat
// skill's chat.py, which already holds the OAuth token in the Windows
// Credential Locker.
package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/angelov-todor/firstpass/internal/runner"
)

// Message is one Google Chat message.
type Message struct {
	Name       string    `json:"name"`
	Text       string    `json:"text"`
	CreateTime time.Time `json:"createTime"`
	Sender     struct {
		Name string `json:"name"`
	} `json:"sender"`
}

type listResponse struct {
	Messages []Message `json:"messages"`
	Error    *apiErrorBody `json:"error"`
}

type spacesResponse struct {
	Spaces []struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	} `json:"spaces"`
	Error *apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// APIError is a Google Chat API error surfaced by chat.py.
type APIError struct {
	Code    int
	Status  string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("chat api %d %s: %s", e.Code, e.Status, e.Message)
}

// Fatal reports whether retrying could ever help. A missing OAuth scope or a
// revoked grant cannot be fixed by waiting, so the daemon should stop rather
// than log the same failure every five minutes forever.
func (e *APIError) Fatal() bool {
	if e.Status == "PERMISSION_DENIED" || e.Status == "UNAUTHENTICATED" {
		return true
	}
	return strings.Contains(e.Message, "ACCESS_TOKEN_SCOPE_INSUFFICIENT")
}

// Client drives chat.py for one space.
type Client struct {
	r      runner.Runner
	python string
	script string
	space  string
}

func New(r runner.Runner, python, script, space string) *Client {
	return &Client{r: r, python: python, script: script, space: space}
}

// Fetch returns messages newest-first, stopping before sinceName. An empty
// sinceName returns everything the limit allows. A sinceName that is not in the
// window returns everything, which is the safe direction: it re-offers messages
// whose PRs the store will then filter out, rather than dropping new ones.
func (c *Client) Fetch(ctx context.Context, sinceName string, limit int) ([]Message, error) {
	res, err := c.r.Run(ctx, "", c.python, c.script,
		"get-messages", c.space, "--limit", strconv.Itoa(limit))
	if err != nil {
		return nil, fmt.Errorf("run chat.py get-messages: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("chat.py get-messages exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	var lr listResponse
	if err := json.Unmarshal(StripLeadingNoise(res.Stdout), &lr); err != nil {
		return nil, fmt.Errorf("decode chat.py output: %w", err)
	}
	if lr.Error != nil {
		return nil, &APIError{Code: lr.Error.Code, Status: lr.Error.Status, Message: lr.Error.Message}
	}

	if sinceName == "" {
		return lr.Messages, nil
	}
	for i, m := range lr.Messages {
		if m.Name == sinceName {
			return lr.Messages[:i], nil
		}
	}
	return lr.Messages, nil
}

// HasNamedRooms reports whether the authenticated account can see any named
// space. Two Google accounts exist on this machine, and the personal one lists
// none; without this check, sweeping the wrong account looks exactly like
// "nobody posted a PR".
func (c *Client) HasNamedRooms(ctx context.Context) (bool, error) {
	res, err := c.r.Run(ctx, "", c.python, c.script, "list-spaces")
	if err != nil {
		return false, fmt.Errorf("run chat.py list-spaces: %w", err)
	}
	if res.ExitCode != 0 {
		return false, fmt.Errorf("chat.py list-spaces exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	var sr spacesResponse
	if err := json.Unmarshal(StripLeadingNoise(res.Stdout), &sr); err != nil {
		return false, fmt.Errorf("decode chat.py output: %w", err)
	}
	if sr.Error != nil {
		return false, &APIError{Code: sr.Error.Code, Status: sr.Error.Status, Message: sr.Error.Message}
	}
	for _, s := range sr.Spaces {
		if s.DisplayName != "" {
			return true, nil
		}
	}
	return false, nil
}

// StripLeadingNoise drops anything chat.py prints before its JSON payload. It
// writes "Access token expired, refreshing..." and "Token refreshed
// successfully." to stdout ahead of the response, which makes a naive decode
// fail even though the call succeeded.
func StripLeadingNoise(b []byte) []byte {
	for i, ch := range b {
		if ch == '{' || ch == '[' {
			return b[i:]
		}
	}
	return b
}
```

- [ ] **Step 5: Simplify the test helpers**

The hand-rolled `contains`/`indexOf` in the test were only there to avoid an import. Replace them with `strings.Contains` and delete both helpers:

```go
// at the top of chat_test.go, add "strings" to the imports, then in
// TestFetchParsesNoisyOutput use:
		if !strings.Contains(line, want) {
			t.Errorf("command %q missing %q", line, want)
		}
```

Delete the `contains` and `indexOf` functions entirely.

- [ ] **Step 6: Run the test to verify it passes**

Run: `go test ./internal/chat/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/chat
git commit -m "$(cat <<'EOF'
feat: Google Chat reader over the google-chat skill's chat.py

Strips the token-refresh preamble chat.py prints ahead of its JSON, stops
at the watermark, and reports an account that can see no named rooms --
the tell for the wrong Google account, which otherwise looks identical to
an empty space.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: GitHub PR inspection

**Files:**
- Create: `internal/ghpr/ghpr.go`
- Test: `internal/ghpr/ghpr_test.go`

**Interfaces:**
- Consumes: `prref.PRRef` (Task 1), `runner.Runner` (Task 4).
- Produces: `type PRInfo struct { State string; IsDraft bool; Author, HeadSHA string }`; `func New(r runner.Runner, gh string) *Client`; `func (*Client) Inspect(ctx context.Context, ref prref.PRRef) (PRInfo, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/ghpr/ghpr_test.go`:

```go
package ghpr

import (
	"context"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/runner"
)

var ref = prref.PRRef{Owner: "Example-Org", Repo: "aex-balances", Number: 12}

func TestInspectParsesGhJSON(t *testing.T) {
	body := `{"state":"OPEN","isDraft":false,"author":{"login":"stefan-cvetkovic"},"headRefOid":"deadbeef"}`
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "pr view", Result: runner.Result{Stdout: []byte(body)}},
	}}

	got, err := New(f, "gh").Inspect(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	want := PRInfo{State: "OPEN", IsDraft: false, Author: "stefan-cvetkovic", HeadSHA: "deadbeef"}
	if got != want {
		t.Errorf("Inspect() = %+v, want %+v", got, want)
	}

	line := f.Calls[0].String()
	for _, want := range []string{"pr", "view", "12", "--repo", "Example-Org/aex-balances", "state,isDraft,author,headRefOid"} {
		if !strings.Contains(line, want) {
			t.Errorf("command %q missing %q", line, want)
		}
	}
}

func TestInspectParsesDraft(t *testing.T) {
	body := `{"state":"OPEN","isDraft":true,"author":{"login":"x"},"headRefOid":"a"}`
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "pr view", Result: runner.Result{Stdout: []byte(body)}},
	}}

	got, err := New(f, "gh").Inspect(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsDraft {
		t.Error("IsDraft must decode as true")
	}
}

func TestInspectReportsNonZeroExit(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "pr view", Result: runner.Result{ExitCode: 1, Stderr: []byte("could not resolve to a PullRequest")}},
	}}

	_, err := New(f, "gh").Inspect(context.Background(), ref)
	if err == nil {
		t.Fatal("a non-zero gh exit must be an error")
	}
	if !strings.Contains(err.Error(), ref.Key()) {
		t.Errorf("the error must name the PR, got %q", err)
	}
}

func TestInspectReportsBadJSON(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "pr view", Result: runner.Result{Stdout: []byte("not json")}},
	}}

	if _, err := New(f, "gh").Inspect(context.Background(), ref); err == nil {
		t.Error("undecodable gh output must be an error")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/ghpr/`
Expected: FAIL — `undefined: New`, `undefined: PRInfo`.

- [ ] **Step 3: Write the implementation**

Create `internal/ghpr/ghpr.go`:

```go
// Package ghpr answers the questions firstpass asks about a pull request before
// deciding whether to review it.
package ghpr

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/runner"
)

// PRInfo is the subset of pull request state firstpass's filters need.
type PRInfo struct {
	State   string
	IsDraft bool
	Author  string
	HeadSHA string
}

type rawPR struct {
	State   string `json:"state"`
	IsDraft bool   `json:"isDraft"`
	Author  struct {
		Login string `json:"login"`
	} `json:"author"`
	HeadRefOid string `json:"headRefOid"`
}

// Client wraps the gh CLI, which already holds this machine's GitHub auth.
type Client struct {
	r  runner.Runner
	gh string
}

func New(r runner.Runner, gh string) *Client { return &Client{r: r, gh: gh} }

func (c *Client) Inspect(ctx context.Context, ref prref.PRRef) (PRInfo, error) {
	res, err := c.r.Run(ctx, "", c.gh,
		"pr", "view", strconv.Itoa(ref.Number),
		"--repo", ref.Owner+"/"+ref.Repo,
		"--json", "state,isDraft,author,headRefOid")
	if err != nil {
		return PRInfo{}, fmt.Errorf("gh pr view %s: %w", ref.Key(), err)
	}
	if res.ExitCode != 0 {
		return PRInfo{}, fmt.Errorf("gh pr view %s exit %d: %s",
			ref.Key(), res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	var v rawPR
	if err := json.Unmarshal(res.Stdout, &v); err != nil {
		return PRInfo{}, fmt.Errorf("decode gh output for %s: %w", ref.Key(), err)
	}
	return PRInfo{
		State:   v.State,
		IsDraft: v.IsDraft,
		Author:  v.Author.Login,
		HeadSHA: v.HeadRefOid,
	}, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/ghpr/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/ghpr
git commit -m "$(cat <<'EOF'
feat: read PR state, draft flag, author and head SHA via gh

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: Sweep pipeline

The whole decision table lives here, behind four interfaces, so every rule is tested in milliseconds with no subprocess and no network. This is the task where correctness matters most: a mistake here posts comments on the wrong PR, or silently posts none at all.

**Files:**
- Create: `internal/pipeline/pipeline.go`
- Test: `internal/pipeline/pipeline_test.go`

**Interfaces:**
- Consumes: `config.Config` (Task 2), `*store.Store` and its types (Task 3), `chat.Message` (Task 5), `ghpr.PRInfo` (Task 6), `prref.PRRef`/`Extract`/`ParseKey` (Task 1), `review.Result` (Task 9 — declared there; for this task use the field set `{ExitCode int; ReportPath string; Stdout []byte}`).
- Produces: interfaces `ChatSource`, `PRInspector`, `Worktrees`, `Reviewer`; `type Action string` with `ActionReview`, `ActionSkip`, `ActionDefer`, `ActionNeedsAttention`, `ActionWouldReview`; `type Decision struct { Ref prref.PRRef; Action Action; Reason string }`; `type SweepReport struct { MessagesScanned int; ColdStart, Paused bool; Decisions []Decision; Reviewed int }`; `type Options struct { PrintOnly bool; Backfill int }`; `type Pipeline struct { Cfg config.Config; Store *store.Store; Chat ChatSource; PRs PRInspector; WTs Worktrees; Rev Reviewer; Log *slog.Logger; Now func() time.Time }` with `func (*Pipeline) Sweep(ctx context.Context, opts Options) (SweepReport, error)`.

> **Order matters and is load-bearing.** Owner allowlist first (so a stranger's repo is never even queried), then the existing record (so `in_flight` is converted before anything else touches the PR), then pause and the per-sweep cap (which park the ref *without* counting an attempt), and only then GitHub. Reordering any of these changes behaviour.

- [ ] **Step 1: Create the review.Result placeholder**

`pipeline` needs `review.Result` to compile, and Task 9 builds the real package. Create the type now so Task 7 stands alone; Task 9 fills in the rest of the file.

Create `internal/review/review.go`:

```go
// Package review runs a code review over a prepared checkout.
package review

// Result is the outcome of one review run.
type Result struct {
	ExitCode   int
	ReportPath string
	Stdout     []byte
}
```

- [ ] **Step 2: Write the failing test**

Create `internal/pipeline/pipeline_test.go`:

```go
package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/review"
	"github.com/angelov-todor/firstpass/internal/store"
)

// ---- fakes ----

type fakeChat struct {
	msgs      []chat.Message
	lastSince string
	lastLimit int
	err       error
}

func (f *fakeChat) Fetch(_ context.Context, since string, limit int) ([]chat.Message, error) {
	f.lastSince, f.lastLimit = since, limit
	if f.err != nil {
		return nil, f.err
	}
	if since == "" {
		return f.msgs, nil
	}
	for i, m := range f.msgs {
		if m.Name == since {
			return f.msgs[:i], nil
		}
	}
	return f.msgs, nil
}

type fakePRs struct {
	info map[string]ghpr.PRInfo
	err  map[string]error
}

func (f *fakePRs) Inspect(_ context.Context, ref prref.PRRef) (ghpr.PRInfo, error) {
	if e, ok := f.err[ref.Key()]; ok {
		return ghpr.PRInfo{}, e
	}
	if i, ok := f.info[ref.Key()]; ok {
		return i, nil
	}
	return ghpr.PRInfo{State: "OPEN", Author: "colleague", HeadSHA: "sha-" + ref.Repo}, nil
}

type fakeWTs struct {
	prepared  []string
	cleanedUp int
	err       error
}

func (f *fakeWTs) Prepare(_ context.Context, ref prref.PRRef) (string, func(), error) {
	if f.err != nil {
		return "", func() {}, f.err
	}
	f.prepared = append(f.prepared, ref.Key())
	return filepath.Join("work", ref.Repo), func() { f.cleanedUp++ }, nil
}

type fakeRev struct {
	ran []string
	err error
}

func (f *fakeRev) Run(_ context.Context, _ string, ref prref.PRRef) (review.Result, error) {
	f.ran = append(f.ran, ref.Key())
	if f.err != nil {
		return review.Result{}, f.err
	}
	return review.Result{ExitCode: 0}, nil
}

// ---- harness ----

type harness struct {
	p   *Pipeline
	st  *store.Store
	ch  *fakeChat
	prs *fakePRs
	wts *fakeWTs
	rev *fakeRev
	cfg config.Config
}

func newHarness(t *testing.T, msgs []chat.Message) *harness {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	cfg.StateDir = dir
	cfg.GithubLogin = "angelov-todor"
	cfg.AllowOwners = []string{"Example-Org"}

	h := &harness{
		st:  st,
		ch:  &fakeChat{msgs: msgs},
		prs: &fakePRs{info: map[string]ghpr.PRInfo{}, err: map[string]error{}},
		wts: &fakeWTs{},
		rev: &fakeRev{},
		cfg: cfg,
	}
	h.p = &Pipeline{
		Cfg: cfg, Store: st, Chat: h.ch, PRs: h.prs, WTs: h.wts, Rev: h.rev,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) },
	}
	return h
}

// seedWatermark takes the pipeline past the cold-start guard.
func (h *harness) seedWatermark(t *testing.T) {
	t.Helper()
	if err := h.st.SetWatermark(store.Watermark{
		MessageName: "spaces/A/messages/m0",
		CreateTime:  time.Date(2026, 9, 3, 6, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) apply() { h.p.Cfg = h.cfg }

func msg(name, text string) chat.Message {
	return chat.Message{Name: name, Text: text, CreateTime: time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)}
}

func prURL(repo string, n int) string {
	return "https://github.com/Example-Org/" + repo + "/pull/" + strconv.Itoa(n)
}

func decisionFor(rep SweepReport, key string) (Decision, bool) {
	for _, d := range rep.Decisions {
		if d.Ref.Key() == key {
			return d, true
		}
	}
	return Decision{}, false
}

// ---- tests ----

func TestColdStartReviewsNothingAndSetsWatermark(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m9", prURL("aex-balances", 12))})

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.ColdStart {
		t.Error("a first run must be reported as a cold start")
	}
	if rep.Reviewed != 0 || len(h.rev.ran) != 0 {
		t.Errorf("a first run against a populated space must review nothing, ran %v", h.rev.ran)
	}
	wm, ok, err := h.st.Watermark()
	if err != nil || !ok {
		t.Fatalf("cold start must set the watermark (ok=%v err=%v)", ok, err)
	}
	if wm.MessageName != "spaces/A/messages/m9" {
		t.Errorf("watermark = %q, want the newest message", wm.MessageName)
	}
}

func TestBackfillOverridesColdStart(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m9", prURL("aex-balances", 12))})

	rep, err := h.p.Sweep(context.Background(), Options{Backfill: 10})
	if err != nil {
		t.Fatal(err)
	}
	if rep.ColdStart {
		t.Error("an explicit backfill must not be treated as a cold start")
	}
	if len(h.rev.ran) != 1 {
		t.Errorf("backfill must review the PRs it finds, ran %v", h.rev.ran)
	}
	if h.ch.lastLimit != 10 || h.ch.lastSince != "" {
		t.Errorf("backfill must ignore the watermark and use its own limit (since=%q limit=%d)",
			h.ch.lastSince, h.ch.lastLimit)
	}
}

func TestReviewsNewPR(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 1 {
		t.Fatalf("Reviewed = %d, want 1 (decisions: %+v)", rep.Reviewed, rep.Decisions)
	}
	if len(h.wts.prepared) != 1 {
		t.Errorf("a worktree must be prepared, got %v", h.wts.prepared)
	}
	if h.wts.cleanedUp != 1 {
		t.Errorf("the worktree must be cleaned up, cleanedUp = %d", h.wts.cleanedUp)
	}

	rec, ok, err := h.st.Review("Example-Org/aex-balances#12")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if rec.Outcome != store.OutcomeReviewed {
		t.Errorf("Outcome = %q, want reviewed", rec.Outcome)
	}
	if rec.TriggerMessage != "spaces/A/messages/m1" {
		t.Errorf("TriggerMessage = %q", rec.TriggerMessage)
	}
	if rec.HeadSHA == "" {
		t.Error("HeadSHA must be recorded")
	}
}

func TestSkipsOwnPR(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	h.prs.info["Example-Org/aex-balances#12"] = ghpr.PRInfo{
		State: "OPEN", Author: "angelov-todor", HeadSHA: "sha",
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("own PRs must not be reviewed, ran %v", h.rev.ran)
	}
	d, _ := decisionFor(rep, "Example-Org/aex-balances#12")
	if d.Action != ActionSkip {
		t.Errorf("Action = %q, want skip", d.Action)
	}
	rec, _, _ := h.st.Review("Example-Org/aex-balances#12")
	if rec.Outcome != store.OutcomeSkippedAuthor {
		t.Errorf("Outcome = %q, want skipped_author", rec.Outcome)
	}
}

func TestSkipsMergedPRTerminally(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	h.prs.info["Example-Org/aex-balances#12"] = ghpr.PRInfo{State: "MERGED", Author: "colleague"}

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := h.st.Review("Example-Org/aex-balances#12")
	if !ok || rec.Outcome != store.OutcomeSkippedState {
		t.Errorf("a merged PR must be recorded terminally, got ok=%v outcome=%q", ok, rec.Outcome)
	}
	if _, pending, _ := h.st.Pending("Example-Org/aex-balances#12"); pending {
		t.Error("a merged PR must not linger in pending")
	}
}

func TestOwnerNotAllowedIsTerminalAndNeverQueried(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", "look at this https://github.com/torvalds/linux/pull/999"),
	})
	h.seedWatermark(t)
	h.prs.err["torvalds/linux#999"] = errors.New("must not be called")

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, ok := decisionFor(rep, "torvalds/linux#999")
	if !ok || d.Action != ActionSkip {
		t.Fatalf("an outside org must be skipped, got %+v", d)
	}
	rec, found, _ := h.st.Review("torvalds/linux#999")
	if !found || rec.Outcome != store.OutcomeSkippedOwner {
		t.Errorf("Outcome = %q, want skipped_owner", rec.Outcome)
	}
	if len(h.wts.prepared) != 0 {
		t.Error("an outside org's repo must never be cloned")
	}
}

func TestDeniedRepoIsSkipped(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-secret", 3))})
	h.seedWatermark(t)
	h.cfg.DenyRepos = []string{"Example-Org/aex-secret"}
	h.apply()

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := h.st.Review("Example-Org/aex-secret#3")
	if !ok || rec.Outcome != store.OutcomeSkippedRepo {
		t.Errorf("Outcome = %q, want skipped_repo", rec.Outcome)
	}
}

func TestDraftGoesToPendingThenIsReviewedWhenReady(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	key := "Example-Org/aex-balances#12"
	h.prs.info[key] = ghpr.PRInfo{State: "OPEN", IsDraft: true, Author: "colleague", HeadSHA: "sha"}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, key)
	if d.Action != ActionDefer {
		t.Fatalf("a draft must be deferred, got %q", d.Action)
	}
	pd, ok, _ := h.st.Pending(key)
	if !ok || pd.Attempts != 1 {
		t.Fatalf("a draft must count an attempt, got ok=%v attempts=%d", ok, pd.Attempts)
	}
	if _, found, _ := h.st.Review(key); found {
		t.Error("a draft must not get a terminal record; it may become ready later")
	}

	// Marked ready, and the message has since scrolled past the watermark.
	h.prs.info[key] = ghpr.PRInfo{State: "OPEN", IsDraft: false, Author: "colleague", HeadSHA: "sha"}
	h.ch.msgs = nil

	rep2, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Reviewed != 1 {
		t.Fatalf("a pending draft must be picked up once ready, decisions %+v", rep2.Decisions)
	}
	if _, ok, _ := h.st.Pending(key); ok {
		t.Error("a reviewed PR must be cleared from pending")
	}
}

func TestInspectFailureDefersAndRetries(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	key := "Example-Org/aex-balances#12"
	h.prs.err[key] = errors.New("network unreachable")

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	pd, ok, _ := h.st.Pending(key)
	if !ok || pd.Attempts != 1 {
		t.Fatalf("a transient gh failure must defer with an attempt, ok=%v attempts=%d", ok, pd.Attempts)
	}

	delete(h.prs.err, key)
	h.ch.msgs = nil
	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 1 {
		t.Errorf("the retry must review it, decisions %+v", rep.Decisions)
	}
}

func TestDedupeWithinBatch(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m2", "reposting "+prURL("aex-balances", 12)),
		msg("spaces/A/messages/m1", "please review "+prURL("aex-balances", 12)),
	})
	h.seedWatermark(t)

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 1 || len(h.rev.ran) != 1 {
		t.Fatalf("the same PR in two messages must be reviewed once, ran %v", h.rev.ran)
	}
	rec, _, _ := h.st.Review("Example-Org/aex-balances#12")
	if rec.TriggerMessage != "spaces/A/messages/m1" {
		t.Errorf("TriggerMessage = %q, want the oldest post in the batch", rec.TriggerMessage)
	}
}

func TestAlreadyReviewedIsSkipped(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	key := "Example-Org/aex-balances#12"
	if err := h.st.PutReview(store.Review{Key: key, Outcome: store.OutcomeReviewed}); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("an already-reviewed PR must not be reviewed again, ran %v", h.rev.ran)
	}
	d, _ := decisionFor(rep, key)
	if d.Action != ActionSkip {
		t.Errorf("Action = %q, want skip", d.Action)
	}
}

func TestPerSweepCapDefersTheRestWithoutBurningAttempts(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m1", prURL("a", 1)+" "+prURL("b", 2)+" "+prURL("c", 3)),
	})
	h.seedWatermark(t)
	h.cfg.MaxReviewsPerSweep = 2
	h.apply()

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reviewed != 2 {
		t.Fatalf("Reviewed = %d, want 2 (the cap)", rep.Reviewed)
	}

	all, err := h.st.AllPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("AllPending() = %d, want the one over the cap", len(all))
	}
	if all[0].Attempts != 0 {
		t.Errorf("Attempts = %d; hitting the cap is not a failure and must not count against expiry", all[0].Attempts)
	}
}

func TestPauseFileStopsReviewsWithoutBurningAttempts(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	if err := os.WriteFile(h.cfg.PauseFile(), []byte("paused"), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Paused {
		t.Error("the report must say the sweep was paused")
	}
	if len(h.rev.ran) != 0 {
		t.Errorf("a paused sweep must post nothing, ran %v", h.rev.ran)
	}
	pd, ok, _ := h.st.Pending("Example-Org/aex-balances#12")
	if !ok {
		t.Fatal("a paused sweep must still queue the ref")
	}
	if pd.Attempts != 0 {
		t.Errorf("Attempts = %d; a long pause must not silently expire the backlog", pd.Attempts)
	}
}

func TestInFlightBecomesNeedsAttentionAndIsNotRetried(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	key := "Example-Org/aex-balances#12"
	if err := h.st.PutReview(store.Review{
		Key: key, Outcome: store.OutcomeInFlight, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, key)
	if d.Action != ActionNeedsAttention {
		t.Fatalf("Action = %q, want needs_attention", d.Action)
	}
	if len(h.rev.ran) != 0 {
		t.Error("a review that died mid-post must not be retried automatically: comments may already be on the PR")
	}
	rec, _, _ := h.st.Review(key)
	if rec.Outcome != store.OutcomeNeedsAttention {
		t.Errorf("Outcome = %q, want needs_attention", rec.Outcome)
	}
	if rec.Detail == "" {
		t.Error("the record must explain what happened and how to act on it")
	}
}

func TestReviewFailureBecomesNeedsAttention(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	h.rev.err = context.DeadlineExceeded

	rep, err := h.p.Sweep(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	key := "Example-Org/aex-balances#12"
	d, _ := decisionFor(rep, key)
	if d.Action != ActionNeedsAttention {
		t.Fatalf("Action = %q, want needs_attention", d.Action)
	}
	rec, _, _ := h.st.Review(key)
	if rec.Outcome != store.OutcomeNeedsAttention {
		t.Errorf("Outcome = %q; a timeout may have fired mid-post, so it must not be retried", rec.Outcome)
	}
	if _, ok, _ := h.st.Pending(key); ok {
		t.Error("a needs_attention PR must not also sit in pending, or it would be retried")
	}
	if h.wts.cleanedUp != 1 {
		t.Error("the worktree must be cleaned up even when the review fails")
	}
}

func TestWorktreeFailureDefers(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)
	h.wts.err = errors.New("clone failed")

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	key := "Example-Org/aex-balances#12"
	pd, ok, _ := h.st.Pending(key)
	if !ok || pd.Attempts != 1 {
		t.Fatalf("a clone failure happens before claude starts, so it must defer: ok=%v attempts=%d", ok, pd.Attempts)
	}
	if _, found, _ := h.st.Review(key); found {
		t.Error("nothing was posted, so there must be no terminal record")
	}
}

func TestWatermarkAdvancesToNewestMessage(t *testing.T) {
	h := newHarness(t, []chat.Message{
		msg("spaces/A/messages/m3", "chatter"),
		msg("spaces/A/messages/m2", prURL("aex-balances", 12)),
	})
	h.seedWatermark(t)

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	wm, ok, _ := h.st.Watermark()
	if !ok || wm.MessageName != "spaces/A/messages/m3" {
		t.Errorf("watermark = %q, want the newest message even though it held no PR", wm.MessageName)
	}
}

func TestPrintOnlyChangesNoState(t *testing.T) {
	h := newHarness(t, []chat.Message{msg("spaces/A/messages/m1", prURL("aex-balances", 12))})
	h.seedWatermark(t)

	rep, err := h.p.Sweep(context.Background(), Options{PrintOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := decisionFor(rep, "Example-Org/aex-balances#12")
	if d.Action != ActionWouldReview {
		t.Fatalf("Action = %q, want would_review", d.Action)
	}
	if len(h.rev.ran) != 0 || len(h.wts.prepared) != 0 {
		t.Error("print-only must not prepare a worktree or run a review")
	}
	if _, found, _ := h.st.Review("Example-Org/aex-balances#12"); found {
		t.Error("print-only must write no review record")
	}
	wm, _, _ := h.st.Watermark()
	if wm.MessageName != "spaces/A/messages/m0" {
		t.Errorf("print-only must not advance the watermark, got %q", wm.MessageName)
	}
}

func TestPendingExpiresAfterTooManyAttempts(t *testing.T) {
	h := newHarness(t, nil)
	h.seedWatermark(t)
	key := "Example-Org/aex-balances#12"
	h.cfg.PendingMaxAttempts = 3
	h.apply()
	if err := h.st.PutPending(store.Pending{
		Key: key, FirstSeen: time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC),
		Attempts: 3, LastReason: "draft",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := h.st.Review(key)
	if !ok || rec.Outcome != store.OutcomeExpired {
		t.Errorf("Outcome = %q, want expired", rec.Outcome)
	}
	if _, still, _ := h.st.Pending(key); still {
		t.Error("an expired entry must leave pending, or it is re-inspected forever")
	}
}

func TestPendingExpiresAfterMaxAge(t *testing.T) {
	h := newHarness(t, nil)
	h.seedWatermark(t)
	key := "Example-Org/aex-balances#12"
	if err := h.st.PutPending(store.Pending{
		Key: key, FirstSeen: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.p.Sweep(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := h.st.Review(key)
	if !ok || rec.Outcome != store.OutcomeExpired {
		t.Errorf("Outcome = %q, want expired after max age", rec.Outcome)
	}
}

func TestChatFailurePropagatesAndHoldsTheWatermark(t *testing.T) {
	h := newHarness(t, nil)
	h.seedWatermark(t)
	h.ch.err = errors.New("chat.py exit 1")

	if _, err := h.p.Sweep(context.Background(), Options{}); err == nil {
		t.Fatal("a chat failure must be returned")
	}
	wm, _, _ := h.st.Watermark()
	if wm.MessageName != "spaces/A/messages/m0" {
		t.Errorf("the watermark must not advance when the fetch failed, got %q", wm.MessageName)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/pipeline/`
Expected: FAIL — `undefined: Pipeline`, `undefined: Options`, `undefined: SweepReport`.

- [ ] **Step 4: Write the implementation**

Create `internal/pipeline/pipeline.go`:

```go
// Package pipeline holds the whole decision table for one sweep: what to
// review, what to skip, what to defer, and what to hand back to a human.
package pipeline

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/review"
	"github.com/angelov-todor/firstpass/internal/store"
)

// The four seams to the outside world. Each has a fake in the tests, which is
// why every rule below is verified without a subprocess.
type (
	ChatSource interface {
		Fetch(ctx context.Context, sinceName string, limit int) ([]chat.Message, error)
	}
	PRInspector interface {
		Inspect(ctx context.Context, ref prref.PRRef) (ghpr.PRInfo, error)
	}
	Worktrees interface {
		Prepare(ctx context.Context, ref prref.PRRef) (dir string, cleanup func(), err error)
	}
	Reviewer interface {
		Run(ctx context.Context, dir string, ref prref.PRRef) (review.Result, error)
	}
)

// Action is what the sweep did about one pull request.
type Action string

const (
	ActionReview         Action = "review"
	ActionSkip           Action = "skip"
	ActionDefer          Action = "defer"
	ActionNeedsAttention Action = "needs_attention"
	ActionWouldReview    Action = "would_review"
)

// Decision is one line of the sweep's report.
type Decision struct {
	Ref    prref.PRRef
	Action Action
	Reason string
}

// SweepReport summarises one sweep.
type SweepReport struct {
	MessagesScanned int
	ColdStart       bool
	Paused          bool
	Decisions       []Decision
	Reviewed        int
}

// Options tune a single sweep.
type Options struct {
	// PrintOnly decides and reports without touching state, GitHub or disk.
	PrintOnly bool
	// Backfill takes the last N messages and ignores the watermark.
	Backfill int
}

// Pipeline runs sweeps.
type Pipeline struct {
	Cfg   config.Config
	Store *store.Store
	Chat  ChatSource
	PRs   PRInspector
	WTs   Worktrees
	Rev   Reviewer
	Log   *slog.Logger
	Now   func() time.Time
}

type candidate struct {
	ref     prref.PRRef
	trigger string
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

func (p *Pipeline) paused() bool {
	_, err := os.Stat(p.Cfg.PauseFile())
	return err == nil
}

// Sweep reads the space once and acts on whatever it finds.
func (p *Pipeline) Sweep(ctx context.Context, opts Options) (SweepReport, error) {
	rep := SweepReport{Paused: p.paused()}

	wm, hasWM, err := p.Store.Watermark()
	if err != nil {
		return rep, err
	}

	since, limit := wm.MessageName, p.Cfg.FetchLimit
	if opts.Backfill > 0 {
		since, limit = "", opts.Backfill
	}

	msgs, err := p.Chat.Fetch(ctx, since, limit)
	if err != nil {
		return rep, err
	}
	rep.MessagesScanned = len(msgs)

	// Cold start. A first run against a populated space must review nothing:
	// otherwise launch day sweeps months of history and comments on PRs that
	// were merged long ago.
	if !hasWM && opts.Backfill == 0 {
		rep.ColdStart = true
		if len(msgs) > 0 && !opts.PrintOnly {
			if err := p.setWatermark(msgs[0]); err != nil {
				return rep, err
			}
		}
		return rep, nil
	}

	for _, c := range p.candidates(msgs) {
		rep.Decisions = append(rep.Decisions, p.handle(ctx, c, &rep, opts))
	}

	if !opts.PrintOnly && opts.Backfill == 0 && len(msgs) > 0 {
		if err := p.setWatermark(msgs[0]); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func (p *Pipeline) setWatermark(m chat.Message) error {
	return p.Store.SetWatermark(store.Watermark{MessageName: m.Name, CreateTime: m.CreateTime})
}

// candidates lists the refs to consider: everything in the new messages,
// walked oldest-first so the earliest post is recorded as the trigger, followed
// by refs still parked in the pending bucket.
func (p *Pipeline) candidates(msgs []chat.Message) []candidate {
	var out []candidate
	seen := map[string]bool{}

	for i := len(msgs) - 1; i >= 0; i-- {
		for _, ref := range prref.Extract(msgs[i].Text) {
			if seen[ref.Key()] {
				continue
			}
			seen[ref.Key()] = true
			out = append(out, candidate{ref: ref, trigger: msgs[i].Name})
		}
	}

	pend, err := p.Store.AllPending()
	if err != nil {
		p.Log.Error("read pending", "err", err)
		return out
	}
	for _, pd := range pend {
		if seen[pd.Key] {
			continue
		}
		ref, err := prref.ParseKey(pd.Key)
		if err != nil {
			p.Log.Error("unparseable pending key", "key", pd.Key, "err", err)
			continue
		}
		seen[pd.Key] = true
		out = append(out, candidate{ref: ref})
	}
	return out
}

// handle applies the decision table to one candidate. The order of the checks
// is load-bearing; see the comments at each gate.
func (p *Pipeline) handle(ctx context.Context, c candidate, rep *SweepReport, opts Options) Decision {
	ref := c.ref
	dec := func(a Action, reason string) Decision {
		return Decision{Ref: ref, Action: a, Reason: reason}
	}

	// Owner first: a repo outside the allowlist must never be queried, let
	// alone cloned. The space is a chat room, so unrelated links do turn up.
	if !p.Cfg.OwnerAllowed(ref.Owner) {
		p.terminal(ref, store.OutcomeSkippedOwner, c.trigger, "owner not in allow_owners")
		return dec(ActionSkip, "owner not allowed")
	}
	if p.Cfg.RepoDenied(ref.Owner, ref.Repo) {
		p.terminal(ref, store.OutcomeSkippedRepo, c.trigger, "repo in deny_repos")
		return dec(ActionSkip, "repo in deny_repos")
	}

	// The existing record comes next, so a run that died mid-post is converted
	// before any other rule can send this PR back through a review.
	if prev, ok, err := p.Store.Review(ref.Key()); err != nil {
		p.Log.Error("read review", "key", ref.Key(), "err", err)
		return dec(ActionDefer, "store read failed")
	} else if ok {
		if prev.Outcome == store.OutcomeInFlight {
			prev.Outcome = store.OutcomeNeedsAttention
			prev.DecidedAt = p.now()
			prev.Detail = "a previous run died mid-review, so comments may already be posted; " +
				"run `firstpass replay " + ref.URL() + "` to review it again deliberately"
			if err := p.Store.PutReview(prev); err != nil {
				p.Log.Error("put review", "key", ref.Key(), "err", err)
			}
			if err := p.Store.DeletePending(ref.Key()); err != nil {
				p.Log.Error("delete pending", "key", ref.Key(), "err", err)
			}
			p.Log.Warn("needs attention", "key", ref.Key(), "reason", "previous run died mid-review")
			return dec(ActionNeedsAttention, "previous run died mid-review")
		}
		return dec(ActionSkip, "already decided: "+string(prev.Outcome))
	}

	if p.expirePending(ref) {
		return dec(ActionSkip, "pending expired")
	}

	// Pause and the cap park the ref without counting an attempt: neither is a
	// failure, and letting either burn attempts would silently expire a backlog
	// during a long pause.
	if rep.Paused {
		p.hold(ref, "paused")
		return dec(ActionDefer, "paused")
	}
	if rep.Reviewed >= p.Cfg.MaxReviewsPerSweep {
		p.hold(ref, "per-sweep cap reached")
		return dec(ActionDefer, "per-sweep cap reached")
	}

	info, err := p.PRs.Inspect(ctx, ref)
	if err != nil {
		p.deferAttempt(ref, "inspect failed: "+err.Error())
		return dec(ActionDefer, "inspect failed: "+err.Error())
	}
	if info.State != "OPEN" {
		p.terminal(ref, store.OutcomeSkippedState, c.trigger, "state "+info.State)
		return dec(ActionSkip, "state "+info.State)
	}
	if info.IsDraft {
		// Deferred, not terminal: a draft is routinely marked ready later, and
		// by then the message has scrolled past the watermark.
		p.deferAttempt(ref, "draft")
		return dec(ActionDefer, "draft")
	}
	if strings.EqualFold(info.Author, p.Cfg.GithubLogin) {
		p.terminal(ref, store.OutcomeSkippedAuthor, c.trigger, "authored by "+info.Author)
		return dec(ActionSkip, "own PR")
	}

	if opts.PrintOnly {
		return dec(ActionWouldReview, "OPEN, not draft, author "+info.Author)
	}

	dir, cleanup, err := p.WTs.Prepare(ctx, ref)
	if err != nil {
		p.deferAttempt(ref, "worktree failed: "+err.Error())
		return dec(ActionDefer, "worktree failed: "+err.Error())
	}
	defer cleanup()

	started := p.now()
	rec := store.Review{
		Key:            ref.Key(),
		Outcome:        store.OutcomeInFlight,
		HeadSHA:        info.HeadSHA,
		TriggerMessage: c.trigger,
		StartedAt:      started,
	}
	// Written before claude starts: this record is the only evidence that a
	// review was underway if the process dies while posting comments.
	if err := p.Store.PutReview(rec); err != nil {
		p.Log.Error("record in_flight", "key", ref.Key(), "err", err)
		return dec(ActionDefer, "could not record in_flight")
	}

	rctx, cancel := context.WithTimeout(ctx, p.Cfg.ReviewTimeout.D())
	defer cancel()

	res, rerr := p.Rev.Run(rctx, dir, ref)
	done := p.now()

	rec.DecidedAt = done
	rec.DurationMS = done.Sub(started).Milliseconds()
	rec.ExitCode = res.ExitCode
	rec.ReportPath = res.ReportPath

	if rerr != nil {
		rec.Outcome = store.OutcomeNeedsAttention
		rec.Detail = "review did not finish (" + rerr.Error() + "); comments may be partially posted, " +
			"so it will not be retried automatically"
		if err := p.Store.PutReview(rec); err != nil {
			p.Log.Error("put review", "key", ref.Key(), "err", err)
		}
		if err := p.Store.DeletePending(ref.Key()); err != nil {
			p.Log.Error("delete pending", "key", ref.Key(), "err", err)
		}
		p.Log.Warn("needs attention", "key", ref.Key(), "err", rerr)
		return dec(ActionNeedsAttention, "review did not finish: "+rerr.Error())
	}

	rec.Outcome = store.OutcomeReviewed
	if err := p.Store.PutReview(rec); err != nil {
		p.Log.Error("put review", "key", ref.Key(), "err", err)
	}
	if err := p.Store.DeletePending(ref.Key()); err != nil {
		p.Log.Error("delete pending", "key", ref.Key(), "err", err)
	}
	rep.Reviewed++
	p.Log.Info("reviewed", "key", ref.Key(), "ms", rec.DurationMS, "report", rec.ReportPath)
	return dec(ActionReview, "reviewed")
}

// terminal closes the book on a ref and clears any pending entry for it.
func (p *Pipeline) terminal(ref prref.PRRef, o store.Outcome, trigger, detail string) {
	if err := p.Store.PutReview(store.Review{
		Key:            ref.Key(),
		Outcome:        o,
		TriggerMessage: trigger,
		DecidedAt:      p.now(),
		Detail:         detail,
	}); err != nil {
		p.Log.Error("put review", "key", ref.Key(), "err", err)
	}
	if err := p.Store.DeletePending(ref.Key()); err != nil {
		p.Log.Error("delete pending", "key", ref.Key(), "err", err)
	}
}

// hold parks a ref for a later sweep without counting an attempt.
func (p *Pipeline) hold(ref prref.PRRef, reason string) { p.upsertPending(ref, reason, false) }

// deferAttempt parks a ref and counts an attempt against its expiry budget.
func (p *Pipeline) deferAttempt(ref prref.PRRef, reason string) { p.upsertPending(ref, reason, true) }

func (p *Pipeline) upsertPending(ref prref.PRRef, reason string, countAttempt bool) {
	pd, ok, err := p.Store.Pending(ref.Key())
	if err != nil {
		p.Log.Error("read pending", "key", ref.Key(), "err", err)
		return
	}
	if !ok {
		pd = store.Pending{Key: ref.Key(), FirstSeen: p.now()}
	}
	if countAttempt {
		pd.Attempts++
		pd.LastAttempt = p.now()
	}
	pd.LastReason = reason
	if err := p.Store.PutPending(pd); err != nil {
		p.Log.Error("put pending", "key", ref.Key(), "err", err)
	}
}

// expirePending retires a ref that has been retried too often or waited too
// long, so a PR abandoned in draft is not re-inspected forever.
func (p *Pipeline) expirePending(ref prref.PRRef) bool {
	pd, ok, err := p.Store.Pending(ref.Key())
	if err != nil || !ok {
		return false
	}
	age := p.now().Sub(pd.FirstSeen)
	tooMany := pd.Attempts >= p.Cfg.PendingMaxAttempts
	tooOld := age > p.Cfg.PendingMaxAge.D()
	if !tooMany && !tooOld {
		return false
	}

	reason := "expired after " + strconv.Itoa(pd.Attempts) + " attempts"
	if tooOld {
		reason = "expired after " + age.Round(time.Hour).String() + " pending"
	}
	p.Log.Warn("pending expired", "key", pd.Key, "reason", reason, "last", pd.LastReason)
	p.terminal(ref, store.OutcomeExpired, "", reason)
	return true
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/pipeline/ -v`
Expected: PASS, all 20 tests.

- [ ] **Step 6: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/pipeline internal/review
git commit -m "$(cat <<'EOF'
feat: sweep pipeline with the full decision table

Gate order is load-bearing: owner allowlist before any GitHub query, the
existing record before anything can re-review, then pause and the
per-sweep cap -- both of which park a ref without counting an attempt, so
a long pause cannot silently expire the backlog.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---
### Task 8: CLI skeleton — `scan --print-only`, `doctor`, `pause`/`resume`

**This is the checkpoint task.** After it, `firstpass scan --print-only --backfill 200` prints every PR it *would* review, checked against months of real messages — and it cannot write to GitHub, clone anything, or run `claude`, because `WTs` and `Rev` are deliberately left nil and `scan` refuses to run without `--print-only` until Task 11.

Parser correctness is the thing most likely to be quietly wrong, and this is the cheapest way to prove it against real data.

**Files:**
- Create: `cmd/firstpass/main.go`
- Create: `cmd/firstpass/app.go`
- Create: `cmd/firstpass/cmd_scan.go`
- Create: `cmd/firstpass/cmd_doctor.go`
- Create: `cmd/firstpass/cmd_pause.go`
- Test: `cmd/firstpass/cmd_scan_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–7.
- Produces: `func openApp(configPath string, live bool, withReview bool) (*app, error)`; `type app struct { cfg config.Config; store *store.Store; log *slog.Logger; pipe *pipeline.Pipeline; chat *chat.Client }` with `func (*app) Close() error`; `func renderSweep(w io.Writer, rep pipeline.SweepReport, dryRun bool)`.

- [ ] **Step 1: Write the failing test**

Create `cmd/firstpass/cmd_scan_test.go`:

```go
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/pipeline"
	"github.com/angelov-todor/firstpass/internal/prref"
)

func TestRenderSweepListsEveryDecisionWithItsReason(t *testing.T) {
	rep := pipeline.SweepReport{
		MessagesScanned: 12,
		Decisions: []pipeline.Decision{
			{Ref: prref.PRRef{Owner: "Example-Org", Repo: "aex-balances", Number: 12},
				Action: pipeline.ActionWouldReview, Reason: "OPEN, not draft, author colleague"},
			{Ref: prref.PRRef{Owner: "Example-Org", Repo: "aex-margin-service", Number: 3},
				Action: pipeline.ActionSkip, Reason: "own PR"},
		},
	}

	var buf bytes.Buffer
	renderSweep(&buf, rep, true)
	out := buf.String()

	for _, want := range []string{
		"12 messages",
		"Example-Org/aex-balances#12",
		"would_review",
		"OPEN, not draft, author colleague",
		"Example-Org/aex-margin-service#3",
		"own PR",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderSweepAnnouncesDryRun(t *testing.T) {
	var buf bytes.Buffer
	renderSweep(&buf, pipeline.SweepReport{}, true)
	if !strings.Contains(buf.String(), "dry run") {
		t.Errorf("a dry run must say so, so nobody assumes comments were posted:\n%s", buf.String())
	}
}

func TestRenderSweepAnnouncesColdStart(t *testing.T) {
	var buf bytes.Buffer
	renderSweep(&buf, pipeline.SweepReport{ColdStart: true, MessagesScanned: 40}, true)
	out := buf.String()
	if !strings.Contains(out, "cold start") {
		t.Errorf("a cold start must be explained, not look like a broken sweep:\n%s", out)
	}
	if !strings.Contains(out, "--backfill") {
		t.Errorf("a cold start must point at the way to review history on purpose:\n%s", out)
	}
}

func TestRenderSweepAnnouncesPause(t *testing.T) {
	var buf bytes.Buffer
	renderSweep(&buf, pipeline.SweepReport{Paused: true}, false)
	if !strings.Contains(buf.String(), "paused") {
		t.Errorf("a paused sweep must say so:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/firstpass/`
Expected: FAIL — `undefined: renderSweep`.

- [ ] **Step 3: Write the command dispatch**

Create `cmd/firstpass/main.go`:

```go
// Command firstpass watches the team's Google Chat space and reviews the pull
// requests posted to it.
package main

import (
	"fmt"
	"os"
)

const usageText = `firstpass — review the PRs posted to the team chat space

usage: firstpass <command> [flags]

commands:
  scan      one sweep, then exit (also the Task Scheduler entry point)
  watch     sweep on a ticker until interrupted
  status    what has been reviewed, skipped or deferred
  replay    review one PR again, ignoring the dedupe record
  doctor    check every external dependency
  pause     stop reviewing and posting; sweeps keep queueing
  resume    undo pause

run "firstpass <command> -h" for a command's flags
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}

	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "scan":
		err = cmdScan(args)
	case "doctor":
		err = cmdDoctor(args)
	case "pause":
		err = cmdPause(args, true)
	case "resume":
		err = cmdPause(args, false)
	case "-h", "--help", "help":
		fmt.Print(usageText)
		return
	default:
		fmt.Fprintf(os.Stderr, "firstpass: unknown command %q\n\n%s", os.Args[1], usageText)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "firstpass:", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 4: Write the wiring**

Create `cmd/firstpass/app.go`. `withReview` is false for now — Task 11 flips it on once the worktree and review packages exist:

```go
package main

import (
	"log/slog"
	"os"

	"github.com/angelov-todor/firstpass/internal/chat"
	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/pipeline"
	"github.com/angelov-todor/firstpass/internal/runner"
	"github.com/angelov-todor/firstpass/internal/store"
)

type app struct {
	cfg   config.Config
	store *store.Store
	log   *slog.Logger
	pipe  *pipeline.Pipeline
	chat  *chat.Client
}

// openApp loads the config, opens the store, and wires a pipeline.
//
// withReview is false until the worktree and review packages exist; without it
// the pipeline can only be driven in print-only mode, which is exactly what is
// wanted while the extraction rules are still being checked against real
// messages.
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

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	r := runner.OS{}
	ch := chat.New(r, cfg.Paths.Python, cfg.Paths.ChatScript, cfg.Space)

	p := &pipeline.Pipeline{
		Cfg:   cfg,
		Store: st,
		Chat:  ch,
		PRs:   ghpr.New(r, cfg.Paths.GH),
		Log:   log,
	}
	_ = withReview // wired in Task 11

	return &app{cfg: cfg, store: st, log: log, pipe: p, chat: ch}, nil
}

func (a *app) Close() error { return a.store.Close() }
```

- [ ] **Step 5: Write the scan command**

Create `cmd/firstpass/cmd_scan.go`:

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/pipeline"
)

func cmdScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "config file")
	printOnly := fs.Bool("print-only", false, "decide and print, changing no state")
	backfill := fs.Int("backfill", 0, "take the last N messages, ignoring the watermark")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if !*printOnly {
		return errors.New("only -print-only is supported so far; reviewing is wired up in a later task")
	}

	a, err := openApp(*cfgPath, false, false)
	if err != nil {
		return err
	}
	defer a.Close()

	rep, err := a.pipe.Sweep(context.Background(), pipeline.Options{
		PrintOnly: *printOnly,
		Backfill:  *backfill,
	})
	if err != nil {
		return err
	}
	renderSweep(os.Stdout, rep, a.cfg.DryRun)
	return nil
}

// renderSweep prints a sweep so a human can audit the decisions.
func renderSweep(w io.Writer, rep pipeline.SweepReport, dryRun bool) {
	mode := "live — comments are posted to GitHub"
	if dryRun {
		mode = "dry run — nothing is posted"
	}
	fmt.Fprintf(w, "%d messages scanned (%s)\n", rep.MessagesScanned, mode)

	if rep.Paused {
		fmt.Fprintln(w, "sweep is paused: refs were queued, nothing was reviewed or posted")
	}
	if rep.ColdStart {
		fmt.Fprintln(w, "cold start: the watermark was set to the newest message and nothing was reviewed.")
		fmt.Fprintln(w, "to review history on purpose, run with --backfill N")
		return
	}
	if len(rep.Decisions) == 0 {
		fmt.Fprintln(w, "no PR references found")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PR\tACTION\tREASON")
	for _, d := range rep.Decisions {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", d.Ref.Key(), d.Action, d.Reason)
	}
	tw.Flush()
	fmt.Fprintf(w, "\n%d reviewed\n", rep.Reviewed)
}
```

- [ ] **Step 6: Write the pause and doctor commands**

Create `cmd/firstpass/cmd_pause.go`:

```go
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/angelov-todor/firstpass/internal/config"
)

// cmdPause writes or removes the kill-switch file. It is a file rather than a
// signal so it works when the daemon is wedged and survives a restart.
func cmdPause(args []string, on bool) error {
	name := "resume"
	if on {
		name = "pause"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return err
	}

	if on {
		if err := os.WriteFile(cfg.PauseFile(), []byte("paused by firstpass pause\n"), 0o600); err != nil {
			return err
		}
		fmt.Println("paused:", cfg.PauseFile())
		fmt.Println("sweeps keep queueing PRs; nothing is reviewed or posted until `firstpass resume`")
		return nil
	}

	if err := os.Remove(cfg.PauseFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Println("resumed")
	return nil
}
```

Create `cmd/firstpass/cmd_doctor.go`:

```go
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

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

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
		add("config valid", cfg.Validate(), fmt.Sprintf("dry_run=%v allow_owners=%v", cfg.DryRun, cfg.AllowOwners))
		add("state dir writable", writable(cfg.StateDir), cfg.StateDir)
		add("chat.py present", exists(cfg.Paths.ChatScript), cfg.Paths.ChatScript)

		r := runner.OS{}
		add("git works", version(ctx, r, cfg.Paths.Git, "--version"), cfg.Paths.Git)
		add("claude works", version(ctx, r, cfg.Paths.Claude, "--version"), cfg.Paths.Claude)
		add("gh authenticated", ghAuth(ctx, r, cfg.Paths.GH), cfg.Paths.GH)

		ch := chat.New(r, cfg.Paths.Python, cfg.Paths.ChatScript, cfg.Space)
		named, nerr := ch.HasNamedRooms(ctx)
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
```

- [ ] **Step 7: Run the tests and build**

Run: `go test ./... && go build ./cmd/firstpass`
Expected: all packages PASS, binary builds.

- [ ] **Step 8: Commit**

```bash
gofmt -l . && go vet ./...
git add cmd/firstpass
git commit -m "$(cat <<'EOF'
feat: firstpass CLI with print-only scan, doctor and pause

scan refuses to run without -print-only for now, and the pipeline is
wired without a worktree or reviewer, so this build cannot clone, run
claude, or post anything.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

- [ ] **Step 9: CHECKPOINT — verify extraction against real messages**

Run, in order:

```bash
go build -o firstpass.exe ./cmd/firstpass
./firstpass.exe doctor
./firstpass.exe scan -print-only -backfill 200
```

`doctor` must pass every check. If "google chat account" fails, the wrong Google account is authenticated — fix that before reading anything into the scan output.

Then read the scan table carefully and **stop to confirm with the user**:

- Does every PR link posted in the space over those 200 messages appear in the table? Cross-check a handful by opening the space.
- Is anything in the table that should not be — a link from outside `Example-Org`, an issue mistaken for a PR, a number pulled out of prose?
- Do the `own PR` and `state MERGED` skips look right?

This is the cheapest place in the whole project to find a parser bug. Do not continue to Task 9 until the user has looked at this output.

---

### Task 9: Worktree preparation

**Files:**
- Create: `internal/worktree/worktree.go`
- Test: `internal/worktree/worktree_test.go`

**Interfaces:**
- Consumes: `prref.PRRef` (Task 1), `runner.Runner` (Task 4).
- Produces: `func New(r runner.Runner, git, reposDir, workDir string) *Manager`; field `URLFor func(prref.PRRef) string`; `func (*Manager) Prepare(ctx context.Context, ref prref.PRRef) (dir string, cleanup func(), err error)`.

> `URLFor` exists so the test can clone a local fixture instead of reaching GitHub. Left nil, it builds the real `https://github.com/owner/repo.git`.
>
> The mirror lives in firstpass's own cache directory and is the only clone it ever touches. The user's working copies in `example-org/` are never opened, so a review running in the background cannot disturb an active branch or uncommitted work.

- [ ] **Step 1: Write the failing test**

Create `internal/worktree/worktree_test.go`:

```go
package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/runner"
)

// fixtureOrigin builds a real repository with a refs/pull/1/head ref, which is
// what GitHub exposes for a PR head.
func fixtureOrigin(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "base")

	run("checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "feature")
	sha := run("rev-parse", "HEAD")

	run("checkout", "-q", "main")
	run("update-ref", "refs/pull/1/head", sha)
	return dir
}

func newManager(t *testing.T, origin string) *Manager {
	t.Helper()
	base := t.TempDir()
	m := New(runner.OS{}, "git", filepath.Join(base, "repos"), filepath.Join(base, "work"))
	m.URLFor = func(prref.PRRef) string { return filepath.ToSlash(origin) }
	return m
}

var ref = prref.PRRef{Owner: "Example-Org", Repo: "aex-balances", Number: 1}

func TestPrepareChecksOutThePRHead(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real git")
	}
	m := newManager(t, fixtureOrigin(t))

	dir, cleanup, err := m.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if _, err := os.Stat(filepath.Join(dir, "feature.txt")); err != nil {
		t.Errorf("the PR head must be checked out: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "base.txt")); err != nil {
		t.Errorf("the base commit must be present too: %v", err)
	}
}

func TestPrepareIsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real git")
	}
	m := newManager(t, fixtureOrigin(t))

	_, cleanup1, err := m.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	cleanup1()

	dir, cleanup2, err := m.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatalf("a second Prepare must reuse the mirror, not fail: %v", err)
	}
	defer cleanup2()
	if _, err := os.Stat(filepath.Join(dir, "feature.txt")); err != nil {
		t.Errorf("second Prepare produced no checkout: %v", err)
	}
}

func TestPrepareRecoversFromALeftoverWorktree(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real git")
	}
	m := newManager(t, fixtureOrigin(t))

	dir, _, err := m.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a killed run: the worktree is still registered and on disk.
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}

	dir2, cleanup, err := m.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatalf("a worktree left behind by a killed run must not wedge the next review: %v", err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(dir2, "feature.txt")); err != nil {
		t.Errorf("recovery produced no checkout: %v", err)
	}
}

func TestCleanupRemovesTheWorktree(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real git")
	}
	m := newManager(t, fixtureOrigin(t))

	dir, cleanup, err := m.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("cleanup must remove the worktree, stat err = %v", err)
	}
}

func TestPrepareReportsCloneFailure(t *testing.T) {
	base := t.TempDir()
	m := New(runner.OS{}, "git", filepath.Join(base, "repos"), filepath.Join(base, "work"))
	m.URLFor = func(prref.PRRef) string { return filepath.Join(base, "does-not-exist") }

	if _, _, err := m.Prepare(context.Background(), ref); err == nil {
		t.Error("a clone failure must be reported")
	}
}

func TestDefaultURLIsGitHub(t *testing.T) {
	m := New(runner.OS{}, "git", "repos", "work")
	if got := m.url(ref); got != "https://github.com/Example-Org/aex-balances.git" {
		t.Errorf("url() = %q", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/worktree/`
Expected: FAIL — `undefined: New`, `undefined: Manager`.

- [ ] **Step 3: Write the implementation**

Create `internal/worktree/worktree.go`:

```go
// Package worktree puts a pull request's head on disk in a throwaway checkout,
// backed by a bare mirror in firstpass's own cache.
//
// It never opens the user's own clones. A background review that ran git in a
// repository the user was working in is how uncommitted work gets lost.
package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/runner"
)

// Manager owns the mirror cache and the worktree directory.
type Manager struct {
	r        runner.Runner
	git      string
	reposDir string
	workDir  string

	// URLFor overrides the clone URL. Left nil it builds the GitHub HTTPS URL;
	// tests point it at a local fixture repository.
	URLFor func(prref.PRRef) string
}

func New(r runner.Runner, git, reposDir, workDir string) *Manager {
	return &Manager{r: r, git: git, reposDir: reposDir, workDir: workDir}
}

func (m *Manager) url(ref prref.PRRef) string {
	if m.URLFor != nil {
		return m.URLFor(ref)
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", ref.Owner, ref.Repo)
}

func (m *Manager) mirrorPath(ref prref.PRRef) string {
	return filepath.Join(m.reposDir, fmt.Sprintf("%s_%s.git", ref.Owner, ref.Repo))
}

func (m *Manager) workPath(ref prref.PRRef) string {
	return filepath.Join(m.workDir, fmt.Sprintf("%s_%s_%d", ref.Owner, ref.Repo, ref.Number))
}

// Prepare returns a directory holding the PR head, plus a cleanup func the
// caller must always invoke.
func (m *Manager) Prepare(ctx context.Context, ref prref.PRRef) (string, func(), error) {
	noop := func() {}
	mirror := m.mirrorPath(ref)
	work := m.workPath(ref)

	for _, d := range []string{m.reposDir, m.workDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", noop, err
		}
	}

	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); os.IsNotExist(err) {
		if err := m.git0(ctx, "clone", "--bare", m.url(ref), mirror); err != nil {
			return "", noop, fmt.Errorf("clone mirror for %s: %w", ref.Key(), err)
		}
	} else if err != nil {
		return "", noop, err
	}

	prRef := "refs/firstpass/" + strconv.Itoa(ref.Number)
	spec := fmt.Sprintf("+refs/pull/%d/head:%s", ref.Number, prRef)
	if err := m.git0(ctx, "-C", mirror, "fetch", "origin", spec); err != nil {
		return "", noop, fmt.Errorf("fetch head of %s: %w", ref.Key(), err)
	}

	// A worktree left registered by a killed run would make `worktree add`
	// fail, wedging every future review of this PR. Clear it first; both of
	// these are expected to fail on the happy path.
	_ = m.git0(ctx, "-C", mirror, "worktree", "remove", "--force", work)
	if err := os.RemoveAll(work); err != nil {
		return "", noop, err
	}
	_ = m.git0(ctx, "-C", mirror, "worktree", "prune")

	if err := m.git0(ctx, "-C", mirror, "worktree", "add", "--detach", work, prRef); err != nil {
		return "", noop, fmt.Errorf("worktree add for %s: %w", ref.Key(), err)
	}

	cleanup := func() {
		// context.Background: cleanup must still run when the review's deadline
		// has already fired, or the worktree leaks.
		_ = m.git0(context.Background(), "-C", mirror, "worktree", "remove", "--force", work)
		_ = os.RemoveAll(work)
	}
	return work, cleanup, nil
}

func (m *Manager) git0(ctx context.Context, args ...string) error {
	res, err := m.r.Run(ctx, "", m.git, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("git %s exit %d: %s",
			strings.Join(args, " "), res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/worktree/ -v`
Expected: PASS. Then check the fast path still works: `go test -short ./internal/worktree/` skips the git tests.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/worktree
git commit -m "$(cat <<'EOF'
feat: per-PR worktrees over a bare mirror cache

Never opens the user's own clones, and clears a worktree left registered
by a killed run so it cannot wedge every future review of that PR.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 10: Review runner

**Files:**
- Modify: `internal/review/review.go` (created as a stub in Task 7)
- Test: `internal/review/review_test.go`

**Interfaces:**
- Consumes: `prref.PRRef` (Task 1), `runner.Runner` (Task 4).
- Produces: `type Result struct { ExitCode int; ReportPath string; Stdout []byte }` (already declared); `func New(r runner.Runner, claude string, extraArgs []string, dryRun bool, reportsDir string) *Runner`; `func (*Runner) Prompt() string`; `func (*Runner) Run(ctx context.Context, dir string, ref prref.PRRef) (Result, error)`.

> Dry run and live differ by exactly one thing: the `--comment` flag. Same worktree, same reviewer, same skills — so what you read in a dry-run report is what would have been posted.
>
> `extraArgs` carries `--permission-mode bypassPermissions` from config. Headless `claude -p` cannot run `gh` to post comments under the default mode; it would block on a prompt nobody can answer. **This grants the review subprocess unprompted tool use. It is NOT bounded by the worktree** — see the corrected note in the "Additions beyond the spec" section above.

- [ ] **Step 1: Write the failing test**

Create `internal/review/review_test.go`:

```go
package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/runner"
)

var ref = prref.PRRef{Owner: "Example-Org", Repo: "aex-balances", Number: 12}

func TestPromptOmitsCommentInDryRun(t *testing.T) {
	rr := New(&runner.Fake{}, "claude", nil, true, t.TempDir())
	if got := rr.Prompt(); strings.Contains(got, "--comment") {
		t.Errorf("Prompt() = %q; a dry run must not post", got)
	}
	if !strings.Contains(rr.Prompt(), "/code-review") {
		t.Errorf("Prompt() = %q, want the /code-review command", rr.Prompt())
	}
}

func TestPromptIncludesCommentWhenLive(t *testing.T) {
	rr := New(&runner.Fake{}, "claude", nil, false, t.TempDir())
	if got := rr.Prompt(); !strings.Contains(got, "--comment") {
		t.Errorf("Prompt() = %q; a live run must post inline comments", got)
	}
}

func TestRunInvokesClaudeInTheWorktreeWithConfiguredArgs(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "code-review", Result: runner.Result{Stdout: []byte("no findings")}},
	}}
	rr := New(f, "claude", []string{"--permission-mode", "bypassPermissions"}, true, t.TempDir())

	if _, err := rr.Run(context.Background(), filepath.Join("work", "aex-balances"), ref); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("Calls = %+v", f.Calls)
	}
	c := f.Calls[0]
	if c.Dir != filepath.Join("work", "aex-balances") {
		t.Errorf("Dir = %q; claude must run inside the checkout so it sees the repo's CLAUDE.md and skills", c.Dir)
	}
	line := c.String()
	for _, want := range []string{"-p", "/code-review", "--permission-mode", "bypassPermissions"} {
		if !strings.Contains(line, want) {
			t.Errorf("command %q missing %q", line, want)
		}
	}
}

func TestRunWritesAReportInDryRun(t *testing.T) {
	dir := t.TempDir()
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "code-review", Result: runner.Result{Stdout: []byte("finding: off-by-one")}},
	}}
	rr := New(f, "claude", nil, true, dir)

	res, err := rr.Run(context.Background(), "work", ref)
	if err != nil {
		t.Fatal(err)
	}
	if res.ReportPath == "" {
		t.Fatal("a dry run must write a report to read")
	}
	body, err := os.ReadFile(res.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"finding: off-by-one", ref.Key(), ref.URL(), "nothing posted"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("report missing %q:\n%s", want, body)
		}
	}
}

func TestRunWritesNoReportWhenLive(t *testing.T) {
	dir := t.TempDir()
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "code-review", Result: runner.Result{Stdout: []byte("posted")}},
	}}
	rr := New(f, "claude", nil, false, dir)

	res, err := rr.Run(context.Background(), "work", ref)
	if err != nil {
		t.Fatal(err)
	}
	if res.ReportPath != "" {
		t.Errorf("ReportPath = %q; a live run's findings live on the PR", res.ReportPath)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("no report files expected, found %d", len(entries))
	}
}

func TestRunSurfacesNonZeroExit(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "code-review", Result: runner.Result{ExitCode: 1, Stderr: []byte("rate limited")}},
	}}
	rr := New(f, "claude", nil, true, t.TempDir())

	res, err := rr.Run(context.Background(), "work", ref)
	if err == nil {
		t.Fatal("a non-zero claude exit must be an error so the PR is not recorded as reviewed")
	}
	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", res.ExitCode)
	}
}

func TestRunSurfacesTimeout(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "code-review", Err: context.DeadlineExceeded},
	}}
	rr := New(f, "claude", nil, false, t.TempDir())

	if _, err := rr.Run(context.Background(), "work", ref); err == nil {
		t.Fatal("a timeout must be an error: the deadline can fire mid-post")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/review/`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Write the implementation**

Replace `internal/review/review.go` with:

```go
// Package review runs a code review over a prepared checkout by driving the
// claude CLI, so the repository's own CLAUDE.md, the dotnet-techne-code-review
// skill and the sonarqube MCP all apply without firstpass knowing about any of
// them.
package review

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/runner"
)

// Result is the outcome of one review run.
type Result struct {
	ExitCode   int
	ReportPath string
	Stdout     []byte
}

// Runner drives the claude CLI.
type Runner struct {
	r          runner.Runner
	claude     string
	extraArgs  []string
	dryRun     bool
	reportsDir string
}

// New builds a review runner. extraArgs comes from config and normally carries
// --permission-mode bypassPermissions: headless claude cannot run gh to post
// comments under the default mode, and would block on a prompt nobody can
// answer.
func New(r runner.Runner, claude string, extraArgs []string, dryRun bool, reportsDir string) *Runner {
	return &Runner{r: r, claude: claude, extraArgs: extraArgs, dryRun: dryRun, reportsDir: reportsDir}
}

// Prompt is the slash command handed to claude. Dry run and live differ by
// exactly one flag, so a dry-run report is what would have been posted.
func (rr *Runner) Prompt() string {
	if rr.dryRun {
		return "/code-review"
	}
	return "/code-review --comment"
}

// Run reviews the checkout in dir. A non-zero exit or a cancelled context is an
// error, so the caller never records the PR as reviewed on a failed run.
func (rr *Runner) Run(ctx context.Context, dir string, ref prref.PRRef) (Result, error) {
	args := append([]string{"-p", rr.Prompt()}, rr.extraArgs...)

	res, err := rr.r.Run(ctx, dir, rr.claude, args...)
	out := Result{ExitCode: res.ExitCode, Stdout: res.Stdout}
	if err != nil {
		return out, fmt.Errorf("claude for %s: %w", ref.Key(), err)
	}
	if res.ExitCode != 0 {
		return out, fmt.Errorf("claude for %s exit %d: %s",
			ref.Key(), res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	if rr.dryRun {
		path, werr := rr.writeReport(ref, res.Stdout)
		if werr != nil {
			return out, werr
		}
		out.ReportPath = path
	}
	return out, nil
}

func (rr *Runner) writeReport(ref prref.PRRef, body []byte) (string, error) {
	if err := os.MkdirAll(rr.reportsDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(rr.reportsDir,
		fmt.Sprintf("%s_%s_%d.md", ref.Owner, ref.Repo, ref.Number))

	header := fmt.Sprintf("# Review of %s\n\n%s\n\nGenerated %s — dry run, nothing posted.\n\n---\n\n",
		ref.Key(), ref.URL(), time.Now().UTC().Format(time.RFC3339))

	if err := os.WriteFile(path, append([]byte(header), body...), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/review/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/review
git commit -m "$(cat <<'EOF'
feat: drive claude -p /code-review over a prepared checkout

Dry run and live differ by the --comment flag alone, so a dry-run report
is exactly what would have been posted. claude runs with cwd set to the
worktree so the repo's CLAUDE.md and skills apply.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 11: Wire reviewing into the CLI

**Files:**
- Modify: `cmd/firstpass/app.go`
- Modify: `cmd/firstpass/cmd_scan.go`

**Interfaces:**
- Consumes: `worktree.New` (Task 9), `review.New` (Task 10).
- Produces: `openApp` now populates `pipe.WTs` and `pipe.Rev` when `withReview` is true; `scan` accepts `-live` and no longer requires `-print-only`.

- [ ] **Step 1: Write the failing test**

Add to `cmd/firstpass/cmd_scan_test.go`:

```go
func TestOpenAppWithoutReviewLeavesTheReviewerUnwired(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "state_dir: " + filepath.ToSlash(dir) + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := openApp(cfgPath, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if a.pipe.Rev != nil || a.pipe.WTs != nil {
		t.Error("withReview=false must leave the reviewer and worktrees nil, so no build can post by accident")
	}
}

func TestOpenAppWithReviewWiresBothAndHonoursLive(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "state_dir: " + filepath.ToSlash(dir) + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := openApp(cfgPath, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if a.pipe.Rev == nil || a.pipe.WTs == nil {
		t.Fatal("withReview=true must wire both")
	}
	if !a.cfg.DryRun {
		t.Error("dry_run must default to true")
	}

	b, err := openApp(cfgPath, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.cfg.DryRun {
		t.Error("-live must switch dry_run off")
	}
}
```

Add `"os"` and `"path/filepath"` to that file's imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/firstpass/`
Expected: FAIL — `withReview=true must wire both`.

- [ ] **Step 3: Wire the reviewer**

In `cmd/firstpass/app.go`, replace the `_ = withReview // wired in Task 11` line with:

```go
	if withReview {
		p.WTs = worktree.New(r, cfg.Paths.Git, cfg.ReposDir(), cfg.WorkDir())
		p.Rev = review.New(r, cfg.Paths.Claude, cfg.ClaudeArgs, cfg.DryRun, cfg.ReportsDir())
	}
```

and add to the imports:

```go
	"github.com/angelov-todor/firstpass/internal/review"
	"github.com/angelov-todor/firstpass/internal/worktree"
```

- [ ] **Step 4: Let scan actually review**

In `cmd/firstpass/cmd_scan.go`, add a `-live` flag, drop the print-only restriction, and pass `withReview`:

```go
	cfgPath := fs.String("config", config.DefaultConfigPath(), "config file")
	printOnly := fs.Bool("print-only", false, "decide and print, changing no state")
	live := fs.Bool("live", false, "post comments to GitHub even if dry_run is set in config")
	backfill := fs.Int("backfill", 0, "take the last N messages, ignoring the watermark")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := openApp(*cfgPath, *live, !*printOnly)
	if err != nil {
		return err
	}
	defer a.Close()
```

Delete the `if !*printOnly { return errors.New(...) }` block and the now-unused `"errors"` import.

- [ ] **Step 5: Run the tests and build**

Run: `go test ./... && go build ./cmd/firstpass`
Expected: PASS, binary builds.

- [ ] **Step 6: Commit**

```bash
gofmt -l . && go vet ./...
git add cmd/firstpass
git commit -m "$(cat <<'EOF'
feat: wire worktrees and the reviewer into scan

scan now reviews for real, still under dry_run by default; -live is the
explicit opt-in to posting.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

- [ ] **Step 7: CHECKPOINT — read a real dry-run report**

```bash
go build -o firstpass.exe ./cmd/firstpass
./firstpass.exe scan -backfill 20
```

Then open the newest file under `%LOCALAPPDATA%\firstpass\reports\` and read it end to end. **Stop and confirm with the user** that the findings are ones they would be willing to put on a colleague's PR under their own name. Do not proceed to `-live` until they say so.

---

### Task 12: `watch`, `status` and `replay`

**Files:**
- Create: `cmd/firstpass/cmd_watch.go`
- Create: `cmd/firstpass/cmd_status.go`
- Create: `cmd/firstpass/cmd_replay.go`
- Modify: `cmd/firstpass/main.go` (register the three commands)
- Modify: `internal/pipeline/pipeline.go` (add `ReviewOne`)
- Test: `internal/pipeline/replay_test.go`
- Test: `cmd/firstpass/cmd_status_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces: `func (*Pipeline) ReviewOne(ctx context.Context, ref prref.PRRef, opts Options) (Decision, error)`; `func renderStatus(w io.Writer, reviews []store.Review, pending []store.Pending, wm store.Watermark, hasWM, paused, dryRun bool)`.

- [ ] **Step 1: Write the failing test for ReviewOne**

Create `internal/pipeline/replay_test.go`:

```go
package pipeline

import (
	"context"
	"testing"

	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/store"
)

var replayRef = prref.PRRef{Owner: "Example-Org", Repo: "aex-balances", Number: 12}

func TestReviewOneIgnoresATerminalRecord(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.st.PutReview(store.Review{
		Key: replayRef.Key(), Outcome: store.OutcomeReviewed,
	}); err != nil {
		t.Fatal(err)
	}

	d, err := h.p.ReviewOne(context.Background(), replayRef, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionReview {
		t.Fatalf("Action = %q (%s); replay must review despite the existing record", d.Action, d.Reason)
	}
	if len(h.rev.ran) != 1 {
		t.Errorf("ran = %v, want one review", h.rev.ran)
	}
}

func TestReviewOneClearsANeedsAttentionRecord(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.st.PutReview(store.Review{
		Key: replayRef.Key(), Outcome: store.OutcomeNeedsAttention,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.p.ReviewOne(context.Background(), replayRef, Options{}); err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := h.st.Review(replayRef.Key())
	if !ok || rec.Outcome != store.OutcomeReviewed {
		t.Errorf("Outcome = %q, want reviewed after a deliberate replay", rec.Outcome)
	}
}

func TestReviewOneStillHonoursTheOwnerAllowlist(t *testing.T) {
	h := newHarness(t, nil)
	outside := prref.PRRef{Owner: "torvalds", Repo: "linux", Number: 1}

	d, err := h.p.ReviewOne(context.Background(), outside, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionSkip {
		t.Errorf("Action = %q; replay must not be a way around the allowlist", d.Action)
	}
	if len(h.rev.ran) != 0 {
		t.Error("an outside org must never be reviewed, even on an explicit replay")
	}
}

func TestReviewOneIgnoresThePerSweepCap(t *testing.T) {
	h := newHarness(t, nil)
	h.cfg.MaxReviewsPerSweep = 0
	h.apply()

	d, err := h.p.ReviewOne(context.Background(), replayRef, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionReview {
		t.Errorf("Action = %q (%s); an explicit replay is not subject to the throttle", d.Action, d.Reason)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/pipeline/`
Expected: FAIL — `h.p.ReviewOne undefined`.

- [ ] **Step 3: Implement ReviewOne**

Append to `internal/pipeline/pipeline.go`:

```go
// ReviewOne reviews a single pull request on demand, clearing any record that
// would otherwise skip it. The owner allowlist and deny list still apply —
// replay is a way past the dedupe, not past the safety rails — but the
// per-sweep cap does not, since the user asked for this one explicitly.
func (p *Pipeline) ReviewOne(ctx context.Context, ref prref.PRRef, opts Options) (Decision, error) {
	if err := p.Store.DeleteReview(ref.Key()); err != nil {
		return Decision{Ref: ref}, err
	}
	if err := p.Store.DeletePending(ref.Key()); err != nil {
		return Decision{Ref: ref}, err
	}

	rep := SweepReport{Paused: p.paused()}
	// The cap is a sweep throttle; an explicit replay is not throttled.
	saved := p.Cfg.MaxReviewsPerSweep
	p.Cfg.MaxReviewsPerSweep = saved + 1
	defer func() { p.Cfg.MaxReviewsPerSweep = saved }()

	return p.handle(ctx, candidate{ref: ref, trigger: "replay"}, &rep, opts), nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/pipeline/ -v -run ReviewOne`
Expected: PASS.

- [ ] **Step 5: Write the failing test for status rendering**

Create `cmd/firstpass/cmd_status_test.go`:

```go
package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/angelov-todor/firstpass/internal/store"
)

func TestRenderStatusShowsReviewsPendingAndMode(t *testing.T) {
	reviews := []store.Review{
		{Key: "Example-Org/aex-balances#12", Outcome: store.OutcomeReviewed,
			DecidedAt: time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC), DurationMS: 91000},
		{Key: "Example-Org/aex-margin-service#3", Outcome: store.OutcomeNeedsAttention,
			DecidedAt: time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC),
			Detail:    "a previous run died mid-review"},
	}
	pending := []store.Pending{
		{Key: "Example-Org/aex-history-service#48", Attempts: 2, LastReason: "draft"},
	}
	wm := store.Watermark{MessageName: "spaces/A/messages/m9",
		CreateTime: time.Date(2026, 9, 3, 11, 30, 0, 0, time.UTC)}

	var buf bytes.Buffer
	renderStatus(&buf, reviews, pending, wm, true, false, true)
	out := buf.String()

	for _, want := range []string{
		"Example-Org/aex-balances#12", "reviewed",
		"Example-Org/aex-margin-service#3", "needs_attention", "died mid-review",
		"Example-Org/aex-history-service#48", "draft",
		"spaces/A/messages/m9", "dry run",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
}

func TestRenderStatusCallsOutNeedsAttentionAsActionable(t *testing.T) {
	reviews := []store.Review{
		{Key: "o/r#1", Outcome: store.OutcomeNeedsAttention, Detail: "review did not finish"},
	}
	var buf bytes.Buffer
	renderStatus(&buf, reviews, nil, store.Watermark{}, false, false, false)
	out := buf.String()
	if !strings.Contains(out, "replay") {
		t.Errorf("a needs_attention row must tell the user how to act on it:\n%s", out)
	}
}

func TestRenderStatusShowsPaused(t *testing.T) {
	var buf bytes.Buffer
	renderStatus(&buf, nil, nil, store.Watermark{}, false, true, false)
	if !strings.Contains(buf.String(), "PAUSED") {
		t.Errorf("a paused daemon must be obvious:\n%s", buf.String())
	}
}

func TestRenderStatusHandlesAFreshStore(t *testing.T) {
	var buf bytes.Buffer
	renderStatus(&buf, nil, nil, store.Watermark{}, false, false, true)
	out := buf.String()
	if !strings.Contains(out, "no watermark") {
		t.Errorf("a fresh store must say the next run is a cold start:\n%s", out)
	}
}
```

- [ ] **Step 6: Run the test to verify it fails**

Run: `go test ./cmd/firstpass/`
Expected: FAIL — `undefined: renderStatus`.

- [ ] **Step 7: Write the three commands**

Create `cmd/firstpass/cmd_status.go`:

```go
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/store"
)

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := openApp(*cfgPath, false, false)
	if err != nil {
		return err
	}
	defer a.Close()

	reviews, err := a.store.Reviews()
	if err != nil {
		return err
	}
	pending, err := a.store.AllPending()
	if err != nil {
		return err
	}
	wm, hasWM, err := a.store.Watermark()
	if err != nil {
		return err
	}

	_, statErr := os.Stat(a.cfg.PauseFile())
	renderStatus(os.Stdout, reviews, pending, wm, hasWM, statErr == nil, a.cfg.DryRun)
	return nil
}

func renderStatus(w io.Writer, reviews []store.Review, pending []store.Pending,
	wm store.Watermark, hasWM, paused, dryRun bool) {

	mode := "live — comments are posted to GitHub"
	if dryRun {
		mode = "dry run — nothing is posted"
	}
	fmt.Fprintf(w, "mode: %s\n", mode)
	if paused {
		fmt.Fprintln(w, "state: PAUSED — nothing will be reviewed or posted until `firstpass resume`")
	}
	if hasWM {
		fmt.Fprintf(w, "watermark: %s (%s)\n", wm.MessageName, wm.CreateTime.Format(time.RFC3339))
	} else {
		fmt.Fprintln(w, "watermark: none — the next run is a cold start and will review nothing")
	}

	sort.Slice(reviews, func(i, j int) bool { return reviews[i].DecidedAt.After(reviews[j].DecidedAt) })

	fmt.Fprintf(w, "\nreviews (%d)\n", len(reviews))
	if len(reviews) > 0 {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "PR\tOUTCOME\tDECIDED\tTOOK\tDETAIL")
		needsAttention := 0
		for _, r := range reviews {
			decided := "-"
			if !r.DecidedAt.IsZero() {
				decided = r.DecidedAt.Format("2006-01-02 15:04")
			}
			took := "-"
			if r.DurationMS > 0 {
				took = (time.Duration(r.DurationMS) * time.Millisecond).Round(time.Second).String()
			}
			if r.Outcome == store.OutcomeNeedsAttention {
				needsAttention++
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Key, r.Outcome, decided, took, r.Detail)
		}
		tw.Flush()
		if needsAttention > 0 {
			fmt.Fprintf(w, "\n%d need attention. Each may already carry partial comments; "+
				"run `firstpass replay <pr-url>` to review one again deliberately.\n", needsAttention)
		}
	}

	fmt.Fprintf(w, "\npending (%d)\n", len(pending))
	if len(pending) > 0 {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "PR\tATTEMPTS\tLAST REASON")
		for _, p := range pending {
			fmt.Fprintf(tw, "%s\t%d\t%s\n", p.Key, p.Attempts, p.LastReason)
		}
		tw.Flush()
	}
}
```

Create `cmd/firstpass/cmd_replay.go`:

```go
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: firstpass replay [-live] <pr-url | owner/repo#n>")
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

	d, err := a.pipe.ReviewOne(context.Background(), refs[0], pipeline.Options{})
	if err != nil {
		return err
	}
	renderSweep(os.Stdout, pipeline.SweepReport{
		MessagesScanned: 0,
		Decisions:       []pipeline.Decision{d},
	}, a.cfg.DryRun)
	return nil
}
```

Create `cmd/firstpass/cmd_watch.go`:

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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

	a, err := openApp(*cfgPath, *live, true)
	if err != nil {
		return err
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	a.log.Info("watching",
		"space", a.cfg.Space, "interval", a.cfg.Interval.D(), "dry_run", a.cfg.DryRun)

	ticker := time.NewTicker(a.cfg.Interval.D())
	defer ticker.Stop()

	for {
		rep, err := a.pipe.Sweep(ctx, pipeline.Options{})
		switch {
		case err == nil:
			a.log.Info("sweep done",
				"messages", rep.MessagesScanned, "reviewed", rep.Reviewed,
				"decisions", len(rep.Decisions), "paused", rep.Paused)
			for _, d := range rep.Decisions {
				if d.Action == pipeline.ActionNeedsAttention {
					a.log.Warn("needs attention", "pr", d.Ref.URL(), "reason", d.Reason)
				}
			}
		case errors.Is(err, context.Canceled):
			a.log.Info("stopping")
			return nil
		default:
			// A missing scope or a revoked grant cannot be fixed by waiting, so
			// stop rather than log the same failure every interval forever.
			var apiErr *chat.APIError
			if errors.As(err, &apiErr) && apiErr.Fatal() {
				return fmt.Errorf("unrecoverable Google Chat error: %w", err)
			}
			a.log.Error("sweep failed", "err", err)
		}

		select {
		case <-ctx.Done():
			a.log.Info("stopping")
			return nil
		case <-ticker.C:
		}
	}
}
```

- [ ] **Step 8: Register the commands**

In `cmd/firstpass/main.go`, add to the switch:

```go
	case "watch":
		err = cmdWatch(args)
	case "status":
		err = cmdStatus(args)
	case "replay":
		err = cmdReplay(args)
```

- [ ] **Step 9: Run everything**

Run: `go test ./... && go vet ./... && go build ./cmd/firstpass`
Expected: all PASS, binary builds.

- [ ] **Step 10: Commit**

```bash
gofmt -l .
git add cmd/firstpass internal/pipeline
git commit -m "$(cat <<'EOF'
feat: watch loop, status table and deliberate replay

replay clears the dedupe record but not the owner allowlist -- it is a
way past the dedupe, not past the safety rails. watch stops on an
unrecoverable chat error rather than logging it every interval forever.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

- [ ] **Step 11: Final manual verification**

```bash
./firstpass.exe doctor
./firstpass.exe status
./firstpass.exe pause
./firstpass.exe watch      # confirm it sweeps, reviews nothing, logs "paused"
# Ctrl-C
./firstpass.exe resume
./firstpass.exe status
```

Confirm each against the success criteria in the spec, then **stop and hand back to the user**. Going live is their call: it is a one-line config change (`dry_run: false`) or the `-live` flag, and the plan deliberately stops short of making it.

---

## Going live

Not a task — a decision for the user, after they have read real dry-run reports.

```yaml
# %APPDATA%\firstpass\config.yaml
dry_run: false
```

Then `firstpass watch`. To run it unattended, register `firstpass scan` with Task Scheduler on the same interval instead of leaving `watch` in a terminal.
