import { expect, test } from 'claude-code/testing'
import { asksUserToAct, parseWaitReason, shouldNudge } from '../hooks/register'

// WHY for で回す: claude-code/testing の test には each が無い。

const reasons = [
  { name: '空は断る', reason: '', ok: false },
  { name: '空白だけは断る', reason: ' ', ok: false },
  { name: '文字列でなければ断る', reason: 12, ok: false },
  { name: 'ASCII の 80 文字は通す', reason: 'a'.repeat(80), ok: true },
  { name: 'ASCII の 81 文字は断る', reason: 'a'.repeat(81), ok: false },
  { name: '全角の 80 文字は通す', reason: 'あ'.repeat(80), ok: true },
  { name: '全角の 81 文字は断る', reason: 'あ'.repeat(81), ok: false },
  // UTF-16 の length では 160 になる。length で数える実装を落とす
  { name: '絵文字 80 個は通す', reason: '🔑'.repeat(80), ok: true },
  { name: '改行を含めば断る', reason: 'gh にログイン\nしてください', ok: false },
]
for (const { name, reason, ok } of reasons) {
  test(`wait_for_user の reason: ${name}`, () => {
    const parsed = parseWaitReason({ reason })
    expect('reason' in parsed).toBe(ok)
    // 切り詰めない: 通した理由はそのまま返す
    if ('reason' in parsed) expect(parsed.reason).toBe(reason)
  })
}

const messages = [
  { name: '行頭の ! コマンド', text: '次を実行してほしい。\n! gh auth login', want: true },
  { name: 'バッククォートの中の ! コマンド', text: '`! npm login` で入ってほしい。', want: true },
  { name: 'コードブロックの中の行頭の ! コマンド', text: '```\n! gh auth login\n```', want: true },
  { name: '行頭でもバッククォートの中でもない !', text: 'すごい! これで終わり', want: false },
  { name: '空白の無い `!important`', text: '`!important` を外した。', want: false },
  { name: '空', text: '', want: false },
  { name: 'ログインとください', text: 'gh auth login でログインしてください。', want: true },
  { name: '承認とお願いします', text: 'PR の承認をお願いします', want: true },
  { name: '押してとください', text: 'ボタンを押してください', want: true },
  { name: '打ってとください', text: 'コマンドを打ってください', want: true },
  { name: '認証とください', text: 'ブラウザで認証してください', want: true },
  { name: '実行してとください', text: 'スクリプトを実行してください', want: true },
  { name: 'ください だけ', text: 'ご確認ください。', want: false },
  { name: '依頼の語だけ', text: 'ボタンを押して保存した。', want: false },
  { name: '別の文に分かれる', text: 'ログインしました。結果をご確認ください。', want: false },
  { name: 'コードブロックの中の依頼の文', text: '```sh\n# ここでログインしてください\n```', want: false },
  // 2026-11-25 に wait_for_user の説明の返答で止まった 2 文。頼んでいない
  {
    name: 'インラインコードの ! コマンド という書き方の説明',
    text: '呼ぶ場面: ターンを終える直前に、利用者が会話の外で操作するのを待つとき。ログイン、承認、`! コマンド`の実行、ブラウザでの操作が挙がっている',
    want: false,
  },
  {
    name: 'かぎ括弧で引いた例の中の依頼の文',
    text: '理由の書き方: 一覧だけで分かる短い文を、返答の言語で書く。例は「gcloud auth login を ! で実行してください」',
    want: false,
  },
  { name: 'インラインコードの ! <コマンド>', text: '`! <コマンド>` の形で頼む。', want: false },
  { name: '引用符で引いた依頼の文', text: '説明文には "ログインしてください" とある。', want: false },
  { name: 'インラインコードの実際のコマンドを頼む', text: '`! gcloud auth login` を打ってください', want: true },
  { name: 'かぎ括弧の外で頼む', text: '「gh」でログインしてください。', want: true },
]
for (const { name, text, want } of messages) {
  test(`操作の頼みの検知: ${name}`, () => {
    expect(asksUserToAct(text)).toBe(want)
  })
}

const ASKS = 'gh auth login でログインしてください。'
const stops = [
  { name: 'メインで、呼ばずに頼んで終える', change: {}, want: true },
  { name: '同じターンで止めた後', change: { nudged: true }, want: false },
  { name: 'stop_hook_active', change: { stopHookActive: true }, want: false },
  { name: 'そのターンに wait_for_user を呼んだ', change: { calledWait: true }, want: false },
  { name: 'サブエージェントの Stop', change: { isMain: false }, want: false },
  { name: '頼んでいない', change: { lastMessage: '直した。' }, want: false },
]
for (const { name, change, want } of stops) {
  test(`Stop で止めるか: ${name}`, () => {
    const stop = { isMain: true, stopHookActive: false, calledWait: false, nudged: false, lastMessage: ASKS, ...change }
    expect(shouldNudge(stop)).toBe(want)
  })
}
