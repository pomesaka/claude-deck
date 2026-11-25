import type { EngineInterface, Register } from 'claude-code'

// Statuses as the store spells them (session.Status.ID in Go).
type DeckStatus = 'running' | 'idle' | 'waiting_approval' | 'waiting_answer' | 'subagent_running' | 'waiting_action'

const ASK_USER_QUESTION = 'AskUserQuestion'
// The tool the model calls before it ends a turn to wait on the user. The engine
// names a plugin's tool mcp__<plugin>__<name>; the plugin is deck-status (.claude-plugin/plugin.json).
const WAIT_TOOL = 'wait_for_user'
const WAIT_TOOL_ID = 'mcp__deck-status__wait_for_user'
// WHY 80 文字: 一覧の 2 行目の末尾に出す。超えたら切らずにエラーで返し、短くして呼び直させる。
// 黙って切ると、一覧の理由が途中で途切れる（pomesaka と決めた）。
const MAX_WAIT_REASON = 80
const WAIT_TOOL_DESCRIPTION = `Call this right before you end your turn to wait for the user to act outside this conversation: logging in, approving something, running a \`! command\` you asked them to type, or doing something in a browser. claude-deck then lists this session as waiting for the user's action, with the reason, until the user's next message. For a question whose answer the user picks right here, use AskUserQuestion instead. Do not call it when you are not waiting on the user.`
const WAIT_TOOL_SCHEMA = {
  type: 'object',
  properties: {
    reason: {
      type: 'string',
      description: `What the user has to do, as a short sentence they understand from the session list alone (at most ${MAX_WAIT_REASON} characters), in the language you answer the user in. Example: "gcloud auth login を ! で実行してください"`,
    },
  },
  required: ['reason'],
  additionalProperties: false,
}
// What the Stop check tells the model when its last message asks the user to act but it did not call the tool.
const WAIT_NUDGE = `If you are waiting for the user to act (log in, approve, run a \`! command\`), call ${WAIT_TOOL_ID} with the reason, then end your turn. If you are not waiting on the user, end your turn as it is.`
const HOOK_TIMEOUT_MS = 5_000
// How the engine starts the tool_use_id of a call a plugin raised ($.agent.spawn, $.tool.call).
const PLUGIN_TOOL_USE_PREFIX = 'toolu_plugin_'

// WHY 直列化: hook はほぼ同時に続けて発火する（tool.call の前後と PermissionRequest など）。
// 書き込みを並行に走らせると後の状態が先に store に入り、古い状態で上書きされることがある。
let queue: Promise<boolean> = Promise.resolve(true)

// WHY 判定用の hook に .catch(next(e)) を付ける: 状態の報告に失敗しても、ツール呼び出しや承認の流れは止めない。
// deck() は失敗をすべて握りつぶすので、next を呼んだ後に例外が出ることはない。

// The last status this process wrote successfully. Repeating it is skipped:
// tool.call fires for every tool, and each write spawns a process and wakes the TUI.
let lastStatus: DeckStatus | undefined
// The reason written with lastStatus (waiting_action only).
let lastReason: string | undefined

// Why the model waits on the user, from wait_for_user in the current main turn.
// Written as waiting_action when the turn ends; the next main turn clears it.
let waitReason: string | undefined

// Whether the Stop check has stopped the current main turn once.
// WHY 1 回だけ: 判定は文の言い回しで見るので誤検知がある。止め続けるとターンが終わらない。
let nudgedThisTurn = false

// Tool calls (main loop and subagents) between tool.call and its result.
// WHY 数える: 並行に走るツール呼び出しの 1 つが終わった時点で Running に戻すと、
// 別の呼び出しの承認ダイアログが開いたままでも Approve 待ちが消える。
let inFlight = 0

// Whether the main loop is inside a turn. A subagent started in the background
// outlives the turn that started it, and its events arrive while this is false.
let inMainTurn = false

/**
 * The claude-deck binary and this session's deck ID. claude-deck sets both
 * variables when it starts the session; in any other session there is none and
 * the mod does nothing.
 */
async function deckEnv($: EngineInterface): Promise<{ bin: string; id: string } | undefined> {
  const [bin, id] = await Promise.all([$.env.get('CLAUDE_DECK_BIN'), $.env.get('CLAUDE_DECK_SESSION_ID')])
  return bin && id ? { bin, id } : undefined
}

/**
 * Runs `claude-deck hook <args> --session <id>` and resolves whether it succeeded.
 * Failures are dropped: a missed status must not fail the user's turn.
 */
