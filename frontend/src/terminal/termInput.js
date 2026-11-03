import { store, writeTerminal, textToBase64, stop, isPtyDirty, termState, backendAPI } from '../store.js'
import * as paint from '../termPaint.js'
import { enqueueSubmit } from './termSubmit.js'
import { hasCJK } from '../inputRoute.js'
import { endCmdPending, pendingLine, appendPendingLine, clearPendingLine, popPendingLine } from './termEvents.js'
import {
  getTerm,
  isFullscreen,
  isCmdRunning,
  cursorCol,
  displayWidth,
  eraseLocalLine,
  clearLocalEcho,
  flushLocalToPty,
  clearWelcomeHintByKey,
  clearLLMHintByKey,
  surfaceWrite
} from './termSession.js'

// 终端内直接输入：在提示符下就地编辑、回车分类。
//
// 真 PTY 逐字符透传的话，自然语言会被 bash 当命令执行而报错。所以把
// 「shell 提示符下等待输入一条命令」这一常态改成本地行编辑：可打印字符
// 本地回显、退格本地删，回车时 resolveInput 决定交给 Agent 还是发回 PTY。

// 乐观透传窗口（cmdPending）的回车复核。
//
// 窗口期间按键一律直达 PTY，RunShellInTerminal 的硬拒绝链被绕过；命令输出
// 结束后窗口还要静默兜底 6 秒才关，这段时间里提示符早已回来，下一条命令
// 会直灌 bash。这里把窗口内敲的可见字符攒成行，回车时交后端复核
// （CheckShellCommand）：**只有明确 Deny 才拦**，其余一律放行——vim 菜单、
// read 应答、E325 直接回车等交互场景必须零改动，所以宁可漏拦不可误拦。
const gateBusy = new Map() // key → 复核进行中（期间到的输入一律排队，复核后按序补发）
const gateQueues = new Map() // key → 复核期间 hold 住的输入（保序）

// 光标移动键（左/右/Home/End，普通与应用键盘模式两套序列）：不动行内容，
// 攒的行继续有效 —— 见下面 gatePendingKey 里的分支注释。
const CURSOR_KEYS = new Set(['\x1b[D', '\x1b[C', '\x1b[H', '\x1b[F', '\x1b[1~', '\x1b[4~', '\x1bOD', '\x1bOC', '\x1bOH', '\x1bOF'])

// flushGateQueue 把复核期间 hold 住的输入按到达顺序重放。
//
// 重放走 onTermData 的正常路径，就像用户刚刚才敲了它们：被拒绝后窗口已
// 关 → 落到本地行编辑（用户继续打字不受影响）；窗口还开着 → 继续攒新行，
// 队列里若有回车会照常触发新一轮复核。
function flushGateQueue(key) {
  const q = gateQueues.get(key) || []
  gateQueues.delete(key)
  for (const item of q) onTermData(key, item)
}

function gatePendingKey(key, t, d, send) {
  const st = termState(t.hostId, t.sessionId)
  // 只管乐观窗口：真 cmdActive（OSC 133 命令边界）时命令起止有权威信号，
  // 键属于前台程序，轮不到本地裁决。
  if (!st || !st.cmdPending || st.cmdActive) return true
  if (gateBusy.get(key)) {
    // 复核进行中：所有输入先排队。不排队的话，回车后敲的字符会立即直达
    // PTY —— 先于被 hold 的回车抵达远端，被并进当前命令行（输入顺序
    // 颠倒，用户看到的是命令被偷改）。
    const q = gateQueues.get(key) || []
    q.push(d)
    gateQueues.set(key, q)
    return false
  }
  if (d === '\r') {
    const line = pendingLine(key).trim()
    if (!line) return true // 空行直接放行（vim E325 空回车选默认）
    gateBusy.set(key, true)
    backendAPI().CheckShellCommand(t.hostId, line).then(res => {
      gateBusy.set(key, false)
      if (res && res.status === 'denied') {
        // 命令字符早已透传进远端行缓冲：Ctrl-U（bash kill-line）清掉，
        // 再把裁决画到终端——观感与正常硬拒绝路径一致。既然用户是在
        // 提示符下敲命令，说明窗口早该关了，顺手关掉恢复本地行编辑。
        send(textToBase64('\x15'))
        paint.paintShellVerdict(surfaceWrite(key), res)
        endCmdPending(t.hostId, t.sessionId)
      } else {
        send(textToBase64('\r')) // 放行：补发被 hold 的回车，顺序不变
        // 行已被这次回车消费：不清掉的话，下一次复核拿到的是
        // 「旧命令 + 新输入」的拼接体。
        clearPendingLine(key)
      }
      flushGateQueue(key)
    }).catch(() => {
      gateBusy.set(key, false)
      send(textToBase64('\r')) // 复核通道失败宁可放行，别把交互程序卡死
      clearPendingLine(key)
      flushGateQueue(key)
    })
    return false // 本次回车先不发，等复核结果
  }
  if (d === '\x7f') {
    // 退格：远端 readline 删一个字符，攒行同步弹尾而不是作废整行 ——
    // 敲错一个字改回来，不该失去整行的硬拒绝保护。
    popPendingLine(key)
    return true
  }
  if (CURSOR_KEYS.has(d)) {
    // 光标移动不动行内容，攒的行继续有效。历史上所有控制键一律作废攒行，
    // 「敲完命令按一下方向键再回车」就成了绕过硬拒绝的旁路 —— 而方向键
    // 其实一个字都没改。光标位置不可知：之后敲的字符我们仍按行尾追加，
    // 内容集合一致、顺序可能不同，对内容级复核够用。
    return true
  }
  if (/[\x00-\x1f\x7f]/.test(d)) {
    // 其余控制字符（上/下历史键会让远端整行替换、Tab 补全会改写行、
    // Ctrl-C 等）：攒的行已不可靠，作废放行——漏拦的风险交给下一次
    // 干净的行。
    clearPendingLine(key)
    return true
  }
  appendPendingLine(key, d)
  return true
}

