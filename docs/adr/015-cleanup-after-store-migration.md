# ADR-015: store 移行後の整理

## ステータス

Accepted（2026-10-09）

## コンテキスト

ADR-011 で store を deck セッションの信頼できる唯一の情報源にした。移行そのものは終わっていたが、全体のレビューで次の点が見つかった。

- 前の設計の部品が、呼び出し元を失ったまま残っていた。メインプロセスの JSONL ストリーム（ログを描くのは preview サブプロセスで、メインが貯めたログは誰も読まない）、`Session` のメモリ上の遷移（`SetStatus`、`canTransitionTo`）、`process` のポインタと `SessionPhase`
- `Status` を `transitions.go` の外で書く経路が 3 つあり、「遷移は純関数」という説明と合っていなかった
- 終了済みのセッションをもう一度 close すると、resume 用に保存した revision（ADR 009）が消えた
- Claude / Codex の分岐が `usage` の 17 か所にあった
- ワークスペースの作成・削除・GC が `Manager` のメソッドで、`gc` のためだけに tmux セッションを作っていた
- deck-status プラグイン（TypeScript）と `claude-deck hook`（Go）の引数の取り決めを確かめるテストが無かった。プラグインは失敗を握りつぶすので、ずれてもエラーが出ない

## 決定

1. **プロセスの有無は `Status` から導く**。`Session.process` と `Snapshot.HasProcess`、`SessionPhase` をやめ、`DisplayChannel` と `IsProcessAlive` は「終了でも外部でもない」で決める。プロセスを起動・終了させるのは TUI 以外のプロセスのこともあり、TUI はそれを store の `Status` でしか知り得ないので、別に持つ状態は `Status` の写しにしかならない
2. **`Status` を書くのは `transitions.go` だけ**。`abortResume`、`markAdopted`、`reviveForLiveWindow` を足した。`rg '\.Status = ' internal/session` で確かめられる
3. **保存した revision は、ワークスペースを消した close だけが書き換える**（`recordWorkspaceRemoved`）。ワークスペースの無い行の close は触らない
4. **メインプロセスは JSONL のログを読まない**。`StreamSession` と `Session.rt` を消した。ログを読むのは preview サブプロセスだけ
5. **transcript の形式ごとの違いは `usage.format` に集める**（ADR-014 の追記）
6. **ワークスペースの操作は `session.workspaces` に分ける**。セッションの状態を持たず、判断に要る store の行は呼び出し側が渡す。`CollectGarbage` は `Manager` も tmux も使わない
7. **hook の取り決めはテストで固定する**（`cmd/claude-deck/hook_contract_test.go`）。`register.ts` を読み、プラグインが実行する引数を `parseHookArgs` に通す
8. **JSONL は行ごとに読む**（`usage.scanLines`）。`jsontext.Decoder` で値を続けて読むと、1 つの値のデコードに失敗した後はデコーダーが同じ位置でエラーを返し続ける（Go 1.26.0 で確認）。`toolUseResult` が文字列の行でこれが起き、`ReadSessionInfoByID` が終わらなくなっていた。`toolUseResult` は生の値で受けて、オブジェクトのときだけ読む

読む側の無い設定（`[keybinds]` の 8 項目、`[tmux] enabled`、`[theme] border`）と、claude-deck が読まない旧プラグイン（リポジトリ直下の `plugin/`）も消した。

## 結果

### 良い点

- `Session` のロックは `Session.mu` の 1 つになり、ロックの規則は「`Manager.mu` → `Session.mu` の一方向」だけになった
- 読み取りを 1 つ足すとき、`format` に 1 メソッド足せば両方の実装でコンパイルが通らなくなり、片方の書き忘れに気づける
- `gc` が tmux に触れない

### 悪い点

- `claudeFormat.runtimeActivity` は何も返さないメソッドになる。Claude Code の状態は hook が store に書くので、JSONL からは読まない
- 契約テストは `register.ts` を正規表現で読む。`deck($, [...])` の書き方を変えるとテストも直すことになる

### 却下した代替案

- **`StatusUnmanaged` を `Status` から外し、外部セッションかどうかを別のフィールドにする**: 遷移関数の除外条件が消えるが、TUI・store・discovery の全体に及ぶ。今回の整理とは別に判断する
- **`KeybindConfig` を全部消す**: `fork` と `open_term` は実際に効いているので残した
- **契約テストを TypeScript 側に置く**: プラグインには `package.json` もテストの実行環境も無い。Go のテストは `go test ./...` で必ず走る
