// Package control lets other processes (the claude-deck CLI subcommands) ask the
// running TUI process to create, list and close sessions.
//
// Protocol: one JSON Request per Unix socket connection at {DataDir}/control.sock;
// the server writes one JSON Response and closes the connection.
//
// WHY TUI プロセスに依頼する: Manager は起動中のプロセス・終了監視・tmux ウィンドウとの対応を
// メモリに持っている。CLI が自分で Manager を作ってセッションを起動すると、TUI 側は store の
// 5 秒 tick でしか追従できず、終了監視も TUI に付かない。依頼にすれば TUI で n / x を押したときと
// 同じ経路を通り、store の書き手も TUI プロセス 1 つに保てる。
// WHY NOT ファイル経由（preview の IPC と同じ方式）: new は作ったセッションの名前を呼び出し元へ
// 返す必要があり、要求と応答を対にできるソケットの方が単純になる。
package control

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/pomesaka/claude-deck/internal/debuglog"
)

const socketFileName = "control.sock"

// clientTimeout は close のワークスペース削除（node_modules 等）を待ち切れる長さにする。
const clientTimeout = 5 * time.Minute

// ErrNotRunning is returned by Call when no TUI process is listening.
var ErrNotRunning = errors.New("claude-deck が起動していません（TUI を起動してから実行してください）")

// ErrAlreadyListening is returned by Listen when another TUI process owns the socket.
var ErrAlreadyListening = errors.New("別の claude-deck が control socket を使用中です")

// Op identifies the requested operation.
type Op string

const (
	OpNew   Op = "new"
	OpList  Op = "list"
	OpClose Op = "close"
)

// Request is sent from the CLI to the TUI process.
type Request struct {
	Op Op `json:"op"`
	// Dir is the absolute directory to start the session in (OpNew).
	Dir string `json:"dir,omitempty"`
	// NoWorkspace starts the session directly in the repository without a jj workspace (OpNew).
	NoWorkspace bool `json:"no_workspace,omitempty"`
	// Target is the deck session ID or name to close (OpClose).
	Target string `json:"target,omitempty"`
}

// Response is returned from the TUI process. Error is non-empty on failure.
type Response struct {
	Error    string        `json:"error,omitempty"`
	Session  *SessionInfo  `json:"session,omitempty"`
	Sessions []SessionInfo `json:"sessions,omitempty"`
}

// SessionInfo is the machine-readable view of a session.
type SessionInfo struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	RepoPath        string `json:"repo_path"`
	WorkDir         string `json:"work_dir"`
	Status          string `json:"status"`
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
}

// Handler executes requests inside the TUI process.
type Handler interface {
	New(dir string, withWorkspace bool) (SessionInfo, error)
	List() []SessionInfo
	Close(target string) (SessionInfo, error)
}

// SocketPath returns the control socket path for dataDir.
func SocketPath(dataDir string) string {
	return filepath.Join(dataDir, socketFileName)
}

// Listen creates the control socket. A leftover socket file from a crashed process
// is removed; a socket that still accepts connections yields ErrAlreadyListening.
func Listen(dataDir string) (net.Listener, error) {
	path := SocketPath(dataDir)
	if conn, err := net.Dial("unix", path); err == nil {
		conn.Close()
		return nil, ErrAlreadyListening
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("control: remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control: listen: %w", err)
	}
	// 接続できる人 = セッションを起動・終了できる人なので、所有者だけに絞る。
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("control: chmod socket: %w", err)
	}
	return ln, nil
}

// Serve accepts connections until ctx is cancelled, then closes ln.
func Serve(ctx context.Context, ln net.Listener, h Handler) {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() == nil {
					debuglog.Printf("[control] accept: %v", err)
				}
				return
			}
			go handleConn(conn, h)
		}
	}()
}

func handleConn(conn net.Conn, h Handler) {
	defer conn.Close()
	var req Request
	if err := json.UnmarshalDecode(jsontext.NewDecoder(conn), &req); err != nil {
		writeResponse(conn, Response{Error: fmt.Sprintf("invalid request: %v", err)})
		return
	}
	debuglog.Printf("[control] request op=%s dir=%q target=%q", req.Op, req.Dir, req.Target)
	writeResponse(conn, dispatch(h, req))
}

func dispatch(h Handler, req Request) Response {
	switch req.Op {
	case OpNew:
		info, err := h.New(req.Dir, !req.NoWorkspace)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{Session: &info}
	case OpList:
		return Response{Sessions: h.List()}
	case OpClose:
		info, err := h.Close(req.Target)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{Session: &info}
	default:
		return Response{Error: fmt.Sprintf("unknown op: %q", req.Op)}
	}
}

func writeResponse(conn net.Conn, resp Response) {
	if err := json.MarshalWrite(conn, resp); err != nil {
		debuglog.Printf("[control] write response: %v", err)
	}
}

// Call sends req to the TUI process listening under dataDir and returns its response.
// A Response with a non-empty Error is returned as an error.
func Call(dataDir string, req Request) (Response, error) {
	conn, err := net.Dial("unix", SocketPath(dataDir))
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(clientTimeout)); err != nil {
		return Response{}, fmt.Errorf("control: set deadline: %w", err)
	}
	if err := json.MarshalWrite(conn, req); err != nil {
		return Response{}, fmt.Errorf("control: send request: %w", err)
	}
	var resp Response
	if err := json.UnmarshalRead(conn, &resp); err != nil {
		return Response{}, fmt.Errorf("control: read response: %w", err)
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}
