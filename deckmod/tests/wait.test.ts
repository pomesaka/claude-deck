import { expect, test } from 'claude-code/testing'
import { detectWait } from '../hooks/register'

const waits = (command: string) => `\`! ${command}\` の実行を待っています`
// The command that fills the 80 characters of the list exactly: the rest of the sentence takes 15.
const LONGEST = 'a'.repeat(65)

const cases: { name: string; text: string; want: string | undefined }[] = [
  { name: '行頭の ! コマンド', text: '次を打ってください。\n! gcloud auth login', want: waits('gcloud auth login') },
  { name: '字下げした行頭', text: '  ! gh auth login', want: waits('gh auth login') },
  { name: 'コードブロックの中の行', text: '次を実行してください。\n\n```\n! gcloud auth login\n```', want: waits('gcloud auth login') },
  { name: 'いくつかあれば最初のもの', text: '! gh auth login\n! gcloud auth login', want: waits('gh auth login') },
  { name: 'パスで始まるコマンド', text: '! ./scripts/login.sh --force', want: waits('./scripts/login.sh --force') },
  { name: '80 文字ちょうどは切らない', text: `! ${LONGEST}`, want: waits(LONGEST) },
  { name: '80 文字を超えたら切る', text: `! ${LONGEST}b`, want: waits(`${'a'.repeat(64)}…`) },
  { name: 'バッククォートの中の ! コマンド', text: '`! gcloud auth login` を実行してください', want: undefined },
  { name: '行の途中の ! コマンド', text: '次は ! gcloud auth login で入れます', want: undefined },
  { name: '書き方の説明（山かっこ）', text: '! <コマンド> の形で打つと、このセッションで実行される', want: undefined },
  { name: '! の後が日本語', text: '! コマンド で実行できます', want: undefined },
  { name: '! の後に空白が無い', text: '!gcloud auth login', want: undefined },
  { name: '依頼の文だけ', text: 'gcloud にログインしてください。', want: undefined },
  { name: '空の返答', text: '', want: undefined },
]

for (const { name, text, want } of cases) {
  test(`detectWait: ${name}`, () => {
    expect(detectWait(text)).toBe(want)
  })
}
