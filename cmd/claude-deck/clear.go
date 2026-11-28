package main

import (
	"flag"
	"fmt"

	"github.com/pomesaka/claude-deck/internal/session"
	"github.com/pomesaka/claude-deck/internal/store"
	"github.com/pomesaka/claude-deck/internal/tmux"
)

// ClearInfo is the JSON form of what `claude-deck clear` did.
type ClearInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Sent is whether /clear was typed; false with dry_run.
	Sent   bool `json:"sent"`
	DryRun bool `json:"dry_run"`
}

func parseClearArgs(args []string) (cliRequest, error) {
	fs := flag.NewFlagSet("clear", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck clear --id ID|NAME [--dry-run]")
		fmt.Fprintln(fs.Output(), "Types /clear into the session's input box, only while it is idle or waiting_action with nothing typed. It is never sent to this session.")
		fs.PrintDefaults()
	}
	target := fs.String("id", "", "ID or name of the session to clear")
	dryRun := fs.Bool("dry-run", false, "check whether /clear would be sent, without sending it")
	if err := fs.Parse(args); err != nil {
		return cliRequest{}, err
	}
	if fs.NArg() > 0 {
		return cliRequest{}, fmt.Errorf("clear: unexpected arguments: %v", fs.Args())
	}
	if *target == "" {
		return cliRequest{}, fmt.Errorf("clear: --id is required")
	}
	return cliRequest{Op: "clear", Target: *target, DryRun: *dryRun}, nil
}

// clearSession runs `claude-deck clear` against the store and tmux server given.
func clearSession(st *store.Store, runner *tmux.Runner, self session.DeckSessionID, key string, dryRun bool) (ClearInfo, error) {
	s, sent, err := session.ClearStored(st, runner, self, key, dryRun)
	if err != nil {
		return ClearInfo{}, err
	}
	return ClearInfo{ID: string(s.ID), Name: s.Name, Sent: sent, DryRun: dryRun}, nil
}
