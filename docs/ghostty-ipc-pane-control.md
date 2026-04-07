# Ghostty IPC ペインコントロール Design Doc

## 概要

claude-deck の現行アーキテクチャでは、Claude Code セッションを仮想 PTY + VT エミュレータで管理し、Bubble Tea TUI 上にレンダリングしている。本提案では macOS 環境において Ghostty の AppleScript IPC を活用し、1つの Ghostty ウィンドウ内で deck TUI と Claude Code セッションを split ペインで並べて管理するモードを追加する。

## 動機

### 現行方式の制約

- **VT エミュレータの複雑さ**: PTY 出力を `charmbracelet/x/vt` に食わせて ANSI 解釈 → displayCache → TUI 表示。Ink レンダラーのターミナルクエリ応答（DA1/DA2/XTVERSION）も自前実装が必要
- **入力モードの制限**: PTY 入力モード (`Enter`/`i`) でキーバイト変換。マウス操作、クリップボード、IME は非対応
- **レンダリング品質**: Claude Code の Ink TUI を二重変換するため、スクロール、リサイズ、カラーテーマ等で微妙な差異が生じる

### Ghostty ネイティブ化で得られるもの

- **ネイティブレンダリング**: Ghostty が直接 Claude Code の出力を表示。マウス、スクロール、IME、フォント全対応
- **PTY/エミュレータレイヤー不要**: 仮想 PTY → VT エミュレータ → displayCache の変換パイプラインを除去
- **ターミナルクエリ不要**: Ghostty が DA1/DA2/XTVERSION に自動応答
- **ユーザー体験**: 通常のターミナルと同じ操作感で Claude Code を使える

## Ghostty AppleScript API (1.3.0, preview)

### Surface Configuration (sdef より)

```applescript
set cfg to new surface configuration
set command of cfg to "claude --agent my-session"
set initial working directory of cfg to "/path/to/repo"
set initial input of cfg to "some text" & return
set wait after command of cfg to true
set environment variables of cfg to {"CLAUDE_DECK_SESSION_ID=abc123"}
set font size of cfg to 13
set font family of cfg to "JetBrains Mono"
```

| プロパティ | 型 | 説明 |
|-----------|-----|------|
| `command` | text | シェルの代わりに実行するコマンド |
| `initial working directory` | text | 作業ディレクトリ |
| `initial input` | text | 起動後にターミナルに入力するテキスト |
| `wait after command` | boolean | コマンド終了後もターミナルを維持 |
| `environment variables` | list of text | `"KEY=value"` 形式 |
| `font size` | real | フォントサイズ |
| `font family` | text | フォントファミリー |

### 利用可能な操作

| 操作 | API | 備考 |
|------|-----|------|
| ウィンドウ作成 | `new window with configuration cfg` | surface config でコマンド・環境変数指定可 |
| タブ追加 | `new tab in win with configuration cfg` | 特定ウィンドウにタブ追加可能 |
| **分割** | `split term direction right with configuration cfg` | **返値で新ターミナル参照を取得可能** |
| タブ切替 | `perform action "goto_tab:N"` | 1-indexed |
| **フォーカス** | `focus terminal` | ターミナルにフォーカス切替 |
| リサイズ | `perform action "resize_split:right,N"` | N はピクセル単位 |
| ターミナル一覧 | `get every terminal` / `terminals of tab` | index, id(uuid), name, tty, working directory |
| タイトルで検索 | `every terminal whose name is "..."` | |
| UUID で特定 | `get terminal id "uuid"` | |
| テキスト送信 | `send text "..." to terminal` / `input text "..." to terminal` | |
| コンテンツ読取 | `get contents of terminal` | |
| ターミナル閉じ | `close terminal` | |
| フォーカス中取得 | `focused terminal of selected tab of front window` | |

### 操作間の delay

AppleScript 操作間に delay が必要（ghostty-workspace の知見）:
- タブ作成後: `delay 0.4`
- タブ選択後: `delay 0.15`
- 分割後: `delay 0.25`

delay なしだと後続操作が誤ったターミナルを対象にする。

### 制約

- **macOS 限定**: Linux は D-Bus だが機能が極めて限定的
- **Preview API**: 1.4 で破壊的変更の可能性あり
- **ペイン内プロセス差替**: 不可。close → 再 split で対応
- **初期分割比率**: 指定不可。常に ~50/50 → 作成後に resize

## 設計判断の背景

### 現行のセッション切替が高速な理由

現行の PTY モードでは、セッション切替は**メモリ上の displayCache を差し替えるだけ**で完了する:

```
j/k キー押下
  → updateSelected()
    → selectedID を新セッションに変更
    → syncLogViewport()
      → sess.GetPTYDisplayLines()  // displayCache を読むだけ
      → ptyViewport.SetContent()   // viewport にセット
```

各セッションが個別に `displayCache`（PTY 出力）と `JSONLLogEntries`（構造化ログ）を保持しているため、PTY プロセスの付け替えは一切発生しない。切替は瞬時（<1ms）。

### Ghostty ではペインのコンテンツ差替が不可能

