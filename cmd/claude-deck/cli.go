package main

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pomesaka/claude-deck/internal/config"
	"github.com/pomesaka/claude-deck/internal/ratelimits"
	"github.com/pomesaka/claude-deck/internal/session"
	"github.com/pomesaka/claude-deck/internal/store"
	"github.com/pomesaka/claude-deck/internal/usage"
)

// cliRequest is one parsed subcommand.
type cliRequest struct {
	Op          string
	Dir         string // new
	NoWorkspace bool   // new
	Target      string // close
	DryRun      bool   // gc
	// hook
	HookEvent       string
	Session         string
	Status          string
	ClaudeSessionID string
	Source          string
	RateLimits      string // JSON, as ratelimits.ParseMeasured reads it
}

// cliCommands are the subcommands. Each one works on the store directly, so the
// TUI does not need to be running (ADR-011).
var cliCommands = map[string]func(args []string) (cliRequest, error){
	"new":   parseNewArgs,
	"list":  parseListArgs,
	"tree":  parseTreeArgs,
	"close": parseCloseArgs,
	"gc":    parseGCArgs,
	"hook":  parseHookArgs,
}

// SessionInfo is the JSON form of a session printed by new / list / close.
type SessionInfo struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	RepoPath        string `json:"repo_path"`
	WorkDir         string `json:"work_dir"`
	Status          string `json:"status"`
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
	// SessionChain is every Claude Code session ID of the session, oldest first
	// (one more for each /clear). The last one is ClaudeSessionID.
	SessionChain []string `json:"session_chain,omitempty"`
	// ForkedFrom is the Claude Code session ID the session was forked from: an
	// element of another session's SessionChain.
	ForkedFrom string `json:"forked_from,omitempty"`
}

func infoFromSnapshot(s session.Snapshot) SessionInfo {
	var chain []string
	for _, id := range s.Chain() {
		chain = append(chain, string(id))
	}
	return SessionInfo{
		ID:              string(s.ID),
		Name:            s.Name,
		RepoPath:        s.RepoPath,
		WorkDir:         s.WorkDir(),
		Status:          s.Status.ID(),
		ClaudeSessionID: string(s.RuntimeSessionID),
		SessionChain:    chain,
		ForkedFrom:      string(s.ForkedFrom),
	}
}

// GCInfo is the JSON form of what gc removed, or with dry_run would remove.
type GCInfo struct {
	DryRun            bool              `json:"dry_run"`
	Workspaces        []GCWorkspaceInfo `json:"workspaces"`
	ForgottenProjects int               `json:"forgotten_projects"`
}

// GCWorkspaceInfo is one workspace directory in GCInfo.
type GCWorkspaceInfo struct {
	Path    string `json:"path"`
	Warning string `json:"warning,omitempty"`
}

func infoFromGCReport(r session.GCReport) GCInfo {
	info := GCInfo{DryRun: r.DryRun, Workspaces: make([]GCWorkspaceInfo, len(r.Workspaces)), ForgottenProjects: r.ForgottenProjects}
	for i, ws := range r.Workspaces {
		info.Workspaces[i] = GCWorkspaceInfo{Path: ws.Path, Warning: ws.Warning}
	}
	return info
}

