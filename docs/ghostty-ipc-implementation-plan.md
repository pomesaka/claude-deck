# Ghostty IPC ペインコントロール 実装プラン

前提: [Design Doc](./ghostty-ipc-pane-control.md) を参照。

## Phase 1: MVP

### Step 1: AppleScript ラッパー

**ファイル**: `internal/ghostty/applescript.go` (新規, `//go:build darwin`)

osascript コマンドを Go から呼び出す薄いラッパーを実装する。

```go
// GhosttyIPC は Ghostty の AppleScript IPC をラップする interface。
// テスト時にモック差し替え可能。
type GhosttyIPC interface {
    // CreateWindow は新規 Ghostty ウィンドウを作成し、最初のターミナル UUID を返す。
    CreateWindow(cfg SurfaceConfig) (terminalUUID string, err error)

    // SplitTerminal は指定ターミナルを分割し、新ターミナル UUID を返す。
    SplitTerminal(targetUUID string, direction SplitDirection, cfg SurfaceConfig) (newUUID string, err error)

    // CloseTerminal は指定ターミナルを閉じる。
    CloseTerminal(uuid string) error

    // FocusTerminal は指定ターミナルにフォーカスを移す。
    FocusTerminal(uuid string) error

    // ResizeSplit は分割ペインをリサイズする (pixels 単位)。
    ResizeSplit(terminalUUID string, direction SplitDirection, pixels int) error

    // ListTerminals は全ターミナルの情報を返す。
    ListTerminals() ([]TerminalInfo, error)

    // TerminalExists は指定 UUID のターミナルが存在するか確認する。
    TerminalExists(uuid string) (bool, error)

    // GetFrontWindowTerminals は最前面ウィンドウのターミナル UUID 一覧を返す。
    GetFrontWindowTerminals() ([]string, error)
}

type SplitDirection string

const (
    SplitRight SplitDirection = "right"
    SplitDown  SplitDirection = "down"
    SplitLeft  SplitDirection = "left"
    SplitUp    SplitDirection = "up"
)

type SurfaceConfig struct {
    Command       string   // "claude --agent ..." (シェルの代わりに実行)
    WorkDir       string   // initial working directory
    Env           []string // "KEY=value" 形式
    WaitAfterCmd  bool     // コマンド終了後もターミナルを維持
    InitialInput  string   // 起動後にターミナルに入力するテキスト
}

type TerminalInfo struct {
    UUID    string
    Index   int
    Name    string // terminal title
    TTY     string
    WorkDir string
}
```

実装方針:
- `os/exec` で `osascript -e '...'` を実行
- AppleScript のテンプレートは Go 文字列定数で管理
- 操作間に delay を挿入（split 後 250ms、タブ作成後 400ms）
- エラーハンドリング: osascript の stderr を解析
- ビルドタグ `//go:build darwin` で macOS 限定
- Linux 用のスタブ (`applescript_other.go`, `//go:build !darwin`) はエラーを返す

**依存**: なし（新規ファイル）

### Step 2: config に Ghostty モード追加

**ファイル**: `internal/config/config.go`

```go
type GhosttyConfig struct {
    Command string `toml:"command"`
    Mode    string `toml:"mode"` // "split" or "window" (default)
}
```

`Mode` フィールドを追加。デフォルトは `"window"`（既存動作）。

**依存**: なし

### Step 3: Session に Ghostty メタデータ追加

**ファイル**: `internal/session/session.go`, `internal/store/store.go`

```go
type Session struct {
    // ... 既存フィールド ...
    GhosttyTerminalUUID string // Ghostty ターミナルの UUID (split モード用)
}
```

- store の JSON シリアライズに含める
- `SetGhosttyUUID()` / `GetGhosttyUUID()` アクセサ追加（`mu` で保護）

**依存**: なし

### Step 4: Manager に Ghostty split モードのセッション作成を追加

**ファイル**: `internal/session/manager.go`, `internal/session/manager_ghostty.go` (新規)

`CreateSession` / `ResumeSession` / `ForkSession` を分岐:

```go
func (m *Manager) CreateSession(ctx context.Context, ...) (*Session, error) {
    // ... 既存のワークスペース作成等 ...

    if m.ghosttyMode() {
        return m.createGhosttySession(ctx, sess, actualWorkDir, ...)
    }
    return m.createPTYSession(ctx, sess, actualWorkDir, ...)
}
```

