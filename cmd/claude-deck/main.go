package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/pomesaka/sandbox/claude-deck/internal/claudecode"
	"github.com/pomesaka/sandbox/claude-deck/internal/config"
	"github.com/pomesaka/sandbox/claude-deck/internal/debuglog"
	"github.com/pomesaka/sandbox/claude-deck/internal/hooks"
	"github.com/pomesaka/sandbox/claude-deck/internal/jj"
	"github.com/pomesaka/sandbox/claude-deck/internal/pty"
	"github.com/pomesaka/sandbox/claude-deck/internal/session"
	"github.com/pomesaka/sandbox/claude-deck/internal/store"
	"github.com/pomesaka/sandbox/claude-deck/internal/tui"
	"github.com/pomesaka/sandbox/claude-deck/internal/usage"
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

	if err := run(); err != nil {
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
	jj.Command = cfg.Commands.JJ
	pty.Command = cfg.Commands.Claude
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

	// Create session manager
	mgr := session.NewManager(ctx, st, session.ManagerConfig{
		DataDir:               cfg.DataDir,
		DefaultPermissionMode: cfg.Defaults.PermissionMode,
		MaxSessions:           cfg.Session.MaxSessions,
		MaxLogLines:           cfg.Session.MaxLogLines,
		MaxScrollback:         cfg.Session.MaxScrollback,
		DiscoveryDays:         cfg.Session.DiscoveryDays,
		RefreshInterval:       refreshInterval,
	})

	// Load session metadata from store (fast: local JSON files only)
	if err := mgr.LoadExisting(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to load existing sessions: %v\n", err)
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

	// Create and run TUI
	model := tui.NewModel(mgr, cfg, ctx)
	p := tea.NewProgram(model)

	// ストリーマーやバックグラウンド処理からの変更通知を Bubble Tea に伝える
	mgr.SetOnChange(func() {
		p.Send(tui.SessionRefreshMsg{})
	})
	mgr.StartNotifyLoop(ctx)
	mgr.StartSpinnerIdleLoop(ctx)

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
		os.Stdin.Read(b) //nolint:errcheck
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

	return nil
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
