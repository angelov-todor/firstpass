package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadYAML(t *testing.T, body string) (Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

// usable is everything Validate insists on apart from the sources themselves,
// so a test about sources fails on its own subject rather than on a missing
// chat script.
func usable(c Config) Config {
	c.GithubLogin = "angelov-todor"
	c.AllowOwners = []string{"AstraBit-CPT"}
	c.Paths.ChatScript = "chat.py"
	return c
}

// TestBothSourcesAreDeclaredInTheConfig is what the config file is for: it
// names every place firstpass looks, rather than naming one and implying the
// other.
func TestBothSourcesAreDeclaredInTheConfig(t *testing.T) {
	c, err := loadYAML(t, `
sources:
  - type: chat
    space: "spaces/AAQA7zIDu54"
  - type: github
    owner: AstraBit-CPT
    repo_prefixes: [aex-]
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Sources) != 2 {
		t.Fatalf("both sources must survive the decode, got %d", len(c.Sources))
	}
	// A chat source's space is the space: everything downstream keeps reading
	// one field and knows nothing about how it was written.
	if c.Space != "spaces/AAQA7zIDu54" {
		t.Errorf("Space = %q, want the chat source's space", c.Space)
	}
	if err := usable(c).Validate(); err != nil {
		t.Errorf("this is a valid configuration: %v", err)
	}
}

// TestTheChatSpaceCannotBeTurnedOffBySilence is the property that kept chat
// out of the list in the first place, and it survives chat being in the list.
//
// Deleting the chat source does not produce a firstpass that quietly reviews
// only what GitHub offers. It produces an error, because a config that no
// longer watches the team's space is a mistake nobody would see in a log.
func TestTheChatSpaceCannotBeTurnedOffBySilence(t *testing.T) {
	c, err := loadYAML(t, `
sources:
  - type: github
    owner: AstraBit-CPT
`)
	if err != nil {
		t.Fatal(err)
	}
	err = usable(c).Validate()
	if err == nil {
		t.Fatal("a config with no chat space at all must be refused")
	}
	if !strings.Contains(err.Error(), "chat") {
		t.Errorf("the error must say what is missing: %v", err)
	}
}

// An installation written before sources existed has only the top-level key
// and must keep working untouched.
func TestTheTopLevelSpaceStillWorksOnItsOwn(t *testing.T) {
	c, err := loadYAML(t, "space: \"spaces/AAQA7zIDu54\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Space != "spaces/AAQA7zIDu54" {
		t.Errorf("Space = %q", c.Space)
	}
	if err := usable(c).Validate(); err != nil {
		t.Errorf("a config that predates sources must still be valid: %v", err)
	}
}

// Two chat sources would be a config that looks like it watches both spaces
// and silently watches whichever was written last, because everything
// downstream reads a single space.
func TestTwoChatSourcesAreRefused(t *testing.T) {
	c, err := loadYAML(t, `
sources:
  - type: chat
    space: "spaces/one"
  - type: chat
    space: "spaces/two"
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := usable(c).Validate(); err == nil {
		t.Fatal("two chat sources must be refused rather than silently resolved")
	}
}

// The likeliest mistake in this block is a mistyped type, and the settings
// left on the wrong entry are what give it away. Ignoring them would silently
// drop the GitHub source the operator thought they had written.
func TestSettingsOnTheWrongKindOfSourceAreRefused(t *testing.T) {
	cases := map[string]string{
		"github settings on a chat source": `
sources:
  - type: chat
    space: "spaces/x"
    owner: AstraBit-CPT
    repo_prefixes: [aex-]
`,
		"a space on a github source": `
sources:
  - type: chat
    space: "spaces/x"
  - type: github
    owner: AstraBit-CPT
    space: "spaces/y"
`,
		"an unknown type": `
sources:
  - type: chat
    space: "spaces/x"
  - type: gitlab
    owner: AstraBit-CPT
`,
	}
	for name, body := range cases {
		c, err := loadYAML(t, body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := usable(c).Validate(); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

// A source pointed outside allow_owners is not dangerous -- every candidate it
// produced would be refused by the same allowlist -- but it is certainly a
// mistake, and one that looks in the log exactly like a broken search rather
// than a misconfigured one.
func TestASourceOutsideTheAllowlistIsRefused(t *testing.T) {
	c, err := loadYAML(t, `
space: "spaces/x"
sources:
  - type: github
    owner: Someone-Else
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := usable(c).Validate(); err == nil {
		t.Fatal("a source outside allow_owners must be refused")
	}
}
