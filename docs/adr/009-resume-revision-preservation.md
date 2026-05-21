# ADR 009: Resume 時の jj revision 復元

## ステータス

採択済み

## コンテキスト

`x` キーでセッションを終了すると `jj workspace forget` + ディレクトリ削除が実行される。
その後 `r` で resume すると `recreateWorkspace()` が `jj new trunk()` でワークスペースを作り直すため、
Claude Code が行った commit（jj の change）がリポジトリに残っているにもかかわらず、
trunk の最新から再スタートしてしまう問題があった。

**`jj workspace forget` の挙動（実験済み）:**

- `@`（working copy commit）に変更がある場合: `workspace forget` 後も regular commit として残る
- `@` が未変更（working copy に差分がない）場合: `workspace forget` で abandon される
- `@-`（working copy の親）: 常にリポジトリに残る

## 決定

Kill 時に `@` と `@-` の両方の `change_id` を Session メタデータとして保存し、
Resume 時に `jj edit` または `jj new <change_id>` で以前の revision から再開する。

**保存するフィールド（`session.Session`）:**

| フィールド | 内容 |
|---|---|
| `LastJJRevision` | Kill 時の `@` の change_id |
| `LastJJParentRevision` | Kill 時の `@-` の change_id（fallback） |

**Resume 時の revision 復元優先順（`CreateWorkspaceAt` 内で実行）:**

1. `jj edit <LastJJRevision>`（`@` — 変更があった場合は workspace forget 後も残る）
   - `jj new` ではなく `jj edit` を使うことで、変更中のコミットに直接戻れる（余分なコミットが増えない）
2. `jj new <LastJJParentRevision>`（`@-` — `@` が空で abandon された場合の fallback）
3. `jj new trunk()`（両フィールドが空、または上記が全て失敗した場合）

`change_id` は jj の安定識別子（rebase 後も変わらない）のため、リポジトリ更新後も正しく参照できる。

**GetWorkspaceRevisions と cleanupWorkspace の順序依存:**

`GetWorkspaceRevisions`（jj log 実行）は `cleanupWorkspace`（jj workspace forget + RemoveAll）より前に呼ぶ必要がある。workspace forget 後は @ の change_id が取得できない場合があるためである。

**SIGTERM 後の競合リスクについて:**

`Kill()` では `StopProcess`（SIGTERM 送信）直後に `GetWorkspaceRevisions`（jj log × 2）を呼ぶ。SIGTERM はプロセス終了を待機しないため、Claude Code が `jj commit` 等の操作中に `jj log` が実行される理論的な競合ウィンドウが存在する。

この設計を採用した理由: `cleanupWorkspace`（`jj workspace forget`）も同じ SIGTERM 後に実行されており、既存コードと同等のリスクプロファイルである。`GetWorkspaceRevisions` を追加してもリスクは拡大しない。プロセス終了を待ってから実行する設計（`watchProcess` 内での revision 保存等）も検討したが、Kill の非同期性を維持するためにこのトレードオフを受け入れた。

## 結果

**良い点:**
- `x` → `r` の一般的なユースケースで以前の作業ブランチから継続できる
- `change_id` を使うため rebase 耐性がある
- fallback 戦略により古いセッションデータ（フィールドなし）でも動作する

**悪い点:**
- Kill 時に外部コマンド（jj log × 2）を追加実行するため若干遅くなる
- `@` が空（未変更）の場合は `@-` から `jj new` で再開するため、そのコミット自体は復元されない（`--resume` により Claude が再実行され、JSONL のプロンプト履歴から文脈が復元されるため実用上は問題になりにくい）

**却下した代替案:**
- `@` のみを保存: `@` が empty の場合に abandon されると trunk() へ不当にフォールバックしてしまう
- `@-` のみを保存: `@` に変更がある場合（変更が残る）に1コミット古い状態から再開することになるが、`--resume` で Claude が再実行するため許容範囲内とも言える。最終的に `@` を優先することで変更中だった場合も正しく復元できるため両方保存する設計を選んだ
- `KilledRevision` 専用 struct: 2フィールドのペアという不変条件を型で表現でき、部分更新バグを防げる。ただし `{"killed_revision": {"at": "...", "parent": "..."}}` というネスト JSON になり、フラットフィールドとの後方互換性がなくなる（`omitempty` では吸収できない構造変更）ため見送り
