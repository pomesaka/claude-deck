import type { EngineInterface, Register } from 'claude-code'

// Statuses as the store spells them (session.Status.ID in Go).
type DeckStatus = 'running' | 'idle' | 'waiting_approval' | 'waiting_answer'

const ASK_USER_QUESTION = 'AskUserQuestion'
const HOOK_TIMEOUT_MS = 5_000

// WHY 直列化: hook はほぼ同時に続けて発火する（tool.call の前後と PermissionRequest など）。
// 書き込みを並行に走らせると後の状態が先に store に入り、古い状態で上書きされることがある。
let queue: Promise<boolean> = Promise.resolve(true)

// WHY 判定用の hook に .catch(next(e)) を付ける: 状態の報告に失敗しても、ツール呼び出しや承認の流れは止めない。
// deck() は失敗をすべて握りつぶすので、next を呼んだ後に例外が出ることはない。

// The last status this process wrote successfully. Repeating it is skipped:
// tool.call fires for every tool, and each write spawns a process and wakes the TUI.
let lastStatus: DeckStatus | undefined

// Tool calls (main loop and subagents) between tool.call and its result.
// WHY 数える: 並行に走るツール呼び出しの 1 つが終わった時点で Running に戻すと、
// 別の呼び出しの承認ダイアログが開いたままでも Approve 待ちが消える。
let inFlight = 0

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
 */
function deckContext(env: { bin: string; id: string }): string {
  return [
    'このセッションは claude-deck（Claude Code のセッションを tmux と jj ワークスペースで管理するダッシュボード）が起動している。',
    `deck のセッション ID: ${env.id}`,
    `CLI: ${env.bin} new | list | close で、別のセッションを作る・一覧する・閉じることができる。`,
    '使う前に deck-status:claude-deck スキルを読む。',
  ].join('\n')
}

async function setStatus($: EngineInterface, status: DeckStatus): Promise<void> {
  if (status === lastStatus) return
  if (await deck($, ['status', status])) {
    lastStatus = status
  }
}

function isWaiting(): boolean {
  return lastStatus === 'waiting_approval' || lastStatus === 'waiting_answer'
}

export const register: Register = on => {
  // The Claude session ID is linked here: on startup, resume and fork the first one,
  // on /clear and compact the new one. Those also drop the earlier context, so
  // the claude-deck note is handed to the model on every source.
  on('classic.SessionStart', async ($, e, next) => {
    if (e.agent_id !== undefined) return next(e)
    await deck($, ['session-start', '--claude-session-id', e.session_id, '--source', e.source])
    const [env, result] = await Promise.all([deckEnv($), next(e)])
    if (!env) return result
    return { ...result, additionalContext: [...(result.additionalContext ?? []), deckContext(env)] }
  }).catch(($, e, next) => next(e))

  // A subagent's run raises no turn.start, so this is the main loop.
  on('turn.start', async ($, e, next) => {
    await setStatus($, 'running')
    return next(e)
  })

  // next(e) holds the approval dialog and AskUserQuestion's wait: when it resolves,
  // the user has answered and Claude is working again.
  on('tool.call', async ($, e, next) => {
    const isMain = e.agentId === undefined
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
        await setStatus($, 'running')
      }
    }
  }).catch(($, e, next) => next(e))

  // WHY tool.check の ask で判定しない: auto モードでは ask でもダイアログを出さずに実行される。
  // ダイアログが出たときだけ PermissionRequest が発火する（Claude Code 2.1.287 で確認）。
  // サブエージェントのダイアログも利用者を待たせるので agent_id で除外しない。
  on('classic.PermissionRequest', async ($, e, next) => {
    await setStatus($, e.tool_name === ASK_USER_QUESTION ? 'waiting_answer' : 'waiting_approval')
    return next(e)
  }).catch(($, e, next) => next(e))

  // WHY classic.Stop でなく turn.complete: 承認ダイアログで拒否したターンは Stop を発火しない
  // （Claude Code 2.1.287 と 2.1.295 で確認）。turn.complete は reason に answer / aborted / refusal / error を持ち、
  // 中断や API エラーで終わったターンでも発火する（Mods の型定義 TurnCompleteReason）。
  on('turn.complete', async ($, e, next) => {
    if (e.agentId === undefined) {
      await setStatus($, 'idle')
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
