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
			want: []PRRef{{"example-org", "aex-user-service", 91}},
		},
		{
			name: "trailing period",
			text: "see https://github.com/Example-Org/aex-balances/pull/7.",
			want: []PRRef{{"example-org", "aex-balances", 7}},
		},
		{
			name: "files suffix",
			text: "https://github.com/Example-Org/aex-balances/pull/7/files",
			want: []PRRef{{"example-org", "aex-balances", 7}},
		},
		{
			name: "query suffix",
			text: "https://github.com/Example-Org/aex-balances/pull/7?diff=split",
			want: []PRRef{{"example-org", "aex-balances", 7}},
		},
		{
			name: "comment fragment yields only the pr",
			text: "https://github.com/Example-Org/aex-balances/pull/7#issuecomment-1",
			want: []PRRef{{"example-org", "aex-balances", 7}},
		},
		{
			name: "team batch format",
			text: "Please review the following PRs — margin follow-ups:\n" +
				"• https://github.com/Example-Org/aex-margin-service/pull/1\n" +
				"• https://github.com/Example-Org/aex-balances/pull/2\n",
			want: []PRRef{
				{"example-org", "aex-margin-service", 1},
				{"example-org", "aex-balances", 2},
			},
		},
		{
			name: "same pr twice is deduped",
			text: "https://github.com/Example-Org/a/pull/1 and https://github.com/Example-Org/a/pull/1",
			want: []PRRef{{"example-org", "a", 1}},
		},
		{
			name: "shorthand accepted",
			text: "Example-Org/aex-user-service#91 is ready",
			want: []PRRef{{"example-org", "aex-user-service", 91}},
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
			want: []PRRef{{"example-org", "a", 91}},
		},
		{
			name: "issues url with fragment does not parse fragment as shorthand",
			text: "https://github.com/Example-Org/a/issues/5#12",
			want: nil,
		},
		{
			name: "pull url with unrelated shorthand does not extract fabricated pr",
			text: "https://github.com/Example-Org/a/pull/7/Example-Org/b#5",
			want: []PRRef{{"example-org", "a", 7}},
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
	want := PRRef{Owner: "example-org", Repo: "aex-balances", Number: 5}
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

// I4: owner/repo case must not be part of the dedupe identity. GitHub URLs
// are case-insensitive and the operator's own path for this org is lowercase,
// so two spellings of one PR would otherwise become two bbolt keys, two
// review records and two comment sets.
func TestExtractCanonicalisesCase(t *testing.T) {
	got := Extract("https://github.com/example-org/aex-balances/pull/91 and " +
		"https://github.com/Example-Org/aex-balances/pull/91")
	if len(got) != 1 {
		t.Fatalf("Extract() = %v, want the two spellings deduplicated to one ref", got)
	}
	if got[0].Owner != "example-org" || got[0].Repo != "aex-balances" {
		t.Errorf("Extract()[0] = %v, want owner and repo lowercased", got[0])
	}
}

func TestKeyIsIdenticalForBothSpellings(t *testing.T) {
	lower := Extract("https://github.com/example-org/aex-balances/pull/91")
	upper := Extract("Example-Org/aex-balances#91")
	if len(lower) != 1 || len(upper) != 1 {
		t.Fatalf("lower = %v, upper = %v", lower, upper)
	}
	if lower[0].Key() != upper[0].Key() {
		t.Errorf("Key() = %q and %q; one PR must have one key", lower[0].Key(), upper[0].Key())
	}
}

func TestParseKeyCanonicalisesCase(t *testing.T) {
	got, err := ParseKey("Example-Org/aex-balances#5")
	if err != nil {
		t.Fatal(err)
	}
	if got.Key() != "example-org/aex-balances#5" {
		t.Errorf("ParseKey(...).Key() = %q, want the canonical lowercase key", got.Key())
	}
	back, err := ParseKey(got.Key())
	if err != nil {
		t.Fatal(err)
	}
	if back != got {
		t.Errorf("ParseKey(Key()) = %v, want %v", back, got)
	}
}
