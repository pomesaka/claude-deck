package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/pomesaka/claude-deck/deckmod"
	"github.com/pomesaka/claude-deck/internal/claudecode"
	"github.com/pomesaka/claude-deck/internal/config"
	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/ghostty"
	"github.com/pomesaka/claude-deck/internal/jj"
	"github.com/pomesaka/claude-deck/internal/preview"
	"github.com/pomesaka/claude-deck/internal/ratelimits"
	"github.com/pomesaka/claude-deck/internal/session"
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

	if len(os.Args) > 1 {
		if _, ok := cliCommands[os.Args[1]]; ok {
			err := runCLI(os.Args[1], os.Args[2:])
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			return
		}
	}

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
	st, err := session.OpenStore(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("initializing store: %w", err)
	}
	defer st.Close()

	mcfg, err := buildManagerConfig(cfg)
	if err != nil {
		return err
	}

	// Context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create session manager
	mgr := session.NewManager(ctx, st, mcfg)

	// Load session metadata from the store
	if err := mgr.LoadExisting(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to load existing sessions: %v\n", err)
	}

	// Reconcile in-memory state with live tmux windows.
	// Must run after LoadExisting so deck sessions are populated.
	mgr.ReconcileTmux()

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

	// Ensure __preview__ window exists and enter split mode if successful.
	// splitMode=true means the preview window is available; the TUI shows list-only
	// layout with cursor navigation driving the right tmux pane.
	// Actual Ghostty pane splitting (splitTermUUID != "") is best-effort and only
	// happens when claude-deck is running inside Ghostty — splitMode stays true
	// regardless so the list-only layout is active even without a live Ghostty split.
	var splitMode bool
	var splitTermUUID string // Ghostty terminal UUID for cleanup on exit
	if err := mgr.EnsurePreviewWindow(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: preview window setup: %v\n", err)
	} else {
		splitMode = true
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

	// fsnotify で JSONL ファイルを監視し、LastActivity を即時更新する。
	// 失敗しても 5 秒 tick が動くので非致命的。
	if err := mgr.StartFileWatcher(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: file watcher: %v\n", err)
	}

	// CLI・hook・ペインの終了コマンドが store に書いた変更を一覧に反映する。
	go mgr.WatchStore(ctx)

	if _, err := p.Run(); err != nil {
		return fmt.Errorf("running TUI: %w", err)
	}

	// 終了時に JSONL から読んだトークン数などを保存し、次回起動時にすぐ表示できるようにする
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

// buildManagerConfig constructs the ManagerConfig from app config.
// It installs the deck-status plugin under the data directory, since every
// session claude-deck starts (from the TUI or the CLI) loads it with --plugin-dir.
func buildManagerConfig(cfg *config.Config) (session.ManagerConfig, error) {
	refreshInterval, err := time.ParseDuration(cfg.Session.RefreshInterval)
	if err != nil {
		refreshInterval = 5 * time.Second
	}
	deckCommand, err := os.Executable()
	if err != nil {
		return session.ManagerConfig{}, fmt.Errorf("locating claude-deck binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(deckCommand); err == nil {
		deckCommand = resolved
	}
	pluginDir, err := deckmod.Install(filepath.Join(cfg.DataDir, "plugin"))
	if err != nil {
		return session.ManagerConfig{}, err
	}
	return session.ManagerConfig{
		DataDir:               cfg.DataDir,
		ClaudeCommand:         cfg.Commands.Claude,
		JJ:                    &jj.Runner{Command: cfg.Commands.JJ},
		DefaultPermissionMode: cfg.Defaults.PermissionMode,
		MaxSessions:           cfg.Session.MaxSessions,
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
		DeckCommand:           deckCommand,
		PluginDir:             pluginDir,
		TmuxCommand:           cfg.Tmux.Command,
		TmuxSession:           cfg.Tmux.SessionName,
	}, nil
}

// setupGhosttySplit opens the tmux right pane in Ghostty if running inside it.
// Returns the Ghostty terminal UUID for cleanup on exit (empty string if not split).
func setupGhosttySplit(cfg *config.Config) string {
	tmuxSessionNameRE := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
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
