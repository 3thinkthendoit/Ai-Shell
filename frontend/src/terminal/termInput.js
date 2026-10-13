import { store, writeTerminal, textToBase64, stop, isPtyDirty } from '../store.js'
import * as paint from '../termPaint.js'
import { enqueueSubmit } from './termSubmit.js'
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
  clearLLMHintByKey
} from './termSession.js'

// 终端内直接输入：在提示符下就地编辑、回车分类。
//
// 真 PTY 逐字符透传的话，自然语言会被 bash 当命令执行而报错。所以把
// 「shell 提示符下等待输入一条命令」这一常态改成本地行编辑：可打印字符
// 本地回显、退格本地删，回车时 resolveInput 决定交给 Agent 还是发回 PTY。

export function onTermData(key, d) {
  const t = getTerm(key)
  if (!t) return
  const send = s => writeTerminal(t.hostId, t.sessionId, s)

  // 命令运行中（OSC 133;C..D 之间）：输入归命令，所有按键原样透传给 PTY。
  if (isCmdRunning(key)) {
    clearWelcomeHintByKey(key)
    clearLLMHintByKey(key)
    send(textToBase64(d))
    return
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