// runCLI runs one subcommand.
func runCLI(name string, args []string) error {
	req, err := cliCommands[name](args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	st, err := session.OpenStore(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer st.Close()

	switch req.Op {
	case "hook":
		return runHook(req, st, cfg.DataDir)
	case "list":
		snaps, err := session.ListStored(st)
		if err != nil {
			return err
		}
		infos := make([]SessionInfo, len(snaps))
		for i, s := range snaps {
			infos[i] = infoFromSnapshot(s)
		}
		return printJSON(infos)
	case "tree":
		snaps, err := session.ListStored(st)
		if err != nil {
			return err
		}
		_, err = fmt.Print(renderTree(session.BuildTree(snaps)))
		return err
	}

	mcfg, err := buildManagerConfig(cfg)
	if err != nil {
		return err
	}
	if req.Op == "gc" {
		report, err := session.CollectGarbage(st, mcfg, req.DryRun)
		if err != nil {
			return err
		}
		return printJSON(infoFromGCReport(report))
	}

	// new / close start or stop processes, which needs tmux.
	ctx := context.Background()
	mgr := session.NewManager(ctx, st, mcfg)
	mgr.Reload()

	switch req.Op {
	case "new":
		repoPath, workingDir, isJJ := session.ResolveLaunchDir(req.Dir)
		withWorkspace := !req.NoWorkspace
		if withWorkspace && !isJJ {
			return fmt.Errorf("%s は jj リポジトリではないためワークスペースを作れません（--no-workspace で直接起動できます）", req.Dir)
		}
		sess, err := mgr.CreateSession(ctx, repoPath, workingDir, withWorkspace)
		if err != nil {
			return err
		}
		return printJSON(infoFromSnapshot(sess.Snapshot()))
	case "close":
		sess, err := mgr.FindSession(req.Target)
		if err != nil {
			return err
		}
		if err := mgr.Kill(sess.ID); err != nil {
			return err
		}
		return printJSON(infoFromSnapshot(sess.Snapshot()))
	default:
		return fmt.Errorf("unknown command %q", req.Op)
	}
}

func parseNewArgs(args []string) (cliRequest, error) {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck new [--dir DIR] [--no-workspace]")
		fmt.Fprintln(fs.Output(), "TUI で n を押したときと同じく新しいセッションを作り、JSON で返す。TUI が起動していなくてもよい。")
		fs.PrintDefaults()
	}
	dir := fs.String("dir", "", "セッションを起動するディレクトリ（既定: カレントディレクトリ）。jj ワークスペース内なら本体リポジトリの同じ位置に解決する")
	noWorkspace := fs.Bool("no-workspace", false, "jj ワークスペースを作らずに直接起動する（TUI の C-Enter）")
	if err := fs.Parse(args); err != nil {
		return cliRequest{}, err
	}
	if fs.NArg() > 0 {
		return cliRequest{}, fmt.Errorf("new: unexpected arguments: %v", fs.Args())
	}
	absDir, err := resolveDir(*dir)
	if err != nil {
		return cliRequest{}, err
	}
	return cliRequest{Op: "new", Dir: absDir, NoWorkspace: *noWorkspace}, nil
}

func parseListArgs(args []string) (cliRequest, error) {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck list")
		fmt.Fprintln(fs.Output(), "claude-deck のセッションを TUI の一覧と同じ順で JSON で返す。外部セッションは含まない。")
	}
	if err := fs.Parse(args); err != nil {
		return cliRequest{}, err
	}
	if fs.NArg() > 0 {
		return cliRequest{}, fmt.Errorf("list: unexpected arguments: %v", fs.Args())
	}
	return cliRequest{Op: "list"}, nil
}

func parseTreeArgs(args []string) (cliRequest, error) {
	fs := flag.NewFlagSet("tree", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck tree")
		fmt.Fprintln(fs.Output(), "Claude Code のセッションを、/clear とフォークの親子関係でたどった木としてテキストで出す。同じ内容の JSON は list の session_chain と forked_from。")
	}
	if err := fs.Parse(args); err != nil {
		return cliRequest{}, err
	}
	if fs.NArg() > 0 {
		return cliRequest{}, fmt.Errorf("tree: unexpected arguments: %v", fs.Args())
	}
	return cliRequest{Op: "tree"}, nil
}

func parseCloseArgs(args []string) (cliRequest, error) {
	fs := flag.NewFlagSet("close", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck close <ID|NAME>")
		fmt.Fprintln(fs.Output(), "TUI で x を押したときと同じくプロセスを止めてワークスペースを消す。r で再開できる。")
	}
	if err := fs.Parse(args); err != nil {
		return cliRequest{}, err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return cliRequest{}, fmt.Errorf("close: specify exactly one session ID or name")
	}
	return cliRequest{Op: "close", Target: fs.Arg(0)}, nil
}

func parseGCArgs(args []string) (cliRequest, error) {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck gc [--dry-run]")
		fmt.Fprintln(fs.Output(), "どのセッションのものでもないワークスペースと、消えたワークスペースについての Claude Code の記録を消し、JSON で返す。")
		fs.PrintDefaults()
	}
	dryRun := fs.Bool("dry-run", false, "消さずに、消す対象だけを返す")
	if err := fs.Parse(args); err != nil {
		return cliRequest{}, err
	}
	if fs.NArg() > 0 {
		return cliRequest{}, fmt.Errorf("gc: unexpected arguments: %v", fs.Args())
	}
	return cliRequest{Op: "gc", DryRun: *dryRun}, nil
}

