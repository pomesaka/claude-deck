import { expect, test, type TestBody } from 'claude-code/testing'

type On = Parameters<TestBody>[1]

const END = { answer: '', durationMs: 0, isAborted: false, reason: 'answer' } as const
const AGENT = 'a1'
const WAIT_TOOL = 'mcp__deck-status__wait_for_user'
// 80 characters, the most wait_for_user takes, all of them wide.
const LONGEST_REASON = 'あ'.repeat(80)

type Step =
  | { event: 'turn.start' | 'turn.complete'; agentId?: string }
  // An Agent call with no agentId: the main loop's, or one a plugin raised (its id starts with toolu_plugin_).
  | { event: 'tool.call'; toolUseId: string }
  // What $.agent.list() answers from here on.
  | { agent: 'running' | 'completed' }
  // The main loop calls wait_for_user.
  | { wait: string }
  // /clear: SessionStart with source clear.
  | { clear: true }
  // An approval dialog opens (PermissionRequest).
  | { approval: true }

const world = (on: On) => {
  const seen: { statuses: string[]; agent: 'running' | 'completed' | null } = { statuses: [], agent: null }
  on('env.get', (_$, e) => ({ value: e.name === 'CLAUDE_DECK_BIN' ? '/bin/claude-deck' : e.name === 'CLAUDE_DECK_SESSION_ID' ? 'deck1' : undefined }))
  // A status is recorded as its name, and as "name: reason" when --reason is not empty.
  on('process.run', (_$, e) => {
    if (e.argv[1] === 'hook' && e.argv[2] === 'status' && e.argv[3] !== undefined) {
      const at = e.argv.indexOf('--reason')
      const reason = at < 0 ? '' : (e.argv[at + 1] ?? '')
      seen.statuses.push(reason === '' ? e.argv[3] : `${e.argv[3]}: ${reason}`)
    }
    return { value: { exitCode: 0, stdout: '', stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  on('agent.list', () => ({
    value: seen.agent === null ? [] : [{ id: AGENT, description: 'writer', type: 'general-purpose', status: seen.agent }],
  }))
  on('tool.call', () => ({ result: '' }))
  on('turn.start', (_$, e) => ({ turnId: e.turnId }))
  on('turn.complete', () => ({ text: '' }))
  on('classic.SessionStart', () => ({}))
  on('classic.Stop', () => ({}))
  on('classic.PermissionRequest', () => ({}))
  on('ui.log', () => ({ value: undefined }))
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
  {
    name: 'wait_for_user を呼んだターンは、理由つきの waiting_action で終わり、次のターンで消える',
    steps: [
      { event: 'turn.start' },
      { wait: LONGEST_REASON },
      { event: 'turn.complete' },
      { event: 'turn.start' },
      { event: 'turn.complete' },
    ],
    statuses: ['running', `waiting_action: ${LONGEST_REASON}`, 'running', 'idle'],
  },
  {
    name: '断られた理由では待ちにならず、idle で終わる',
    steps: [{ event: 'turn.start' }, { wait: `${LONGEST_REASON}あ` }, { event: 'turn.complete' }],
    statuses: ['running', 'idle'],
  },
  {
    // 守る不具合: サブエージェントの終わりで subagent_running や idle を書くと、利用者の操作待ちが一覧から消える。
    name: 'バックグラウンドのサブエージェントが残っていても waiting_action にし、その終わりで上書きしない',
    steps: [
      { event: 'turn.start' },
      { agent: 'running' },
      { event: 'turn.start', agentId: AGENT },
      { wait: 'gh auth login を実行してください' },
      { event: 'turn.complete' },
      { agent: 'completed' },
      { event: 'turn.complete', agentId: AGENT },
      { event: 'turn.start' },
      { event: 'turn.complete' },
    ],
    statuses: ['running', 'waiting_action: gh auth login を実行してください', 'running', 'idle'],
  },
  {
    // 守る不具合: 待ちの間に開いた承認ダイアログが閉じたとき、idle や subagent_running を書くと待ちが消える。
    name: '待ちの間に開いた承認ダイアログが閉じたら、同じ理由の waiting_action に戻す',
    steps: [
      { event: 'turn.start' },
      { wait: 'gh auth login を実行してください' },
      { event: 'turn.complete' },
      { approval: true },
      // The call the dialog was for, raised outside the main loop.
      { event: 'tool.call', toolUseId: 'toolu_plugin_1' },
    ],
    statuses: [
      'running',
      'waiting_action: gh auth login を実行してください',
      'waiting_approval',
      'waiting_action: gh auth login を実行してください',
    ],
  },
  {
    name: '/clear で待ちを外す',
    steps: [{ event: 'turn.start' }, { wait: 'gh auth login を実行してください' }, { event: 'turn.complete' }, { clear: true }],
    statuses: ['running', 'waiting_action: gh auth login を実行してください', 'idle'],
  },
]

for (const one of cases) {
  test(one.name, async ($, on) => {
    const seen = world(on)
    for (const step of one.steps) {
      if ('agent' in step) {
        seen.agent = step.agent
      } else if ('wait' in step) {
        await $.tool.call({ tool: WAIT_TOOL, reason: step.wait })
      } else if ('clear' in step) {
        await $.classic.SessionStart({ source: 'clear' })
      } else if ('approval' in step) {
        await $.classic.PermissionRequest({ tool_name: 'Bash', tool_input: {}, permission_suggestions: [] })
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

test('利用者に操作を頼んで wait_for_user を呼ばずに終えるターンは、1 回だけ止める', async ($, on) => {
  world(on)
  await $.turn.start({ text: '', turnId: 't1' })
  const stop = { stop_hook_active: false, last_assistant_message: '`! gcloud auth login` を打ってください' }
  const first = await $.classic.Stop(stop)
  expect(first.block).toContain(WAIT_TOOL)
  const second = await $.classic.Stop(stop)
  expect(second.block).toBeUndefined()
  await $.turn.complete({ ...END, turnId: 't1' })
})

test('wait_for_user を呼んだターンは止めない', async ($, on) => {
  world(on)
  await $.turn.start({ text: '', turnId: 't1' })
  await $.tool.call({ tool: WAIT_TOOL, reason: 'gcloud auth login を実行してください' })
  const result = await $.classic.Stop({ stop_hook_active: false, last_assistant_message: '`! gcloud auth login` を打ってください' })
  expect(result.block).toBeUndefined()
  await $.turn.complete({ ...END, turnId: 't1' })
  await $.turn.start({ text: '', turnId: 't2' })
  await $.turn.complete({ ...END, turnId: 't2' })
})