function deck($: EngineInterface, args: string[]): Promise<boolean> {
  const run = async (): Promise<boolean> => {
    try {
      const env = await deckEnv($)
      if (!env) return false
      const { exitCode } = await $.process.run([env.bin, 'hook', ...args, '--session', env.id], {
        timeoutMs: HOOK_TIMEOUT_MS,
      })
      return exitCode === 0
    } catch {
      return false
    }
  }
  queue = queue.then(run, run)
  return queue
}

/**
 * What the model is told when the session starts.
 * WHY 実際のパスと ID を書く: バイナリは PATH に無いことがあり、close は自分の ID を避ける必要がある。
 * 使い方はスキル（skills/claude-deck）に置き、ここには要るときにスキルを読むための手がかりだけを書く。
 * WHY alias だけはここにも書く: スキルは CLI を使う用事ができたときにしか読まれない。
 * 頼まれなくても付けてほしい操作は、最初から見える場所に書かないと実行されない。
 */
function deckContext(env: { bin: string; id: string }): string {
  return [
    'このセッションは claude-deck（Claude Code のセッションを tmux と jj ワークスペースで管理するダッシュボード）が起動している。',
    `deck のセッション ID: ${env.id}`,
    `CLI: ${env.bin} new | list | close で、別のセッションを作る・一覧する・閉じることができる。`,
    `作業の内容が分かったら、頼まれなくても ${env.bin} alias <英数字と - _ .> でこのセッションに名前を付け、内容が変わったら付け直す。利用者は一覧のこの名前でセッションを探す。`,
    '使う前に deck-status:claude-deck スキルを読む。',
  ].join('\n')
}

async function setStatus($: EngineInterface, status: DeckStatus, reason?: string): Promise<void> {
  if (status === lastStatus && reason === lastReason) return
  // WHY いつも --reason を渡す: 呼び出しの形を 1 つにして、Go の契約のテスト（hook_contract_test.go）が読めるようにする。
  // 空の --reason は、理由が無いという意味で CLI が受け付ける。
  if (await deck($, ['status', status, '--reason', reason ?? ''])) {
    lastStatus = status
    lastReason = reason
  }
}

function isWaiting(): boolean {
  return lastStatus === 'waiting_approval' || lastStatus === 'waiting_answer'
}

/**
 * Whether a subagent has work left.
 * WHY 自分で数えず $.agent.list() に聞く: 開始（agent.spawn）と終了（turn.complete）を数えると、
 * Mod の読み込み直しで数が失われ、終了を 1 つ取りこぼすだけで実行中の表示が残り続ける。
 * 注意: ワークフローのエージェントは一覧に出ない（型定義 AgentLoop.agentId の説明）。
 */
async function hasActiveSubagent($: EngineInterface): Promise<boolean> {
  try {
    const agents = await $.agent.list()
    return agents.some(a => a.status === 'pending' || a.status === 'running' || a.status === 'waiting')
  } catch {
    return false
  }
}

/**
 * Writes the status of a session whose main loop is not in a turn.
 * A wait the model reported outranks subagents: the user has something to do either way.
 */
async function setRestingStatus($: EngineInterface): Promise<void> {
  if (waitReason !== undefined) {
    await setStatus($, 'waiting_action', waitReason)
    return
  }
  await setStatus($, (await hasActiveSubagent($)) ? 'subagent_running' : 'idle')
}

/** Whether the Stop check stops the turn: once per main turn, when the model asked the user to act without the tool. */
export function shouldNudge(stop: {
  isMain: boolean
  stopHookActive: boolean
  calledWait: boolean
  nudged: boolean
  lastMessage: string
}): boolean {
  if (!stop.isMain || stop.stopHookActive || stop.calledWait || stop.nudged) return false
  return asksUserToAct(stop.lastMessage)
}

/** Checks the reason given to wait_for_user; returns the error to answer, or the reason. */
export function parseWaitReason(input: unknown): { reason: string } | { error: string } {
  // A plugin tool's input fields arrive on the tool.call event itself (Claude Code 2.1.331 の claude plugin test で確認).
  const reason: unknown = typeof input === 'object' && input !== null ? Reflect.get(input, 'reason') : undefined
  if (typeof reason !== 'string' || reason.trim() === '') {
    return { error: `${WAIT_TOOL}: reason is required: what the user has to do, in one short sentence.` }
  }
  // WHY 改行を断る: 一覧の 2 行目に 1 行で出すので、改行があると行が崩れる。
  if (/[\r\n]/.test(reason.trim())) {
    return { error: `${WAIT_TOOL}: reason must be one line. Call again without line breaks.` }
  }
  const length = [...reason.trim()].length
  if (length > MAX_WAIT_REASON) {
    return {
      error: `${WAIT_TOOL}: reason is ${length} characters; the list shows at most ${MAX_WAIT_REASON}. Shorten it and call again.`,
    }
  }
  return { reason: reason.trim() }
}

