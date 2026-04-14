package tui

import (
	"fmt"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/pomesaka/claude-deck/internal/config"
	"github.com/pomesaka/claude-deck/internal/debuglog"
	"github.com/pomesaka/claude-deck/internal/preview"
	"github.com/pomesaka/claude-deck/internal/session"
)

// PreviewSelectionMsg is sent when the main claude-deck process writes a new
// session ID to the preview IPC file.  The preview process receives this via
// its fsnotify watcher and switches its display to the named session.
type PreviewSelectionMsg struct {
	SessionID session.DeckSessionID
}

// PreviewModel is the Bubble Tea model for the --preview subprocess.
// It renders a full-screen JSONL log viewer for a single session and
// switches sessions in response to PreviewSelectionMsg events.
//
// Unlike the main Model, PreviewModel has no session list, no PTY viewport,
// no session creation/deletion keys, and no pane layout management.
// Interaction is limited to scrolling and quitting.
type PreviewModel struct {
	manager *session.Manager
	config  *config.Config

	width, height int

	selectedID   session.DeckSessionID
	selectedSnap *session.Snapshot
	logViewport  viewport.Model
	logFollow    bool
	logCache     renderCache

	pendingG bool // vim-style gg sequence
}

// NewPreviewModel creates a PreviewModel ready to display the given initial session.
func NewPreviewModel(mgr *session.Manager, cfg *config.Config) PreviewModel {
	vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(20))
	vp.SetContent("")
	return PreviewModel{
		manager:     mgr,
		config:      cfg,
		logViewport: vp,
		logFollow:   true,
	}
}

// Init reads the current preview-selection file so the model shows the right
// session immediately on startup, without relying on a p.Send() call before
// p.Run() (which may not be reliable in all bubbletea versions).
func (m PreviewModel) Init() tea.Cmd {
	dataDir := m.config.DataDir
	return func() tea.Msg {
		sid, err := preview.ReadSelection(dataDir)
		if err != nil || sid == "" {
			return nil
		}
		debuglog.Printf("[preview Init] initial selection: %s", sid)
		return PreviewSelectionMsg{SessionID: sid}
	}
}

// Update handles messages for the preview model.
func (m PreviewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case PreviewSelectionMsg:
		if msg.SessionID != m.selectedID {
			debuglog.Printf("[preview] switching to session %s", msg.SessionID)
			m.selectedID = msg.SessionID
			m.logFollow = true
			m.manager.StreamSession(m.selectedID)
			m.refreshSnap()
			m.syncViewport()
		}
		return m, nil

	case SessionRefreshMsg:
		// Refresh if our session changed (token update, status change, etc.)
		if msg.ChangedIDs == nil || msg.ChangedIDs[m.selectedID] {
			m.refreshSnap()
			m.syncViewport()
		}
		return m, nil

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
	if m.selectedSnap == nil {
		return lipgloss.NewStyle().
			Width(m.width).Height(m.height).
			Render(dimStyle.Render("セッションを選択してください"))
	}

	snap := *m.selectedSnap
	innerWidth := m.width - 2
	if innerWidth < 10 {
		innerWidth = 10
	}

	var sections []string

	switch snap.Display {
	case session.DisplayNone:
		// Running session: show metadata only (Claude Code is in tmux window)
		sections = m.renderHeader(snap, innerWidth)
		sections = append(sections, "")
		sections = append(sections, dimStyle.Render("  tmux ウィンドウで表示中"))
		if snap.CurrentTool != "" {
			sections = append(sections, statusRunningStyle.Render(truncate(fmt.Sprintf("   🔧 %s", snap.CurrentTool), innerWidth)))
		}
		if snap.Status.NeedsAttention() {
			sections = append(sections, statusApproveStyle.Render(truncate("   👆 承認待ち — tmux ウィンドウで操作してください", innerWidth)))
		}

	default:
		// DisplayJSONL (and fallback for any other mode)
		sections = m.renderHeader(snap, innerWidth)
		sections = append(sections, "")
		sections = append(sections, m.logViewport.View())
	}

	footer := dimStyle.Render(truncate("  j/k スクロール  gg/G 先頭/末尾  q 終了", m.width))
	sections = append(sections, footer)

	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

func (m PreviewModel) renderHeader(snap session.Snapshot, innerWidth int) []string {
	var h []string

	title := fmt.Sprintf("📋 %s (%s)", snap.Name, snap.RepoName)
	h = append(h, titleStyle.Render(truncate(title, innerWidth)))
	h = append(h, dimStyle.Render(truncate(fmt.Sprintf("   パス: %s", snap.WorkspacePath), innerWidth)))

	idLine := fmt.Sprintf("   ID: %s  Claude: %s", snap.ID, snap.ClaudeSessionID)
	if snap.ClearCount > 0 {
		idLine += fmt.Sprintf("  (/clear×%d)", snap.ClearCount)
	}
	h = append(h, dimStyle.Render(truncate(idLine, innerWidth)))

	if snap.CurrentTool != "" {
		h = append(h, statusRunningStyle.Render(truncate(fmt.Sprintf("   🔧 %s", snap.CurrentTool), innerWidth)))
	}
	if snap.Status.NeedsAttention() {
		h = append(h, statusApproveStyle.Render(truncate("   👆 承認待ち", innerWidth)))
	}
	if snap.Status == session.StatusError && snap.ErrorMessage != "" {
		h = append(h, statusErrorStyle.Render(truncate("   ✗ "+snap.ErrorMessage, innerWidth)))
	}

	return h
}

// ── internal helpers ──────────────────────────────────────────────────────────

func (m *PreviewModel) refreshSnap() {
	if m.selectedID == "" {
		m.selectedSnap = nil
		return
	}
	if sess := m.manager.GetSession(m.selectedID); sess != nil {
		snap := sess.Snapshot()
		m.selectedSnap = &snap
		return
	}
	m.selectedSnap = nil
}

// headerLineCount returns how many header lines renderHeader will produce.
func (m *PreviewModel) headerLineCount() int {
	if m.selectedSnap == nil {
		return 0
	}
	snap := *m.selectedSnap
	n := 3 // title + path + id
	if snap.CurrentTool != "" {
		n++
	}
	if snap.Status.NeedsAttention() {
		n++
	}
	if snap.Status == session.StatusError && snap.ErrorMessage != "" {
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
	if m.selectedID == "" || m.selectedSnap == nil {
		m.logViewport.SetContent("")
		return
	}
	snap := *m.selectedSnap
	if snap.Display == session.DisplayNone {
		m.logViewport.SetContent("")
		return
	}

	sess := m.manager.GetSession(m.selectedID)
	if sess == nil {
		m.logViewport.SetContent(dimStyle.Render("(セッションが見つかりません)"))
		return
	}

	innerWidth := m.width - 2
	if innerWidth < 10 {
		innerWidth = 10
	}

	entries := sess.GetStructuredLogs()
	if len(entries) > 0 {
		rendered := RenderLogs(entries, innerWidth, &m.logCache)
		m.logViewport.SetContent(rendered)
	} else {
		m.logViewport.SetContent(dimStyle.Render("(出力なし)"))
	}

	if m.logFollow {
		m.logViewport.GotoBottom()
	}
}
