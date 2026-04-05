# Ghostty IPC ペインコントロール 実装プラン

前提: [Design Doc](./ghostty-ipc-pane-control.md) を参照。

## Phase 0: SessionBackend interface の抽出（リファクタリング）

Ghostty ネイティブ実装の前に、TUI→Manager 間の PTY 依存操作を interface 越しに行うようリファクタリングする。これにより PTY 実装と Ghostty 実装を差し替え可能にする。

### 現状の TUI→Manager 依存関係

TUI が呼ぶ Manager メソッドは大きく2種類に分かれる:

**PTY 固有（backend interface に抽出する）:**

| メソッド | 呼び出し元 | 用途 |
|---------|-----------|------|
| `CreateSession(ctx, repoPath, workDir, withWS, cols, rows)` | keys.go (n キー) | セッション作成 + PTY 起動 |
| `ResumeSession(ctx, sessionID, cols, rows)` | keys.go (r キー) | セッション再開 + PTY 起動 |
| `ForkSession(ctx, sourceID, cols, rows)` | keys.go (f キー) | セッションフォーク + PTY 起動 |
| `Kill(sessionID)` | keys.go (x キー) | PTY プロセス終了 |
| `WriteToSession(sessionID, data)` | keys.go (PTY入力モード) | PTY stdin に書き込み |
| `ResizeSession(sessionID, cols, rows)` | view.go (viewport sync) | PTY + エミュレータ リサイズ |
| `HasActiveProcess(sessionID)` | keys.go, view.go | PTY プロセス生存確認 |

**汎用（Manager に残す、interface 不要）:**

| メソッド | 用途 |
|---------|------|
| `ListSessions()` | セッション一覧 |
| `GetSession(id)` | セッション取得 |
| `DeleteSession(id)` / `RemoveSession(id)` | セッション削除 |
| `StreamSession(id)` | JSONL ストリーミング開始 |
| `RefreshFromJSONL()` | メタデータ更新 |
| `SetOnChange(fn)` | UI 更新コールバック |

**Session メソッド（PTY 固有）:**

| メソッド | 用途 |
|---------|------|
| `GetPTYDisplayLines()` | PTY 出力の表示行 |
| `GetPTYCursorPosition()` | PTY カーソル位置 |

### Step 0-1: SessionBackend interface の定義

**ファイル**: `internal/session/backend.go` (新規)

```go
// SessionBackend は Claude Code セッションの起動・制御を抽象化する。
// PTY モード (ptyBackend) と Ghostty split モード (ghosttyBackend) の
// 2つの実装を切り替え可能にする。
type SessionBackend interface {
    // セッションライフサイクル
    StartSession(ctx context.Context, sess *Session, opts StartOpts) error
    ResumeSession(ctx context.Context, sess *Session, opts ResumeOpts) error
    ForkSession(ctx context.Context, sess *Session, source *Session, opts ForkOpts) error
    StopSession(sessionID string) error

    // セッション状態
    IsActive(sessionID string) bool

    // 表示（PTY モードのみ使用、Ghostty モードでは空/ゼロを返す）
    DisplayLines(sessionID string) []string
    CursorPosition(sessionID string) (x, y int)

    // 入力（PTY モードのみ使用、Ghostty モードでは focus 移動）
    WriteInput(sessionID string, data []byte) error

    // リサイズ（PTY モードのみ使用、Ghostty モードでは no-op）
    Resize(sessionID string, cols, rows int)

    // クリーンアップ
    Close() error
}

type StartOpts struct {
    WorkDir        string
    WithWorkspace  bool
    Cols, Rows     int
    AdditionalArgs []string
    Env            []string
}

type ResumeOpts struct {
    Cols, Rows int
}

type ForkOpts struct {
    Cols, Rows int
}
```

**設計意図:**
- `CreateSession` 等の Manager メソッドは「セッションオブジェクト作成 + ワークスペース作成」と「プロセス起動」を現在混在させている。interface では「プロセス起動」部分のみを抽出
- Manager は引き続きセッションオブジェクトの作成・永続化・ワークスペース管理を担当
- Backend はプロセスの起動・停止・表示・入力のみを担当

### Step 0-2: ptyBackend の実装（現行ロジックの移動）

**ファイル**: `internal/session/backend_pty.go` (新規)

現在 `manager.go` に散在する PTY 操作を `ptyBackend` 構造体に集約:

