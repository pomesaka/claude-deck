package main

import (
	json "encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/instance"
	"github.com/pomesaka/claude-deck/internal/session"
	"github.com/pomesaka/claude-deck/internal/tui"
)

// VersionInfo is the JSON form of `claude-deck version`: the build the binary is.
type VersionInfo struct {
	Revision  string `json:"revision,omitempty"`
	Modified  bool   `json:"modified"`
	GoVersion string `json:"go_version"`
}

func parseVersionArgs(args []string) (cliRequest, error) {
	if len(args) > 0 {
		return cliRequest{}, fmt.Errorf("version: unexpected arguments: %v", args)
	}
	return cliRequest{Op: "version"}, nil
}

// buildVersion reads the build this binary is from its build info.
func buildVersion() VersionInfo {
	v := VersionInfo{GoVersion: runtime.Version()}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				v.Revision = s.Value
			case "vcs.modified":
				v.Modified = s.Value == "true"
			}
		}
	}
	return v
}

// ReloadInfo is the JSON form of what `claude-deck reload` did.
type ReloadInfo struct {
	PID int `json:"pid"`
	// Binary is what the TUI starts again from (or would, with dry_run): the
	// path it was started from, its links followed.
	Binary string `json:"binary"`
	// Version is what Binary's `version` printed (dry_run only).
	Version  *VersionInfo `json:"version,omitempty"`
	Reloaded bool         `json:"reloaded"`
	// TUIRevision is the build the TUI runs after the reload, and CLIRevision
	// the build of this command. They differ when the TUI's binary is not the
	// build just made (a build in a workspace while the TUI runs another path).
	TUIRevision string `json:"tui_revision,omitempty"`
	CLIRevision string `json:"cli_revision,omitempty"`
	DryRun      bool   `json:"dry_run"`
}

func parseReloadArgs(args []string) (cliRequest, error) {
	fs := flag.NewFlagSet("reload", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck reload [--dry-run]")
		fmt.Fprintln(fs.Output(), "Asks the running TUI to start again from its binary, after checking that the binary runs. Sessions keep running.")
		fs.PrintDefaults()
	}
	dryRun := fs.Bool("dry-run", false, "find the TUI and check its binary, without asking it to reload")
	if err := fs.Parse(args); err != nil {
		return cliRequest{}, err
	}
	if fs.NArg() > 0 {
		return cliRequest{}, fmt.Errorf("reload: unexpected arguments: %v", fs.Args())
	}
	return cliRequest{Op: "reload", DryRun: *dryRun}, nil
}

// errNoTUI is returned by reload when no TUI runs with the data directory.
var errNoTUI = errors.New("reload: no claude-deck TUI is running with this data directory")

// reloadSignal asks the TUI to reload.
// WHY SIGUSR1: 送るのは instance のロックを持つ TUI、つまりこの頼みを受ける版だけ。
// ロックを持たない古い版に送ると、SIGUSR1 の既定の動作でプロセスが終わる。
const reloadSignal = syscall.SIGUSR1

// reloadWait is how long reload waits for the TUI to start again.
var reloadWait = 30 * time.Second

// reloadTUI asks the TUI running with dataDir to start again from its binary,
// and waits until it has, or has refused.
func reloadTUI(dataDir string, dryRun bool) (ReloadInfo, error) {
	st, ok, err := instance.Find(dataDir)
	if err != nil {
		return ReloadInfo{}, err
	}
	if !ok {
		return ReloadInfo{}, errNoTUI
	}
	target, err := instance.Target(st.Command)
	if err != nil {
		return ReloadInfo{}, fmt.Errorf("reload: the TUI's binary: %w", err)
	}
	info := ReloadInfo{PID: st.PID, Binary: target, CLIRevision: buildVersion().Revision, DryRun: dryRun}
	if dryRun {
		out, err := instance.CheckBinary(target)
		if err != nil {
			return info, err
		}
		var v VersionInfo
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			return info, fmt.Errorf("reload: %s version did not print its version: %w", target, err)
		}
		info.Version = &v
		return info, nil
	}
	asked := time.Now()
	if err := syscall.Kill(st.PID, reloadSignal); err != nil {
		return info, fmt.Errorf("reload: signalling pid %d: %w", st.PID, err)
	}
	for deadline := asked.Add(reloadWait); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		// exec の間はロックが外れるので、見つからなくても待ち続ける。
		now, ok, err := instance.Find(dataDir)
		if err != nil || !ok {
			continue
		}
		if now.StartedAt.After(asked) {
			info.Reloaded = true
			info.TUIRevision = now.Revision
			return info, nil
		}
		if now.ReloadErrorAt != nil && now.ReloadErrorAt.After(asked) {
			return info, fmt.Errorf("reload: the TUI kept running the old binary: %s", now.ReloadError)
		}
	}
	return info, fmt.Errorf("reload: the TUI did not start again within %s", reloadWait)
}

