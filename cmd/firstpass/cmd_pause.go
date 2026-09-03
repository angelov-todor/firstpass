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