```go
type ptyBackend struct {
    mu        sync.RWMutex
    processes map[string]*pty.Process
    manager   *Manager // handleOutput, watchProcess 等のコールバック用
}

func (b *ptyBackend) StartSession(ctx context.Context, sess *Session, opts StartOpts) error {
    // 現在の CreateSession 内の pty.Start() 〜 watchProcess 部分を移動
}

func (b *ptyBackend) IsActive(sessionID string) bool {
    // 現在の HasActiveProcess() を移動
}

func (b *ptyBackend) DisplayLines(sessionID string) []string {
    // sess.GetPTYDisplayLines() を委譲
}

func (b *ptyBackend) WriteInput(sessionID string, data []byte) error {
    // 現在の WriteToSession() を移動
}

// ... 他メソッドも同様
```

**移動元 → 移動先:**

| 現在の場所 | 移動先 |
|-----------|--------|
| `Manager.processes` map | `ptyBackend.processes` |
| `CreateSession` 内の `pty.Start()` 〜 `watchProcess` | `ptyBackend.StartSession()` |
| `ResumeSession` 内の `pty.Start()` 〜 `watchProcess` | `ptyBackend.ResumeSession()` |
| `ForkSession` 内の `pty.Start()` 〜 `watchProcess` | `ptyBackend.ForkSession()` |
| `Kill()` | `ptyBackend.StopSession()` |
| `WriteToSession()` | `ptyBackend.WriteInput()` |
| `ResizeSession()` | `ptyBackend.Resize()` |
| `HasActiveProcess()` | `ptyBackend.IsActive()` |
| `handleOutput()` | `ptyBackend` 内 (Manager への callback で通知) |
| `watchProcess()` | `ptyBackend` 内 (Manager への callback で通知) |

### Step 0-3: Manager のリファクタリング

**ファイル**: `internal/session/manager.go`

```go
type Manager struct {
    mu       sync.RWMutex
    sessions map[string]*Session
    backend  SessionBackend  // ← 新規: PTY or Ghostty
    store    *store.Store
    // ... processes フィールドを削除（backend に移動）
}

func (m *Manager) CreateSession(ctx context.Context, repoPath, workDir string, 
    withWorkspace bool, cols, rows int) (*Session, error) {
    
    // 1. セッションオブジェクト作成（変更なし）
    // 2. ワークスペース作成（変更なし）
    // 3. backend.StartSession() ← PTY 固有部分を委譲
    // 4. persist + notifyChange（変更なし）
}

// HasActiveProcess は backend に委譲
func (m *Manager) HasActiveProcess(sessionID string) bool {
    return m.backend.IsActive(sessionID)
}

// WriteToSession は backend に委譲
func (m *Manager) WriteToSession(sessionID string, data []byte) error {
    return m.backend.WriteInput(sessionID, data)
}

// ResizeSession は backend に委譲
func (m *Manager) ResizeSession(sessionID string, cols, rows int) {
    m.backend.Resize(sessionID, cols, rows)
}
```

### Step 0-4: TUI の変更（最小限）

**ファイル**: `internal/tui/view.go`

TUI 側の変更は最小限。Manager の公開 API は変わらないため:
- `HasActiveProcess()` → そのまま（内部で backend に委譲）
- `WriteToSession()` → そのまま
- `ResizeSession()` → そのまま
- `GetPTYDisplayLines()` / `GetPTYCursorPosition()` → Session メソッドは変更なし

唯一の変更: `HasActiveProcess` が false のとき、Ghostty モードでは「アクティブだがPTY表示なし」のケースが生じる。これは Phase 1 で対応。

### Step 0 の検証

```bash
# リファクタリング後、既存の全テストが通ること
GOEXPERIMENT=jsonv2 go test ./...

# 手動確認: PTY モードの動作が一切変わらないこと
```

**Step 0 完了時点では機能変更ゼロ。** 内部構造のみの変更で、全テストがパスすること。

---

## Phase 1: Ghostty Split モード MVP

### Step 1: AppleScript ラッパー

**ファイル**: `internal/ghostty/applescript.go` (新規, `//go:build darwin`)