// Hook events accepted by `claude-deck hook`.
const (
	hookStatus       = "status"
	hookSessionStart = "session-start"
	hookExited       = "exited"
	hookRateLimits   = "rate-limits"
)

// hookStatuses are the statuses a hook may report. Completed and Error come from
// the exit command and close, which also record FinishedAt.
var hookStatuses = map[string]bool{
	session.StatusRunning.ID():         true,
	session.StatusIdle.ID():            true,
	session.StatusWaitingApproval.ID(): true,
	session.StatusWaitingAnswer.ID():   true,
}

// parseHookArgs parses `hook <event> [args] --session ID`. These are called by
// the deck-status plugin and by the pane's exit command, not by people.
func parseHookArgs(args []string) (cliRequest, error) {
	usage := "Usage: claude-deck hook status <running|idle|waiting_approval|waiting_answer> | session-start --claude-session-id ID --source SOURCE | exited | rate-limits <JSON>  [--session DECK_ID]"
	if len(args) == 0 {
		return cliRequest{}, fmt.Errorf("hook: missing event\n%s", usage)
	}
	req := cliRequest{Op: "hook", HookEvent: args[0]}
	fs := flag.NewFlagSet("hook "+args[0], flag.ContinueOnError)
	fs.StringVar(&req.Session, "session", os.Getenv(session.EnvSessionID), "deck session ID（既定: $"+session.EnvSessionID+"）")
	switch req.HookEvent {
	case hookStatus:
		if len(args) < 2 {
			return cliRequest{}, fmt.Errorf("hook status: missing status\n%s", usage)
		}
		req.Status = args[1]
		if !hookStatuses[req.Status] {
			return cliRequest{}, fmt.Errorf("hook status: unknown status %q", req.Status)
		}
		args = args[2:]
	case hookSessionStart:
		fs.StringVar(&req.ClaudeSessionID, "claude-session-id", "", "Claude Code のセッション ID")
		fs.StringVar(&req.Source, "source", "", "SessionStart の source（startup / resume / fork / clear / compact）")
		args = args[1:]
	case hookExited:
		args = args[1:]
	case hookRateLimits:
		if len(args) < 2 {
			return cliRequest{}, fmt.Errorf("hook rate-limits: missing JSON\n%s", usage)
		}
		req.RateLimits = args[1]
		if _, err := ratelimits.ParseMeasured([]byte(req.RateLimits)); err != nil {
			return cliRequest{}, fmt.Errorf("hook rate-limits: %w", err)
		}
		args = args[2:]
	default:
		return cliRequest{}, fmt.Errorf("hook: unknown event %q\n%s", req.HookEvent, usage)
	}
	if err := fs.Parse(args); err != nil {
		return cliRequest{}, err
	}
	if fs.NArg() > 0 {
		return cliRequest{}, fmt.Errorf("hook %s: unexpected arguments: %v", req.HookEvent, fs.Args())
	}
	if req.Session == "" {
		return cliRequest{}, fmt.Errorf("hook %s: no session (--session or $%s)", req.HookEvent, session.EnvSessionID)
	}
	return req, nil
}

func runHook(req cliRequest, st *store.Store, dataDir string) error {
	id := session.DeckSessionID(req.Session)
	switch req.HookEvent {
	case hookRateLimits:
		status, _ := ratelimits.ParseMeasured([]byte(req.RateLimits)) // validated in parseHookArgs
		if !status.FiveHourAvailable && !status.SevenDayAvailable {
			return nil // nothing the TUI shows: keep the last reading
		}
		return ratelimits.Save(dataDir, status)
	case hookStatus:
		status, _ := session.StatusFromID(req.Status) // validated in parseHookArgs
		return session.RecordHookStatus(st, id, status)
	case hookSessionStart:
		return session.RecordSessionStart(st, id, session.ClaudeSessionID(req.ClaudeSessionID), req.Source)
	case hookExited:
		return session.MarkExited(st, usage.NewReader(""), id)
	default:
		return fmt.Errorf("hook: unknown event %q", req.HookEvent)
	}
}

func resolveDir(dir string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getting working directory: %w", err)
		}
		return wd, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", dir, err)
	}
	return abs, nil
}

func printJSON(v any) error {
	if err := json.MarshalWrite(os.Stdout, v, jsontext.WithIndent("  ")); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	fmt.Println()
	return nil
}
