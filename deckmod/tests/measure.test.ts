import { expect, test } from 'claude-code/testing'

type Measure = { tokens?: number; window: number; changed: ('context' | 'rateLimits' | 'cost')[] }

const cases: { name: string; measure: Measure; want: string[][] }[] = [
  {
    name: '文脈の量が動いたら、トークン数と窓を送る',
    measure: { tokens: 120_000, window: 200_000, changed: ['context'] },
    want: [['context', '--tokens', '120000', '--window', '200000']],
  },
  {
    name: '0 トークンも測った値として送る',
    measure: { tokens: 0, window: 200_000, changed: ['context'] },
    want: [['context', '--tokens', '0', '--window', '200000']],
  },
  {
    name: 'まだ応答の無い窓は、トークン数を空で送る',
    measure: { window: 1_000_000, changed: ['context'] },
    want: [['context', '--tokens', '', '--window', '1000000']],
  },
  {
    name: '窓の大きさが分からなければ送らない',
    measure: { tokens: 120_000, window: 0, changed: ['context'] },
    want: [],
  },
  {
    name: '文脈の量が動いていなければ送らない',
    measure: { tokens: 120_000, window: 200_000, changed: ['cost'] },
    want: [],
  },
]

for (const one of cases) {
  test(one.name, async ($, on) => {
    const sent: string[][] = []
    on('env.get', (_$, e) => ({ value: e.name === 'CLAUDE_DECK_BIN' ? '/bin/claude-deck' : e.name === 'CLAUDE_DECK_SESSION_ID' ? 'deck1' : undefined }))
    on('process.run', (_$, e) => {
      if (e.argv[1] === 'hook' && e.argv[2] === 'context') sent.push(e.argv.slice(2, e.argv.indexOf('--session')))
      return { value: { exitCode: 0, stdout: '', stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
    })
    on('session.measure', (_$, e) => ({ changed: e.changed }))
    const { tokens, window, changed } = one.measure
    await $.session.measure({ context: { window, ...(tokens === undefined ? {} : { tokens }) }, rateLimits: [], changed })
    expect(sent).toEqual(one.want)
  })
}
