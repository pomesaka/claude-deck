# Ghostty IPC ペインコントロール 実装プラン

前提: [Design Doc](./ghostty-ipc-pane-control.md) を参照。

## Phase 1: MVP

### Step 1: AppleScript ラッパー

**ファイル**: `internal/ghostty/applescript.go`

osascript コマンドを Go から呼び出す薄いラッパーを実装する。

```go
type AppleScriptClient struct{}

// ウィンドウ作成（claude-deck 専用ウィンドウがなければ作成）
func (c *AppleScriptClient) CreateWindow(cfg SurfaceConfig) (windowRef string, err error)

// タブ作成（指定ウィンドウに追加）
func (c *AppleScriptClient) CreateTab(windowRef string, cfg SurfaceConfig) (terminalUUID string, err error)

// ターミナル一覧取得
func (c *AppleScriptClient) ListTerminals() ([]TerminalInfo, error)

// UUID でターミナル存在確認
func (c *AppleScriptClient) TerminalExists(uuid string) (bool, error)

// タブ切替
func (c *AppleScriptClient) GotoTab(index int) error

// ターミナル閉じ
func (c *AppleScriptClient) CloseTerminal(uuid string) error

type SurfaceConfig struct {
    WorkDir string
    Env     []string
    Command string // "claude --resume ..." etc.
}

type TerminalInfo struct {
    Index     int
    UUID      string
    Title     string
    TTY       string
    WorkDir   string
}
```

実装方針:
- `os/exec` で `osascript -e '...'` を実行
- AppleScript のテンプレートは Go 文字列定数で管理
- エラーハンドリング: osascript の stderr を解析
- ビルドタグ `//go:build darwin` で macOS 限定

**依存**: なし（新規ファイル）

### Step 2: config に Ghostty モード追加

**ファイル**: `internal/config/config.go`

```go
type GhosttyConfig struct {
    Command string `toml:"command"`
    Mode    string `toml:"mode"` // "native" or "window" (default)
}
```

`Mode` フィールドを追加。デフォルトは `"window"`（既存動作）。

**依存**: なし

### Step 3: Session に Ghostty メタデータ追加

**ファイル**: `internal/session/session.go`, `internal/store/store.go`

```go
type Session struct {
    // ... 既存フィールド ...
    GhosttyTerminalUUID string // Ghostty タブの UUID
}
```

- store の JSON シリアライズに含める
- `SetGhosttyUUID()` / `GetGhosttyUUID()` アクセサ追加

**依存**: なし

### Step 4: Manager に Ghostty モードのセッション作成を追加

**ファイル**: `internal/session/manager.go`

`CreateSession` / `ResumeSession` / `ForkSession` を分岐:

```go
func (m *Manager) CreateSession(ctx context.Context, ...) (*Session, error) {
    // ... 既存のワークスペース作成等 ...

    if m.ghosttyMode {
        return m.createGhosttySession(ctx, sess, actualWorkDir, ...)
    }
    return m.createPTYSession(ctx, sess, actualWorkDir, ...)
}
```

`createGhosttySession` の処理:
1. `AppleScriptClient.CreateTab()` で Ghostty タブ作成
   - command: `claude --agent <name>` (新規) / `claude --resume <id>` (再開) / `claude --resume <id> --fork-session` (フォーク)
   - env: `CLAUDE_DECK_SESSION_ID=<deckID>`
   - workDir: ワークスペースパス
2. 返された UUID を `sess.GhosttyTerminalUUID` に保存
3. `sess.managed = true`, `sess.SetStatus(StatusIdle)`
4. persist + notifyChange
5. PTY 関連の処理（emulator 作成、handleOutput、watchProcess）はスキップ

**依存**: Step 1, 3

### Step 5: Ghostty ターミナルポーリング（終了検知）

**ファイル**: `internal/session/manager_ghostty.go` (新規)

```go
// StartGhosttyWatcher は Ghostty タブの生存をポーリングで確認する。
// 消失したターミナルのセッションを Completed に遷移させる。
func (m *Manager) StartGhosttyWatcher(ctx context.Context) {
    go func() {
        ticker := time.NewTicker(5 * time.Second)
        defer ticker.Stop()
        for {
            select {
            case <-ctx.Done():
                return
            case <-ticker.C:
                m.checkGhosttyTerminals()
            }
        }
    }()
}

func (m *Manager) checkGhosttyTerminals() {
    // 1. ListTerminals() で全ターミナル UUID を取得
    // 2. managed セッションの GhosttyTerminalUUID と照合
    // 3. 消失していたら sess.SetStatus(StatusCompleted) + persist
}
```

**依存**: Step 1, 3, 4

### Step 6: JSONL 更新による Running 検知

**ファイル**: `internal/session/manager_jsonl.go`

既存の `handleFileWrite` を拡張:

