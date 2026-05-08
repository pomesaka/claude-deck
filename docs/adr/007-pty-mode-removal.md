# ADR-007: PTY モード（埋め込み仮想ターミナル）の削除

## ステータス

Accepted

## コンテキスト

claude-deck には2つのバックエンドモードが存在していた:

- **PTY (BackendPTY)**: claude-deck 自身がプロセスを擬似端末で起動し、VT100 エミュレータ（charmbracelet/x/vt）で出力を解釈してTUI内で描画する。スクロールバックや Braille スピナー検知によるステータス遷移を担う。
- **tmux (BackendTmux)**: Claude Code を tmux ウィンドウで起動し、TUI はメタデータと JSONL ログのみ表示する。

PTY モードは以下の問題を抱えていた:

1. **複雑性**: VT100 エミュレータ（charmbracelet/x/vt）のローカルパッチ（`_vt_local/`）が必要で、上流ライブラリの更新に追随できない状態だった。
2. **精度**: Braille スピナー検知はヒューリスティクスであり、ステータス遷移が不正確になるケースがあった。hooks ベースの遷移の方が信頼性が高い。
3. **未使用**: tmux + Ghostty native モードのみを使用しており、PTY モードは実運用で使われなくなっていた。
4. **表示品質**: TUI 内で VT100 をエミュレートするより、Ghostty + tmux で直接描画する方がネイティブの入力補完・色再現・フォントレンダリングが得られる。

## 決定

PTY バックエンドとその関連コードを全て削除し、tmux バックエンド一本に絞る。

削除対象:
- `internal/pty/`, `internal/session/backend_pty.go`
- `internal/session/process_supervisor.go`（PTY プロセスレジストリ）
- `internal/session/ptydisplay.go`, `emulator.go`（VT100 エミュレータラッパー）
- `internal/session/ptyfilter.go`, `spinner.go`（Chrome 検出・Braille スピナー検知）
- `_vt_local/`（charmbracelet/x/vt ローカルパッチ）
- `BackendMode` 型、`MaxScrollback` / `MaxLogLines` 設定フィールド
- `WriteInput` / `Resize` インターフェースメソッド
- TUI の PTY viewport・入力モード（`ptyInputActive`、`keyToBytes`、`handlePTYInputKey`）
- `HostingMode` 型（ADR-003 で導入されたが常に `HostExternal` のみになったため YAGNI 削除）

## 結果

**良い点:**
- コードベースが大幅に簡素化（~1000行削除）
- `charmbracelet/x/vt` と `creack/pty` 依存が除去
- ステータス遷移が hook 一本になり信頼性が向上
- テスト範囲が実際に使われるパスに集中する

**悪い点（許容済み）:**
- PTY モードへの復帰には再実装が必要
- 外部ターミナル（tmux + Ghostty）が必須環境になる

**却下した代替案:**
- PTY モードを残して無効化フラグを設ける → dead code が残りメンテナンス負荷が増す
- VT100 エミュレータを別ライブラリに換装 → 根本的な複雑性は変わらない
