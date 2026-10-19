import { getTerm, surfaceWrite } from './termSession.js'
import { ask, runShellInTerminal, writeTerminal, textToBase64, backendAPI, hasAgentTurn } from '../store.js'
import { resolveInput } from '../inputRoute.js'
import * as paint from '../termPaint.js'
import { t } from '../i18n'

// submitQueue 让同一块表面（同一任务）的提交串行：onData 是同步回调没法 await，
// 连敲回车会让多个 runShellInTerminal 并发（store.busy 交错、PTY 输出互踩）。
const submitQueue = new Map()

export function enqueueSubmit(key, line) {
  const prev = submitQueue.get(key) || Promise.resolve()
  const next = prev.then(() => submitLine(key, line)).catch(() => {})
  submitQueue.set(key, next)
  return next
}

async function submitLine(key, line) {
  // 注意：局部变量不能叫 t —— 它会遮蔽 i18n 的翻译函数 t()（命令分支要用）。
  const state = getTerm(key)
  if (!state) return
  const { hostId, sessionId } = state
  const text = (line || '').trim()
  if (!text) {
    // 空回车：与 composer 同形，只给 PTY 一个换行让 shell 回显新提示符。
    writeTerminal(hostId, sessionId, textToBase64('\n'))
    return
  }
  // 输入分流：已知 Linux 命令始终走 shell。
  // 未知词（含单个 bt）先做远端探测，存在就执行。
  // 对话中的回复同形词（id/true）和 yes/open 当回复，不执行。
  const route = await resolveInput(text, backendAPI(), hostId, hasAgentTurn(hostId, sessionId))
  await dispatchLine(key, text, route)
}

// dispatchLine 真正把一行交出去：agent 走 ask，shell 走硬拒绝过滤 + PTY。
async function dispatchLine(key, text, route) {
  const state = getTerm(key)
  if (!state) return
  const { hostId, sessionId } = state
  if (route.kind === 'agent') {
    if (!route.text) {
      paint.paintSystem(surfaceWrite(key), t('console.submit.emptyHint'))
      return
    }
    if (route.missing) {
      const name = route.binary ? `\`${route.binary}\`` : t('console.submit.thisCmd')
      paint.paintSystem(surfaceWrite(key), t('console.submit.unknownCmd', { name }))
    }
    ask(route.text)
    return
  }
  // shell：交回常驻终端通道（硬拒绝过滤 + 写进 PTY，PTY 自己回显执行）。
  await runShellInTerminal(hostId, sessionId, route.text)
}
