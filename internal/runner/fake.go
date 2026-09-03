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
