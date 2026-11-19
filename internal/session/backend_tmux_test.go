package session

import (
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// buildShellCommand quotes each argument so that the shell tmux runs hands it
// to the program unchanged, as one argument. The first prompt goes through it.
func TestBuildShellCommand_RoundTrip(t *testing.T) {
	check := func(t *testing.T, args []string) {
		t.Helper()
		// printf prints each argument followed by NUL, so the arguments can be split back.
		cmd := buildShellCommand("printf", append([]string{`%s\0`}, args...))
		out, err := exec.Command("/bin/sh", "-c", cmd).Output()
		if err != nil {
			t.Fatalf("sh -c %q: %v", cmd, err)
		}
		got := strings.Split(string(out), "\x00")
		got = got[:len(got)-1]
		if len(args) == 0 {
			got = nil
		}
		if !slices.Equal(got, args) {
			t.Fatalf("args %q came back as %q", args, got)
		}
	}
	for _, args := range [][]string{
		{"a'b"},
		{"line1\nline2"},
		{"$(rm x); echo `id`"},
		{"-- x", "--add-dir", "/a b"},
		{""},
	} {
		check(t, args)
	}
	rng := rand.New(rand.NewPCG(5, 6))
	alphabet := []rune("ab '\"$`\\;&|\n\t-*?~#!()")
	for range 300 {
		args := make([]string, 1+rng.IntN(3))
		for i := range args {
			r := make([]rune, rng.IntN(8))
			for j := range r {
				r[j] = alphabet[rng.IntN(len(alphabet))]
			}
			args[i] = string(r)
		}
		check(t, args)
	}
}

// A command too long for tmux runs through a script that does the same and
// removes itself; a short one is passed as it is.
func TestLaunchCommand(t *testing.T) {
	short := buildShellCommand("printf", []string{`%s\0`, "hi"})
	if got, err := launchCommand(short); err != nil || got != short {
		t.Fatalf("short command = %q, %v; want it unchanged", got, err)
	}

	long := strings.Repeat("x'y\n", 10_000) // 40KB, past the tmux limit
	cmd, err := launchCommand(buildShellCommand("printf", []string{`%s\0`, long}))
	if err != nil {
		t.Fatal(err)
	}
	if len(cmd) > maxInlineCommand {
		t.Fatalf("launch command is %d bytes, want at most %d", len(cmd), maxInlineCommand)
	}
	script := strings.Trim(strings.TrimPrefix(cmd, "'/bin/sh' "), "'")
	out, err := exec.Command("/bin/sh", "-c", cmd).Output()
	if err != nil {
		t.Fatalf("sh -c %q: %v", cmd, err)
	}
	if got := strings.TrimSuffix(string(out), "\x00"); got != long {
		t.Errorf("the long argument came back with %d bytes, want %d", len(got), len(long))
	}
	if _, err := os.Stat(script); !os.IsNotExist(err) {
		t.Errorf("launch script %s is left behind (stat: %v)", script, err)
	}
}
