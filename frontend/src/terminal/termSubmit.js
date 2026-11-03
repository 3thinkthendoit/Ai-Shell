import { getTerm, surfaceWrite } from './termSession.js'
import { ask, runShellInTerminal, writeTerminal, textToBase64, backendAPI, agentAwaitingReply, markShellRun, termState } from '../store.js'
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
  // 输入分流：**终端屏幕状态优先于会话时间线**。
  //
  // 提示符状态是用户肉眼可见的事实（`root@…:~$` 停在屏幕上），时间线不是：
  // AI 上一轮说过话后时间线以 assistant/tool 结尾，会话跨重连、跨清屏一直
  // 保持这个形状 —— 若据此判定 agentMode，用户对着空提示符敲 `ls` 会被
  // Agent 劫持（实测事故：重连后第一条命令全被吞，还烧掉一轮 token）。
  // 终端空闲在提示符（无命令运行、无全屏接管、无透传窗口）→ 一律命令优先；
  // 终端忙（状态不可得之外的任何忙态其实走不到这里）才退回时间线判断。
  // 自然语言不受影响：`好的`/`2`/中文提问靠文本规则照走 Agent。
  const st = termState(hostId, sessionId)
  const atPrompt = !!st && !st.cmdActive && !st.tuiActive && !st.cmdPending
  const agentMode = atPrompt ? false : agentAwaitingReply(hostId, sessionId)
  const route = await resolveInput(text, backendAPI(), hostId, agentMode)
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
  // 记录一次直跑：AI 若在等回复，从此回到命令模式。
  markShellRun(hostId, sessionId)
  await runShellInTerminal(hostId, sessionId, route.text)
}
