package main

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pomesaka/claude-deck/internal/config"
	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/jj"
	"github.com/pomesaka/claude-deck/internal/ratelimits"
	"github.com/pomesaka/claude-deck/internal/session"
	"github.com/pomesaka/claude-deck/internal/store"
	"github.com/pomesaka/claude-deck/internal/tmux"
	"github.com/pomesaka/claude-deck/internal/usage"
)

// cliRequest is one parsed subcommand.
type cliRequest struct {
	Op          string
	Dir         string // new
	NoWorkspace bool   // new
	// Prompt is the first instruction given to the new session's agent, or "".
	Prompt string // new, fork
	Target string // close, alias, fork, clear
	Alias  string // alias: the alias to set. list: the text to filter aliases by
	Repo   string // list: the text to filter repository names by
	DryRun bool   // gc, clear
	// project (Dir is the project)
	ProjectOp  string
	ClaudeArgs []string // projectSetArgs: the arguments to set
	// hook
	HookEvent string
	Session   string
	// Activity is what `hook status` reports. Completed and Error are not
	// activities: they come from the exit command and close, which also record FinishedAt.
	Activity session.Activity
	// StatusReason is why a waiting_action session waits on the user.
	StatusReason string
	// WaitKind is what a waiting_action session waits for.
	WaitKind        session.WaitKind
	ClaudeSessionID string
	Source          string
	RateLimits      string // JSON, as ratelimits.ParseMeasured reads it
	// ContextTokens is the context window's fill reported by `hook context`,
	// nil before a response of the live window reported one.
	ContextTokens *int
	ContextWindow int
}

// cliParser parses the arguments of a subcommand. stdin is read only by the
// options that say so (new --prompt -).
type cliParser func(args []string, stdin io.Reader) (cliRequest, error)

// ignoreStdin adapts a parser that never reads stdin.
func ignoreStdin(parse func(args []string) (cliRequest, error)) cliParser {
	return func(args []string, _ io.Reader) (cliRequest, error) { return parse(args) }
}

// cliCommands are the subcommands. Each one works on the store directly, so the
// TUI does not need to be running.
var cliCommands = map[string]cliParser{
	"new":     parseNewArgs,
	"fork":    parseForkArgs,
	"list":    ignoreStdin(parseListArgs),
	"tree":    ignoreStdin(parseTreeArgs),
	"close":   ignoreStdin(parseCloseArgs),
	"alias":   ignoreStdin(parseAliasArgs),
	"gc":      ignoreStdin(parseGCArgs),
	"project": ignoreStdin(parseProjectArgs),
	"clear":   ignoreStdin(parseClearArgs),
	"hook":    ignoreStdin(parseHookArgs),
}

// SessionInfo is the JSON form of a session printed by new / fork / list / close.
type SessionInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Alias is the label given with `claude-deck alias`.
	Alias    string `json:"alias,omitempty"`
	RepoPath string `json:"repo_path"`
	WorkDir  string `json:"work_dir"`
	Status   string `json:"status"`
	// StatusReason is what the user has to do, while Status is waiting_action.
	StatusReason    string `json:"status_reason,omitempty"`
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
	// SessionChain is every Claude Code session ID of the session, oldest first
	// (one more for each /clear). The last one is ClaudeSessionID.
	SessionChain []string `json:"session_chain,omitempty"`
	// ForkedFrom is the Claude Code session ID the session was forked from: an
	// element of another session's SessionChain.
	ForkedFrom string `json:"forked_from,omitempty"`
	// Context is the context window's fill Claude Code last measured; null
	// until measured, so a session that was not measured is not read as empty.
	Context *ContextInfo `json:"context"`
	// Compactions is how many times the current Claude Code session compacted
	// its conversation; null when it is not known.
	Compactions *int `json:"compactions"`
	// Hints are the next steps suggested for the session, derived each time it
	// is listed; [] when there are none.
	Hints []HintInfo `json:"hints"`
}

// ContextInfo is the JSON form of session.ContextUsage.
type ContextInfo struct {
	UsedTokens   int `json:"used_tokens"`
	WindowTokens int `json:"window_tokens"`
}