```go
// GhosttyIPC は Ghostty の AppleScript IPC をラップする interface。
type GhosttyIPC interface {
    CreateWindow(cfg SurfaceConfig) (terminalUUID string, err error)
    SplitTerminal(targetUUID string, direction SplitDirection, cfg SurfaceConfig) (newUUID string, err error)
    CloseTerminal(uuid string) error
    FocusTerminal(uuid string) error
    ResizeSplit(terminalUUID string, direction SplitDirection, pixels int) error
    ListTerminals() ([]TerminalInfo, error)
    TerminalExists(uuid string) (bool, error)
}

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
    Name    string
    TTY     string
    WorkDir string
}
```

実装方針:
- `os/exec` で `osascript -e '...'` を実行
- 操作間に delay を挿入（split 後 250ms、タブ作成後 400ms）
- ビルドタグ `//go:build darwin` で macOS 限定
- Linux 用スタブ (`applescript_other.go`, `//go:build !darwin`) はエラーを返す

**依存**: なし

### Step 2: config に Ghostty モード追加

**ファイル**: `internal/config/config.go`

```go
type GhosttyConfig struct {
    Command string `toml:"command"`
    Mode    string `toml:"mode"` // "split" or "window" (default)
}
```

**依存**: なし

### Step 3: Session に Ghostty メタデータ追加

**ファイル**: `internal/session/session.go`, `internal/store/store.go`

```go
type Session struct {
    // ... 既存フィールド ...
    GhosttyTerminalUUID string // Ghostty ターミナルの UUID
}
```

**依存**: なし

### Step 4: ghosttyBackend の実装

**ファイル**: `internal/session/backend_ghostty.go` (新規, `//go:build darwin`)

`SessionBackend` interface の Ghostty 実装:

```go
type ghosttyBackend struct {
    ipc          ghostty.GhosttyIPC
    deckTermUUID string             // 左ペイン (deck TUI) の UUID
    manager      *Manager           // コールバック用
}

func (b *ghosttyBackend) StartSession(ctx context.Context, sess *Session, opts StartOpts) error {
    cfg := ghostty.SurfaceConfig{
        Command:      buildClaudeCommand(sess, opts),
        WorkDir:      opts.WorkDir,
        Env:          opts.Env,
        WaitAfterCmd: true,
    }
    uuid, err := b.ipc.SplitTerminal(b.deckTermUUID, ghostty.SplitRight, cfg)
    if err != nil {
        return err
    }
    sess.SetGhosttyUUID(uuid)
    // resize: deck を狭く
    b.ipc.ResizeSplit(b.deckTermUUID, ghostty.SplitLeft, deckWidthPixels)
    // focus を deck に戻す
    b.ipc.FocusTerminal(b.deckTermUUID)
    return nil
}

func (b *ghosttyBackend) StopSession(sessionID string) error {
    sess := b.manager.GetSession(sessionID)
    uuid := sess.GetGhosttyUUID()
    return b.ipc.CloseTerminal(uuid)
}

func (b *ghosttyBackend) IsActive(sessionID string) bool {
    sess := b.manager.GetSession(sessionID)
    uuid := sess.GetGhosttyUUID()
    if uuid == "" { return false }
    exists, _ := b.ipc.TerminalExists(uuid)
    return exists
}

// PTY 表示系は Ghostty モードでは空を返す
func (b *ghosttyBackend) DisplayLines(sessionID string) []string { return nil }
func (b *ghosttyBackend) CursorPosition(sessionID string) (int, int) { return 0, 0 }

// 入力は Ghostty ターミナルにフォーカスを移す
func (b *ghosttyBackend) WriteInput(sessionID string, data []byte) error {
    sess := b.manager.GetSession(sessionID)
    uuid := sess.GetGhosttyUUID()
    return b.ipc.FocusTerminal(uuid)
}

// リサイズは Ghostty が自動処理するため no-op
func (b *ghosttyBackend) Resize(sessionID string, cols, rows int) {}
```

**依存**: Step 0, 1, 3

### Step 5: セッション切替で右 split を入れ替え

**ファイル**: `internal/session/backend_ghostty.go`

```go
// SwitchSession は右 split のセッションを切り替える。
func (b *ghosttyBackend) SwitchSession(oldSessionID, newSessionID string) error {
    // 1. 旧セッションの GhosttyTerminalUUID があれば close
    // 2. 新セッションがアクティブなら split 作成 + resize
    // 3. 完了済みなら右 split なし
}
```

TUI の `j`/`k` キーハンドラから呼び出す。

**依存**: Step 4

### Step 6: Ghostty ターミナルポーリング（終了検知）

