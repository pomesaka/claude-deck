# TODO

## Notification Hooks 連携

現在の idle/running 検出は画面更新の有無（2秒閾値）に基づいている。
より正確なステータス検出のために、Claude Code の Notification Hooks を活用する。

### 実装方針

1. claude-deck 起動時に `~/.claude/settings.json` に Notification Hook を登録
   - `hooks.notification` に claude-deck 用のフック定義を追加
2. フックが発火するイベント:
   - `permission_prompt`: パーミッション承認待ち
   - `elicitation_dialog`: ユーザーへの質問待ち
   - `idle_prompt`: タスク完了後の入力待ち
3. イベント発火時にファイルに書き込み、claude-deck が fsnotify で監視
4. セッション終了時にフックをクリーンアップ
