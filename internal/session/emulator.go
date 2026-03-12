package session

import (
	"io"
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

	// エミュレータは DA1/DA2 等のターミナルクエリを受け取ると e.pw（io.Pipe 書き込み端）へ
	// 応答を書く。claude-deck はこの応答を使わないが、誰も e.pr を読まないと
	// io.Pipe が unbuffered なため Write() が即座にブロックする。
	// goroutine で Read() を捨て続けることでブロックを防ぐ。
	go io.Copy(io.Discard, em) //nolint:errcheck

	em.SetCallbacks(vt.Callbacks{
		Title: func(title string) {
			// emuMu 保持中に呼ばれる。lock 順 (emuMu → mu) を守り mu を自前で取る。
			// 行分割で OSC シーケンスが途中切断されると不完全な UTF-8 が渡される。
			// 不正な UTF-8 は無視して前回のタイトルを維持する。
			if !utf8.ValidString(title) {
				debuglog.Printf("[session:%s] OSC title invalid UTF-8, ignoring: %x", s.ID, title)
				return
			}
			clean := stripSpinnerPrefix(title)
			debuglog.Printf("[session:%s] OSC title raw=%q clean=%q", s.ID, title, clean)
			s.mu.Lock()
			s.TerminalTitle = clean
			s.mu.Unlock()
		},
		ScrollOut: func(plain, styled string) {
			// emuMu 保持中に呼ばれる。lock 順 (emuMu → mu) を守り mu を自前で取る。
			s.mu.Lock()
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
			s.mu.Unlock()
		},
	})
	return em
}

// refreshDisplayCacheLocked は emuMu を保持した状態で呼ぶ。
// エミュレータのスナップショットを取り、displayCache を更新する。
// lock 順: emuMu 保持中に mu.RLock を取得（emuMu → mu の順で正しい）。
func (s *Session) refreshDisplayCacheLocked() {
	plain := s.emulator.String()
	styled := s.emulator.Render()
	cursorY := s.emulator.CursorPosition().Y

	// mu.RLock で scrollback/title を読む（emuMu は保持中、lock 順: emuMu → mu）
	s.mu.RLock()
	title := s.TerminalTitle
	scrollbackStyled := s.scrollbackStyled
	s.mu.RUnlock()

	lines := buildDisplayLines(plain, styled, cursorY, title, scrollbackStyled, s.ID)
	s.displayCache.Store(&lines)
}

// buildDisplayLines はエミュレータのスナップショットからディスプレイ行を構築する。
// ロックを取らない純粋関数。refreshDisplayCacheLocked から呼ぶ。
func buildDisplayLines(plain, styled string, cursorY int, title string, scrollbackStyled []string, sessionID string) []string {
	if plain == "" {
		return nil
	}

	plainLines := strings.Split(plain, "\n")
	styledLines := strings.Split(styled, "\n")

	// カーソル Y 座標（0-indexed）を取得。Ink が最後に描画した行がカーソル位置なので、
	// それより下はリドロー前の残像であり表示すべきでない。
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
	if title != "" {
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
				debuglog.Printf("[session:%s] title filter: removed line[%d] %q (title=%q)", sessionID, i, line, title)
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
	if len(scrollbackStyled) > 0 {
		result = make([]string, 0, len(scrollbackStyled)+len(styledLines))
		result = append(result, scrollbackStyled...)
		result = append(result, styledLines...)
	} else {
		result = styledLines
	}
	debuglog.Printf("[session:%s] buildDisplayLines: %d lines (%d scrollback + %d screen, cursorY=%d)",
		sessionID, len(result), len(scrollbackStyled), len(styledLines), cursorY)
	return result
}

// GetPTYDisplayLines returns the current screen state from the virtual terminal.
// AppendRaw 内で更新された displayCache を返すためブロックしない。
func (s *Session) GetPTYDisplayLines() []string {
	if p := s.displayCache.Load(); p != nil {
		return *p
	}
	return nil
}

