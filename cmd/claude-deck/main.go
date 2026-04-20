package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"runtime/debug"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/pomesaka/claude-deck/internal/claudecode"
	"github.com/pomesaka/claude-deck/internal/config"
	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/ghostty"
	"github.com/pomesaka/claude-deck/internal/hooks"
	"github.com/pomesaka/claude-deck/internal/jj"
	"github.com/pomesaka/claude-deck/internal/preview"
	"github.com/pomesaka/claude-deck/internal/ratelimits"
	"github.com/pomesaka/claude-deck/internal/session"
	"github.com/pomesaka/claude-deck/internal/store"
	"github.com/pomesaka/claude-deck/internal/tui"
	"github.com/pomesaka/claude-deck/internal/usage"
)

func main() {
	defer func() {
		if r := recover(); r != nil {
			// bubbletea の alt screen を抜けてから表示するため、
			// リセットシーケンスを出力
			fmt.Fprint(os.Stderr, "\x1b[?1049l\x1b[?25h")
			fmt.Fprintf(os.Stderr, "\nclaude-deck panic: %v\n\n%s\n", r, debug.Stack())
			debuglog.Printf("PANIC: %v\n%s", r, debug.Stack())
			os.Exit(1)
		}
	}()

	previewMode := flag.Bool("preview", false, "run in preview-only mode (JSONL log viewer, for tmux __preview__ window)")
	flag.Parse()

	var err error
	if *previewMode {
		err = runPreview()
	} else {
		err = run()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Initialize debug logging (controlled by CLAUDE_DECK_DEBUG env var)
	if err := debuglog.Init(); err != nil {
		return fmt.Errorf("debuglog init: %w", err)
	}
	defer debuglog.Close()

	// Load config
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Apply config to package-level settings
	tui.InitStyles(cfg.Theme)
	usage.SetPricing(cfg.Pricing.InputPerMTok, cfg.Pricing.OutputPerMTok, cfg.Pricing.CacheWritePerMTok, cfg.Pricing.CacheReadPerMTok)
	usage.MaxEntries = cfg.Session.MaxJSONLEntries

	// Ensure data directory
	if err := cfg.EnsureDataDir(); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}

	// Claude Code の workspace trust プロンプトを回避するため、
	// dataDir に .git を配置し trusted として登録する（初回のみ実効）
	if err := claudecode.EnsureDataDirTrusted(cfg.DataDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: trust setup: %v\n", err)
	}

	// Claude Code の statusline スクリプトを配置し ~/.claude/settings.json に登録する。
	// スクリプトは各アシスタントメッセージ後に rate_limits データを DataDir に書き出す。
	if err := claudecode.SetupStatuslineHook(cfg.DataDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: statusline setup: %v\n", err)
	}

	// Initialize store
	st, err := store.New(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("initializing store: %w", err)
	}

	// Context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Parse refresh interval
	refreshInterval, err := time.ParseDuration(cfg.Session.RefreshInterval)
	if err != nil {
		refreshInterval = 5 * time.Second
	}

	// Resolve backend mode from config.
	backendMode := session.BackendPTY
	if cfg.Tmux.Enabled {
		backendMode = session.BackendTmux
	}

	// Create session manager
	mgr := session.NewManager(ctx, st, buildManagerConfig(cfg, backendMode, refreshInterval))

	// Load session metadata from store (fast: local JSON files only)
	if err := mgr.LoadExisting(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to load existing sessions: %v\n", err)
	}

	// tmux mode: reconcile in-memory state with live tmux windows.
	// Must run after LoadExisting so deck sessions are populated.
	if mgr.IsTmuxMode() {
		mgr.ReconcileTmux()
	}

	// Heavy JSONL reads はバックグラウンドで実行し TUI を即座に表示する。
	// 初回は offset=0 で最初の30件だけ discover して即表示。
	// 続きは 5秒 tick の RefreshFromJSONL に委ねて段階的に読み込む。
	go func() {
		mgr.HydrateFromJSONL()
		mgr.DiscoverExternalSessions()
	}()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// tmux mode: ensure __preview__ window exists (for split mode).
	// The preview window runs "claude-deck --preview" and shows JSONL logs for
	// the session selected in the list TUI via file-based IPC.
	var splitMode bool
	var splitTermUUID string // Ghostty terminal UUID for cleanup on exit
	if mgr.IsTmuxMode() {
		if err := mgr.EnsurePreviewWindow(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: preview window setup: %v\n", err)
		} else {
			splitMode = true
		}

		splitTermUUID = setupGhosttySplit(cfg)
	}

	// Create and run TUI
	model := tui.NewModel(mgr, cfg, ctx, tui.ModelOptions{SplitMode: splitMode})
	p := tea.NewProgram(model)

	// rate_limits ファイルを監視し、更新があれば TUI に通知する。
	// Pro/Max サブスクリプションユーザーのみ有効（APIキーユーザーはデータなし）。
	if err := ratelimits.Watch(ctx, cfg.DataDir, func(s ratelimits.Status) {
		p.Send(tui.RateLimitsUpdatedMsg{Status: s})
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: rate limits watcher: %v\n", err)
	}

	// ストリーマーやバックグラウンド処理からの変更通知を Bubble Tea に伝える
	mgr.SetOnChange(func(changed map[session.DeckSessionID]bool) {
		p.Send(tui.SessionRefreshMsg{ChangedIDs: changed})
	})
	mgr.StartNotifyLoop(ctx)
	// Spinner idle loop is only needed for PTY mode, where braille spinner
	// detection drives status transitions.  In tmux mode, hooks handle this.
	if !mgr.IsTmuxMode() {
		mgr.StartSpinnerIdleLoop(ctx)
	}

	// fsnotify で JSONL ファイルを監視し、LastActivity を即時更新する。
	// 失敗しても 5 秒 tick が動くので非致命的。
	if err := mgr.StartFileWatcher(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: file watcher: %v\n", err)
	}

	// Hook のセットアップ状態を確認し、イベント監視を開始する。
	// プラグイン方式への移行を案内。レガシー hooks はそのまま動作する。
	// Plugin 管理以外は起動前に警告を出してキー入力で続行する。
	if msg := hookWarningMessage(hooks.CheckHooks()); msg != "" {
		fmt.Print(msg)
		fmt.Fprint(os.Stderr, "Press any key to continue...")
		b := make([]byte, 1)
		if _, err := os.Stdin.Read(b); err != nil {
			return err
		}
		fmt.Fprint(os.Stderr, "\033[2K\r") // "Press any key..." 行だけクリア
	}
	if err := mgr.StartEventWatcher(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: event watcher: %v\n", err)
	}

	if _, err := p.Run(); err != nil {
		return fmt.Errorf("running TUI: %w", err)
	}

	// 終了時に全 managed セッションを永続化（TerminalTitle 等の実行時更新を保存）
	mgr.PersistAll()

	// claude-deck が開いた Ghostty 右ペインと preview ウィンドウを閉じる。
	// 自分で開いたものは自分で閉じる原則。
	if splitMode {
		if err := mgr.KillPreviewWindow(); err != nil {
			debuglog.Printf("kill preview window: %v", err)
		}
	}
	if splitTermUUID != "" {
		if err := ghostty.CloseTerminal(splitTermUUID); err != nil {
			debuglog.Printf("close ghostty terminal: %v", err)
		}
	}

	return nil
}

// runPreview runs claude-deck in preview-only mode.
// This mode is used when running inside the tmux __preview__ window:
// it watches the preview-selection file for PreviewSpec changes (written by main)
// and renders the JSONL structured log for the described session.
//
// Unlike the main process, preview owns no session.Manager — it is a read-only
// view driven entirely by the IPC payload from the main process.
func runPreview() error {
	if err := debuglog.Init(); err != nil {
		return fmt.Errorf("debuglog init: %w", err)
	}
	defer debuglog.Close()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	tui.InitStyles(cfg.Theme)
	usage.SetPricing(cfg.Pricing.InputPerMTok, cfg.Pricing.OutputPerMTok, cfg.Pricing.CacheWritePerMTok, cfg.Pricing.CacheReadPerMTok)
	usage.MaxEntries = cfg.Session.MaxJSONLEntries

	if err := cfg.EnsureDataDir(); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	model := tui.NewPreviewModel(cfg, ctx)
	p := tea.NewProgram(model)

	// Watch the preview-selection file and forward PreviewSpec changes to the TUI.
	// The initial selection is read inside PreviewModel.Init() so we don't
	// need to p.Send() before p.Run() (which is unreliable before start).
	if err := preview.WatchSpec(ctx, cfg.DataDir, func(spec preview.PreviewSpec) {
		p.Send(tui.PreviewSpecMsg{Spec: spec})
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: preview selection watcher: %v\n", err)
	}

	if _, err := p.Run(); err != nil {
		return fmt.Errorf("running preview TUI: %w", err)
	}
	return nil
}

// buildManagerConfig constructs the ManagerConfig from app config and derived values.
// Centralised here so run() and runPreview() stay in sync.
func buildManagerConfig(cfg *config.Config, backendMode session.BackendMode, refreshInterval time.Duration) session.ManagerConfig {
	return session.ManagerConfig{
		DataDir:               cfg.DataDir,
		ClaudeCommand:         cfg.Commands.Claude,
		JJ:                    &jj.Runner{Command: cfg.Commands.JJ},
		DefaultPermissionMode: cfg.Defaults.PermissionMode,
		MaxSessions:           cfg.Session.MaxSessions,
		MaxLogLines:           cfg.Session.MaxLogLines,
		MaxScrollback:         cfg.Session.MaxScrollback,
		DiscoveryDays:         cfg.Session.DiscoveryDays,
		RefreshInterval:       refreshInterval,
		Pricing: session.PricingPolicy{
			InputPerMTok:      cfg.Pricing.InputPerMTok,
			OutputPerMTok:     cfg.Pricing.OutputPerMTok,
			CacheWritePerMTok: cfg.Pricing.CacheWritePerMTok,
			CacheReadPerMTok:  cfg.Pricing.CacheReadPerMTok,
		},
		WorkspaceSymlinksFunc: cfg.WorkspaceSymlinks,
		AddDirsFunc:           cfg.ResolvedAddDirs,
		BackendMode:           backendMode,
		TmuxCommand:           cfg.Tmux.Command,
		TmuxSession:           cfg.Tmux.SessionName,
	}
}

// setupGhosttySplit opens the tmux right pane in Ghostty if running inside it.
// Returns the Ghostty terminal UUID for cleanup on exit (empty string if not split).
var tmuxSessionNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func setupGhosttySplit(cfg *config.Config) string {
	tmuxSession := cfg.Tmux.SessionName
	if tmuxSession == "" {
		tmuxSession = "claude-deck"
	}
	if !tmuxSessionNameRE.MatchString(tmuxSession) {
		fmt.Fprintf(os.Stderr, "warning: tmux session name %q contains invalid characters; falling back to \"claude-deck\"\n", tmuxSession)
		tmuxSession = "claude-deck"
	}

	if !ghostty.IsRunningInGhostty() {
		fmt.Fprintf(os.Stderr, "  別ペインで: tmux attach-session -t %s\n", tmuxSession)
		return ""
	}

	// Ghostty 内なら右ペインに自動分割して tmux attach を起動する。
	attachCmd := "tmux attach-session -t " + tmuxSession
	uuid, err := ghostty.SplitRight(attachCmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: Ghostty split: %v\n", err)
		return ""
	}

	// 分割後に少し待ってからリサイズし、フォーカスをリストペインに戻す
	time.Sleep(300 * time.Millisecond)
	if cfg.Ghostty.DeckWidth > 0 {
		if err := ghostty.ResizeSplit(cfg.Ghostty.DeckWidth); err != nil {
			debuglog.Printf("ghostty resize split: %v", err)
		}
	}
	// SplitRight で右ペインにフォーカスが移るので左ペイン（一覧 TUI）に戻す
	if err := ghostty.FocusLeft(); err != nil {
		debuglog.Printf("ghostty focus left: %v", err)
	}

	return uuid
}

func hookWarningMessage(status hooks.HookStatus) string {
	switch status {
	case hooks.HookStatusNone:
		return "⚠ claude-deck plugin not installed. Session status tracking requires hooks.\n" +
			"  Run:\n" +
			"    claude plugin marketplace add pomesaka/claude-deck\n" +
			"    claude plugin install claude-deck\n"
	case hooks.HookStatusOutdated:
		return "⚠ claude-deck plugin is outdated (latest: " + hooks.PluginVersion + ").\n" +
			"  Run:\n" +
			"    claude plugin update claude-deck\n"
	default:
		return ""
	}
}