`createGhosttySession` の処理 (`manager_ghostty.go`):
1. `SurfaceConfig` を構築:
   - `command`: `"claude --agent <name>"` (新規) / `"claude --resume <id>"` (再開) / `"claude --resume <id> --fork-session"` (フォーク)
   - `env`: `["CLAUDE_DECK_SESSION_ID=<deckID>"]`
   - `workDir`: ワークスペースパス
   - `waitAfterCmd`: `true`（終了後もターミナル維持 → ポーリングで検知）
2. `ipc.SplitTerminal(deckTermUUID, SplitRight, cfg)` で右 split 作成
3. `ipc.ResizeSplit(deckTermUUID, SplitLeft, N)` で deck を狭く
4. `ipc.FocusTerminal(deckTermUUID)` でフォーカスを deck に戻す
5. 返された UUID を `sess.GhosttyTerminalUUID` に保存
6. `sess.managed = true`, `sess.SetStatus(StatusIdle)`
7. persist + notifyChange
8. PTY 関連の処理（emulator 作成、handleOutput、watchProcess）はスキップ

**依存**: Step 1, 2, 3

### Step 5: セッション切替で右 split を入れ替え

**ファイル**: `internal/session/manager_ghostty.go`

```go
// SwitchGhosttySession は右 split のセッションを切り替える。
// 旧ターミナルを close し、新セッションの split を作成する。
// 完了済みセッションの場合は右 split を作成しない（deck TUI が全幅になる）。
func (m *Manager) SwitchGhosttySession(oldSessionID, newSessionID string) error {
    // 1. 旧セッションの GhosttyTerminalUUID があれば close
    // 2. 新セッションが managed かつアクティブなら:
    //    a. 既に GhosttyTerminalUUID があるか確認 (まだ生きてたら何もしない)
    //    b. なければ split 作成 + resize
    // 3. 完了済みなら右 split なし (deck TUI で JSONL を表示)
}
```

**依存**: Step 1, 3, 4

### Step 6: Ghostty ターミナルポーリング（終了検知）

**ファイル**: `internal/session/manager_ghostty.go`

```go
// StartGhosttyWatcher は Ghostty ターミナルの生存をポーリングで確認する。
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
    // 3. 消失していたら:
    //    a. sess.SetStatus(StatusCompleted)
    //    b. sess.GhosttyTerminalUUID = "" (クリア)
    //    c. persist + notifyChange
}
```

**依存**: Step 1, 3, 4

### Step 7: JSONL 更新による Running 検知

**ファイル**: `internal/session/manager_jsonl.go`

既存の `handleFileWrite` を拡張:

```go
func (m *Manager) handleFileWrite(claudeSessionID string) {
    // ... 既存の LastActivity 更新 ...

    // Ghostty モード: JSONL 更新 = Running の近似
    if m.ghosttyMode() {
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

**依存**: Step 2 (ghosttyMode 判定)

### Step 8: TUI の変更

**ファイル**: `internal/tui/model.go`, `internal/tui/view.go`, `internal/tui/keys.go`

#### レイアウト変更

Ghostty split モード時の deck TUI:
- 右ペイン（PTY ビューポート）を廃止
- 左ペイン（セッションリスト）をメインに
- 下部にメタ情報エリア（トークン、ツール、ステータス等）を表示
- 完了済みセッション選択時のみ JSONL 構造化ログを表示

```go
func (m Model) renderGhosttyLayout(sess *session.Session) string {
    // 上部: セッションリスト (既存)
    // 下部: メタ情報 or JSONL ログ
    //   - アクティブセッション: トークン、ツール、ステータスのサマリー
    //   - 完了セッション: JSONL 構造化ログ (既存の renderLogs)
}
```

#### キーバインド変更

```go
// Ghostty split モード
case "enter", "i", "t":
    // 右 split の Claude Code にフォーカスを移動
    m.manager.FocusGhosttyTerminal(selectedSessionID)

case "j", "k":
    // セッション選択変更 + 右 split 切替
    m.manager.SwitchGhosttySession(oldID, newID)

case "n":
    // セッション作成 → Ghostty split 作成
    // (Manager.CreateSession が内部で分岐)

case "x":
    // 右 split を close → ポーリングで Completed 検知
    m.manager.CloseGhosttyTerminal(selectedSessionID)
