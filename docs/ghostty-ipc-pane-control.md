# Ghostty IPC ペインコントロール Design Doc

## 概要

claude-deck の現行アーキテクチャでは、Claude Code セッションを仮想 PTY + VT エミュレータで管理し、Bubble Tea TUI 上にレンダリングしている。本提案では macOS 環境において Ghostty の AppleScript IPC を活用し、各セッションを Ghostty のネイティブタブとして管理するモードを追加する。

## 動機

### 現行方式の制約

- **VT エミュレータの複雑さ**: PTY 出力を `charmbracelet/x/vt` に食わせて ANSI 解釈 → displayCache → TUI 表示。Ink レンダラーのターミナルクエリ応答（DA1/DA2/XTVERSION）も自前実装が必要
- **入力モードの制限**: PTY 入力モード (`Enter`/`i`) でキーバイト変換。マウス操作、クリップボード、IME は非対応
- **レンダリング品質**: Claude Code の Ink TUI を二重変換するため、スクロール、リサイズ、カラーテーマ等で微妙な差異が生じる

### Ghostty ネイティブ化で得られるもの

- **ネイティブレンダリング**: Ghostty が直接 Claude Code の出力を表示。マウス、スクロール、IME、フォント全対応
- **PTY/エミュレータレイヤー不要**: 仮想 PTY → VT エミュレータ → displayCache の変換パイプラインを除去
- **ターミナルクエリ不要**: Ghostty が DA1/DA2/XTVERSION に自動応答
- **ユーザー体験**: 通常のターミナルタブと同じ操作感

## Ghostty AppleScript API (1.3.0, preview)

### 利用可能な操作

| 操作 | API | 備考 |
|------|-----|------|
| ウィンドウ作成 | `new window with configuration cfg` | surface configuration で環境変数・作業ディレクトリ指定可 |
| タブ追加 | `new tab in win with configuration cfg` | 特定ウィンドウにタブ追加可能 |
| タブ切替 | `perform action "goto_tab:N"` | 1-indexed |
| ターミナル一覧 | `get every terminal` | index, uuid, title, tty, working directory, contents |
| タイトルで検索 | `every terminal whose title is "..."` | OSC 0/2 でタイトル設定可能 |
| UUID で特定 | `get terminal id "uuid"` | |
| TTY パス取得 | `get tty of terminal N` | |
| テキスト送信 | `send text "..." to terminal N` | |
| コンテンツ読取 | `get contents of terminal N` | |
| ターミナル閉じ | `close terminal N` | |
| 分割 | `split primaryTerm direction right with configuration cfg` | |
| フォーカス中取得 | `focused terminal of selected tab of front window` | |

### Surface Configuration

```applescript
set cfg to new surface configuration
set initial working directory of cfg to "/path/to/repo"
set font size of cfg to 13
set environment variables of cfg to {"CLAUDE_DECK_SESSION_ID=abc123", "KEY=val"}
```

### 制約

- **macOS 限定**: Linux は D-Bus だが `new-window` のみ
- **Preview API**: 1.4 で破壊的変更の可能性あり
- **タブタイトル設定**: AppleScript から直接設定不可。OSC エスケープシーケンスで設定する必要あり
- **ペイン内プロセス差替**: 不可。close → new で再作成のみ

## アーキテクチャ設計

### 方針: ハイブリッドモデル

TUI を廃止するのではなく、TUI はダッシュボード（セッション一覧・ステータス・JSONL ログ）に専念し、実際の Claude Code 操作は Ghostty タブで行う。

