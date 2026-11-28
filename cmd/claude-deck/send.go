package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/pomesaka/claude-deck/internal/session"
	"github.com/pomesaka/claude-deck/internal/store"
	"github.com/pomesaka/claude-deck/internal/tmux"
)

// clearText is what `claude-deck clear` sends: it is `send -p /clear`.
const clearText = "/clear"

// SendInfo is the JSON form of what send, clear and broadcast did for one session.
type SendInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Sent is whether the text was typed; false with dry_run.
	Sent bool `json:"sent"`
	// SkippedReason is why it was not typed (broadcast only); empty when it
	// was, or with dry_run would have been.
	SkippedReason string `json:"skipped_reason,omitempty"`
	DryRun        bool   `json:"dry_run"`
}

// errNoTargets is returned by broadcast when no live session passes the filter.
// WHY 0 以外で止める: 打ち間違えた絞り込みを、送れたと取り違えないため。
var errNoTargets = errors.New("broadcast: no live session matches the filter")

// textFlag registers -p on fs.
func textFlag(fs *flag.FlagSet) *string {
	return fs.String("p", "", "the text to type, one line")
}

func parseSendArgs(args []string) (cliRequest, error) {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck send --id ID|NAME | --self -p TEXT [--dry-run]")
		fmt.Fprintln(fs.Output(), "Types TEXT and Enter into the session's input box, as if the user typed it, only while it is idle or waiting_action with nothing typed. --self types into this session, mid-turn too: Claude Code runs the line once the turn ends.")
		fs.PrintDefaults()
	}
	self := fs.Bool("self", false, "type into this session ($"+session.EnvSessionID+") instead of --id")
	text := textFlag(fs)
	req, err := parseTargetArgs(fs, "send", args, text)
	if err != nil {
		return cliRequest{}, err
	}
	switch {
	case *self && req.Target != "":
		return cliRequest{}, errors.New("send: give --id or --self, not both")
	case *self && os.Getenv(session.EnvSessionID) == "":
		return cliRequest{}, fmt.Errorf("send --self: %w ($%s is not set)", session.ErrSendNoSelf, session.EnvSessionID)
	case !*self && req.Target == "":
		return cliRequest{}, errors.New("send: --id or --self is required")
	}
	req.Self = *self
	return req, nil
}

func parseClearArgs(args []string) (cliRequest, error) {
	fs := flag.NewFlagSet("clear", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck clear --id ID|NAME [--dry-run]")
		fmt.Fprintln(fs.Output(), "Same as send -p /clear: empties the session's context.")
		fs.PrintDefaults()
	}
	text := clearText
	req, err := parseTargetArgs(fs, "clear", args, &text)
	if err == nil && req.Target == "" {
		err = errors.New("clear: --id is required")
	}
	return req, err
}

// parseTargetArgs parses --id and --dry-run of send and clear into a send
// request. The caller checks that a target was given.
func parseTargetArgs(fs *flag.FlagSet, cmd string, args []string, text *string) (cliRequest, error) {
	target := fs.String("id", "", "ID or name of the session")
	dryRun := fs.Bool("dry-run", false, "check whether the text would be sent, without sending it")
	if err := fs.Parse(args); err != nil {
		return cliRequest{}, err
	}
	if fs.NArg() > 0 {
		return cliRequest{}, fmt.Errorf("%s: unexpected arguments: %v", cmd, fs.Args())
	}
	if err := session.CheckSendText(*text); err != nil {
		return cliRequest{}, fmt.Errorf("%s: -p: %w", cmd, err)
	}
	return cliRequest{Op: "send", Target: *target, Text: *text, DryRun: *dryRun}, nil
}

func parseBroadcastArgs(args []string) (cliRequest, error) {
	fs := flag.NewFlagSet("broadcast", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck broadcast -p TEXT [--alias TEXT] [--repo TEXT] [--only ID|NAME,...] [--dry-run]")
		fmt.Fprintln(fs.Output(), "Types TEXT into every live session that passes the filter (as list takes it) and accepts input, as send does. Prints what it did for each.")
		fs.PrintDefaults()
	}
	text := textFlag(fs)
	var filter sessionFilter
	filter.register(fs)
	dryRun := fs.Bool("dry-run", false, "list what would be sent and skipped, without sending")
	if err := fs.Parse(args); err != nil {
		return cliRequest{}, err
	}
	if fs.NArg() > 0 {
		return cliRequest{}, fmt.Errorf("broadcast: unexpected arguments: %v", fs.Args())
	}
	if err := session.CheckSendText(*text); err != nil {
		return cliRequest{}, fmt.Errorf("broadcast: -p: %w", err)
	}
	return cliRequest{Op: "broadcast", Filter: filter, Text: *text, DryRun: *dryRun}, nil
}

// sendSession runs `claude-deck send` (and clear) against the store and tmux
// server given: into key, or into self when toSelf (send --self).
func sendSession(st *store.Store, runner *tmux.Runner, self session.DeckSessionID, key string, toSelf bool, text string, dryRun bool) (SendInfo, error) {
	var (
		s    session.Snapshot
		sent bool
		err  error
	)
	if toSelf {
		s, sent, err = session.SendSelf(st, runner, self, text, dryRun)
	} else {
		s, sent, err = session.SendStored(st, runner, self, key, text, dryRun)
	}
	if sent {
		noteSent(st, s, session.SentBySend, text)
	}
	if err != nil {
		return SendInfo{}, err
	}
	return SendInfo{ID: string(s.ID), Name: s.Name, Sent: sent, DryRun: dryRun}, nil
}

// broadcast runs `claude-deck broadcast`: it sends text to the live sessions
// that pass filter, and returns what it did for each, this session included
// (skipped). It fails only when no live session passes filter.
//
// WHY 飛ばしたセッションがあっても 0 で終える: 宛先が全部か絞り込んだ集まりなので、
// 動いているセッションが飛ばされるのはふつうの結果で、どれを飛ばしたかは出力の skipped_reason にある。
// 1 つの宛先を指す send と clear は、送れなければ頼まれたことができていないので 0 以外で終える。
func broadcast(st *store.Store, runner *tmux.Runner, self session.DeckSessionID, filter sessionFilter, text string, dryRun bool) ([]SendInfo, error) {
	snaps, err := session.ListStored(st)
	if err != nil {
		return nil, err
	}
	var targets []session.Snapshot
	for _, s := range snaps {
		if s.HasProcess() && filter.matches(s) {
			targets = append(targets, s)
		}
	}
	if len(targets) == 0 {
		return nil, errNoTargets
	}
	if err := checkOnly(targets, filter.Only); err != nil {
		return nil, fmt.Errorf("broadcast: %w", err)
	}
	deliveries, err := session.Broadcast(runner, targets, self, text, dryRun)
	if err != nil {
		return nil, err
	}
	infos := make([]SendInfo, len(deliveries))
	for i, d := range deliveries {
		infos[i] = SendInfo{ID: string(d.Session.ID), Name: d.Session.Name, Sent: d.Sent, DryRun: dryRun}
		if d.Skipped != nil {
			infos[i].SkippedReason = d.Skipped.Error()
		}
		if d.Sent {
			noteSent(st, d.Session, session.SentByBroadcast, text)
		}
	}
	return infos, nil
}

// noteSent records that text reached s (session.RecordSent), and warns on
// stderr when it could not: a text with no row is read as the user's own words.
// WHY 送ったことは取り消さない: 文はもう届いている。
func noteSent(st *store.Store, s session.Snapshot, source session.SentSource, text string) {
	if _, err := session.RecordSent(st, s, source, text, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v; claude-deck sent will not list it\n", err)
	}
}
