package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/pomesaka/claude-deck/internal/store"
)

// SentInfo is the JSON form of a text claude-deck typed into a session on
// the user's behalf (store.Sent).
type SentInfo struct {
	SessionID       string    `json:"session_id"`
	ClaudeSessionID string    `json:"claude_session_id,omitempty"`
	SentAt          time.Time `json:"sent_at"`
	Source          string    `json:"source"`
	TextSHA256      string    `json:"text_sha256"`
}

func sentInfos(rows []store.Sent) []SentInfo {
	out := make([]SentInfo, len(rows))
	for i, r := range rows {
		out[i] = SentInfo{SessionID: r.SessionID, ClaudeSessionID: r.ClaudeSessionID, SentAt: r.SentAt, Source: r.Source, TextSHA256: r.TextSHA256}
	}
	return out
}

func parseSentArgs(args []string) (cliRequest, error) {
	fs := flag.NewFlagSet("sent", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck sent [--since TIME]")
		fmt.Fprintln(fs.Output(), "Lists the texts claude-deck typed into sessions on the user's behalf (new/fork --prompt, send, clear, broadcast), oldest first, with the SHA-256 of each. Claude Code records them as the user's own words.")
		fs.PrintDefaults()
	}
	since := fs.String("since", "", "only texts sent at this time or later: RFC 3339 (2026-11-28T09:00:00+09:00) or a duration back from now (24h)")
	if err := fs.Parse(args); err != nil {
		return cliRequest{}, err
	}
	if fs.NArg() > 0 {
		return cliRequest{}, fmt.Errorf("sent: unexpected arguments: %v", fs.Args())
	}
	t, err := parseSince(*since, time.Now())
	if err != nil {
		return cliRequest{}, fmt.Errorf("sent: --since: %w", err)
	}
	return cliRequest{Op: "sent", Since: t}, nil
}

// parseSince reads --since: empty is the beginning, a duration counts back from now.
func parseSince(v string, now time.Time) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d < 0 {
			return time.Time{}, fmt.Errorf("%q is negative", v)
		}
		return now.Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither RFC 3339 nor a duration", v)
	}
	return t, nil
}