export function onTermData(key, d) {
  const t = getTerm(key)
  if (!t) return
  const send = s => writeTerminal(t.hostId, t.sessionId, s)

  // 命令运行中（OSC 133;C..D / cmdPending）：输入归前台程序，按键原样透传。
  // 例外：乐观窗口（仅 cmdPending、非全屏）里敲到中文 → 关窗改走本地行编辑。
  // 中文是自然语言提问，不是 vim E325 / 菜单选项；直灌 bash 会报 command not found，
  // Agent 也收不到（`执行报错 ./start.sh` 用户截图）。真 cmdActive 仍全透传。
  if (isCmdRunning(key)) {
    const st = termState(t.hostId, t.sessionId)
    const cjkBreaksPending = !!(st && st.cmdPending && !st.cmdActive) &&
      !isFullscreen(key) &&
      d.length > 0 &&
      !/[\x00-\x1f\x7f]/.test(d) &&
      hasCJK(d)
    if (cjkBreaksPending) {
      endCmdPending(t.hostId, t.sessionId)
      // 落到下面的本地行编辑，让回车走 resolveInput → Agent。
    } else {
      clearWelcomeHintByKey(key)
      clearLLMHintByKey(key)
      if (!gatePendingKey(key, t, d, send)) return
      send(textToBase64(d))
      return
    }
  }

  // agent 正在跑：此刻的按键不属于任何命令行，**必须丢掉**。
  // Ctrl-C 是例外：它是「等不下去了，中断」唯一的键盘出口，放行给 stop()。
  if (store.running || store.busy) {
    if (d.includes('\x03')) stop()
    return
  }

  // 占位符撤退：用户开始操作了，先把欢迎引导与 LLM 提示都擦干净。
  clearWelcomeHintByKey(key)
  clearLLMHintByKey(key)

  // 已降级透传（本地缓冲已被控制键 flush 给远端、这一行交给 shell readline 接管）。
  if (t.passthrough) {
    send(textToBase64(d))
    if (/[\r\n\x03]/.test(d)) t.passthrough = false
    return
  }

  // 多字符输入分三类：含换行=多行粘贴，逐字符回放让 \r 走提交分支；
  // 转义/控制序列（方向键、功能键）=整体透传；
  // 纯可打印（粘贴一段命令）=整体本地缓冲。
  if (d.length > 1) {
    if (/[\r\n]/.test(d)) {
      for (const ch of d) onTermData(key, ch)
      return
    }
    if (/[\x00-\x1f\x7f]/.test(d)) {
      flushLocalToPty(t)
      send(textToBase64(d))
      t.passthrough = !d.includes('\x03')
      t.lineSynthetic = false
      return
    }
    if (!t.line) t.startCol = cursorCol(t)
    t.line += d
    t.term.write(d)
    return
  }

  // 全屏程序接管：擦掉残留本地回显后整体透传，绝不抢键。
  if (isFullscreen(key)) {
    clearLocalEcho(t)
    t.lineSynthetic = false
    send(textToBase64(d))
    return
  }

  // 回车：提交本地缓冲的这一行（串行化，避免连敲并发）。
  if (d === '\r' || d === '\n') {
    const line = t.line
    t.line = ''
    if (t.lineSynthetic) {
      t.lineSynthetic = false
      t.term.write('\r\x1b[2K')
    } else {
      eraseLocalLine(t, line)
    }
    enqueueSubmit(key, line)
    return
  }

  // 退格：本地删一个字符（按码点、按显示宽度）。
  if (d === '\x7f') {
    if (t.line) {
      const chars = Array.from(t.line)
      const last = chars.pop()
      t.line = chars.join('')
      t.term.write('\b \b'.repeat(displayWidth(last)))
    }
    return
  }

  // 其它控制字符 / 转义序列：放弃本地行编辑，把缓冲 flush 给 PTY 后透传该键。
  if (/[\x00-\x1f\x7f]/.test(d)) {
    flushLocalToPty(t)
    send(textToBase64(d))
    t.passthrough = !d.includes('\x03')
    t.lineSynthetic = false
    return
  }

  // 普通可打印字符：首次进入本行记下起始列，本地回显 + 缓冲。
  if (!t.line) t.startCol = cursorCol(t)
  t.line += d
  t.term.write(d)
}

// agent 回合结束 → 回到输入态。提问不走 PTY，远端 shell 不会打印新提示符；
// 若本轮 PTY 一次都没被写过（纯问答），表面补一个合成 ❯ 提示符。
export function paintInputPromptIfNeeded(key) {
  const t = getTerm(key)
  if (!t || t.passthrough) return
  if (isPtyDirty(t.hostId, t.sessionId)) return
  if (t.lineSynthetic) return
  paint.paintInputPrompt(s => {
    const cur = getTerm(key)
    if (cur) cur.term.write(s)
  })
  t.lineSynthetic = true
}
