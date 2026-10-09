import type { EngineInterface, Register } from 'claude-code'

// Statuses as the store spells them (session.Status.ID in Go).
type DeckStatus = 'running' | 'idle' | 'waiting_approval' | 'waiting_answer'

const ASK_USER_QUESTION = 'AskUserQuestion'
const HOOK_TIMEOUT_MS = 5_000

// WHY 直列化: hook はほぼ同時に続けて発火する（tool.call の前後と PermissionRequest など）。
// 書き込みを並行に走らせると後の状態が先に store に入り、古い状態で上書きされることがある。
let queue: Promise<void> = Promise.resolve()

// WHY 判定用の hook に .catch(next(e)) を付ける: 状態の報告に失敗しても、ツール呼び出しや承認の流れは止めない。
// deck() は失敗をすべて握りつぶすので、next を呼んだ後に例外が出ることはない。

// The last status this process wrote. Repeating it is skipped: tool.call fires for
// every tool, and each write spawns a process and wakes the TUI.
let lastStatus: DeckStatus | undefined

/**
 * Runs `claude-deck hook <args> --session <id>`. claude-deck sets both variables
 * when it starts the session; in any other session the mod does nothing.
 * Failures are dropped: a missed status must not fail the user's turn.
 */
function deck($: EngineInterface, args: string[]): Promise<void> {
  const run = async () => {
    try {
      const [bin, id] = await Promise.all([$.env.get('CLAUDE_DECK_BIN'), $.env.get('CLAUDE_DECK_SESSION_ID')])
      if (!bin || !id) return
      await $.process.run([bin, 'hook', ...args, '--session', id], { timeoutMs: HOOK_TIMEOUT_MS })
    } catch {
      // dropped; see above
    }
  }
  queue = queue.then(run, run)
  return queue
}

function setStatus($: EngineInterface, status: DeckStatus): Promise<void> {
  if (status === lastStatus) return queue
  lastStatus = status
  return deck($, ['status', status])
}

export const register: Register = on => {
  // The Claude session ID is linked here: on startup, resume and fork the first one,
  // on /clear and compact the new one.
  on('classic.SessionStart', async ($, e, next) => {
    if (e.agent_id === undefined) {
      await deck($, ['session-start', '--claude-session-id', e.session_id, '--source', e.source])
    }
    return next(e)
  }).catch(($, e, next) => next(e))

  // A subagent's run raises no turn.start, so this is the main loop.
  on('turn.start', async ($, e, next) => {
    await setStatus($, 'running')
    return next(e)
  })

  // next(e) holds the approval dialog and AskUserQuestion's wait: when it resolves,
  // the user has answered and Claude is working again.
  on('tool.call', async ($, e, next) => {
    if (e.agentId !== undefined) {
      const result = await next(e)
      // A subagent's approval dialog also waits on the user (PermissionRequest below).
      if (lastStatus === 'waiting_approval' || lastStatus === 'waiting_answer') {
        await setStatus($, 'running')
      }
      return result
    }
    await setStatus($, e.tool === ASK_USER_QUESTION ? 'waiting_answer' : 'running')
    const result = await next(e)
    await setStatus($, 'running')
    return result
  }).catch(($, e, next) => next(e))

  // WHY tool.check の ask で判定しない: auto モードでは ask でもダイアログを出さずに実行される。
  // ダイアログが出たときだけ PermissionRequest が発火する（Claude Code 2.1.287 で確認）。
  // サブエージェントのダイアログも利用者を待たせるので agent_id で除外しない。
  on('classic.PermissionRequest', async ($, e, next) => {
    await setStatus($, e.tool_name === ASK_USER_QUESTION ? 'waiting_answer' : 'waiting_approval')
    return next(e)
  }).catch(($, e, next) => next(e))

  // WHY classic.Stop でなく turn.complete: 承認ダイアログで拒否したターンは Stop を発火しない
  // （Claude Code 2.1.287 で確認）。turn.complete は拒否・中断・API エラーのどれでも発火する。
  on('turn.complete', async ($, e, next) => {
    if (e.agentId === undefined) {
      await setStatus($, 'idle')
    }
    return next(e)
  })
}
