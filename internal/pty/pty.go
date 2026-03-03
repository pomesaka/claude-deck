package pty

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

// Command is the claude executable path. Override from config before use.
var Command = "claude"

// Process wraps a Claude Code CLI process running in a pseudo-terminal.
type Process struct {
	cmd    *exec.Cmd
	ptmx   *os.File
	cancel context.CancelFunc
	done   chan struct{}

	ptmxMu     sync.Mutex // guards ptmx access against concurrent close
	ptmxClosed bool
}

// OutputHandler is called for each line of output from the process.
type OutputHandler func(line string)

// StartOptions configures a new Claude Code process.
type StartOptions struct {
	WorkDir          string
	Prompt           string
	PermissionMode   string
	ResumeSessionID  string // if set, resume an existing Claude Code session
	ForkSession      bool   // if true, add --fork-session flag (use with ResumeSessionID)
	AdditionalArgs   []string
	Env              []string // additional environment variables (KEY=VALUE)
	Cols             uint16   // PTY 幅。0 の場合デフォルト 120
	Rows             uint16   // PTY 高さ。0 の場合デフォルト 40
}

// Start launches a new Claude Code session in a pty.
func Start(ctx context.Context, opts StartOptions, handler OutputHandler) (*Process, error) {
	ctx, cancel := context.WithCancel(ctx)

	args := []string{}
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
		if opts.ForkSession {
			args = append(args, "--fork-session")
		}
	} else if opts.Prompt != "" {
		args = append(args, "-p", opts.Prompt)
	}
	if opts.PermissionMode != "" {
		args = append(args, "--permission-mode", opts.PermissionMode)
	}
	args = append(args, opts.AdditionalArgs...)

	cmd := exec.CommandContext(ctx, Command, args...)
	cmd.Dir = opts.WorkDir
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"CLAUDE_CODE_ENTRYPOINT=cli",
	)
	cmd.Env = append(cmd.Env, opts.Env...)

	cols := opts.Cols
	if cols == 0 {
		cols = 120
	}
	rows := opts.Rows
	if rows == 0 {
		rows = 40
	}

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("starting pty: %w", err)
	}

	p := &Process{
		cmd:    cmd,
		ptmx:   ptmx,
		cancel: cancel,
		done:   make(chan struct{}),
	}

	go func() {
		// Drain all output first, then close pty and wait for process.
		p.readLoop(ptmx, handler)
		p.closePty()
		_ = cmd.Wait()
		close(p.done)
	}()

	return p, nil
}

func (p *Process) readLoop(r io.Reader, handler OutputHandler) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		handler(scanner.Text())
	}
}

// Write sends data to the PTY process's stdin.
func (p *Process) Write(data []byte) (int, error) {
	p.ptmxMu.Lock()
	defer p.ptmxMu.Unlock()
	if p.ptmxClosed {
		return 0, fmt.Errorf("pty closed")
	}
	return p.ptmx.Write(data)
}

// PID returns the process ID.
func (p *Process) PID() int {
	if p.cmd.Process != nil {
		return p.cmd.Process.Pid
	}
	return 0
}

// Done returns a channel that is closed when the process exits.
func (p *Process) Done() <-chan struct{} {
	return p.done
}

// Resize updates the PTY window size. Claude Code re-renders for the new dimensions.
func (p *Process) Resize(cols, rows uint16) error {
	p.ptmxMu.Lock()
	defer p.ptmxMu.Unlock()
	if p.ptmxClosed {
		return nil
	}
	return pty.Setsize(p.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}

// Kill forcefully terminates the process.
func (p *Process) Kill() error {
	p.cancel()
	if p.cmd.Process != nil {
		return p.cmd.Process.Kill()
	}
	return nil
}

func (p *Process) closePty() {
	p.ptmxMu.Lock()
	defer p.ptmxMu.Unlock()
	if p.ptmxClosed {
		return
	}
	p.ptmxClosed = true
	_ = p.ptmx.Close()
}

// Close cleans up the pty and cancels the context.
func (p *Process) Close() error {
	p.cancel()
	p.closePty()
	return nil
}
