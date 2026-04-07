# ドメイン分析・責務駆動設計の手法と実践ガイド

開発時に美しく実用的な設計を行うための手法・マインドセットを整理する。

---

## 目次

1. [Domain-Driven Design（DDD）](#1-domain-driven-designddd)
2. [責務駆動設計（RDD）](#2-責務駆動設計responsibility-driven-design)
3. [Event Storming](#3-event-storming)
4. [Domain Storytelling](#4-domain-storytelling)
5. [ドメイン中心アーキテクチャ](#5-ドメイン中心アーキテクチャ)
6. [関数型ドメインモデリング](#6-関数型ドメインモデリング)
7. [アンチパターン](#7-アンチパターン)
8. [現代の実践的アプローチ](#8-現代の実践的アプローチ2024-2026)
9. [スキルを磨くには](#9-スキルを磨くには)
10. [参考文献・リソース](#10-参考文献リソース)

---

## 1. Domain-Driven Design（DDD）

Eric Evans が2003年の著書で体系化した手法。ビジネスドメインをソフトウェア設計の中心に据える。

### 戦略的パターン（Strategic Design）

| パターン | 概要 |
|----------|------|
| **Bounded Context** | モデルが一貫した意味を持つ境界。マイクロサービスの境界設計にも直結する |
| **Ubiquitous Language** | 開発者とドメインエキスパートが共有する用語体系。コード内の命名もこれに従う |
| **Context Mapping** | Bounded Context 間の関係性を明示する（後述） |
| **Core Domain Chart** | Evans が2019年に提唱。ドメインを Core / Supporting / Generic に分類し投資優先度を決める |

#### Context Mapping パターン

| パターン | 説明 |
|----------|------|
| **Partnership** | 2つのコンテキストが相互依存。片方の失敗がもう片方の失敗を引き起こす |
| **Shared Kernel** | 両チームが合意した共有モデルのサブセット。小さく保つことが重要 |
| **Anti-Corruption Layer (ACL)** | 下流が上流のモデルを自ドメインの用語に翻訳する隔離層 |
| **Open Host Service** | 上流がプロトコルを公開して複数の下流にサービス提供 |
| **Published Language** | 情報交換用の共通言語（JSON, Protobuf, Avro 等） |
| **Customer/Supplier** | 上流が下流のニーズを考慮して開発 |
| **Conformist** | 下流が上流のモデルにそのまま従う |
| **Separate Ways** | 2つのコンテキスト間に有意な関係がない |
| **Big Ball of Mud** | レガシーな混沌としたシステムとの境界 |

### 戦術的パターン（Tactical Design）

| パターン | 役割 |
|----------|------|
| **Entity** | ID で識別されるオブジェクト。ライフサイクルを持つ |
| **Value Object** | 値で等価判定。Money, Address, DateRange など。不変性がバグを防ぐ |
| **Aggregate** | 整合性の境界単位。Aggregate Root を通じてのみ状態変更 |
| **Domain Service** | Entity/VO に属さないドメインロジック |
| **Repository** | 永続化の抽象。コレクションのようなインターフェースを提供 |
| **Domain Event** | ドメインで起きた事実を表現。システム間連携や監査にも有用 |
| **Factory** | 複雑なオブジェクト生成をカプセル化 |

#### Vaughn Vernon の Aggregate 設計4ルール

1. **真の不変条件を整合性境界でモデリング**: 単一トランザクション内で完全に一貫性を保つ
2. **小さな集約を設計**: 大きなクラスタ集約はパフォーマンスもスケーラビリティも悪い
3. **他の集約は ID で参照**: オブジェクト参照ではなく識別子で参照
4. **境界外では結果整合性を使用**: 集約間のルールはイベント処理やバッチ処理で解決

---

## 2. 責務駆動設計（Responsibility-Driven Design）

Rebecca Wirfs-Brock が提唱。「このオブジェクトは**何を知っているか**」「**何をするか**」「**誰と協調するか**」で設計を考える。

### CRC カード

- **C**lass / **R**esponsibility / **C**ollaboration をインデックスカードに書く
- チームで物理カードを動かしながら設計するワークショップ手法
- 責務の偏り（God Object）を早期に発見できる

### ロールステレオタイプ（Wirfs-Brock の分類）

| ステレオタイプ | 責務 |
|----------------|------|
| **Information Holder** | 情報を保持し提供する |
| **Structurer** | オブジェクト間の関係を維持する |
| **Service Provider** | 処理を実行し結果を返す |
| **Coordinator** | 他オブジェクトに仕事を委譲する |
| **Controller** | 判断を行い他オブジェクトを制御する |
| **Interfacer** | 外部システムとの境界を管理する |

### DDD との補完関係

DDD が「何をモデリングするか」に焦点を当てるのに対し、RDD は「各オブジェクトにどう責務を配分するか」を扱う。両者を組み合わせると、ドメインモデルの粒度と責務配分の両面から設計を検討できる。

---

## 3. Event Storming

Alberto Brandolini が2013年に考案したワークショップ形式。大きな壁にオレンジの付箋（ドメインイベント）を時系列で貼っていく。

### 3段階のレベル

| レベル | 目的 | 参加者 |
|--------|------|--------|
| **Big Picture** | ビジネス全体の俯瞰。ボトルネックや未知の領域の発見 | 全ステークホルダー（10-30名可） |
| **Process Modeling** | 特定プロセスの詳細化。コマンド・アクター・ポリシーを追加 | ドメインエキスパート + 開発者 |
| **Software Design** | Aggregate の境界特定。コードへの橋渡し | 開発者中心 |

### 付箋の色分け（標準的な記法）

| 色 | 要素 |
|----|------|
| オレンジ | Domain Event（「〜された」過去形） |
| 青 | Command（「〜する」命令形） |
| 黄（小） | Actor / User |
| 黄（大） | Aggregate |
| ピンク | External System |
| 赤 | Hot Spot / 問題点 |
| 紫 | Policy（「〜のとき、〜する」） |

### 効果

数時間で数ヶ月分の要件定義に匹敵する共有理解が得られる。サイロ化した組織で特に威力を発揮する。

---

## 4. Domain Storytelling

Stefan Hofer & Henning Schwentner が体系化。ドメインエキスパートが業務の流れを「物語」として語り、ピクトグラムと番号付き矢印で図式化する。

### Event Storming との比較

| 観点 | Event Storming | Domain Storytelling |
|------|---------------|-------------------|
| 形式 | 付箋ワークショップ（カオス的） | 構造化された図 |
| 出発点 | ドメインイベント（過去形） | アクターの行動（物語） |
| 適する場面 | 未知のドメインの探索、全体俯瞰 | 既知の業務フローの可視化、as-is/to-be 分析 |
| スケール | 大人数（10-30名）でも可能 | 少人数（3-7名）向き |
| 成果物 | 付箋の壁写真 → Bounded Context 候補 | ドメインストーリー図 → ユースケース |

**使い分け**: 未開拓のドメインには Event Storming、既存業務の理解と改善には Domain Storytelling が向く。併用も効果的。

---

## 5. ドメイン中心アーキテクチャ

### Hexagonal Architecture（Ports & Adapters）

Alistair Cockburn 提唱。アプリケーションの「内側」（ドメインロジック）と「外側」（インフラ）を Port と Adapter で分離する。

```
         [Web UI]  [CLI]  [Test]
              \      |     /
               Port (入力)
              ┌──────────┐
              │  Domain   │
              │  Logic    │
              └──────────┘
               Port (出力)
              /      |     \
      [PostgreSQL] [Redis] [Mock]
```

### Clean Architecture（Robert C. Martin）

同心円モデル。依存の方向は常に外→内。

```
外 → Frameworks & Drivers
     → Interface Adapters
         → Use Cases (Application)
             → Entities (Domain)   ← 最内部
```

### Onion Architecture（Jeffrey Palermo）

Domain Model → Domain Services → Application Services → Infrastructure の層構成。

### 3つの共通原則

1. **依存性逆転**: ドメインが外部に依存しない。インフラがドメインに依存する
2. **テスタビリティ**: ドメインロジックを外部依存なしにテスト可能
3. **交換可能性**: データベースや UI フレームワークの変更がドメインに影響しない

---

## 6. 関数型ドメインモデリング

Scott Wlaschin の「Domain Modeling Made Functional」が体系化。型システムでドメインを表現する。

### 代数的データ型（ADT）によるモデリング

- **直積型（Product Types）**: 「A かつ B」を表現（レコード、タプル）
- **直和型（Sum Types / Discriminated Unions）**: 「A または B」を表現（選択肢のモデリングに最適）

### Make Illegal States Unrepresentable

型システムでビジネスルールをエンコードし、不正な状態をコンパイル時に排除する。

- `string` ではなく `EmailAddress` 型を使い、ドメインの意図を表現
- **Parse, Don't Validate**: 生の入力を型付けされた値にパースすることで不変条件を確立
- 各関数は入力型から出力型への変換であり、コンパイラが型の整合性を保証

### 他言語への応用

Go の場合、直和型は interface + 型スイッチ、あるいは sealed interface パターンで近似できる。TypeScript では Discriminated Unions、Rust では enum、Kotlin では sealed class が対応する。

---

## 7. アンチパターン

### 戦略レベル

| アンチパターン | 問題 |
|----------------|------|
| **ドメインエキスパート不在** | 技術者だけでドメインを語り、ビジネス側に相談しない |
| **DDD Lite** | Aggregate/Entity だけ使い戦略設計を無視。最も多いアンチパターン |
| **ユビキタス言語の不統一** | 同じ用語が異なる部分で異なる意味で使用される |
| **技術駆動の分割** | ドメインでなく技術層でサービスを分割する |

### 戦術レベル

| アンチパターン | 問題 |
|----------------|------|
| **Anemic Domain Model** | Entity がデータ保持のみでロジックが Service に流出 |
| **巨大な Aggregate** | 大きくなりすぎ、他の Aggregate への参照を含む |
| **過剰な共通化** | Shared Kernel を広げすぎてコンテキスト間が密結合 |
| **教条的な DDD** | パターンを盲目的に適用し、コンテキストや実際のニーズを無視 |

---

## 8. 現代の実践的アプローチ（2024-2026）

### 協調的モデリングの組み合わせ

Event Storming / Domain Storytelling / Example Mapping を組み合わせた「モデリング会」が定着。

- **Example Mapping**: BDD の文脈でルール・例・質問を整理するミニワークショップ。Event Storming の補完として使われる
- **Wardley Mapping + DDD + Team Topologies**: Susanne Kaiser が提唱する統合アプローチ
  1. Wardley Map でユーザーニーズ（= 問題ドメイン）を可視化
  2. コンポーネントから Bounded Context を特定
  3. サブドメイン分類を問題空間に適用
  4. Context Mapping で依存関係を設計
  5. Team Topologies でチーム構造を最適化

### DDD とマイクロサービス

Bounded Context はマイクロサービスのサービス境界を定義する最も効果的な手法として広く認識されている。

- 各マイクロサービスは特定のビジネス機能に対応すべき（技術的関心事ではなく）
- ACL を使ってレガシーシステムとの境界を保護しながら段階的にサービスを切り出す

---

## 9. スキルを磨くには

### 実践的な習得法

1. **コードで学ぶ**: 既存コードを「このクラスの責務は何か？」「この境界は正しいか？」と問い直すリファクタリングが最も効果的
2. **モデリング会を開く**: 週1回30分でも、ホワイトボードの前でドメインモデルを議論する習慣
3. **ドメインエキスパートと話す**: ユビキタス言語は会議室で作るものではなく、日常の会話から育てるもの
4. **小さく始める**: 全社導入ではなく、1つの Bounded Context から。まず Value Object を導入するだけでも効果がある
5. **Kata で練習**: "Cargo Shipping"（Evans の例）、"Banking"、"Library" などのドメインで繰り返しモデリング練習
6. **対象ドメインの教科書を読む**: 会計、物流、医療など、ドメインの本を読むことがモデリング力を上げる最短路
7. **Refactoring Toward Deeper Insight**: 最初のモデルは必ず間違っている。使いながら深い洞察を得て繰り返しリファクタリングする

### 問いから手法を選ぶ

```
問い                          → 手法
──────────────────────────────────────────
ビジネスの全体像は？          → Event Storming (Big Picture)
業務フローの詳細は？          → Domain Storytelling / Process Modeling
どこに境界を引く？            → Bounded Context + Context Mapping
各オブジェクトの責務は？      → CRC カード / RDD
不変条件の範囲は？            → Aggregate 設計
ドメインをコードでどう表現？  → Tactical DDD + Hexagonal Architecture
何が Core Domain か？         → Core Domain Chart + Wardley Map
```

---

## 10. 参考文献・リソース

### 必読書

| 書籍 | 著者 | ポイント |
|------|------|----------|
| **Domain-Driven Design** (Blue Book) | Eric Evans | 原典。Part III "Refactoring Toward Deeper Insight" が特に重要 |
| **Implementing Domain-Driven Design** (Red Book) | Vaughn Vernon | 実装寄りの解説 |
| **Domain-Driven Design Distilled** | Vaughn Vernon | DDD 入門書として最適 |
| **Learning Domain-Driven Design** | Vlad Khononov | 最もモダンで実践的な入門書 |
| **Domain Modeling Made Functional** | Scott Wlaschin | 関数型での DDD |
| **Object Design: Roles, Responsibilities, and Collaborations** | Wirfs-Brock & McKean | RDD の教科書 |
| **Introducing EventStorming** | Alberto Brandolini | Leanpub で入手可能 |
| **Domain Storytelling** | Hofer & Schwentner | O'Reilly 刊 |
| **Architecture for Flow** | Susanne Kaiser | DDD + Wardley Mapping + Team Topologies の統合 |

### 日本語リソース

| 書籍・リソース | 著者 | 特徴 |
|----------------|------|------|
| **ドメイン駆動設計入門** | 成瀬允宣 | ボトムアップで解説。日本語 DDD 入門の定番 |
| **ドメイン駆動設計をはじめよう** | Vlad Khononov 著、増田亨・綿引琢磨 訳（2024年） | Learning DDD の日本語訳 |
| **[入門]ドメイン駆動設計** | 増田亨ほか共著（2024年） | Software Design 別冊 |
| **現場で役立つシステム設計の原則** | 増田亨 | ドメインモデルの実践を日本の現場目線で解説 |

### 主要人物（日本）

- **増田亨**: 日本における DDD の第一人者。講演・執筆多数
- **成瀬允宣**: ボトムアップアプローチで日本の DDD 普及に貢献
- **和智右桂**: Evans「Blue Book」日本語訳の翻訳者。ワークショップも開催

### オンラインリソース

- [DDD-Crew Free Learning Resources](https://github.com/ddd-crew/free-ddd-learning-resources)
- [Awesome DDD](https://github.com/heynickc/awesome-ddd)
- [DDD-Crew Context Mapping](https://github.com/ddd-crew/context-mapping)
- [SAP Curated Resources for DDD](https://github.com/SAP/curated-resources-for-domain-driven-design)
- [F# for Fun and Profit](https://fsharpforfunandprofit.com/books/)
- DDD Europe カンファレンス（年次開催）
- DDD-CQRS-ES Discord Server

---

## 核心

> 美しい設計は「パターンの適用」ではなく「ドメインの深い理解」から生まれる。技術的な構造はドメインの構造に従うべきであり、その逆ではない。Evans の言う "Refactoring Toward Deeper Insight"（より深い洞察へのリファクタリング）こそが、実用的かつ美しい設計への道筋。
