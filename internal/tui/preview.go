package tui

import (
	"context"
	"fmt"
	"sync"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/pomesaka/claude-deck/internal/config"
	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/preview"
	"github.com/pomesaka/claude-deck/internal/usage"
)

// PreviewSpecMsg is sent when the main claude-deck process writes a new PreviewSpec
// to the IPC file. The preview process receives this via its fsnotify watcher.
type PreviewSpecMsg struct {
	Spec preview.PreviewSpec
}

// previewLogUpdateMsg carries updated log entries from the background JSONL streamer.
// ch is the streamer's output channel — carrying it in the message enables safe
// self-looping without a reference back to the streamer (see listenStream).
type previewLogUpdateMsg struct {
	entries []usage.LogEntry
	ch      <-chan []usage.LogEntry
}

// listenStream returns a tea.Cmd that waits for the next batch of log entries on ch.
// When ch is closed (stream stopped or restarted), the Cmd returns nil and exits cleanly.
func listenStream(ch <-chan []usage.LogEntry) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		entries, ok := <-ch
		if !ok {
			return nil
		}
		return previewLogUpdateMsg{entries: entries, ch: ch}
	}
}

// previewStreamer streams JSONL log entries for a given spec in a background goroutine.
// Each call to Start cancels the previous goroutine and allocates a fresh output channel.
// The goroutine closes its channel on exit, which unblocks any waiting listenStream Cmd.
//
// This is the preview-local equivalent of Manager.StreamSession, with no Manager dependency.
type previewStreamer struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

// Start cancels any running stream and launches a new one for spec.
// Returns the output channel for the new stream, or nil if spec.JSONLPath is empty
// or spec.Display is "tmux" (running session — Claude Code is live in the tmux window, no JSONL streaming needed).
// The returned channel is closed by the goroutine when it exits.
func (ps *previewStreamer) Start(rootCtx context.Context, spec preview.PreviewSpec) <-chan []usage.LogEntry {
	// Capture and clear the old cancel before releasing the lock to avoid
	// calling an arbitrary function (cancel) while holding ps.mu.
	ps.mu.Lock()
	oldCancel := ps.cancel
	if spec.JSONLPath == "" || spec.Display == "" || spec.Display == "tmux" {
		ps.cancel = nil
		ps.mu.Unlock()
		if oldCancel != nil {
			oldCancel()
		}
		return nil
	}
	ctx, cancel := context.WithCancel(rootCtx)
	ps.cancel = cancel
	ps.mu.Unlock()

	if oldCancel != nil {
		oldCancel() // signal old goroutine to exit; it will close its channel
	}
	out := make(chan []usage.LogEntry, 1)
	go ps.run(ctx, spec, out)
	return out
}

// Stop cancels the running stream without starting a new one.
func (ps *previewStreamer) Stop() {
	ps.mu.Lock()
	if ps.cancel != nil {
		ps.cancel()
		ps.cancel = nil
	}
	ps.mu.Unlock()
}

func (ps *previewStreamer) run(ctx context.Context, spec preview.PreviewSpec, out chan []usage.LogEntry) {
	// Closing out signals any waiting listenStream Cmd to return nil.
	defer close(out)

	// Attempt a non-blocking send; if the channel is full, replace the stale value.
	// ctx.Done() is included so we don't block when the context is cancelled.
	trySend := func(entries []usage.LogEntry) {
		select {
		case out <- entries:
		case <-ctx.Done():
		default:
			select {
			case <-out:
			default:
			}
			select {
			case out <- entries:
			case <-ctx.Done():
			}
		}
	}

	// Build prefix entries from /clear history (oldest first).
	// Uses the same algorithm as Manager.StreamSession.
	var prefixEntries []usage.LogEntry
	for i := len(spec.PriorJSONLPaths) - 1; i >= 0; i-- {
		prev := usage.NewLogStreamer(spec.PriorJSONLPaths[i])
		prev.ReadAll()
		prefixEntries = append(prev.Entries(), prefixEntries...)
		if len(prefixEntries) >= usage.MaxEntries {
			break
		}
	}
	if len(prefixEntries) > usage.MaxEntries {
		prefixEntries = prefixEntries[len(prefixEntries)-usage.MaxEntries:]
	}

	merge := func(current []usage.LogEntry) []usage.LogEntry {
		if len(prefixEntries) == 0 {
			return current
		}
		merged := make([]usage.LogEntry, 0, len(prefixEntries)+len(current))
		merged = append(merged, prefixEntries...)
		merged = append(merged, current...)
		if len(merged) > usage.MaxEntries {
			merged = merged[len(merged)-usage.MaxEntries:]
		}
		return merged
	}

	// Phase 1: tail-read the current file for instant display.
	s := usage.NewLogStreamer(spec.JSONLPath)
	fileSize := s.ReadTail(512 * 1024) // 512KB
	if ctx.Err() != nil {
		return
	}
	trySend(merge(s.Entries()))

	// Phase 2: watch for new writes (tail-follow).
	for {
		err := s.RunFrom(ctx, fileSize, func(entries []usage.LogEntry) {
			trySend(merge(entries))
		})
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			return
		}
		// Error — restart from scratch after a brief pause.
		s = usage.NewLogStreamer(spec.JSONLPath)
		fileSize = s.ReadTail(512 * 1024)
		if ctx.Err() != nil {
			return
		}
		trySend(merge(s.Entries()))
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// ── PreviewModel ──────────────────────────────────────────────────────────────

// PreviewModel is the Bubble Tea model for the --preview subprocess.
// It renders a full-screen JSONL log viewer for a single session and
// switches sessions in response to PreviewSpecMsg events from the main process.
//
// Unlike the main Model, PreviewModel owns no session.Manager — it renders
// only what the main process describes in the PreviewSpec IPC payload.
// Interaction is limited to scrolling and quitting.
type PreviewModel struct {
	config *config.Config
	ctx    context.Context

	width, height int

	spec        preview.PreviewSpec
	logEntries  []usage.LogEntry
	logViewport viewport.Model
	logFollow   bool
	logCache    renderCache
	streamer    *previewStreamer

	pendingG bool // vim-style gg sequence
}

// NewPreviewModel creates a PreviewModel ready to display JSONL logs.
func NewPreviewModel(cfg *config.Config, ctx context.Context) PreviewModel {
	vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(20))
	vp.SetContent("")
	return PreviewModel{
		config:      cfg,
		ctx:         ctx,
		logViewport: vp,
		logFollow:   true,
		streamer:    &previewStreamer{},
	}
}

