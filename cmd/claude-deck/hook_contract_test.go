package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/pomesaka/claude-deck/internal/session"
)

// The deck-status plugin (TypeScript) and `claude-deck hook` (Go) agree on the
// command line only by convention, and the plugin drops every failure: if one
// side changes alone, statuses stop updating with no error anywhere. These tests
// read the plugin's source and run what it would run through parseHookArgs.

const registerTS = "../../deckmod/hooks/register.ts"

var (
	// deck($, ['status', status]) → "'status', status"
	deckCallRe = regexp.MustCompile(`deck\(\$, \[([^\]]*)\]\)`)
	// type DeckStatus = 'running' | 'idle' | …
	deckStatusTypeRe = regexp.MustCompile(`(?m)^type DeckStatus = (.+)$`)
	// $.process.run([env.bin, 'hook', ...args, '--session', env.id], …
	deckRunRe   = regexp.MustCompile(`\$\.process\.run\(\[env\.bin, '([^']+)', \.\.\.args, '([^']+)', env\.id\]`)
	tsLiteralRe = regexp.MustCompile(`^'([^']*)'$`)
	envGetRe    = regexp.MustCompile(`\$\.env\.get\('([^']+)'\)`)
)

func readRegisterTS(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(registerTS)
	if err != nil {
		t.Fatalf("reading the plugin source: %v", err)
	}
	return string(src)
}

// deckCalls returns the argument list of every deck($, [...]) call, keyed by
// its event. Arguments that are not string literals are returned as "".
func deckCalls(t *testing.T, src string) map[string][]string {
	t.Helper()
	calls := make(map[string][]string)
	for _, m := range deckCallRe.FindAllStringSubmatch(src, -1) {
		var args []string
		for part := range strings.SplitSeq(m[1], ", ") {
			if lit := tsLiteralRe.FindStringSubmatch(part); lit != nil {
				args = append(args, lit[1])
			} else {
				args = append(args, "")
			}
		}
		if len(args) == 0 || args[0] == "" {
			t.Fatalf("deck() call without a literal event: %q", m[0])
		}
		if prev, ok := calls[args[0]]; ok && !slices.Equal(prev, args) {
			t.Fatalf("event %q is called with two argument shapes: %v and %v", args[0], prev, args)
		}
		calls[args[0]] = args
	}
	return calls
}

func TestHookContract_Statuses(t *testing.T) {
	m := deckStatusTypeRe.FindStringSubmatch(readRegisterTS(t))
	if m == nil {
		t.Fatal("type DeckStatus not found in the plugin source")
	}
	var got []string
	for part := range strings.SplitSeq(m[1], " | ") {
		lit := tsLiteralRe.FindStringSubmatch(part)
		if lit == nil {
			t.Fatalf("DeckStatus member %q is not a string literal", part)
		}
		got = append(got, lit[1])
	}
	slices.Sort(got)

	want := []string{"idle", "running", "subagent_running", "waiting_answer", "waiting_approval"}
	if !slices.Equal(got, want) {
		t.Errorf("plugin statuses = %v, want %v", got, want)
	}
	for _, status := range got {
		if _, err := parseHookArgs([]string{"status", status, "--session", "abc"}); err != nil {
			t.Errorf("hook status %s: %v", status, err)
		}
	}
}

func TestHookContract_Calls(t *testing.T) {
	src := readRegisterTS(t)
	calls := deckCalls(t, src)

	run := deckRunRe.FindStringSubmatch(src)
	if run == nil {
		t.Fatal("the $.process.run call of deck() not found in the plugin source")
	}
	if run[1] != "hook" {
		t.Fatalf("plugin runs subcommand %q, want hook", run[1])
	}
	sessionFlag := run[2]

	// values fill the arguments the plugin computes at run time, in order.
	tests := []struct {
		event  string
		values []string
	}{
		{"status", []string{"running"}},
		{"session-start", []string{"11111111-aaaa", "clear"}},
		{"rate-limits", []string{`[{"kind":"five_hour","percentUsed":12,"resetsAt":"2026-10-09T12:00:00Z"}]`}},
	}

	var tested []string
	for _, tt := range tests {
		tested = append(tested, tt.event)
		t.Run(tt.event, func(t *testing.T) {
			shape, ok := calls[tt.event]
			if !ok {
				t.Fatalf("the plugin no longer calls hook %s", tt.event)
			}
			args := slices.Clone(shape)
			values := tt.values
			for i, a := range args {
				if a != "" {
					continue
				}
				if len(values) == 0 {
					t.Fatalf("call %v has more computed arguments than the test fills", shape)
				}
				args[i], values = values[0], values[1:]
			}
			if len(values) > 0 {
				t.Fatalf("call %v has fewer computed arguments than the test fills", shape)
			}
			args = append(args, sessionFlag, "abc")

			req, err := parseHookArgs(args)
			if err != nil {
				t.Fatalf("parseHookArgs(%v): %v", args, err)
			}
			if req.Session != "abc" {
				t.Errorf("Session = %q, want abc", req.Session)
			}
		})
	}

	var called []string
	for event := range calls {
		called = append(called, event)
	}
	slices.Sort(called)
	slices.Sort(tested)
	if !slices.Equal(called, tested) {
		t.Errorf("plugin calls hooks %v, this test covers %v", called, tested)
	}
}

func TestHookContract_EnvVars(t *testing.T) {
	var got []string
	for _, m := range envGetRe.FindAllStringSubmatch(readRegisterTS(t), -1) {
		got = append(got, m[1])
	}
	slices.Sort(got)
	want := []string{session.EnvCommand, session.EnvSessionID}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("plugin reads env vars %v, claude-deck sets %v", got, want)
	}
}
