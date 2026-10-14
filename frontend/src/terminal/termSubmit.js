import { getTerm, surfaceWrite } from './termSession.js'
import { ask, runShellInTerminal, writeTerminal, textToBase64, backendAPI, hasAgentTurn } from '../store.js'
import { resolveInput } from '../inputRoute.js'
import * as paint from '../termPaint.js'

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
  const t = getTerm(key)
  if (!t) return
  const { hostId, sessionId } = t
  const text = (line || '').trim()
  if (!text) {
    // 空回车：与 composer 同形，只给 PTY 一个换行让 shell 回显新提示符。
    writeTerminal(hostId, sessionId, textToBase64('\n'))
    return
  }
  // 输入分流：agent 对话中（本会话已有 Agent 回复/工具调用）一律当回复交给 Agent，
  // 不做命令识别/执行；要跑命令用 ! 前缀或原始终端。
  const route = await resolveInput(text, backendAPI(), hostId, hasAgentTurn(hostId, sessionId))
  await dispatchLine(key, text, route)
}

// dispatchLine 真正把一行交出去：agent 走 ask，shell 走硬拒绝过滤 + PTY。
async function dispatchLine(key, text, route) {
  const t = getTerm(key)
  if (!t) return
  const { hostId, sessionId } = t
  if (route.kind === 'agent') {
    if (!route.text) {
      paint.paintSystem(surfaceWrite(key), '输入要问 Agent 的内容，或直接敲一条 shell 命令。')
      return
    }
    if (route.missing) {
      const name = route.binary ? `\`${route.binary}\`` : '这条命令'
      paint.paintSystem(surfaceWrite(key), `未找到命令 ${name}，已作为问题交给 Agent 理解你的意图…`)
    }
    ask(route.text)
    return
  }
  // shell：交回常驻终端通道（硬拒绝过滤 + 写进 PTY，PTY 自己回显执行）。
  await runShellInTerminal(hostId, sessionId, route.text)
}