// Init reads the current preview-selection file so the model shows the right
// session immediately on startup, without relying on a p.Send() call before
// p.Run() (which may not be reliable in all bubbletea versions).
func (m PreviewModel) Init() tea.Cmd {
	dataDir := m.config.DataDir
	return func() tea.Msg {
		spec, err := preview.ReadSpec(dataDir)
		if err != nil || spec.Display == "" {
			return nil
		}
		debuglog.Printf("[preview Init] initial selection: %s display=%s", spec.DeckSessionID, spec.Display)
		return PreviewSpecMsg{Spec: spec}
	}
}

// Update handles messages for the preview model.
func (m PreviewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case PreviewSpecMsg:
		prevSpec := m.spec
		m.spec = msg.Spec

		if msg.Spec.Display == "" {
			// Selection cleared
			m.streamer.Stop()
			m.logEntries = nil
			m.logCache = renderCache{}
			m.syncViewport()
			return m, nil
		}

		// Restart streaming when the session or JSONL path changes.
		// Status/tool updates with the same path reuse the existing listener.
		needsRestart := msg.Spec.JSONLPath != prevSpec.JSONLPath ||
			msg.Spec.DeckSessionID != prevSpec.DeckSessionID
		if needsRestart {
			debuglog.Printf("[preview] switching to session %s display=%s", msg.Spec.DeckSessionID, msg.Spec.Display)
			m.logEntries = nil
			m.logFollow = true
			m.logCache = renderCache{}
			ch := m.streamer.Start(m.ctx, msg.Spec)
			m.syncViewport()
			// Old listenStream Cmd sees the old channel close and returns nil.
			// This new Cmd listens on the fresh channel.
			return m, listenStream(ch)
		}
		// Metadata-only update (Status, CurrentTool, etc.) — keep existing listener.
		m.syncViewport()
		return m, nil

	case previewLogUpdateMsg:
		m.logEntries = msg.entries
		m.syncViewport()
		// Re-listen on the same channel for subsequent updates.
		return m, listenStream(msg.ch)

	case tea.WindowSizeMsg:
		debuglog.Printf("[preview] WindowSizeMsg: %dx%d", msg.Width, msg.Height)
		m.width = msg.Width
		m.height = msg.Height
		m.resizeViewport()
		m.syncViewport()
		return m, nil

	case tea.MouseWheelMsg:
		var cmd tea.Cmd
		m.logViewport, cmd = m.logViewport.Update(msg)
		m.logFollow = m.logViewport.AtBottom()
		return m, cmd

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m PreviewModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	if m.pendingG {
		m.pendingG = false
		if key == "g" {
			m.logViewport.GotoTop()
			m.logFollow = false
			return m, nil
		}
	}

	switch key {
	case "q", "ctrl+c":
		return m, tea.Quit

	case "g":
		m.pendingG = true
		return m, nil

	case "G":
		m.logViewport.GotoBottom()
		m.logFollow = true
		return m, nil

	case "j", "down":
		m.logViewport.ScrollDown(1)
		m.logFollow = m.logViewport.AtBottom()
		return m, nil

	case "k", "up":
		m.logViewport.ScrollUp(1)
		m.logFollow = false
		return m, nil

	case "ctrl+d", "ctrl+f", "pgdown":
		m.logViewport.HalfPageDown()
		m.logFollow = m.logViewport.AtBottom()
		return m, nil

	case "ctrl+u", "ctrl+b", "pgup":
		m.logViewport.HalfPageUp()
		m.logFollow = false
		return m, nil
	}
	return m, nil
}