// HintInfo is the JSON form of session.Hint.
type HintInfo struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// infoFromSnapshot is the JSON form of s with the hints given for it.
func infoFromSnapshot(s session.Snapshot, hints []session.Hint) SessionInfo {
	var chain []string
	for _, id := range s.Chain() {
		chain = append(chain, string(id))
	}
	var context *ContextInfo
	if s.Context != nil {
		context = &ContextInfo{UsedTokens: s.Context.UsedTokens, WindowTokens: s.Context.WindowTokens}
	}
	hintInfos := []HintInfo{}
	for _, h := range hints {
		hintInfos = append(hintInfos, HintInfo{Kind: string(h.Kind), Message: h.Message})
	}
	return SessionInfo{
		Context:         context,
		Compactions:     s.Compactions,
		Hints:           hintInfos,
		ID:              string(s.ID),
		Name:            s.Name,
		Alias:           s.Alias,
		RepoPath:        s.RepoPath,
		WorkDir:         s.WorkDir(),
		Status:          s.Status.ID(),
		StatusReason:    s.StatusReason,
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
	req, err := cliCommands[name](args, os.Stdin)
	if err != nil {
		return err
	}
	if req.Op == "project" {
		return runProject(req, config.DefaultPath())
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
		infos := []SessionInfo{}
		now := time.Now()
		jjRunner := &jj.Runner{Command: cfg.Commands.JJ}
		for _, s := range snaps {
			if aliasMatches(s.Alias, req.Alias) && repoMatches(s.RepoPath, req.Repo) {
				infos = append(infos, infoFromSnapshot(s, listHints(s, now, jjRunner)))
			}
		}
		return printJSON(infos)
	case "alias":
		snap, err := session.SetAlias(st, req.Target, req.Alias)
		if err != nil {
			return err
		}
		return printJSON(infoWithHints(snap))
	case "clear":
		runner := &tmux.Runner{Command: cfg.Tmux.Command, SessionName: cfg.Tmux.SessionName}
		info, err := clearSession(st, runner, session.DeckSessionID(os.Getenv(session.EnvSessionID)), req.Target, req.DryRun)
		if err != nil {
			return err
		}
		return printJSON(info)
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

	// new / fork / close start or stop processes, which needs tmux.
	ctx := context.Background()
	mgr := session.NewManager(ctx, st, mcfg)
	mgr.Reload()

	switch req.Op {
	case "new":
		repoPath, workingDir, isJJ := session.ResolveLaunchDir(req.Dir)
		withWorkspace := !req.NoWorkspace
		if withWorkspace && !isJJ {
			return fmt.Errorf("%s is not a jj repository, so no workspace can be made (use --no-workspace to run in place)", req.Dir)
		}
		sess, err := mgr.CreateSession(ctx, repoPath, workingDir, withWorkspace, req.Prompt)
		if err != nil {
			return err
		}
		return printJSON(infoWithHints(sess.Snapshot()))
	case "fork":
		src, err := mgr.FindSession(req.Target)
		if err != nil {
			return err
		}
		sess, err := mgr.ForkSession(ctx, src.ID, req.Prompt)
		if err != nil {
			return err
		}
		return printJSON(infoWithHints(sess.Snapshot()))
	case "close":
		sess, err := mgr.FindSession(req.Target)
		if err != nil {
			return err
		}
		res, err := mgr.Kill(sess.ID)
		if err != nil {
			return err
		}
		if res.WorkspaceKept != "" {
			fmt.Fprintf(os.Stderr, "warning: jj could not snapshot the working copy, so the workspace was kept: %s\n", res.WorkspaceKept)
		}
		// WHY ヒントを付けない: 閉じた直後の行に、閉じるかを勧める stale は要らない。
		// ワークスペースを残したとき（WorkspaceKept）は、警告の行で知らせる。
		return printJSON(infoFromSnapshot(sess.Snapshot(), nil))
	default:
		return fmt.Errorf("unknown command %q", req.Op)
	}
}

func parseNewArgs(args []string, stdin io.Reader) (cliRequest, error) {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck new [--dir DIR] [--no-workspace] [--prompt TEXT|-]")
		fmt.Fprintln(fs.Output(), "Creates a session as n does in the TUI and prints it as JSON. The TUI need not be running.")
		fs.PrintDefaults()
	}
	dir := fs.String("dir", "", "directory to start the session in (default: current directory); inside a jj workspace it resolves to the same place in the main repository")
	noWorkspace := fs.Bool("no-workspace", false, "run in the directory without a jj workspace (C-Enter in the TUI)")
	var prompt optionalString
	fs.Var(&prompt, "prompt", "first instruction for the session's agent; - reads it from stdin")
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
	text, err := prompt.read("new", stdin)
	if err != nil {
		return cliRequest{}, err
	}
	return cliRequest{Op: "new", Dir: absDir, NoWorkspace: *noWorkspace, Prompt: text}, nil
}

func parseForkArgs(args []string, stdin io.Reader) (cliRequest, error) {
	fs := flag.NewFlagSet("fork", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck fork [--session ID|NAME] [--prompt TEXT|-]")
		fmt.Fprintln(fs.Output(), "Forks a session's conversation into a new session in a new workspace, as f does in the TUI, and prints the new session as JSON.")
		fs.PrintDefaults()
	}
	target := fs.String("session", os.Getenv(session.EnvSessionID), "ID or name of the session to fork (default: $"+session.EnvSessionID+", this session)")
	var prompt optionalString
	fs.Var(&prompt, "prompt", "first instruction for the forked session's agent; - reads it from stdin")
	if err := fs.Parse(args); err != nil {
		return cliRequest{}, err
	}
	if fs.NArg() > 0 {
		return cliRequest{}, fmt.Errorf("fork: unexpected arguments: %v", fs.Args())
	}
	if *target == "" {
		return cliRequest{}, fmt.Errorf("fork: no session (--session or $%s)", session.EnvSessionID)
	}
	text, err := prompt.read("fork", stdin)
	if err != nil {
		return cliRequest{}, err
	}
	return cliRequest{Op: "fork", Target: *target, Prompt: text}, nil
}

// optionalString is a string flag that records whether it was given, so an
// empty --prompt "" is an error rather than the same as no --prompt.
type optionalString struct {
	value string
	set   bool
}

func (o *optionalString) String() string { return o.value }

func (o *optionalString) Set(v string) error {
	o.value, o.set = v, true
	return nil
}

// read returns the --prompt text: "" when it was not given, stdin when it is "-".
// An empty or blank prompt is an error for the command cmd.
func (o *optionalString) read(cmd string, stdin io.Reader) (string, error) {
	if !o.set {
		return "", nil
	}
	text := o.value
	if text == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("%s: reading the prompt from stdin: %w", cmd, err)
		}
		text = string(data)
	}
	// 前後の空白と改行は落とす（echo やヒアドキュメントの末尾の改行）。中の改行は残す。
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("%s: --prompt is empty", cmd)
	}
	return text, nil
}