Ghostty の設計上、1ターミナル=1プロセスであり、既存ペインのプロセスを差し替える API は存在しない。検討した代替手段:

| 方法 | 評価 |
|------|------|
| ペイン内のプロセスを入れ替え | **不可**。API なし |
| `send text Ctrl+C` → 新コマンド | 脆弱。プロセスが SIGINT を正しく処理する保証なし |
| close → 再 split | **採用**。唯一の信頼できる方法。~0.5s のちらつきあり |

この制約により、セッション切替のたびに右ペインの close → 再 split が必要になる。現行の瞬時切替（displayCache swap）と比べて ~0.5 秒の遅延が生じるが、実用上は許容範囲と判断。

### Linux サポートの見送り

Ghostty の Linux (GTK) 版は D-Bus 経由の IPC だが、現時点で `new-window` のみサポート。split、テキスト送信、ターミナル一覧取得等は未実装。Ghostty メンテナはプラットフォームネイティブ IPC の拡張を表明しているが時期未定。

**判断**: Linux は当面 PTY モード（現行方式）のみ。Ghostty split モードは macOS 限定とする。将来 D-Bus API が拡張されれば `GhosttyIPC` interface の D-Bus 実装を追加する。

## アーキテクチャ設計

### 方針: 1ウィンドウ Split モデル

1つの Ghostty ウィンドウ内で左ペイン（deck TUI）と右ペイン（Claude Code）を split で配置する。deck TUI はセッションリストに専念し、Claude Code はネイティブターミナルで直接操作する。

```
Ghostty ウィンドウ
┌──────────────────┬────────────────────────────────────┐
│ deck TUI         │ Claude Code (Ghostty ネイティブ)     │
│                  │                                      │
│ ● session-1      │ $ claude --agent session-1           │
│   session-2      │ > I'll help you with...              │
│ ◆ session-3      │                                      │
│                  │ ╭──────────────────────────╮         │
│ ──────────────── │ │ Edit: src/main.go        │         │
│ Token: 12.3k     │ ╰──────────────────────────╯         │
│ Tool: Edit       │                                      │
│ Status: Running  │                                      │
└──────────────────┴────────────────────────────────────┘
```

- **左ペイン**: deck TUI（Bubble Tea）。セッションリスト + 選択中セッションのメタ情報（トークン、ツール、ステータス）
- **右ペイン**: 選択中セッションの Claude Code ターミナル。Ghostty がネイティブ管理
- **セッション切替**: 左ペインで j/k → 右ペインを close → 再 split で新セッションを表示

### セッション切替のフロー

```
ユーザーが deck TUI で j/k でセッション切替
  → deck TUI が AppleScript 発行:
    1. close terminal id "<旧セッション UUID>"
       (左ペインが全幅に拡張)
    2. split deckTerm direction right with configuration newCfg
       (新セッションの Claude Code が右に出現)
    3. perform action "resize_split:left,N"
       (deck を狭く、Claude Code を広く)
    4. focus deckTerm
       (操作をdeckに戻す)
```

所要時間: close + split + resize で ~0.5秒程度（delay 含む）。一瞬のちらつきはあるが実用上問題なし。

### 右ペインがない状態

選択中のセッションにアクティブな Ghostty ターミナルがない場合（完了済みセッション等）:
- 右 split は作成しない
- deck TUI が全幅で表示される
- deck TUI 内で JSONL 構造化ログを右ペインに表示（既存の完了セッション表示と同じ）

### TUI レイアウトの変更

| 状態 | 左ペイン (deck TUI) | 右ペイン (Ghostty split) |
|------|-------------------|-----------------------|
| アクティブセッション選択中 | セッションリスト + メタ情報 | Claude Code ネイティブ |
| 完了セッション選択中 | セッションリスト + JSONL ログ (全幅) | なし |
| セッションなし | セッションリストのみ (全幅) | なし |

deck TUI のセッションリストは常に表示。メタ情報エリアには:
- トークン使用量
- 現在のツール実行状況
- セッションステータス
- 最終アクティビティ時刻

これらは全て JSONL + Hook から取得可能（PTY 非依存）。

### セッションライフサイクル管理の変更

#### PTY 依存だった機能の代替

| 機能 | 現行 (PTY) | 新方式 (Ghostty) | 代替手段 |
|------|-----------|-----------------|---------|
| Running 検知 | Braille スピナー検出 | 不可 | JSONL ファイル更新 (fsnotify) → Running 推定 |
| Idle 遷移 | スピナータイムアウト (3s) | 不可 | Hook `Stop` イベント (既存、変更不要) |
| WaitingApproval/Answer | Hook Notification | そのまま | 変更不要 |
| プロセス終了検知 | `proc.Done()` チャネル | 不可 | 後述「終了検知」参照 |
| PTY ログ表示 | emulator → displayCache | 不要 | Ghostty split で直接表示 |
| PTY 入力 | `proc.Write()` | 不要 | Ghostty split で直接入力 |
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

#### 新方式 (Ghostty Split モード)

