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
	// Nothing firstpass runs may ever stop and wait for a human: it is a daemon
	// on a machine whose operator is not watching it. Git honours this in the
	// environment only, which is why it is set here for the whole process
	// rather than per invocation — runner.Runner deliberately exposes no
	// environment parameter. The git -c options in internal/worktree cover the
	// credential-helper side, which this variable alone does not.
	if err := os.Setenv("GIT_TERMINAL_PROMPT", "0"); err != nil {
		fmt.Fprintln(os.Stderr, "firstpass: could not disable git terminal prompts:", err)
		os.Exit(1)
	}

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}

	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "scan":
		err = cmdScan(args)
	case "watch":
		err = cmdWatch(args)
	case "status":
		err = cmdStatus(args)
	case "replay":
		err = cmdReplay(args)
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