// infoWithHints is the JSON form of s with its hints now. The workspace is
// not read: only list shows unsaved.
func infoWithHints(s session.Snapshot) SessionInfo {
	return infoFromSnapshot(s, session.Hints(s, time.Now(), nil))
}

// workspaceReader reads the work a workspace holds outside trunk() (jj.Runner).
type workspaceReader interface {
	WorkspaceChanges(wsPath string) (files, commits int, err error)
}

// listHints are the hints `list` shows for s at now, with the workspace read
// when session.NeedsWorkspaceRead asks for it.
func listHints(s session.Snapshot, now time.Time, ws workspaceReader) []session.Hint {
	var changes *session.WorkspaceChanges
	if session.NeedsWorkspaceRead(s, now) {
		files, commits, err := ws.WorkspaceChanges(s.WorkDir())
		if err != nil {
			// 読めなければ unsaved を出さない。stale は出すので、close の前に確かめる手がかりは残る。
			debuglog.Printf("[list] reading the workspace of %s: %v", s.ID, err)
		} else {
			changes = &session.WorkspaceChanges{Files: files, Commits: commits}
		}
	}
	return session.Hints(s, now, changes)
}

func parseListArgs(args []string) (cliRequest, error) {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck list [--alias TEXT] [--repo TEXT]")
		fmt.Fprintln(fs.Output(), "Prints the claude-deck sessions as JSON, in the order of the TUI list. External sessions are not included.")
		fs.PrintDefaults()
	}
	alias := fs.String("alias", "", "only sessions whose alias contains this text (case-insensitive)")
	repo := fs.String("repo", "", "only sessions whose repository name (the last element of repo_path) contains this text (case-insensitive)")
	if err := fs.Parse(args); err != nil {
		return cliRequest{}, err
	}
	if fs.NArg() > 0 {
		return cliRequest{}, fmt.Errorf("list: unexpected arguments: %v", fs.Args())
	}
	return cliRequest{Op: "list", Alias: *alias, Repo: *repo}, nil
}