```

**依存**: Step 4, 5

## Phase 2: 自動セットアップ・UX 改善

### Step 9: 起動時の自動 split 構築

- `claude-deck` 起動時に自分が Ghostty 内で動作しているか検出
  - 環境変数 `GHOSTTY_RESOURCES_DIR` の有無で判定
- Ghostty 内なら AppleScript で自身のターミナルを特定し、`deckTermUUID` として保持
- 既にアクティブなセッションがあれば自動で右 split を作成

### Step 10: `x` キーでの右 split 閉鎖

- `ipc.CloseTerminal(uuid)` を呼ぶ
- ポーリングで Completed を検知 → TUI 更新

### Step 11: セッション名をターミナルタイトルに反映

- `SurfaceConfig.InitialInput` で OSC 0 設定を送信:
  ```
  printf '\033]0;session-name\007'
  ```
- または `command` にシェル経由で組み込み:
  ```
  /bin/sh -c 'printf "\033]0;session-name\007" && claude --agent session-name'
  ```

### Step 12: split 比率の設定対応

```toml
[ghostty]
mode = "split"
# deck ペインの幅比率 (0.0-1.0, デフォルト 0.3)
deck_ratio = 0.3
```

- System Events で window size を取得 → pixel 計算 → resize_split

## Phase 3: 拡張

### Step 13: 別ウィンドウ + タブ方式のサポート

Split 方式の代替として、別ウィンドウにタブを並べる方式もサポート:

```toml
[ghostty]
mode = "tabs"  # "split" | "tabs" | "window"
```

- `mode = "tabs"`: 別 Ghostty ウィンドウにセッション毎のタブを作成
- deck TUI は独立したターミナルで動作
- `goto_tab:N` でタブ切替

### Step 14: Linux D-Bus 対応

- Ghostty 側の D-Bus API 拡張待ち
- `GhosttyIPC` interface の D-Bus 実装 (`internal/ghostty/dbus.go`)

## ファイル一覧

| ファイル | 変更種別 | Step |
|---------|---------|------|
| `internal/ghostty/applescript.go` | **新規** | 1 |
| `internal/ghostty/applescript_other.go` | **新規** | 1 (Linux スタブ) |
| `internal/ghostty/ghostty.go` | 改修 | 1 (interface 抽出) |
| `internal/config/config.go` | 改修 | 2 |
| `internal/session/session.go` | 改修 | 3 |
| `internal/store/store.go` | 改修 | 3 |
| `internal/session/manager.go` | 改修 | 4 |
| `internal/session/manager_ghostty.go` | **新規** | 4, 5, 6 |
| `internal/session/manager_jsonl.go` | 改修 | 7 |
| `internal/tui/model.go` | 改修 | 8 |
| `internal/tui/view.go` | 改修 | 8 |
| `internal/tui/keys.go` | 改修 | 8 |

## テスト戦略

### ユニットテスト

- `GhosttyIPC` interface のモック実装でロジックテスト
- `SwitchGhosttySession`: close → split の順序確認
- `checkGhosttyTerminals`: UUID 消失時の Completed 遷移
- `handleFileWrite` の Running 遷移: 既存テストの拡張

### 統合テスト (手動)

- macOS + Ghostty 1.3+ 環境で E2E 確認
- TCC (Automation) 権限の初回承認フロー確認
- セッション作成 → 右 split 確認 → 操作 → 終了 → TUI 反映のフルサイクル
- セッション切替時のちらつき許容度の確認
- delay 値の調整（環境依存）

### ビルド

```bash
# macOS (Ghostty split モード含む)
GOEXPERIMENT=jsonv2 go build -o claude-deck ./cmd/claude-deck

# Linux (AppleScript は除外、GhosttyIPC はスタブ)
GOEXPERIMENT=jsonv2 go build -o claude-deck ./cmd/claude-deck
```

## 実装順序の根拠

1. **Step 1 (AppleScript ラッパー)** を最初に。他の全ステップがこれに依存
2. **Step 2-3 (config, session)** は独立して進められるデータ構造変更
3. **Step 4 (Manager 分岐)** がコア。PTY と Ghostty の分岐点
4. **Step 5 (セッション切替)** は split モードの核心。UX に直結
5. **Step 6-7 (終了検知, Running 検知)** はセッション管理の必須要素
6. **Step 8 (TUI)** は最後。Manager が動作確認できてから UI を接続

Phase 1 完了で MVP として使用可能。Phase 2 以降は使用感を見てから優先度を判断する。
