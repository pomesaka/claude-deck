import type { EngineInterface, Register } from 'claude-code'

// Statuses as the store spells them (session.Status.ID in Go).
type DeckStatus = 'running' | 'idle' | 'waiting_approval' | 'waiting_answer' | 'subagent_running' | 'waiting_action'

const ASK_USER_QUESTION = 'AskUserQuestion'
// WHY 80 文字: 一覧の 2 行目の末尾に 1 行で出す（Go の側は長さを見ない）。
const MAX_WAIT_REASON = 80
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

// What the user has to do: run the `! command` found in the last message of the main turn (classic.Stop),
// or send anything after an API error ended the turn (turn.complete).
// Written as waiting_action when the turn ends; the next main turn and /clear clear it.
let waitReason: string | undefined

const API_ERROR_REASON = 'API の誤りで止まりました。何か送ると再開します'

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
 * A `! command` the user was asked to run outranks subagents: the user has something to do either way.
 */
async function setRestingStatus($: EngineInterface): Promise<void> {
  if (waitReason !== undefined) {
    await setStatus($, 'waiting_action', waitReason)
    return
  }
  await setStatus($, (await hasActiveSubagent($)) ? 'subagent_running' : 'idle')
}

// A `! command` the user is asked to type: `! ` at the start of a line, with an
// ASCII command name right after it. Lines in code blocks count: a `! command`
// is usually shown in one.
// WHY ASCII のコマンド名のときだけ: 「! コマンド」「! <コマンド>」のような書き方の説明を
// 頼みと取り違えた（2026-11-25）。
// WHY NOT バッククォートの中の `! ` を数える: 説明の文に例として書いたコマンドで操作待ちになる（pomesaka と決めた）。
// WHY NOT 「ログイン」「ください」のような文の言い回しで見る: 引用や説明を頼みと取り違え、
// 裏の verifier を待つだけのターンも操作待ちに見えた（pomesaka と決めた）。
const BANG_COMMAND = /(?:^|\n)[ \t]*! ([A-Za-z0-9.\/_~-][^\n]*)/

/**
 * The wait the main loop's last message asks for: what to show in the list for
 * the first `! command` in it, or undefined when there is none.
 */
export function detectWait(text: string): string | undefined {
  const m = BANG_COMMAND.exec(text)
  const command = m?.[1]?.trim()
  if (!command) return undefined
  const reason = (c: string) => `\`! ${c}\` の実行を待っています`
  const room = MAX_WAIT_REASON - [...reason('')].length
  const chars = [...command]
  return reason(chars.length <= room ? command : `${chars.slice(0, room - 1).join('')}…`)
}

export const register: Register = on => {
  // The Claude session ID is linked here: on startup, resume and fork the first one,
  // on /clear and compact the new one. Those also drop the earlier context, so
  // the claude-deck note is handed to the model on every source.
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

  // WHY classic.Stop で見る: ターンの最後の返答（last_assistant_message）が来るのはここだけ
  // （Claude Code 2.1.331 の型定義 StopHookInput）。Stop はメインのもので、サブエージェントの終わりは SubagentStop。
  // Stop only notes the wait: the list changes when the turn ends (turn.complete below).
  // WHY 既にターンが終わっていたら書く: Stop と turn.complete のどちらが先に届くかは確かめていない。
  on('classic.Stop', async ($, e, next) => {
    if (e.agent_id === undefined) {
      waitReason = detectWait(e.last_assistant_message ?? '')
      if (!inMainTurn) await setRestingStatus($)
    }
    return next(e)
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
  // WHY API の誤りを利用者の操作待ちにする: 通信が落ちたセッションは idle になり、報告を待っているだけの
  // セッションと一覧で見分けられなかった。再開するには利用者が何か送るしかないので、! コマンドと同じ待ちにする。
  // reason が error になるのは、Claude Code が自分で再試行し終えた後（Mods の型定義 TurnCompleteReason）。
  on('turn.complete', async ($, e, next) => {
    if (e.agentId === undefined) {
      inMainTurn = false
      if (e.reason === 'error') waitReason = API_ERROR_REASON
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
