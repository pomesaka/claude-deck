package pty

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"github.com/pomesaka/claude-deck/internal/debuglog"
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

// OutputHandler is called for each chunk of raw bytes read from the PTY.
// チャンクは改行で区切られた「行」ではなく、io.Read が返す任意サイズのバイト列。
// ターミナルアプリは \n なしで画面を更新するため、行単位の読み込みは使わない。
type OutputHandler func(data []byte)

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

	debuglog.Printf("[pty.Start] cmd=%s args=%v workDir=%q cols=%d rows=%d", Command, args, opts.WorkDir, opts.Cols, opts.Rows)

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

	debuglog.Printf("[pty.Start] calling pty.StartWithSize cols=%d rows=%d", cols, rows)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		debuglog.Printf("[pty.Start] pty.StartWithSize failed: %v", err)
		cancel()
		return nil, fmt.Errorf("starting pty: %w", err)
	}
	debuglog.Printf("[pty.Start] pty started pid=%d", cmd.Process.Pid)

	p := &Process{
		cmd:    cmd,
		ptmx:   ptmx,
		cancel: cancel,
		done:   make(chan struct{}),
	}

	go func() {
		debuglog.Printf("[pty.readLoop] starting pid=%d", cmd.Process.Pid)
		// Drain all output first, then close pty and wait for process.
		p.readLoop(ptmx, handler)
		debuglog.Printf("[pty.readLoop] ended pid=%d, closing pty", cmd.Process.Pid)
		p.closePty()
		debuglog.Printf("[pty.readLoop] pty closed, waiting for process pid=%d", cmd.Process.Pid)
		err := cmd.Wait()
		debuglog.Printf("[pty.readLoop] process exited pid=%d err=%v", cmd.Process.Pid, err)
		close(p.done)
	}()

	return p, nil
}

// readLoop は PTY から raw バイト列を継続的に読み込み handler に渡す。
// bufio.Scanner（行単位）ではなく io.Read（チャンク単位）を使う理由:
// ターミナルアプリは入力エコーや画面更新を \n なしで行うため、Scanner だと
// 次の \n が来るまでブロックして画面更新が止まる。
// また、ターミナルクエリ（DA1/DA2/XTVERSION）を検知して応答を返す。
// native Claude バイナリはレンダリングのたびにこれらを送信し、応答がないと
// ~30 秒待ってから次の描画に進むため、応答しないと入力後の画面更新が凍る。
func (p *Process) readLoop(r io.Reader, handler OutputHandler) {
	buf := make([]byte, 32*1024)
	chunkCount := 0
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunkCount++
			if chunkCount <= 3 {
				debuglog.Printf("[pty.readLoop] chunk #%d: %d bytes", chunkCount, n)
			}
			// buf はループで再利用するためコピーしてから渡す
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			// ターミナルクエリへの応答を先に送る（claude が応答を待ってブロックするのを防ぐ）
			p.respondToTerminalQueries(chunk)
			handler(chunk)
		}
		if err != nil {
			debuglog.Printf("[pty.readLoop] Read ended: %v (total chunks: %d)", err, chunkCount)
			break
		}
	}
}

// respondToTerminalQueries は claude が送出したターミナルクエリを検知し、
// PTY master に応答を書き戻す。claude-deck は「ターミナル」としてふるまう必要があり、
// 応答しないと claude が最大 ~30 秒タイムアウトまでレンダリングを止める。
//
// 対象クエリ:
//   - ESC [ c      (DA1: Primary Device Attributes)   → ESC [ ? 62 ; 22 c
//   - ESC [ > c    (DA2: Secondary Device Attributes)  → ESC [ > 64 ; 1 ; 0 c
//   - ESC [ > 0 q  (XTVERSION)                         → ESC P > | VTE \x1b \\
func (p *Process) respondToTerminalQueries(data []byte) {
	var response []byte

	// XTVERSION: ESC [ > 0 q  (DA1 の ESC [ c より先にチェック)
	if bytes.Contains(data, []byte("\x1b[>0q")) {
		debuglog.Printf("[pty.respondToTerminalQueries] XTVERSION query detected, responding")
		response = append(response, "\x1bP>|VTE\x1b\\"...)
	}

	// DA2: ESC [ > c
	if bytes.Contains(data, []byte("\x1b[>c")) {
		debuglog.Printf("[pty.respondToTerminalQueries] DA2 query detected, responding")
		response = append(response, "\x1b[>64;1;0c"...)
	}

	// DA1: ESC [ c
	if bytes.Contains(data, []byte("\x1b[c")) {
		debuglog.Printf("[pty.respondToTerminalQueries] DA1 query detected, responding")
		response = append(response, "\x1b[?62;22c"...)
	}

	if len(response) == 0 {
		return
	}

	p.ptmxMu.Lock()
	defer p.ptmxMu.Unlock()
	if !p.ptmxClosed {
		_, _ = p.ptmx.Write(response)
	}
}

// Write sends data to the PTY process's stdin.
func (p *Process) Write(data []byte) (int, error) {
	debuglog.Printf("[pty.Write] %d bytes: %x", len(data), data)
	p.ptmxMu.Lock()
	defer p.ptmxMu.Unlock()
	if p.ptmxClosed {
		debuglog.Printf("[pty.Write] pty already closed")
		return 0, fmt.Errorf("pty closed")
	}
	n, err := p.ptmx.Write(data)
	debuglog.Printf("[pty.Write] wrote %d bytes err=%v", n, err)
	return n, err
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
