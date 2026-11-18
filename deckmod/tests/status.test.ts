import { expect, test, type TestBody } from 'claude-code/testing'

type On = Parameters<TestBody>[1]

const END = { answer: '', durationMs: 0, isAborted: false, reason: 'answer' } as const
const AGENT = 'a1'

type Step =
  | { event: 'turn.start' | 'turn.complete'; agentId?: string }
  // An Agent call with no agentId: the main loop's, or one a plugin raised (its id starts with toolu_plugin_).
  | { event: 'tool.call'; toolUseId: string }
  // What $.agent.list() answers from here on.
  | { agent: 'running' | 'completed' }

const world = (on: On) => {
  const seen: { statuses: string[]; agent: 'running' | 'completed' | null } = { statuses: [], agent: null }
  on('env.get', (_$, e) => ({ value: e.name === 'CLAUDE_DECK_BIN' ? '/bin/claude-deck' : e.name === 'CLAUDE_DECK_SESSION_ID' ? 'deck1' : undefined }))
  on('process.run', (_$, e) => {
    if (e.argv[1] === 'hook' && e.argv[2] === 'status' && e.argv[3] !== undefined) seen.statuses.push(e.argv[3])
    return { value: { exitCode: 0, stdout: '', stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  on('agent.list', () => ({
    value: seen.agent === null ? [] : [{ id: AGENT, description: 'writer', type: 'general-purpose', status: seen.agent }],
  }))
  on('tool.call', () => ({ result: '' }))
  on('turn.start', (_$, e) => ({ turnId: e.turnId }))
  on('turn.complete', () => ({ text: '' }))
  return seen
}

// WHY どのケースもメインのターンで始め、idle で終える: モジュール変数（lastStatus、inMainTurn）が
// テストをまたいで残っても、次のケースの最初の書き込みが running になる。
const cases: { name: string; steps: Step[]; statuses: string[] }[] = [
  {
    name: 'メインのターンは running で始まり、idle で終わる',
    steps: [{ event: 'turn.start' }, { event: 'turn.complete' }],
    statuses: ['running', 'idle'],
  },
  {
    // 守る不具合: Mod がメインの停止中に立てたエージェントの開始を、メインの開始として running にすると、
    // メインの turn.complete が来ないので running のまま残る。
    name: 'メインが止まっている間に始まったサブエージェントは subagent_running にし、終わったら idle に戻す',
    steps: [
      { event: 'turn.start' },
      { event: 'turn.complete' },
      { agent: 'running' },
      { event: 'turn.start', agentId: AGENT },
      { agent: 'completed' },
      { event: 'turn.complete', agentId: AGENT },
    ],
    statuses: ['running', 'idle', 'subagent_running', 'idle'],
  },
  {
    // 守る不具合: Mod の $.agent.spawn は、agentId の無い Agent の tool.call として届く。メインの呼び出しとして
    // running にすると、メインのターンが始まらないので running のまま残る（retro の書き手で起きた）。
    name: 'メインが止まっている間に Mod が立てたエージェントの呼び出しでは running にしない',
    steps: [
      { event: 'turn.start' },
      { event: 'turn.complete' },
      { agent: 'running' },
      { event: 'tool.call', toolUseId: 'toolu_plugin_1' },
      { event: 'turn.start', agentId: AGENT },
      { agent: 'completed' },
      { event: 'turn.complete', agentId: AGENT },
    ],
    statuses: ['running', 'idle', 'subagent_running', 'idle'],
  },
  {
    name: 'メインのツール呼び出しは、ターンの中で running のままにする',
    steps: [{ event: 'turn.start' }, { event: 'tool.call', toolUseId: 'toolu_1' }, { event: 'turn.complete' }],
    statuses: ['running', 'idle'],
  },
  {
    name: 'メインのターンの中で始まったサブエージェントは、メインが終わるまで running のままにする',
    steps: [
      { event: 'turn.start' },
      { agent: 'running' },
      { event: 'turn.start', agentId: AGENT },
      { event: 'turn.complete' },
      { agent: 'completed' },
      { event: 'turn.complete', agentId: AGENT },
    ],
    statuses: ['running', 'subagent_running', 'idle'],
  },
]

for (const one of cases) {
  test(one.name, async ($, on) => {
    const seen = world(on)
    for (const step of one.steps) {
      if ('agent' in step) {
        seen.agent = step.agent
      } else if (step.event === 'tool.call') {
        await $.tool.call({ tool: 'Agent', tool_use_id: step.toolUseId, description: 'writer', prompt: 'write', subagent_type: 'general-purpose' })
      } else if (step.event === 'turn.start') {
        await $.turn.start({ text: '', turnId: 't1', ...(step.agentId === undefined ? {} : { agentId: step.agentId }) })
      } else {
        await $.turn.complete({ ...END, turnId: 't1', ...(step.agentId === undefined ? {} : { agentId: step.agentId }) })
      }
    }
    expect(seen.statuses).toEqual(one.statuses)
  })
}