```
┌──────────────────────────────────────────────────┐
│ claude-deck TUI (ダッシュボード)                    │
│ ┌────────────────┐ ┌──────────────────────────┐  │
│ │ session-1 ● Run│ │ JSONL 構造化ログ          │  │
│ │ session-2 ◆ Wait│ │ Token: 12.3k / 45.6k    │  │
│ │ session-3 ○ Idle│ │ Tool: Edit (file.go)     │  │
│ │ session-4 ✓ Done│ │ ...                      │  │
│ └────────────────┘ └──────────────────────────┘  │
└──────────────────────────────────────────────────┘
           ↕ AppleScript IPC
┌──────────────────────────────────────────────────┐
│ Ghostty ウィンドウ (claude-deck 専用)               │
│ [session-1] [session-2] [session-3]  ← タブ       │
│                                                    │
│  $ claude --session session-1                      │
│  > ネイティブ Claude Code TUI                      │
│                                                    │
└──────────────────────────────────────────────────┘
```

### セッションライフサイクル管理の変更

#### PTY 依存だった機能の代替

| 機能 | 現行 (PTY) | 新方式 (Ghostty) | 代替手段 |
|------|-----------|-----------------|---------|
| Running 検知 | Braille スピナー検出 | 不可 | JSONL ファイル更新 (fsnotify) → Running 推定 |
| Idle 遷移 | スピナータイムアウト (3s) | 不可 | Hook `Stop` イベント (既存、変更不要) |
| WaitingApproval/Answer | Hook Notification | そのまま | 変更不要 |
| プロセス終了検知 | `proc.Done()` チャネル | 不可 | 後述「終了検知」参照 |
| PTY ログ表示 | emulator → displayCache | 不要 | Ghostty タブで直接表示 |
| PTY 入力 | `proc.Write()` | 不要 | Ghostty タブで直接入力 |
| ターミナルクエリ応答 | 自前 DA1/DA2 応答 | 不要 | Ghostty が自動応答 |
| リサイズ | `proc.Resize()` | 不要 | Ghostty が自動処理 |

#### 終了検知の設計

複数の検知手段を組み合わせる:

1. **Hook `SessionEnd` イベント (reason: "logout")**
   - Claude Code が正常終了 (`exit`、`/logout`、Ctrl+C) した場合に発火
   - `ClaudeDeckSessionID` でセッション特定可能
   - 最も信頼性が高い

2. **AppleScript ポーリング (フォールバック)**
   - 定期的に `get every terminal` でターミナル一覧を取得
   - 管理中のターミナル UUID が消えていたら → Completed に遷移
   - タブを直接閉じた場合（kill）に対応
   - ポーリング間隔: 5秒（既存の metadataTickMsg と同じ）

3. **TTY 生存確認 (補助)**
   - `get tty of terminal N` で取得した TTY パスの存在確認
   - `os.Stat` で確認可能だが、ポーリング必要

**推奨**: 方法1 + 方法2 の併用。Hook で正常終了をキャッチし、ポーリングでタブ強制閉鎖をカバー。

#### Running 検知の設計

PTY スピナー検知の代替:

```
JSONL ファイルに Write イベント発生
  → 既存の handleFileWrite() で LastActivity 更新
  → Status が Idle なら Running に遷移

Hook Stop イベント到着
  → Running → Idle に遷移 (既存ロジック、変更不要)
```

既存の `StartFileWatcher` → `handleFileWrite` の仕組みをそのまま活用。JSONL への書き込みは Claude Code がツール実行・テキスト出力するたびに発生するため、Running の近似指標として十分。

### データフロー比較

#### 現行 (PTY モード)

```
Claude CLI → PTY → readLoop → handleOutput → emulator.Write → displayCache → TUI ptyViewport
                                    ↓
                              spinner検知 → Running
```

#### 新方式 (Ghostty モード)

```
Claude CLI → Ghostty タブ (ネイティブ表示)

Claude CLI → JSONL ファイル → fsnotify → handleFileWrite → Running 推定
Claude CLI → Hook JSONL    → fsnotify → handleHookEvent → Idle/Waiting 遷移
                                                         → SessionEnd 検知
AppleScript ポーリング → ターミナル UUID 消失 → Completed
```

### TUI の変更

#### 右ペイン (詳細ペイン)