// aliasMatches reports whether a session with the alias passes `list --alias filter`.
// An empty filter passes every session; otherwise the alias must contain the
// filter, ignoring case, so a session without an alias never passes.
func aliasMatches(alias, filter string) bool {
	return filter == "" || strings.Contains(strings.ToLower(alias), strings.ToLower(filter))
}

// repoMatches reports whether a session in the repository repoPath passes
// `list --repo filter`. An empty filter passes every session; otherwise the
// repository's name, the last element of repoPath, must contain the filter,
// ignoring case.
// WHY 末尾の名前だけ: パス全体だと、親ディレクトリ（github.com や利用者名）の語に
// どのリポジトリも当たってしまう。
func repoMatches(repoPath, filter string) bool {
	return filter == "" || strings.Contains(strings.ToLower(filepath.Base(repoPath)), strings.ToLower(filter))
}

// WHY NOT list --tree: フラグの有無で list の出力が JSON とテキストに分かれる。
func parseTreeArgs(args []string) (cliRequest, error) {
	fs := flag.NewFlagSet("tree", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck tree")
		fmt.Fprintln(fs.Output(), "Prints the Claude Code sessions as a tree of /clear and fork. The same data is in session_chain and forked_from of list.")
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
		fmt.Fprintln(fs.Output(), "Stops the process and removes the workspace as x does in the TUI. r resumes it.")
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

func parseAliasArgs(args []string) (cliRequest, error) {
	fs := flag.NewFlagSet("alias", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck alias [--session ID|NAME] <ALIAS>")
		fmt.Fprintln(fs.Output(), "Gives a session a display name and prints it as JSON. Letters, digits, - _ . and up to 40 characters. An empty ALIAS removes it.")
		fs.PrintDefaults()
	}
	target := fs.String("session", os.Getenv(session.EnvSessionID), "ID or name of the session (default: $"+session.EnvSessionID+", this session)")
	if err := fs.Parse(args); err != nil {
		return cliRequest{}, err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return cliRequest{}, fmt.Errorf("alias: specify exactly one alias (\"\" removes it)")
	}
	if *target == "" {
		return cliRequest{}, fmt.Errorf("alias: no session (--session or $%s)", session.EnvSessionID)
	}
	return cliRequest{Op: "alias", Target: *target, Alias: fs.Arg(0)}, nil
}

func parseGCArgs(args []string) (cliRequest, error) {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck gc [--dry-run]")
		fmt.Fprintln(fs.Output(), "Removes workspaces no session owns, and Claude Code's records about removed workspaces, and prints them as JSON.")
		fs.PrintDefaults()
	}
	dryRun := fs.Bool("dry-run", false, "print what would be removed without removing it")
	if err := fs.Parse(args); err != nil {
		return cliRequest{}, err
	}
	if fs.NArg() > 0 {
		return cliRequest{}, fmt.Errorf("gc: unexpected arguments: %v", fs.Args())
	}
	return cliRequest{Op: "gc", DryRun: *dryRun}, nil
}

// Operations of `claude-deck project`.
const (
	projectList      = "list"
	projectShowArgs  = "show-args"
	projectSetArgs   = "set-args"
	projectClearArgs = "clear-args"
)

// ProjectInfo is the JSON form of one [projects."<path>"] table of the config file.
type ProjectInfo struct {
	Path              string   `json:"path"`
	WorkspaceSymlinks []string `json:"workspace_symlinks,omitempty"`
	AddDirs           []string `json:"add_dirs,omitempty"`
	ClaudeArgs        []string `json:"claude_args"`
}

func projectInfo(path string, pc config.ProjectConfig) ProjectInfo {
	args := pc.ClaudeArgs
	if args == nil {
		args = []string{}
	}
	return ProjectInfo{Path: path, WorkspaceSymlinks: pc.WorkspaceSymlinks, AddDirs: pc.AddDirs, ClaudeArgs: args}
}

func parseProjectArgs(args []string) (cliRequest, error) {
	usage := "Usage: claude-deck project list | project args [--clear] <DIR> [-- <ARG>...]"
	if len(args) == 0 {
		return cliRequest{}, fmt.Errorf("project: missing operation\n%s", usage)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("project list", flag.ContinueOnError)
		fs.Usage = func() {
			fmt.Fprintln(fs.Output(), "Usage: claude-deck project list")
			fmt.Fprintln(fs.Output(), "Prints the per-project settings in [projects] of config.toml as JSON.")
		}
		if err := fs.Parse(args[1:]); err != nil {
			return cliRequest{}, err
		}
		if fs.NArg() > 0 {
			return cliRequest{}, fmt.Errorf("project list: unexpected arguments: %v", fs.Args())
		}
		return cliRequest{Op: "project", ProjectOp: projectList}, nil
	case "args":
		fs := flag.NewFlagSet("project args", flag.ContinueOnError)
		fs.Usage = func() {
			fmt.Fprintln(fs.Output(), "Usage: claude-deck project args [--clear] <DIR> [-- <ARG>...]")
			fmt.Fprintln(fs.Output(), "Reads or writes the arguments added when Claude Code starts in the repository DIR (claude_args in config.toml), and prints them as JSON.")
			fmt.Fprintln(fs.Output(), "Arguments after -- replace them, --clear removes them, and neither prints them. They apply from the next start, resume or fork.")
			fs.PrintDefaults()
		}
		clear := fs.Bool("clear", false, "remove the setting")
		if err := fs.Parse(args[1:]); err != nil {
			return cliRequest{}, err
		}
		// WHY "--" を自分で探す: flag は最初の位置引数（DIR）で読むのをやめるので、
		// その後ろの "--" は rest に残る。
		rest := fs.Args()
		if len(rest) == 0 {
			fs.Usage()
			return cliRequest{}, fmt.Errorf("project args: specify the project directory")
		}
		dir, err := resolveDir(rest[0])
		if err != nil {
			return cliRequest{}, err
		}
		req := cliRequest{Op: "project", ProjectOp: projectShowArgs, Dir: dir}
		switch {
		case len(rest) == 1 && *clear:
			req.ProjectOp = projectClearArgs
		case len(rest) == 1:
		case rest[1] != "--":
			return cliRequest{}, fmt.Errorf("project args: put the arguments after \"--\": %v", rest[1:])
		case *clear:
			return cliRequest{}, fmt.Errorf("project args: --clear takes no arguments")
		case len(rest) == 2:
			return cliRequest{}, fmt.Errorf("project args: no arguments after \"--\" (use --clear to remove the setting)")
		default:
			req.ProjectOp = projectSetArgs
			req.ClaudeArgs = rest[2:]
		}
		return req, nil
	default:
		return cliRequest{}, fmt.Errorf("project: unknown operation %q\n%s", args[0], usage)
	}
}

// runProject reads and rewrites the per-project settings in the config file at path.
func runProject(req cliRequest, path string) error {
	// WHY ResolveLaunchDir: Manager が起動時に設定を引くキーは、new と同じ規則で決めた
	// リポジトリのルート。ワークスペースやサブディレクトリを渡されても同じキーにする。
	repoPath, _, _ := session.ResolveLaunchDir(req.Dir)
	switch req.ProjectOp {
	case projectSetArgs:
		if err := config.SetProjectClaudeArgs(path, repoPath, req.ClaudeArgs); err != nil {
			return err
		}
	case projectClearArgs:
		if err := config.SetProjectClaudeArgs(path, repoPath, nil); err != nil {
			return err
		}
	}
	cfg, err := config.LoadFrom(path)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if req.ProjectOp != projectList {
		return printJSON(projectInfo(repoPath, cfg.Projects[repoPath]))
	}
	infos := []ProjectInfo{}
	for _, p := range slices.Sorted(maps.Keys(cfg.Projects)) {
		infos = append(infos, projectInfo(p, cfg.Projects[p]))
	}
	return printJSON(infos)
}

// Hook events accepted by `claude-deck hook`.
const (
	hookStatus       = "status"
	hookSessionStart = "session-start"
	hookExited       = "exited"
	hookRateLimits   = "rate-limits"
	hookContext      = "context"
)

// parseHookArgs parses `hook <event> [args] --session ID`. These are called by
// the deck-status plugin and by the pane's exit command, not by people.
func parseHookArgs(args []string) (cliRequest, error) {
	usage := "Usage: claude-deck hook status <running|idle|waiting_approval|waiting_answer|subagent_running|waiting_action --reason TEXT --wait-kind command|api_error> | session-start --claude-session-id ID --source SOURCE | exited | rate-limits <JSON> | context --tokens N --window N  [--session DECK_ID]"
	if len(args) == 0 {
		return cliRequest{}, fmt.Errorf("hook: missing event\n%s", usage)
	}
	req := cliRequest{Op: "hook", HookEvent: args[0]}
	fs := flag.NewFlagSet("hook "+args[0], flag.ContinueOnError)
	fs.StringVar(&req.Session, "session", os.Getenv(session.EnvSessionID), "deck session ID (default: $"+session.EnvSessionID+")")
	switch req.HookEvent {
	case hookStatus:
		if len(args) < 2 {
			return cliRequest{}, fmt.Errorf("hook status: missing status\n%s", usage)
		}
		a, ok := session.ActivityFromID(args[1])
		if !ok {
			return cliRequest{}, fmt.Errorf("hook status: unknown status %q", args[1])
		}
		req.Activity = a
		fs.StringVar(&req.StatusReason, "reason", "", "why the session waits on the user (waiting_action only)")
		fs.Func("wait-kind", "what the session waits for: command or api_error (waiting_action only)", func(v string) error {
			if v == "" {
				return nil
			}
			k, ok := session.WaitKindFromID(v)
			if !ok {
				return fmt.Errorf("unknown wait kind %q", v)
			}
			req.WaitKind = k
			return nil
		})
		args = args[2:]
	case hookSessionStart:
		fs.StringVar(&req.ClaudeSessionID, "claude-session-id", "", "Claude Code session ID")
		fs.StringVar(&req.Source, "source", "", "source of SessionStart (startup / resume / fork / clear / compact)")
		args = args[1:]
	case hookExited:
		args = args[1:]
	case hookContext:
		// WHY 空の --tokens を受ける: 測った値が無いとき（新しい会話、compaction の直後）も
		// 呼び出しの形を 1 つにして、契約のテスト（hook_contract_test.go）が読めるようにする。
		fs.Func("tokens", "input tokens of the last response; empty before the first response of the live window", func(v string) error {
			if v == "" {
				return nil
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return fmt.Errorf("--tokens %q: want a count", v)
			}
			req.ContextTokens = &n
			return nil
		})
		fs.Func("window", "the context window of the session's model, in tokens", func(v string) error {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return fmt.Errorf("--window %q: want a positive count", v)
			}
			req.ContextWindow = n
			return nil
		})
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
	if req.HookEvent == hookStatus {
		if err := checkStatusReason(req.Activity, req.StatusReason, req.WaitKind); err != nil {
			return cliRequest{}, err
		}
	}
	if req.HookEvent == hookContext && req.ContextWindow == 0 {
		return cliRequest{}, fmt.Errorf("hook context: --window is required")
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
		return session.RecordHookStatus(st, id, req.Activity, session.Wait{Kind: req.WaitKind, Reason: req.StatusReason})
	case hookContext:
		return session.RecordContext(st, id, req.ContextTokens, req.ContextWindow)
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

// checkStatusReason requires --reason with waiting_action and refuses it and
// --wait-kind with any other status: the list shows the reason only for that wait.
// WHY 長さはここで見ない: 理由の文はプラグインが作り、一覧に収まる長さに切ってから渡す。
// WHY waiting_action に --wait-kind を求めない: 動いているセッションは、起動したときの古い deckmod を
// 読み込んだままで、種類を送らない。断ると待ちが一覧に出なくなる。種類の無い行にはヒントを出さないだけにする。
func checkStatusReason(a session.Activity, reason string, kind session.WaitKind) error {
	switch {
	case a == session.ActivityWaitingAction && strings.TrimSpace(reason) == "":
		return fmt.Errorf("hook status %s: --reason is required", a.Status().ID())
	case a != session.ActivityWaitingAction && kind != "":
		return fmt.Errorf("hook status %s: --wait-kind is only for %s", a.Status().ID(), session.StatusWaitingAction.ID())
	case strings.ContainsAny(reason, "\r\n"):
		// 一覧の 2 行目に 1 行で出すので、改行があると行が崩れる。
		return fmt.Errorf("hook status %s: --reason must be one line", a.Status().ID())
	case a != session.ActivityWaitingAction && reason != "":
		return fmt.Errorf("hook status %s: --reason is only for %s", a.Status().ID(), session.StatusWaitingAction.ID())
	}
	return nil
}