```go
func (m *Manager) handleFileWrite(claudeSessionID string) {
    // ... 既存の LastActivity 更新 ...

    // Ghostty モード: JSONL 更新 = Running の近似
    if m.ghosttyMode {
        sess := m.findSessionByClaudeID(claudeSessionID)
        if sess != nil {
            status := sess.GetStatus()
            if status == StatusIdle {
                sess.SetStatus(StatusRunning)
                m.notifyChange()
            }
        }
    }
}
```

**依存**: Step 4 (ghosttyMode フラグ)

### Step 7: TUI のキーバインド変更

**ファイル**: `internal/tui/keys.go`, `internal/tui/model.go`

Ghostty モード時:
- `Enter`/`i`: `AppleScriptClient.GotoTab(N)` でフォーカス切替
  - セッションの GhosttyTerminalUUID からタブ index を逆引き
- `t`: 同上（`Enter` と同じ動作）
- `n`/`r`/`f`: Ghostty タブ作成に切替（Manager 経由）

**依存**: Step 1, 4

### Step 8: TUI 右ペインの変更

**ファイル**: `internal/tui/view.go`

Ghostty モードではアクティブセッションも JSONL ログ表示:

```go
func (m Model) renderDetailPane(...) string {
    if m.ghosttyMode {
        // 常に JSONL 構造化ログを表示（PTY ビューポート不使用）
        return m.renderJSONLDetailPane(sess, ...)
    }
    // 既存: アクティブなら PTY ビューポート、完了なら JSONL
    // ...
}
```

**依存**: Step 4

## Phase 2: ウィンドウ管理・タブ連動

### Step 9: 専用ウィンドウ管理

- 起動時に claude-deck 専用 Ghostty ウィンドウを作成（または既存を検出）
- ウィンドウ参照を Manager に保持
- 新規タブは常にこのウィンドウに追加

### Step 10: TUI ↔ Ghostty タブ連動

- TUI でセッション選択を切り替えたら、Ghostty 側も `goto_tab:N` で追従
- Ghostty 側のタブ切替を TUI に反映（ポーリングで focused terminal を監視）

### Step 11: タブタイトル設定

- セッション作成時の command に OSC 0 設定を含める:
  ```
  printf '\033]0;session-name\007' && claude --agent session-name
  ```
- セッション名変更時も `send text` で OSC を送信

### Step 12: `x` キーでタブ閉鎖

- `AppleScriptClient.CloseTerminal(uuid)` を呼ぶ
- ポーリングで終了を検知 → Completed

## Phase 3: 分割・Linux 対応

### Step 13: Ghostty 分割レイアウト

- TUI と Claude Code を1つの Ghostty ウィンドウの分割ペインに配置
- `split direction right` で左=TUI、右=Claude Code のレイアウト

### Step 14: Linux D-Bus 対応

- Ghostty 側の D-Bus API 拡張待ち
- `internal/ghostty/dbus.go` で D-Bus ラッパーを実装
- interface を定義し、AppleScript / D-Bus を差し替え可能に

## ファイル一覧

| ファイル | 変更種別 | Step |
|---------|---------|------|
| `internal/ghostty/applescript.go` | **新規** | 1 |
| `internal/ghostty/ghostty.go` | 改修 | 1 (interface 抽出) |
| `internal/config/config.go` | 改修 | 2 |
| `internal/session/session.go` | 改修 | 3 |
| `internal/store/store.go` | 改修 | 3 |
| `internal/session/manager.go` | 改修 | 4 |
| `internal/session/manager_ghostty.go` | **新規** | 5 |
| `internal/session/manager_jsonl.go` | 改修 | 6 |
| `internal/tui/keys.go` | 改修 | 7 |
| `internal/tui/model.go` | 改修 | 7, 8 |
| `internal/tui/view.go` | 改修 | 8 |

## テスト戦略

### ユニットテスト

- `AppleScriptClient`: osascript のモック（interface + テスト用 stub）
- `Manager` Ghostty モード: タブ作成/終了検知のロジックテスト
- `handleFileWrite` の Running 遷移: 既存テストの拡張

### 統合テスト (手動)

- macOS + Ghostty 1.3+ 環境で E2E 確認
- TCC (Automation) 権限の初回承認フロー確認
- タブ作成 → 操作 → 終了 → TUI 反映のフルサイクル

### ビルド

```bash
# macOS (Ghostty モード含む)
GOEXPERIMENT=jsonv2 go build -o claude-deck ./cmd/claude-deck

# Linux (Ghostty モードは除外される: //go:build darwin)
GOEXPERIMENT=jsonv2 go build -o claude-deck ./cmd/claude-deck
```

## 実装順序の根拠

1. **Step 1 (AppleScript ラッパー)** を最初に。他の全ステップがこれに依存
2. **Step 2-3 (config, session)** は独立して進められるデータ構造変更
3. **Step 4 (Manager 分岐)** がコア。PTY と Ghostty の分岐点
4. **Step 5-6 (終了検知, Running 検知)** はセッション管理の必須要素
5. **Step 7-8 (TUI)** は最後。Manager が動作確認できてから UI を接続

Phase 1 完了で MVP として使用可能。Phase 2 以降は使用感を見てから優先度を判断する。