```
Claude CLI → Ghostty 右ペイン (ネイティブ表示、ユーザーが直接操作)

Claude CLI → JSONL ファイル → fsnotify → handleFileWrite → Running 推定
                                                          → TUI メタ情報更新
Claude CLI → Hook JSONL    → fsnotify → handleHookEvent → Idle/Waiting 遷移
                                                         → SessionEnd 検知
AppleScript ポーリング → ターミナル UUID 消失 → Completed
```

### キーバインド変更

| キー | PTY モード | Ghostty Split モード |
|------|-----------|---------------------|
| `j`/`k` | セッション選択 | セッション選択 + 右 split 切替 |
| `Enter`/`i` | PTY 入力モード | 右 split (Claude Code) にフォーカス移動 |
| `Ctrl+D` / `Esc` | PTY 入力モード終了 | — (Ghostty のキーバインドで左ペインに戻る) |
| `t` | Ghostty ウィンドウ起動 | 右 split (Claude Code) にフォーカス移動 |
| `n` | セッション作成 (PTY 起動) | セッション作成 (Ghostty split 作成) |
| `r` | セッション再開 (PTY 起動) | セッション再開 (Ghostty split 作成) |
| `f` | セッションフォーク (PTY 起動) | セッションフォーク (Ghostty split 作成) |
| `x` | プロセス kill | `close terminal` で右 split 閉じ |

**deck TUI ↔ Claude Code 間のフォーカス移動**:
- deck → Claude Code: `Enter`/`i`/`t` で右ペインにフォーカス
- Claude Code → deck: Ghostty の split 移動キーバインド (`Ctrl+Shift+方向キー` 等、ユーザー設定依存)

### セッション ↔ ターミナルの紐付け

```
Session.ID (deck内部ID)
  ↕ CLAUDE_DECK_SESSION_ID 環境変数
ClaudeSessionID (Claude Code UUID)

Session.GhosttyTerminalUUID (新規フィールド)
  ↕ AppleScript terminal id
Ghostty terminal (右 split)
```

1. **作成時**: `split deckTerm direction right with configuration cfg` → 返値の terminal から UUID を即取得・保存
2. **復元時**: UUID でターミナルを検索。見つからなければ「右 split なし」状態
3. **ポーリング時**: UUID の存在確認でターミナル生存判定

## スコープ

### Phase 1 (MVP)

- Ghostty AppleScript ラッパー (`internal/ghostty/applescript.go`)
  - surface config: `command`, `initial working directory`, `environment variables`, `wait after command`
  - 操作: `split`, `close`, `focus`, `resize_split`, `ListTerminals`
- セッション作成/再開/フォークで右 split を作成
- `Enter`/`i`/`t` で右 split にフォーカス移動
- `j`/`k` でセッション切替時に右 split を入れ替え (close → 再 split)
- ターミナル UUID によるセッション追跡
- AppleScript ポーリングによる終了検知
- JSONL 更新による Running 推定
- deck TUI の左ペイン化（セッションリスト + メタ情報）

### Phase 2

- deck TUI 起動時に自動で Ghostty ウィンドウ + 左右 split を構築
- `x` キーでの右 split 閉鎖
- セッション名をターミナルタイトルに反映（`initial input` で OSC 設定 or `command` に組み込み）
- split 比率の設定対応 (`config.toml`)

### Phase 3

- 別ウィンドウ + タブ方式のサポート（split 方式との選択制）
- Linux D-Bus 対応（Ghostty 側の API 拡張待ち）

## リスクと軽減策

| リスク | 影響 | 軽減策 |
|-------|------|--------|
| AppleScript API が 1.4 で破壊的変更 | Ghostty モード動作不可 | ラッパー層で吸収。PTY モードをフォールバックとして維持 |
| Hook イベントが発火しないケース | 終了検知漏れ | AppleScript ポーリングでフォールバック |
| JSONL 更新が Running の正確な指標にならない | ステータス表示の遅延・不正確さ | Hook Stop との組み合わせで許容範囲内 |
| Ghostty 未インストール環境 | 起動不可 | PTY モードを維持し、設定で切替 |
| Automation 権限 (TCC) の未承認 | AppleScript 実行失敗 | 初回実行時にガイダンス表示 |
| セッション切替時のちらつき (~0.5s) | UX の微妙な劣化 | 実用上許容範囲。将来的に Ghostty API が改善されれば解消 |
| 操作間 delay の信頼性 | タイミング依存の不安定さ | delay を設定可能にし、環境に応じて調整 |

## 設定

```toml
[ghostty]
# "split" = 1ウィンドウ Split モード (macOS only, 推奨)
# "window" = 従来の新規ウィンドウ起動 (デフォルト)
mode = "split"
command = "ghostty"
```

`mode = "split"` の場合:
- deck TUI は Ghostty ウィンドウの左 split で動作
- セッション作成/再開/フォーク → 右 split に Claude Code を起動
- セッション切替 → 右 split を入れ替え
- PTY/エミュレータ不使用

`mode = "window"` (デフォルト):
- 従来通り PTY + TUI 表示
- `t` キーで Ghostty ウィンドウ起動
