# ADR-003: HostingMode と DisplayChannel の導入

## ステータス

Accepted

## コンテキスト

detail pane を外部ターミナル (Ghostty+tmux) に分離する計画において、2つの概念が暗黙的だった:

1. **PTY の所有者は誰か** — claude-deck が PTY を管理する (埋め込み) のか、外部ターミナルが管理するのか
2. **detail pane に何を表示するか** — PTY のリアルタイム画面か、JSONL のログか、何も表示しないか

TUI コードは `manager.HasActiveProcess(id)` という bool で両方を判断していた。これは「プロセスが生きている → PTY 表示」「死んでいる → JSONL 表示」の二択しか表現できず、外部ホスト型セッション (プロセスは生きているが PTY 表示はしない) に対応できなかった。

## 決定

**2つの直交するドメイン概念を型として明示化する。**

### HostingMode (根本属性)

セッション起動時に決定し、一生変わらない:
- `HostEmbedded` — claude-deck が PTY を所有
- `HostExternal` — 外部ターミナルが PTY を所有

### DisplayChannel (導出投影)

HostingMode と managed フラグから導出する (永続化しない):
- `DisplayPTY` — Embedded + managed
- `DisplayJSONL` — !managed (どちらの HostingMode でも)
- `DisplayNone` — External + managed

TUI は `snap.Display` で分岐し、`HasActiveProcess` に依存しない。

## 結果

**良い点**:
- TUI から `HasActiveProcess` 依存が完全に除去され、表示ロジックがドメイン概念で駆動される
- 3値分岐により外部ホスト型セッションの DisplayNone ケースが自然に表現される
- `detailPaneLayout`, `syncViewportContent`, キーバインドのビューポート操作がすべて DisplayChannel ベースに統一

**悪い点**:
- `selectedDisplayChannel()` ヘルパーが Snapshot を取得するため、高頻度呼び出し時にわずかなオーバーヘッド
- DisplayChannel の導出ロジック (`displayChannelLocked()`) が HostingMode × managed の組み合わせを知る必要がある

**トレードオフ**:
- DisplayChannel は永続化しない投影なので、Session 状態の復元時に自動的に正しい値が導出される。永続化すると Store と導出ロジックの不整合リスクが生じるため、投影として留める