**ファイル**: `internal/session/backend_ghostty.go`

```go
func (b *ghosttyBackend) StartWatcher(ctx context.Context) {
    go func() {
        ticker := time.NewTicker(5 * time.Second)
        defer ticker.Stop()
        for {
            select {
            case <-ctx.Done():
                return
            case <-ticker.C:
                b.checkTerminals()
            }
        }
    }()
}
```

**依存**: Step 4

### Step 7: JSONL 更新による Running 検知

**ファイル**: `internal/session/manager_jsonl.go`

既存の `handleFileWrite` を拡張。Ghostty モード時、JSONL 更新で Idle → Running 遷移。

**依存**: Step 0 (ghosttyMode 判定)

### Step 8: TUI の変更

**ファイル**: `internal/tui/model.go`, `internal/tui/view.go`, `internal/tui/keys.go`

- Ghostty モード時の左ペイン化（セッションリスト + メタ情報）
- `Enter`/`i`/`t` でフォーカス移動
- `j`/`k` でセッション切替 + 右 split 入れ替え
- 完了セッションは deck TUI 内で JSONL 表示

**依存**: Step 4, 5

---

## Phase 2: 自動セットアップ・UX 改善

### Step 9: 起動時の自動 split 構築

- Ghostty 内かを `GHOSTTY_RESOURCES_DIR` で判定
- 自身のターミナル UUID を特定し `deckTermUUID` として保持
- アクティブセッションがあれば自動で右 split 作成

### Step 10: セッション名をターミナルタイトルに反映

- `SurfaceConfig.Command` にシェル経由で OSC 0 設定を組み込み

### Step 11: split 比率の設定対応

```toml
[ghostty]
deck_ratio = 0.3
```

### Step 12: `x` キーでの右 split 閉鎖

---

## Phase 3: 拡張

### Step 13: 別ウィンドウ + タブ方式のサポート

### Step 14: Linux D-Bus 対応

---

## ファイル一覧

| ファイル | 変更種別 | Step |
|---------|---------|------|
| `internal/session/backend.go` | **新規** | 0-1 |
| `internal/session/backend_pty.go` | **新規** | 0-2 |
| `internal/session/manager.go` | 改修 | 0-3 |
| `internal/session/manager_ghostty.go` | 削除予定 | 0-3 (backend_ghostty.go に統合) |
| `internal/ghostty/applescript.go` | **新規** | 1 |
| `internal/ghostty/applescript_other.go` | **新規** | 1 (Linux スタブ) |
| `internal/config/config.go` | 改修 | 2 |
| `internal/session/session.go` | 改修 | 3 |
| `internal/store/store.go` | 改修 | 3 |
| `internal/session/backend_ghostty.go` | **新規** | 4 |
| `internal/session/manager_jsonl.go` | 改修 | 7 |
| `internal/tui/model.go` | 改修 | 8 |
| `internal/tui/view.go` | 改修 | 8 |
| `internal/tui/keys.go` | 改修 | 8 |

## テスト戦略

### Phase 0 (リファクタリング)

```bash
# 既存テストが全てパスすること（機能変更ゼロ）
GOEXPERIMENT=jsonv2 go test ./...
```

- `ptyBackend` はテスト用モックが不要（既存テストがそのまま動く）
- `SessionBackend` interface のモック実装を用意し、Manager のユニットテストを追加

### Phase 1 (Ghostty MVP)

- `GhosttyIPC` interface のモック実装でロジックテスト
- `ghosttyBackend.StartSession`: split 呼び出し順序の確認
- `ghosttyBackend.SwitchSession`: close → split の順序確認
- `checkTerminals`: UUID 消失時の Completed 遷移

### 統合テスト (手動)

- macOS + Ghostty 1.3+ で E2E 確認
- セッション作成 → 右 split → 操作 → 終了 → TUI 反映のフルサイクル
- セッション切替時のちらつき許容度の確認

## 実装順序の根拠

1. **Phase 0** を最初に。interface 境界を確立してからでないと、Ghostty 実装が Manager のあちこちに侵食する
2. Phase 0 は機能変更ゼロなので安全にマージ可能
3. Phase 1 の Step 1-3 は並行作業可能（独立したファイル追加）
4. Step 4 (ghosttyBackend) が Phase 1 のコア
5. Step 5-8 は Step 4 の上に積み上げ

Phase 0 + Phase 1 完了で MVP として使用可能。
