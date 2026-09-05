package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLoadRejectsAKeyInTheWrongPlace is the fix for a silent failure that did
// real damage rather than a hypothetical one.
//
// state_dir is top-level. Written under `paths:` it parses cleanly, sets
// nothing, and leaves firstpass on the default state directory. During a
// diagnostic session a config written specifically to isolate a test run from
// production was accepted exactly that way: `status` then reported the
// production watermark and all 61 production review records, and a review run
// under it would have written to the live database. Nothing in the accepted
// config said so.
//
// The general shape -- a key that is present, parses, and means nothing -- is
// the same one that shipped a config.yaml.example with two `paths:` blocks.
func TestLoadRejectsAKeyInTheWrongPlace(t *testing.T) {
	p := writeConfig(t, `
space: "spaces/AAA"
paths:
  state_dir: "C:/somewhere/else"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("state_dir under paths: must be rejected, not silently ignored while " +
			"firstpass keeps using the default state directory")
	}
	if !strings.Contains(err.Error(), "state_dir") {
		t.Errorf("the error must name the offending key, got: %v", err)
	}
}

func TestLoadRejectsAnUnknownKey(t *testing.T) {
	p := writeConfig(t, "space: \"spaces/AAA\"\nrevue_concurrency: 3\n")
	if _, err := Load(p); err == nil {
		t.Error("a misspelled key must be rejected: it silently takes the default otherwise")
	}
}

// TestLoadAcceptsAConfigThatOmitsFields guards the other direction. Strictness
// must reject only keys that are present and meaningless -- an absent key is
// how every default is meant to be taken, and config.yaml.example says so
// explicitly.
func TestLoadAcceptsAConfigThatOmitsFields(t *testing.T) {
	p := writeConfig(t, "space: \"spaces/AAA\"\n")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("a config that sets one field must load: %v", err)
	}
	if c.ReviewConcurrency != 1 || c.MaxReviewsPerSweep != 3 {
		t.Errorf("omitted fields must keep their defaults, got concurrency=%d cap=%d",
			c.ReviewConcurrency, c.MaxReviewsPerSweep)
	}
	if c.StateDir != DefaultStateDir() {
		t.Errorf("StateDir = %q, want the default", c.StateDir)
	}
}

// TestLoadAcceptsAnEmptyFile pins the io.EOF branch. An empty config is
// every default, and Validate is what then reports the required fields --
// distinguishing "you configured nothing" from "your file is malformed".
func TestLoadAcceptsAnEmptyFile(t *testing.T) {
	p := writeConfig(t, "")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("an empty config file must load as the defaults: %v", err)
	}
	if c.MaxReviewsPerSweep != 3 {
		t.Errorf("MaxReviewsPerSweep = %d, want the default 3", c.MaxReviewsPerSweep)
	}
	if err := c.Validate(); err == nil {
		t.Error("defaults alone must still fail validation")
	}
}

// TestLoadLenientKeepsTheKillSwitchReachable is the counterweight to strict
// decoding.
//
// Strictness is right for the commands that act: a key in the wrong place
// silently keeps its default, and firstpass acting on a configuration the
// operator does not believe it has is the failure worth being loud about. It
// is not right for `firstpass pause`, which needs state_dir and nothing else.
// A typo in some unrelated key must not take away the operator's ability to
// stop a live sweep that is posting comments to colleagues' pull requests.
func TestLoadLenientKeepsTheKillSwitchReachable(t *testing.T) {
	p := writeConfig(t, `
space: "spaces/AAA"
state_dir: "C:/state"
revue_concurrency: 3
paths:
  state_dir: "C:/wrong"
`)
	if _, err := Load(p); err == nil {
		t.Fatal("the strict loader must reject this file, or this test proves nothing")
	}

	c, unknown, err := LoadLenient(p)
	if err != nil {
		t.Fatalf("the lenient loader must still load: %v", err)
	}
	if c.StateDir != "C:/state" {
		t.Errorf("StateDir = %q, want the value from the file: pause writes its file there", c.StateDir)
	}
	if len(unknown) == 0 {
		t.Error("the ignored keys must be reported, or leniency is just the silence " +
			"strict decoding was added to fix")
	}
}

// Malformed YAML is still an error under the lenient loader: it relaxes which
// keys are accepted, not whether the file parses at all.
func TestLoadLenientStillRejectsMalformedYAML(t *testing.T) {
	p := writeConfig(t, "space: \"unterminated\n  - [{\n")
	if _, _, err := LoadLenient(p); err == nil {
		t.Error("unparseable YAML must fail even leniently")
	}
}