/**
 * Whether the model's last message asks the user to act outside the conversation:
 * a `! command` at the start of a line or inside backquotes, or a request word
 * (ログイン, 承認, …) and a please (ください, お願いします) in one sentence.
 * WHY `! ` はコードブロックの中も見る: `! コマンド` はコードブロックで示すのがふつうの頼み方。
 * WHY `!` の後が ASCII のコマンド名のときだけ数える: 「`! コマンド`」「`! <コマンド>`」のような
 * 書き方の説明を頼みと取り違えた（2026-11-25、wait_for_user を説明した返答で止まった）。
 * WHY 依頼の文はコードブロックとかぎ括弧・引用符の中を見ない: 例や引用（「… を実行してください」）は
 * 頼みでなく紹介のことが多い（同じ返答で止まった）。
 * WHY 文を「。」「！」「？」と改行で区切る: `.` は URL やコマンドに出る。
 */
export function asksUserToAct(text: string): boolean {
  if (/(^|\n)[ \t]*! [A-Za-z0-9.\/_~-]/.test(text) || /`! [A-Za-z0-9.\/_~-][^`]*`/.test(text)) return true
  return text
    .replace(/```[\s\S]*?```/g, '')
    .replace(/「[^」]*」|『[^』]*』|“[^”]*”|"[^"\n]*"/g, '')
    .split(/[。！？\n]/)
    .some(sentence => /ログイン|承認|認証|打って|押して|実行して/.test(sentence) && /ください|お願いします/.test(sentence))
}

export const register: Register = on => {
  // The Claude session ID is linked here: on startup, resume and fork the first one,
  // on /clear and compact the new one. Those also drop the earlier context, so
  // the claude-deck note is handed to the model on every source.
  // WHY next(e) より先に登録する: Claude Code は最初のプロンプトの前に session.start の hook を待つので、
  // 先に登録すれば最初のターンからツールが見える（Mods の型定義の session.start の説明、Claude Code 2.1.331）。
  // deck のセッションでだけ登録する。
  on('session.start', async ($, e, next) => {
    if (await deckEnv($)) {
      await $.tool.register({ name: WAIT_TOOL, description: WAIT_TOOL_DESCRIPTION, inputSchema: WAIT_TOOL_SCHEMA, isDeferred: false })
    }
    return next(e)
  }).catch(($, e, next) => next(e))

  on('classic.SessionStart', async ($, e, next) => {
    if (e.agent_id !== undefined) return next(e)
    await deck($, ['session-start', '--claude-session-id', e.session_id, '--source', e.source])
    // /clear drops the conversation that asked the user to act, so the wait goes with it.
    if (e.source === 'clear' && waitReason !== undefined) {
      waitReason = undefined
      await setRestingStatus($)
    }
    const [env, result] = await Promise.all([deckEnv($), next(e)])
    if (!env) return result
    return { ...result, additionalContext: [...(result.additionalContext ?? []), deckContext(env)] }
  }).catch(($, e, next) => next(e))

  // WHY agentId で分ける: turn.start はサブエージェントの実行でも発火する（Claude Code 2.1.327 の実機で確認）。
  // メインの開始として running にすると、メインが止まっている間に Mod が立てたエージェントの後、
  // メインの turn.complete が来ないので running のまま残る（その報告を別の Mod が prompt.submit で止めたとき。
  // 止められた prompt.submit では、メインの turn.start も turn.complete も発火しない。同じ版で確認）。
  on('turn.start', async ($, e, next) => {
    if (e.agentId === undefined) {
      inMainTurn = true
      waitReason = undefined
      nudgedThisTurn = false
      await setStatus($, 'running')
    } else if (!inMainTurn && !isWaiting() && waitReason === undefined) {
      await setStatus($, 'subagent_running')
    }
    return next(e)
  })

  // next(e) holds the approval dialog and AskUserQuestion's wait: when it resolves,
  // the user has answered and Claude is working again.
  // WHY Mod が起こした呼び出しをメインから除く: Mod の $.agent.spawn は、agentId の無い Agent の tool.call として、
  // メインのターンの外でも届く。メインの呼び出しとして running にすると、メインの turn.complete が来ないので
  // running のまま残る。その tool_use_id は toolu_plugin_ で始まる（Claude Code 2.1.327 の実機で確認。型定義には無い）。
  on('tool.call', async ($, e, next) => {
    const isMain = e.agentId === undefined && !(e.tool_use_id ?? '').startsWith(PLUGIN_TOOL_USE_PREFIX)
    if (isMain) inMainTurn = true
    inFlight++
    try {
      if (isMain && e.tool === ASK_USER_QUESTION) {
        await setStatus($, 'waiting_answer')
      } else if (isMain && !isWaiting()) {
        await setStatus($, 'running')
      }
      return await next(e)
    } finally {
      inFlight--
      // A subagent's approval dialog also waits on the user (PermissionRequest below),
      // so its end clears a wait too. Otherwise a subagent leaves the main loop's status alone.
      if (inFlight === 0 && (isMain || isWaiting())) {
        // Outside the main loop's turn, a background subagent's or a plugin's dialog was answered.
        if (inMainTurn) await setStatus($, 'running')
        else await setRestingStatus($)
      }
    }
  }).catch(($, e, next) => next(e))

  // WHY tool.check の ask で判定しない: auto モードでは ask でもダイアログを出さずに実行される。
  // ダイアログが出たときだけ PermissionRequest が発火する（Claude Code 2.1.287 で確認）。
  // サブエージェントのダイアログも利用者を待たせるので agent_id で除外しない。
  // The call only notes the reason: the list changes when the turn ends (turn.complete below),
  // so it does not show a wait while the model is still writing its answer.
  // WHY サブエージェントからは断る: waiting_action はメインのターンの終わりにだけ入る状態で、
  // サブエージェントの理由をいつ消すかの規則が要らなくなる。
  on('tool.call', { tool: WAIT_TOOL_ID }, async (_$, e) => {
    if (e.agentId !== undefined) return { deny: `${WAIT_TOOL} is for the main conversation only.` }
    const parsed = parseWaitReason(e)
    if ('error' in parsed) return { deny: parsed.error }
    waitReason = parsed.reason
    return { result: 'claude-deck lists this session as waiting for your action once the turn ends. End your turn now.' }
  }).catch((_$, _e, next) => ({ deny: `${WAIT_TOOL} failed: ${next.error.message}` }))

  // WHY classic.Stop で見る: last_assistant_message と stop_hook_active が来る。block で同じターンを続けさせられる
  // （Claude Code 2.1.331 の型定義 StopHookInput と ClassicResult.block）。
  on('classic.Stop', async ($, e, next) => {
    const stop = {
      // Stop is the main loop's; a subagent's end is SubagentStop. agent_id is checked in case that changes.
      isMain: e.agent_id === undefined,
      stopHookActive: e.stop_hook_active,
      calledWait: waitReason !== undefined,
      nudged: nudgedThisTurn,
      lastMessage: e.last_assistant_message ?? '',
    }
    if (!shouldNudge(stop) || !(await deckEnv($))) return next(e)
    nudgedThisTurn = true
    $.ui.log(`deck-status: stopped the turn once to ask for ${WAIT_TOOL}`, { to: 'debug' })
    return { ...(await next(e)), block: WAIT_NUDGE }
  }).catch(($, e, next) => next(e))

  on('classic.PermissionRequest', async ($, e, next) => {
    await setStatus($, e.tool_name === ASK_USER_QUESTION ? 'waiting_answer' : 'waiting_approval')
    return next(e)
  }).catch(($, e, next) => next(e))

  // WHY classic.Stop でなく turn.complete: 承認ダイアログで拒否したターンは Stop を発火しない
  // （Claude Code 2.1.287 と 2.1.295 で確認）。turn.complete は reason に answer / aborted / refusal / error を持ち、
  // 中断や API エラーで終わったターンでも発火する（Mods の型定義 TurnCompleteReason）。
  // WHY サブエージェントのターンでも見る: バックグラウンドのサブエージェントは、メインのターンが
  // 終わった後も動き続ける。メインが止まっている間に最後の 1 つが終わったら idle にする。
  // この時点で、終わったエージェントは一覧で completed になっている（Claude Code 2.1.296 で確認）。
  // メインの turn.complete の時点では、バックグラウンドのサブエージェントは一覧に running で出る（同じ版で確認）。
  // WHY NOT classic.Stop の background_tasks で数える: シェルやワークフローも入るが、メインが止まったまま
  // サブエージェントが終わったときに読み直す契機が無い。
  on('turn.complete', async ($, e, next) => {
    if (e.agentId === undefined) {
      inMainTurn = false
      if (nudgedThisTurn) {
        $.ui.log(`deck-status: after the stop, ${WAIT_TOOL} was ${waitReason === undefined ? 'not ' : ''}called`, { to: 'debug' })
      }
      await setRestingStatus($)
    } else if (lastStatus === 'subagent_running') {
      await setRestingStatus($)
    }
    return next(e)
  })

  // The rate limits are the account's, not the session's: every deck session
  // reports them to the one file the TUI watches, and the last report wins.
  // The JSON is read by ratelimits.ParseMeasured in Go.
  on('session.measure', async ($, e, next) => {
    if (e.changed.includes('rateLimits') && e.rateLimits.length > 0) {
      await deck($, ['rate-limits', JSON.stringify(e.rateLimits)])
    }
    return next(e)
  })
}
