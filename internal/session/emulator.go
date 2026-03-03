package session

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/vt"
	"github.com/pomesaka/claude-deck/internal/debuglog"
)

// Default PTY dimensions (must match pty.StartOptions defaults).
const (
	defaultPTYCols = 120
	defaultPTYRows = 40
)

// newEmulatorWithCallbacks creates a vt.Emulator with OSC title callback wired to the session.
func newEmulatorWithCallbacks(s *Session, cols, rows int) *vt.Emulator {
	if cols <= 0 {
		cols = defaultPTYCols
	}
	if rows <= 0 {
		rows = defaultPTYRows
	}
	em := vt.NewEmulator(cols, rows)
	em.SetCallbacks(vt.Callbacks{
		Title: func(title string) {
			// AppendLog → emulator.Write → callback の呼び出しチェーンで
			// s.mu.Lock() が既に保持されているため直接代入で安全

			// 行分割で OSC シーケンスが途中切断されると不完全な UTF-8 が渡される。
			// 不正な UTF-8 は無視して前回のタイトルを維持する。
			if !utf8.ValidString(title) {
				debuglog.Printf("[session:%s] OSC title invalid UTF-8, ignoring: %x", s.ID, title)
				return
			}

			clean := stripSpinnerPrefix(title)
			debuglog.Printf("[session:%s] OSC title raw=%q clean=%q", s.ID, title, clean)
			s.TerminalTitle = clean
		},
		ScrollOut: func(plain, styled string) {
			// AppendLog → emulator.Write → ScrollUp → callback の呼び出しチェーンで
			// s.mu.Lock() が既に保持されているため直接代入で安全
			limit := s.maxScrollback
			if limit <= 0 {
				limit = 2000
			}
			s.scrollbackPlain = append(s.scrollbackPlain, plain)
			s.scrollbackStyled = append(s.scrollbackStyled, styled)
			if len(s.scrollbackPlain) > limit {
				drop := len(s.scrollbackPlain) - limit
				// 新しいスライスにコピーしてバッキング配列を解放
				newPlain := make([]string, limit)
				copy(newPlain, s.scrollbackPlain[drop:])
				s.scrollbackPlain = newPlain
				newStyled := make([]string, limit)
				copy(newStyled, s.scrollbackStyled[drop:])
				s.scrollbackStyled = newStyled
			}
		},
	})
	return em
}

// GetPTYDisplayLines returns the current screen state from the virtual terminal.
// charmbracelet/x/vt が ANSI エスケープシーケンスを完全に解釈し、
// bubbletea ベースの TUI 出力を読みやすいスクリーンスナップショットとして返す。
// Claude Code の入力エリアやステータスバーも含めて全画面を返す。
// raw PTY 入力モードではユーザーが直接 Claude Code の UI を操作するため、
// 下部クロームのフィルタリングは行わない。
func (s *Session) GetPTYDisplayLines() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.emulator == nil {
		return nil
	}
	plain := s.emulator.String()
	styled := s.emulator.Render()
	if plain == "" {
		return nil
	}

	// カーソル Y 座標（0-indexed）を取得。Ink が最後に描画した行がカーソル位置なので、
	// それより下はリドロー前の残像であり表示すべきでない。
	cursorY := s.emulator.CursorPosition().Y

	plainLines := strings.Split(plain, "\n")
	styledLines := strings.Split(styled, "\n")

	// カーソル行より下を切り捨て（残像の根本除去）
	limit := cursorY + 1
	if limit < len(plainLines) {
		plainLines = plainLines[:limit]
	}
	if limit < len(styledLines) {
		styledLines = styledLines[:limit]
	}

	// 末尾の空行を除去（スクリーンバッファの未使用行）
	for len(plainLines) > 0 && strings.TrimRight(plainLines[len(plainLines)-1], " ") == "" {
		plainLines = plainLines[:len(plainLines)-1]
	}
	if len(plainLines) == 0 {
		return nil
	}
	if len(styledLines) > len(plainLines) {
		styledLines = styledLines[:len(plainLines)]
	}

	// Ink はスクリーン末尾付近にタブタイトル行を可視テキストとして描画する。
	// リアルターミナルではタブバーに隠れるが、PTY エミュレータでは見える。
	// さらに Ink のインクリメンタルレンダラは行全体を消去せず先頭数文字だけ
	// 上書きするため、タイトルが部分欠損して残る（例: "Weather Inquiry" → "   ather Inquiry"）。
	// 双方向の部分文字列マッチで、完全一致・部分欠損の両方を捕捉する。
	if title := s.TerminalTitle; title != "" {
		const scanRange = 8
		scanStart := max(0, len(plainLines)-scanRange)
		for i := len(plainLines) - 1; i >= scanStart; i-- {
			line := strings.TrimSpace(plainLines[i])
			if line == "" {
				continue
			}
			// 正方向: 行がタイトル全体を含む（完全一致ケース）
			// 逆方向: タイトルが行を含む（Ink の部分上書きで先頭欠損したケース）
			// 逆方向は誤マッチ防止のため最低 4 文字を要求
			if strings.Contains(line, title) || (len(line) >= 4 && strings.Contains(title, line)) {
				debuglog.Printf("[session:%s] title filter: removed line[%d] %q (title=%q)", s.ID, i, line, title)
				plainLines = append(plainLines[:i], plainLines[i+1:]...)
				if i < len(styledLines) {
					styledLines = append(styledLines[:i], styledLines[i+1:]...)
				}
			}
		}
		// タイトル除去後に末尾に残った空行を再トリム
		for len(plainLines) > 0 && strings.TrimRight(plainLines[len(plainLines)-1], " ") == "" {
			plainLines = plainLines[:len(plainLines)-1]
		}
		if len(styledLines) > len(plainLines) {
			styledLines = styledLines[:len(plainLines)]
		}
	}

	// スクロールバック（画面上端から消えた行）+ 現画面を結合
	var result []string
	if len(s.scrollbackStyled) > 0 {
		result = make([]string, 0, len(s.scrollbackStyled)+len(styledLines))
		result = append(result, s.scrollbackStyled...)
		result = append(result, styledLines...)
	} else {
		result = styledLines
	}
	debuglog.Printf("[session:%s] GetPTYDisplayLines: %d lines (%d scrollback + %d screen, cursorY=%d)",
		s.ID, len(result), len(s.scrollbackStyled), len(styledLines), cursorY)
	return result
}

