// Package instance tells other claude-deck processes about the running TUI,
// so `claude-deck reload` can ask it to start again from its binary.
//
// The TUI holds an flock on {DataDir}/tui.lock while it runs and keeps a JSON
// State in the same file. A process that cannot take the lock knows a TUI is
// running, reads its pid from the file and signals it.
//
// WHY flock とファイル: pid のファイルだけだと、TUI が落ちた後に残った pid が別のプロセスに再利用され、
// そこへシグナルを送りうる。flock はプロセスが終われば外れるので、残ったファイルを生きていると取り違えない。
// WHY NOT store で頼む: store はセッションの行の置き場で、TUI は PRAGMA data_version でどの書き込みにも起きる。
// 再起動の頼みを行に混ぜると、セッションの書き手の決まり（docs/architecture.md）に例外が増える。
// WHY NOT 制御用のソケット: 頼みは「起動し直せ」の 1 種類だけで、答えは State の書き換えで返せる。
package instance

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// FileName is the basename of the lock and state file under the data directory.
const FileName = "tui.lock"

// State is what the running TUI says about itself.
type State struct {
	PID int `json:"pid"`
	// Command is the path the TUI was started from (os.Executable), links not
	// followed: a reload follows them again, so a link moved to a new build is taken.
	Command string `json:"command"`
	// Revision is the build the TUI runs (vcs.revision), "" when unknown.
	Revision  string    `json:"revision,omitempty"`
	StartedAt time.Time `json:"started_at"`
	// ReloadError is why the last reload was refused (the new binary did not
	// run), and ReloadErrorAt when; empty when none was.
	ReloadError   string     `json:"reload_error,omitempty"`
	ReloadErrorAt *time.Time `json:"reload_error_at,omitempty"`
}

// Instance is the running TUI's hold on the file.
type Instance struct {
	f     *os.File
	state State
}

// ErrRunning is returned by Acquire when another TUI holds the file.
var ErrRunning = errors.New("another claude-deck TUI is running with this data directory")

// Acquire takes the file for the TUI of this process, started from command
// and running the build revision.
// Between taking the lock and writing the state, Find may read the last TUI's
// state; the window is a few system calls.
func Acquire(dataDir, command, revision string) (*Instance, error) {
	f, err := os.OpenFile(filepath.Join(dataDir, FileName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrRunning
		}
		return nil, err
	}
	in := &Instance{f: f, state: State{PID: os.Getpid(), Command: command, Revision: revision, StartedAt: time.Now()}}
	if err := in.write(); err != nil {
		in.Close()
		return nil, err
	}
	return in, nil
}

// RecordReloadError tells `claude-deck reload` why the TUI refused to reload.
func (in *Instance) RecordReloadError(reason error) error {
	now := time.Now()
	in.state.ReloadError = reason.Error()
	in.state.ReloadErrorAt = &now
	return in.write()
}

func (in *Instance) write() error {
	data, err := json.Marshal(in.state, jsontext.EscapeForHTML(false))
	if err != nil {
		return err
	}
	if err := in.f.Truncate(0); err != nil {
		return err
	}
	_, err = in.f.WriteAt(data, 0)
	return err
}

// Close lets the file go. The exec of a reload lets it go too: Go opens files
// close-on-exec, and the new process takes it again.
func (in *Instance) Close() error {
	return in.f.Close()
}

// Find returns the running TUI's state, and false when no TUI holds the file.
func Find(dataDir string) (State, bool, error) {
	f, err := os.OpenFile(filepath.Join(dataDir, FileName), os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err == nil {
		// 取れた: 持っている TUI はいない（残ったファイルを読まない）。
		return State{}, false, nil
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return State{}, false, err
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		return State{}, false, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, false, fmt.Errorf("reading %s: %w", f.Name(), err)
	}
	return st, true, nil
}

// Target is the binary a reload of a TUI started from command runs: command
// with its links followed now.
func Target(command string) (string, error) {
	return filepath.EvalSymlinks(command)
}

// checkTimeout is how long CheckBinary waits for the binary to answer.
var checkTimeout = 10 * time.Second

// CheckBinary runs `<path> version` and returns what it printed, or why it
// could not run: a broken build must not replace a working TUI.
func CheckBinary(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s version: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
