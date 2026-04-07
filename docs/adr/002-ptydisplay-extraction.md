# ADR-002: PTYDisplay の Session からの分離

## ステータス

Accepted

## コンテキスト

Session 構造体がドメインフィールド (Status, SessionChain, TokenUsage) と PTY レンダリングインフラ (vt.Emulator, displayCache, scrollback, 8つの atomic カーソルフィールド) を混在して保持していた。

外部ターミナル (Ghostty+tmux) に detail pane を分離する計画 (HostExternal) があり、外部ホスト型セッションには PTY エミュレータが不要。しかし Session 構造体に emulator フィールドが埋め込まれているため、nil チェックが散在し「表示インフラを持たない」ことを型レベルで表現できなかった。

また NewSession() でデフォルトサイズ (120x40) のエミュレータを作成した後、CreateSession() で実際のターミナルサイズで再作成するという二重初期化が発生していた。

## 決定

**PTYDisplay 構造体を抽出し、Session.display *PTYDisplay として保持する。**

- `PTYDisplay` は emulator, displayCache, scrollback, cursor tracking を所有
- `Session.display` は HostEmbedded のみ非 nil、HostExternal は nil
- `InitDisplay()`, `ResetDisplay()`, `ResizeDisplay()` で Manager からの操作を抽象化
- `Write(data)` と `Lines()` が主要な入出力インターフェース
- PTYDisplay 内のロック (emuMu) は Session.mu と独立 (クロスロック不要)
- Title 変更は `onTitle` コールバックで Session.TerminalTitle に橋渡し
- scrollback 長は atomic.Int32 で公開 (CursorPosition のロックフリー読み取り用)

## 結果

**良い点**:
- HostExternal セッションが display=nil で型安全に表現される
- Session 構造体から atomic フィールド 8個 + scrollback + emulator が消え、ドメインフィールドだけが残る
- エミュレータの二重作成が解消 (NewSession では作らず、InitDisplay で1回だけ)
- PTYDisplay 内部のロック戦略が独立し、Session.mu との関係が単純化

**悪い点**:
- PTYDisplay.Reset() でコールバック再設定のコードが重複 (newPTYDisplay と同じコールバック定義)
- Manager が emulator.Resize() を直接呼べなくなり、ResizeDisplay() 経由が必要 (間接化)

**トレードオフ**:
- コールバック重複は将来的に callback builder を抽出すれば解消できるが、現時点では2箇所のみなので許容