// View renders the full-screen preview.
// AltScreen is always requested so bubbletea enters alt-screen immediately
// at startup, before the first WindowSizeMsg arrives.  Without this, the
// initial tea.View{} causes bubbletea to skip alt-screen mode entirely and
// the terminal appears black when running inside a tmux window.
func (m PreviewModel) View() tea.View {
	var body string
	if m.width > 0 && m.height > 0 {
		body = m.render()
	}
	v := tea.NewView(body)
	v.AltScreen = true
	return v
}

func (m PreviewModel) render() string {
	if m.spec.Display == "" {
		return lipgloss.NewStyle().
			Width(m.width).Height(m.height).
			Render(dimStyle.Render("セッションを選択してください"))
	}

	innerWidth := m.width - 2
	if innerWidth < 10 {
		innerWidth = 10
	}

	var sections []string

	switch m.spec.Display {
	case "tmux":
		// Running session — Claude Code is live in the tmux window.
		sections = m.renderHeader(innerWidth)
		sections = append(sections, "")
		sections = append(sections, dimStyle.Render("  tmux ウィンドウで表示中"))
		if m.spec.CurrentTool != "" {
			sections = append(sections, statusRunningStyle.Render(truncate(fmt.Sprintf("   🔧 %s", m.spec.CurrentTool), innerWidth)))
		}
		if m.spec.NeedsAttention {
			sections = append(sections, statusApproveStyle.Render(truncate("   👆 承認待ち — tmux ウィンドウで操作してください", innerWidth)))
		}

	default:
		// "jsonl" (and fallback for any other mode)
		sections = m.renderHeader(innerWidth)
		sections = append(sections, "")
		sections = append(sections, m.logViewport.View())
	}

	footer := dimStyle.Render(truncate("  j/k スクロール  gg/G 先頭/末尾  q 終了", m.width))
	sections = append(sections, footer)

	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

func (m PreviewModel) renderHeader(innerWidth int) []string {
	var h []string

	title := fmt.Sprintf("📋 %s (%s)", m.spec.Name, m.spec.RepoName)
	h = append(h, titleStyle.Render(truncate(title, innerWidth)))
	h = append(h, dimStyle.Render(truncate(fmt.Sprintf("   パス: %s", m.spec.WorkspacePath), innerWidth)))

	runtimeID := m.spec.RuntimeSessionID
	if runtimeID == "" {
		runtimeID = m.spec.ClaudeSessionID
	}
	idLine := fmt.Sprintf("   ID: %s  Runtime: %s", m.spec.DeckSessionID, runtimeID)
	if m.spec.ClearCount > 0 {
		idLine += fmt.Sprintf("  (/clear×%d)", m.spec.ClearCount)
	}
	h = append(h, dimStyle.Render(truncate(idLine, innerWidth)))

	if m.spec.CurrentTool != "" {
		h = append(h, statusRunningStyle.Render(truncate(fmt.Sprintf("   🔧 %s", m.spec.CurrentTool), innerWidth)))
	}
	if m.spec.NeedsAttention {
		h = append(h, statusApproveStyle.Render(truncate("   👆 承認待ち", innerWidth)))
	}
	if m.spec.Status == "error" && m.spec.ErrorMessage != "" {
		h = append(h, statusErrorStyle.Render(truncate("   ✗ "+m.spec.ErrorMessage, innerWidth)))
	}

	return h
}

// ── internal helpers ──────────────────────────────────────────────────────────

// headerLineCount returns how many lines renderHeader will produce.
func (m *PreviewModel) headerLineCount() int {
	if m.spec.Display == "" {
		return 0
	}
	n := 3 // title + path + id
	if m.spec.CurrentTool != "" {
		n++
	}
	if m.spec.NeedsAttention {
		n++
	}
	if m.spec.Status == "error" && m.spec.ErrorMessage != "" {
		n++
	}
	return n
}

func (m *PreviewModel) resizeViewport() {
	// footer(1) + empty line before viewport(1) + header lines
	headerLines := m.headerLineCount()
	logHeight := m.height - 1 - 1 - headerLines
	if logHeight < 1 {
		logHeight = 1
	}
	m.logViewport.SetWidth(m.width)
	m.logViewport.SetHeight(logHeight)
}

func (m *PreviewModel) syncViewport() {
	if m.spec.Display == "" || m.spec.Display == "tmux" {
		m.logViewport.SetContent("")
		return
	}

	innerWidth := m.width - 2
	if innerWidth < 10 {
		innerWidth = 10
	}

	if len(m.logEntries) > 0 {
		rendered := RenderLogs(m.logEntries, innerWidth, &m.logCache)
		m.logViewport.SetContent(rendered)
	} else {
		m.logViewport.SetContent(dimStyle.Render("(出力なし)"))
	}

	if m.logFollow {
		m.logViewport.GotoBottom()
	}
}
