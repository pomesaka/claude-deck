# ADR-005: Snapshot.Phase をフィールドからメソッドへ変更

ステータス: 採用済み

## コンテキスト

`Snapshot` は `Session` のロックフリーな読み取り専用コピーで、TUI や Manager がロックなしに状態を参照できる。元の設計では `Phase SessionPhase` フィールドを持ち、`Session.Snapshot()` 内で `phaseLocked()` を呼んで値を計算していた。

`SessionPhase` は `Status` と `process` の存在から導出される派生値であり、独立したフィールドとして保持すると「`Phase` フィールドだけ古い値が残る」という不整合が潜在的に発生しうる。特に `Status` が変わったときに `Phase` が追随できていない場合、TUI が矛盾したデータを描画するリスクがある。

## 決定

`Snapshot.Phase` フィールドを廃止し、`Snapshot.Phase()` メソッドとして再実装した。`Status` と `HasProcess` フィールドからオンザフライで導出するため、派生値が常に一致する。

```go
func (s Snapshot) Phase() SessionPhase {
    if s.Status == StatusUnmanaged {
        return PhaseExternal
    }
    if s.Status.IsTerminal() && !s.HasProcess {
        return PhaseArchived
    }
    return PhaseActive
}
```

`HasProcess` フィールドを新設した理由: `Hosting == HostExternal` は「プロセスなし」と「tmux ホストの生きたプロセス」を区別できないため、`process.Load() != nil` を直接 Snapshot に投影する `bool` フィールドが必要だった。

## legacySession 移行コードの削除

同タイミングで `LoadExisting` の `legacySession` 移行コード（claude_session_id / previous_claude_session_id → SessionChain 変換）を削除した。SessionChain は v0.x 以降のデータで常に存在するため、移行コードは機能しない dead code となっていた。

## 結果

### 良い点

- `Phase` の不整合が型レベルで発生不可能になった
- `Snapshot` の構造が「保存フィールド」と「派生メソッド」で明確に分離された
- `phaseLocked()` が削除されコードが簡潔になった
- `HasProcess` フィールドは他の用途（TUI でのプロセス有無判定）にも活用できる

### 却下した代替案

**Phase フィールドを存続させる**: リスクが低く変更コストがゼロだが、「フィールドだけ古い」可能性を残す設計上の負債を積み続ける。

**Hosting == HostExternal を proxy として使う**: `IsTerminal() && Hosting == HostExternal` で Phase を導出する案。tmux ホストの生きたプロセスが StatusCompleted になる稀なレース（watchProcess 着火前）でフィールドが PhaseArchived になる誤りが生じる。
