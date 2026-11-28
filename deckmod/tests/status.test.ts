import { expect, test, type TestBody } from 'claude-code/testing'

type On = Parameters<TestBody>[1]

const END = { answer: '', durationMs: 0, isAborted: false, reason: 'answer' } as const
const AGENT = 'a1'
// The main loop's last message asks the user to run a command.
const ASKS = '次を実行してください。\n\n```\n! gh auth login\n```'
const WAITING = 'waiting_action[command]: `! gh auth login` の実行を待っています'
const STOPPED = 'waiting_action[api_error]: API の誤りで止まりました。何か送ると再開します'

type Step =
  | { event: 'turn.start' | 'turn.complete'; agentId?: string }
  // The turn ends on an API error (a network outage, an overloaded API) after Claude Code's own retries.
  | { event: 'turn.complete'; reason: 'error' }
  // An Agent call with no agentId: the main loop's, or one a plugin raised (its id starts with toolu_plugin_).
  | { event: 'tool.call'; toolUseId: string }
  // What $.agent.list() answers from here on.
  | { agent: 'running' | 'completed' }
  // The main loop's turn stops with this last message (classic.Stop).
  | { stop: string }
  // /clear: SessionStart with source clear.
  | { clear: true }
  // An approval dialog opens (PermissionRequest).
  | { approval: true }

const world = (on: On) => {
  const seen: { statuses: string[]; agent: 'running' | 'completed' | null } = { statuses: [], agent: null }
  on('env.get', (_$, e) => ({ value: e.name === 'CLAUDE_DECK_BIN' ? '/bin/claude-deck' : e.name === 'CLAUDE_DECK_SESSION_ID' ? 'deck1' : undefined }))
  // A status is recorded as its name, and as "name[kind]: reason" when --reason is not empty.
  on('process.run', (_$, e) => {
    if (e.argv[1] === 'hook' && e.argv[2] === 'status' && e.argv[3] !== undefined) {
      const flag = (name: string) => {
        const at = e.argv.indexOf(name)
        return at < 0 ? '' : (e.argv[at + 1] ?? '')
      }
      const reason = flag('--reason')
      seen.statuses.push(reason === '' ? e.argv[3] : `${e.argv[3]}[${flag('--wait-kind')}]: ${reason}`)
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
    name: '最後の返答に行頭の ! コマンドがあるターンは、理由つきの waiting_action で終わり、次のターンで消える',
    steps: [
      { event: 'turn.start' },
      { stop: ASKS },
      { event: 'turn.complete' },
      { event: 'turn.start' },
      { stop: '終わりました。' },
      { event: 'turn.complete' },
    ],
    statuses: ['running', WAITING, 'running', 'idle'],
  },
  {
    name: 'バッククォートの中の ! コマンドだけなら idle で終わる',
    steps: [{ event: 'turn.start' }, { stop: '`! gh auth login` のように書くと、利用者が実行できます。' }, { event: 'turn.complete' }],
    statuses: ['running', 'idle'],
  },
  {
    // Stop と turn.complete のどちらが先に届くかは確かめていないので、後に届いても待ちを書く。
    name: 'ターンの終わりの後に Stop が届いても waiting_action にする',
    steps: [{ event: 'turn.start' }, { event: 'turn.complete' }, { stop: ASKS }],
    statuses: ['running', 'idle', WAITING],
  },
  {
    // 守る不具合: サブエージェントの終わりで subagent_running や idle を書くと、利用者の操作待ちが一覧から消える。
    name: 'バックグラウンドのサブエージェントが残っていても waiting_action にし、その終わりで上書きしない',
    steps: [
      { event: 'turn.start' },
      { agent: 'running' },
      { event: 'turn.start', agentId: AGENT },
      { stop: ASKS },
      { event: 'turn.complete' },
      { agent: 'completed' },
      { event: 'turn.complete', agentId: AGENT },
      { event: 'turn.start' },
      { event: 'turn.complete' },
    ],
    statuses: ['running', WAITING, 'running', 'idle'],
  },
  {
    // 守る不具合: 待ちの間に開いた承認ダイアログが閉じたとき、idle や subagent_running を書くと待ちが消える。
    name: '待ちの間に開いた承認ダイアログが閉じたら、同じ理由の waiting_action に戻す',
    steps: [
      { event: 'turn.start' },
      { stop: ASKS },
      { event: 'turn.complete' },
      { approval: true },
      // The call the dialog was for, raised outside the main loop.
      { event: 'tool.call', toolUseId: 'toolu_plugin_1' },
    ],
    statuses: [
      'running',
      WAITING,
      'waiting_approval',
      WAITING,
    ],
  },
  {
    // 守る不具合: 通信の誤りで止まったセッションが idle になり、報告を待っているだけのセッションと見分けられなかった。
    name: 'API の誤りで終わったターンは、理由つきの waiting_action で終わり、次のターンで消える',
    steps: [
      { event: 'turn.start' },
      { event: 'turn.complete', reason: 'error' },
      { event: 'turn.start' },
      { event: 'turn.complete' },
    ],
    statuses: ['running', STOPPED, 'running', 'idle'],
  },
  {
    name: 'API の誤りの待ちは、バックグラウンドのサブエージェントの終わりで上書きしない',
    steps: [
      { event: 'turn.start' },
      { agent: 'running' },
      { event: 'turn.start', agentId: AGENT },
      { event: 'turn.complete', reason: 'error' },
      { agent: 'completed' },
      { event: 'turn.complete', agentId: AGENT },
      { event: 'turn.start' },
      { event: 'turn.complete' },
    ],
    statuses: ['running', STOPPED, 'running', 'idle'],
  },
  {
    name: '/clear で待ちを外す',
    steps: [{ event: 'turn.start' }, { stop: ASKS }, { event: 'turn.complete' }, { clear: true }],
    statuses: ['running', WAITING, 'idle'],
  },
]

for (const one of cases) {
  test(one.name, async ($, on) => {
    const seen = world(on)
    for (const step of one.steps) {
      if ('agent' in step) {
        seen.agent = step.agent
      } else if ('stop' in step) {
        await $.classic.Stop({ stop_hook_active: false, last_assistant_message: step.stop })
      } else if ('clear' in step) {
        await $.classic.SessionStart({ source: 'clear' })
      } else if ('approval' in step) {
        await $.classic.PermissionRequest({ tool_name: 'Bash', tool_input: {}, permission_suggestions: [] })
      } else if (step.event === 'tool.call') {
        await $.tool.call({ tool: 'Agent', tool_use_id: step.toolUseId, description: 'writer', prompt: 'write', subagent_type: 'general-purpose' })
      } else if (step.event === 'turn.start') {
        await $.turn.start({ text: '', turnId: 't1', ...(step.agentId === undefined ? {} : { agentId: step.agentId }) })
      } else if ('reason' in step) {
        await $.turn.complete({ ...END, reason: step.reason, turnId: 't1' })
      } else {
        await $.turn.complete({ ...END, turnId: 't1', ...(step.agentId === undefined ? {} : { agentId: step.agentId }) })
      }
    }
    expect(seen.statuses).toEqual(one.statuses)
  })
}