// Environment variables the TUI passes to itself across a reload, and drops
// once it has read them so that nothing it starts inherits them.
const (
	envReloaded      = "CLAUDE_DECK_RELOADED"
	envReloadCommand = "CLAUDE_DECK_RELOAD_COMMAND"
	envReloadCursor  = "CLAUDE_DECK_RELOAD_CURSOR"
	envReloadGhostty = "CLAUDE_DECK_RELOAD_GHOSTTY"
)

var reloadEnvKeys = []string{envReloaded, envReloadCommand, envReloadCursor, envReloadGhostty}

// reloadCarry is what the TUI carries across a reload.
type reloadCarry struct {
	reloaded bool
	// command is the path the first TUI was started from, links not followed.
	// WHY 引き継ぐ: exec するのはリンクをたどった先なので、新しいプロセスの os.Executable はそれを返す。
	// 引き継がないと、次の reload がリンクをたどり直さず、付け替えたリンクの先を拾わない。
	command string
	// cursor is the session the cursor was on.
	cursor session.DeckSessionID
	// ghosttySplit is the Ghostty terminal the TUI split off, which the new
	// process closes on exit instead of splitting another.
	ghosttySplit string
}

// takeReloadCarry reads what a reload passed to this process, and drops it from the environment.
func takeReloadCarry() reloadCarry {
	c := reloadCarry{
		reloaded:     os.Getenv(envReloaded) == "1",
		command:      os.Getenv(envReloadCommand),
		cursor:       session.DeckSessionID(os.Getenv(envReloadCursor)),
		ghosttySplit: os.Getenv(envReloadGhostty),
	}
	for _, k := range reloadEnvKeys {
		os.Unsetenv(k)
	}
	return c
}

// reloadEnv is env, with what the TUI carries across a reload in place of any
// earlier one.
func reloadEnv(env []string, c reloadCarry) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(reloadEnvKeys, k)
	})
	out = append(out, envReloaded+"=1", envReloadCommand+"="+c.command)
	if c.cursor != "" {
		out = append(out, envReloadCursor+"="+string(c.cursor))
	}
	if c.ghosttySplit != "" {
		out = append(out, envReloadGhostty+"="+c.ghosttySplit)
	}
	return out
}

// reloadSignals starts listening for reload requests. Call it before
// instance.Acquire: a request that arrives once the file names this process
// must not end it (SIGUSR1's default).
func reloadSignals() chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, reloadSignal)
	return ch
}

// watchReload answers reload requests on ch for the TUI started from command:
// it checks the binary, then sends the TUI ReloadMsg with it, or records and
// shows why it refused. The TUI refuses too while it is busy.
func watchReload(ch <-chan os.Signal, in *instance.Instance, command string, send func(any)) {
	refuse := func(err error) {
		debuglog.Printf("[reload] refused: %v", err)
		if rerr := in.RecordReloadError(err); rerr != nil {
			debuglog.Printf("[reload] recording the refusal: %v", rerr)
		}
	}
	go func() {
		for range ch {
			target, err := instance.Target(command)
			if err == nil {
				_, err = instance.CheckBinary(target)
			}
			if err != nil {
				refuse(err)
				send(tui.ReloadFailedMsg{Err: err})
				continue
			}
			send(tui.ReloadMsg{Binary: target, Refuse: refuse})
		}
	}()
}

// execReload replaces this process with binary, the one watchReload checked,
// started with the same arguments. It returns only when the exec failed.
// The instance file is not let go first: Go opens it close-on-exec, so a
// successful exec lets it go, and a failed one can still record why.
func execReload(binary string, c reloadCarry) error {
	return syscall.Exec(binary, os.Args, reloadEnv(os.Environ(), c))
}