| セッション状態 | PTY モード | Ghostty モード |
|-------------|-----------|--------------|
| アクティブ (Running/Idle/Waiting) | PTY ビューポート | JSONL 構造化ログ + ステータス表示 |
| 完了 (Completed) | JSONL 構造化ログ | JSONL 構造化ログ (変更なし) |

Ghostty モードではアクティブセッションも JSONL ログ表示。PTY ビューポートは不要（Ghostty タブで見る）。

#### キーバインド変更

| キー | PTY モード | Ghostty モード |
|------|-----------|--------------|
| `Enter`/`i` | PTY 入力モード | Ghostty タブにフォーカス切替 (`goto_tab:N`) |
| `t` | Ghostty ウィンドウ起動 | Ghostty タブにフォーカス切替 (同上) |
| `n` | セッション作成 (PTY 起動) | セッション作成 (Ghostty タブ作成) |
| `r` | セッション再開 (PTY 起動) | セッション再開 (Ghostty タブ作成) |
| `f` | セッションフォーク (PTY 起動) | セッションフォーク (Ghostty タブ作成) |
| `x` | プロセス kill | `close terminal` or `send text Ctrl+C` |

### セッション ↔ タブの紐付け

```
Session.ID (deck内部ID)
  ↕ CLAUDE_DECK_SESSION_ID 環境変数
ClaudeSessionID (Claude Code UUID)

Session.GhosttyTerminalUUID (新規フィールド)
  ↕ AppleScript terminal id
Ghostty terminal
```

1. **作成時**: `new tab in win with configuration cfg` → 返値の terminal オブジェクトから UUID を取得・保存
2. **復元時**: UUID でターミナルを検索。見つからなければ「タブなし」状態
3. **ポーリング時**: UUID の存在確認でタブ生存判定

## スコープ

### Phase 1 (MVP)

- Ghostty AppleScript ラッパー (`internal/ghostty/applescript.go`)
- セッション作成/再開/フォークで Ghostty タブを作成
- `Enter`/`i`/`t` で Ghostty タブにフォーカス切替
- ターミナル UUID によるタブ追跡
- AppleScript ポーリングによる終了検知
- JSONL 更新による Running 推定
- 右ペインを JSONL ログ表示に統一

### Phase 2

- claude-deck 専用 Ghostty ウィンドウの管理（起動時に作成、タブをそこに集約）
- TUI 上でのセッション切替と Ghostty タブ切替の連動
- `x` キーでの Ghostty タブ閉鎖
- セッション名をタブタイトルに反映（OSC エスケープシーケンス経由）

### Phase 3

- Ghostty 分割レイアウト対応（TUI + Claude Code を1ウィンドウに）
- Linux D-Bus 対応（Ghostty 側の API 拡張待ち）

## リスクと軽減策

| リスク | 影響 | 軽減策 |
|-------|------|--------|
| AppleScript API が 1.4 で破壊的変更 | Ghostty モード動作不可 | ラッパー層で吸収。PTY モードをフォールバックとして維持 |
| Hook イベントが発火しないケース | 終了検知漏れ | AppleScript ポーリングでフォールバック |
| JSONL 更新が Running の正確な指標にならない | ステータス表示の遅延・不正確さ | Hook Stop との組み合わせで許容範囲内 |
| Ghostty 未インストール環境 | 起動不可 | PTY モードを維持し、設定で切替 |
| Automation 権限 (TCC) の未承認 | AppleScript 実行失敗 | 初回実行時にガイダンス表示 |

## 設定

```toml
[ghostty]
# "native" = Ghostty タブモード (macOS only)
# "window" = 従来の新規ウィンドウ起動 (デフォルト)
mode = "native"
command = "ghostty"
```

`mode = "native"` の場合:
- セッション作成/再開/フォーク → Ghostty タブ作成
- PTY/エミュレータ不使用
- TUI 右ペインは常に JSONL ログ表示

`mode = "window"` (デフォルト):
- 従来通り PTY + TUI 表示
- `t` キーで Ghostty ウィンドウ起動
