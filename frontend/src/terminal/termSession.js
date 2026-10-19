import { nextTick, ref } from 'vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
// xterm 的布局契约全在这张样式表里（隐藏输入捕获 textarea、行/格对齐、
// 视口滚动裁剪）。谁创建 Terminal 实例谁负责带它 —— 重构拆文件时曾把它
// 留在旧 ConsolePanel 里一起删掉，控制台立刻塌成裸 DOM（顶部冒出可见的
// 输入框与错位乱码行）。放在这里，再拆模块也不会丢。
import '@xterm/xterm/css/xterm.css'
import { TERM_THEMES, currentTermTheme } from '../termTheme.js'
import * as paint from '../termPaint.js'
import { store, bucketKey, termState, openTerminal, resizeTerminal, writeTerminal, textToBase64 } from '../store.js'
import { registerTermSink, unregisterTermSink, registerLLMHint } from './termEvents.js'

// 欢迎引导比真提示符晚多久出现。远端 shell 从握手到打印提示符约 500ms
// （ptyprobe 抓包实测），这里取 700ms 留出余量，保证引导落在提示符**之后**。
const WELCOME_HINT_DELAY_MS = 700

// 欢迎引导的「远端已安静」门槛：最近一次远端输出距今不足该值时不画 ——
// 说明 MOTD/提示符还在刷屏路上，光标位置随时会变，画了必落错。
const WELCOME_HINT_QUIET_MS = 300

// 尺寸上报防抖：连续 resize 合并成一次，取最新尺寸。
const RESIZE_DEBOUNCE_MS = 150

// 已建过容器的表面键（bucketKey）。作为响应式 ref 导出，供 ConsolePanel 模板 v-for。
export const openedKeys = ref([])

// surfaceKey -> 容器元素（v-for 的 ref 回调写进来的）
const surfaceEls = new Map()

// surfaceKey -> xterm 实例及其状态
const terms = new Map()

// 已画过欢迎引导的表面键：一次「打开 → 关闭」生命周期内只提示一次。
const hintedKeys = new Set()

// 尺寸上报的防抖定时器 / 上次已发尺寸（按 surfaceKey）
const pendingResize = new Map()
const lastSentSize = new Map()

export function getTerm(key) {
  return terms.get(key)
}

export function setSurfaceEl(key, el) {
  if (el) surfaceEls.set(key, el)
  else surfaceEls.delete(key)
}

// 容器是否已有可测量尺寸。对话页用 v-show 藏表面时是 0x0；
// 此时 open xterm 会把画布建死，之后 fit 也修不回来（切「终端」白屏）。
function surfaceReady(el) {
  return !!(el && el.clientWidth > 0 && el.clientHeight > 0)
}

// 终端配色跟随亮/暗主题；切换时同步所有已打开的实例。
export function setTheme(theme) {
  const t = TERM_THEMES[theme] || TERM_THEMES.light
  for (const { term } of terms.values()) term.options.theme = t
}

// 终端字体热更新：改完 options 必须重新 fit —— 字号一变，同样容器尺寸下
// 能容纳的行列数就变了；doFit 触发的 term.onResize 会经 reportResize 把新
// 尺寸报给远端 PTY（window-change），vim/top 才不会按旧尺寸排版。
export function setTermFont(font) {
  for (const t of terms.values()) {
    try {
      if (font.family) t.term.options.fontFamily = font.family
      if (font.size) t.term.options.fontSize = font.size
    } catch { /* 替身或已销毁的实例，跳过 */ }
  }
  for (const key of terms.keys()) doFit(key)
}

// doFit 只在容器**确实有尺寸**时才测量。隐藏时（切到归档、或不是当前任务）
// 量到 0x0，fit 会算出 0 列 0 行报给远端，排版彻底乱掉且界面无异常可见。
export function doFit(key) {
  const t = terms.get(key)
  if (!t) return
  const el = surfaceEls.get(key)
  if (!el || el.clientWidth === 0 || el.clientHeight === 0) return
  try {
    t.fit.fit()
  } catch {
    // 布局还没稳定时会抛。下一次 ResizeObserver 会再试，不该变成用户可见的报错。
  }
}

export function focusTerm(key) {
  const t = terms.get(key)
  if (!t) return
  try {
    t.term.focus()
  } catch {
    // 忽略：实例已销毁或替身无 focus
  }
}

function reportResize(key, hostId, sessionId, cols, rows) {
  const size = cols + 'x' + rows
  if (lastSentSize.get(key) === size) return
  const prev = pendingResize.get(key)
  if (prev) clearTimeout(prev)
  const timer = setTimeout(() => {
    pendingResize.delete(key)
    // PTY 还在创建中（opening）：此刻的尺寸是布局稳定期的中间态，发出去必是
    // 冗余 window-change。慢连接下防抖（150ms）会在 OpenTerminal 返回前到期，
    // 只靠「打开完成后作废」拦不住 —— 必须在到期那一刻再查一次状态。
    // 打开完成后 statusLine watcher 必然再 doFit 一次，真实变了尺寸会重新
    // 走到这里（那时已不是 opening，照常发送）。
    if (termState(hostId, sessionId).status === 'opening') return
    lastSentSize.set(key, size)
    resizeTerminal(hostId, sessionId, cols, rows)
  }, RESIZE_DEBOUNCE_MS)
  pendingResize.set(key, timer)
}

export function disposeTerm(key) {
  const pending = pendingResize.get(key)
  if (pending) {
    clearTimeout(pending)
    pendingResize.delete(key)
  }
  lastSentSize.delete(key)
  const t = terms.get(key)
  if (t) {
    if (t.hintRetry) clearTimeout(t.hintRetry)
    unregisterTermSink(t.hostId, t.sessionId, t.sink)
    t.instanceToken.disposed = true
    try {
      t.term.dispose()
    } catch {
      // 已经销毁过就算了
    }
    terms.delete(key)
  }
  openedKeys.value = openedKeys.value.filter(x => x !== key)
  surfaceEls.delete(key)
  // 这一条生命周期结束了：清掉去重标记，下次打开（或切回来重建）时重新提示。
  hintedKeys.delete(key)
}

export async function ensureTerm(hostId, sessionId, currentTheme = 'light') {
  if (!hostId) return
  const key = bucketKey(hostId, sessionId)
  const state = termState(hostId, sessionId)

  if (terms.has(key)) {
    if (state.status === 'closed' || state.status === 'error') {
      // 上一次会话已结束：换掉旧实例，否则新画面会接在上一段后面像「重连没清屏」。
      disposeTerm(key)
    } else {
      await nextTick()
      doFit(key)
      const el = surfaceEls.get(key)
      const t = terms.get(key)
      // 极少数：当初在 0x0 上 open 成功但行列仍是废的。容器已可见则拆本地重建
      // （OpenTerminal 复用远端 PTY，不丢目录/进程）。
      if (surfaceReady(el) && t && (t.term.cols < 2 || t.term.rows < 2)) {
        disposeTerm(key)
      } else {
        return
      }
    }
  }

  // 先把容器渲染出来再 open：xterm 在 open 那一刻就要测量容器、建渲染层。
  // 对话页隐藏时是 0x0 —— 只占位、不 open，等切到「终端」容器有尺寸后再建。
  if (!openedKeys.value.includes(key)) openedKeys.value.push(key)
  await nextTick()

  const el = surfaceEls.get(key)
  if (!el) return
  if (!surfaceReady(el)) return

  const term = new Terminal({
    cursorBlink: true,
    cursorStyle: 'bar',
    cursorWidth: 2,
    scrollback: 5000,
    fontFamily: store.termFont.family,
    fontSize: store.termFont.size,
    theme: currentTermTheme(currentTheme)
  })
  const fit = new FitAddon()
  term.loadAddon(fit)
  term.open(el)

  const sink = bytes => {
    const st = terms.get(key)
    // 远端又画了东西：屏幕已经不是我们离开时那一屏了，占位符的列位置随之作废。
    // 这里**只作废标记、不试图擦除** —— 行的内容已被远端改写，按旧列号擦会
    // 误伤真实输出。提示被盖住就盖住了，下次切回会重新提示。
    if (st) {
      st.hintCols = 0
      // 记录「远端有输出」与最近一次输出时刻：欢迎提示必须等远端真正开口
      // 并且说完（安静一小段）才画，见 paintWelcomeHintIfNeeded 的时序守卫。
      st.sawData = true
      st.lastDataAt = Date.now()
    }
    if (st && st.line && !st.passthrough) {
      // 本地行编辑期间收到外部 PTY 输出（提示符重绘 / 后台打印）。旧的 startCol
      // 确实不可信了，但**不能**像旧实现那样把缓冲 flush 给 PTY 换透传 ——
      // 那等于把还没回车的半行直接交给 bash 执行。改用 readline 的做法：
      // 擦掉本地回显 → 让输出画完 → 在新光标位置把这一行重画出来。
      eraseLocalLine(st, st.line)
      term.write(bytes, () => {
        if (terms.get(key) !== st) return
        if (isFullscreen(key)) {
          st.line = ''
          st.lineSynthetic = false
        } else {
          st.startCol = cursorCol(st)
          if (st.lineSynthetic) {
            paint.paintInputPrompt(s => term.write(s))
            st.startCol = cursorCol(st)
          }
          term.write(st.line)
        }
      })
      return
    }
    term.write(bytes)
  }

  // 顺序不能反：先登记落点再 OpenTerminal。远端 shell 一启动就打印提示符，
  // 登记晚了那一段就落进虚空了。
  registerTermSink(hostId, sessionId, sink)

  const instanceToken = { disposed: false }
  registerLLMHint(hostId, sessionId, () => {
    const cur = terms.get(key)
    if (!cur || instanceToken.disposed || terms.get(key) !== cur) return
    if (cur.line || cur.passthrough || cur.lineSynthetic) return
    if (isFullscreen(key)) return
    const startCol = cursorCol(cur)
    const cols = paint.paintLLMHint(surfaceWrite(key), startCol) || 0
    if (cols) { cur.llmHintCols = cols; cur.llmHintStart = startCol }
  })

  terms.set(key, {
    term, fit, sink, hostId, sessionId, instanceToken, wired: false,
    line: '', startCol: 0, passthrough: false, lineSynthetic: false,
    hintCols: 0, hintPromptCols: 0,
    llmHintCols: 0, llmHintStart: 0,
    sawData: false, lastDataAt: 0, hintRetry: null
  })

  // 尺寸变化 → 后端。少了这一步远端程序会一直按初始尺寸排版。
  term.onResize(({ cols, rows }) => {
    reportResize(key, hostId, sessionId, cols, rows)
  })

  doFit(key)
  requestAnimationFrame(() => doFit(key))
  const createdSize = term.cols + 'x' + term.rows
  await openTerminal(hostId, sessionId, term.cols, term.rows)
  const pending = pendingResize.get(key)
  if (pending) {
    clearTimeout(pending)
    pendingResize.delete(key)
  }
  lastSentSize.set(key, createdSize)

  // 欢迎引导要等真提示符先出来。固定 700ms 只是个猜测：慢连接（首次 SSH
  // 握手 + 认证 + MOTD）完全可能超过它 —— 提示抢在远端 banner 之前画到 (0,0)，
  // banner 以 \r\n 开头到达后它就被永久留在 Welcome 行上方（实测截图），而
  // 远端数据一到 sink 就清零 hintCols，这条残留再也擦不掉。所以定时器到点时
  // 不直接画：远端还没开口、或正在刷屏（300ms 内有输出）就推迟再查。
  const created = terms.get(key)
  setTimeout(() => {
    if (terms.get(key) !== created) return
    paintWelcomeHintIfNeeded(key)
  }, WELCOME_HINT_DELAY_MS)
}

// 返回往某块表面写字符串的落点，供 termPaint 画 agent 事件。
// 实例还没建（或已销毁）时返回 undefined —— termPaint 对 undefined 落点安全 no-op。
export function surfaceWrite(key) {
  const t = terms.get(key)
  return t ? (s => t.term.write(s)) : undefined
}

export function paintWelcomeHintIfNeeded(key, force = false) {
  if (!key) return
  if (!force && hintedKeys.has(key)) return
  const t = terms.get(key)
  if (!t) return
  // 时序守卫：远端还没输出过（提示符必然没出来），或仍在刷屏（300ms 内有
  // 新输出，光标位置随时会变），此刻画提示必然落错位置 —— 推迟再查，直到
  // 远端安静下来。若直接取消，慢连接用户就永远看不到引导，所以是推迟而非放弃。
  const quietFor = Date.now() - (t.lastDataAt || 0)
  if (!t.sawData || quietFor < WELCOME_HINT_QUIET_MS) {
    if (t.hintRetry) clearTimeout(t.hintRetry)
    t.hintRetry = setTimeout(() => {
      t.hintRetry = null
      if (terms.get(key) === t) paintWelcomeHintIfNeeded(key, force)
    }, WELCOME_HINT_QUIET_MS)
    return
  }
  if (!force) hintedKeys.add(key)
  if (t.line || t.passthrough || t.lineSynthetic) return
  if (isFullscreen(key) || isCmdRunning(key)) return
  t.hintPromptCols = cursorCol(t)
  t.hintCols = paint.paintWelcomeHint(surfaceWrite(key), t.hintPromptCols) || 0
}

export function clearWelcomeHintByKey(key) {
  const t = terms.get(key)
  if (t) clearWelcomeHint(t)
}

export function clearLLMHintByKey(key) {
  const t = terms.get(key)
  if (t) clearLLMHint(t)
}

function clearWelcomeHint(t) {
  if (!t || !t.hintCols) return
  const n = t.hintCols
  const col = t.hintPromptCols || 0
  t.hintCols = 0
  t.hintPromptCols = 0
  t.term.write('\x1b[' + (col + 1) + 'G' + ' '.repeat(n) + '\x1b[' + (col + 1) + 'G')
}

function clearLLMHint(t) {
  if (!t || !t.llmHintCols) return
  const n = t.llmHintCols
  const col = t.llmHintStart || 0
  t.llmHintCols = 0
  t.llmHintStart = 0
  t.term.write('\x1b[' + (col + 1) + 'G' + ' '.repeat(n) + '\x1b[' + (col + 1) + 'G')
}

export function cursorCol(t) {
  const buf = t.term.buffer && t.term.buffer.active
  return buf && typeof buf.cursorX === 'number' ? buf.cursorX : 0
}

export function isFullscreen(key) {
  const t = terms.get(key)
  const buf = t && t.term.buffer && t.term.buffer.active
  if (buf && buf.type === 'alternate') return true
  return !!t && !!termState(t.hostId, t.sessionId).tuiActive
}

export function isCmdRunning(key) {
  const t = terms.get(key)
  if (!t) return false
  const st = termState(t.hostId, t.sessionId)
  // cmdActive：OSC 133 报的「命令运行中」；cmdPending：刚提交 shell 命令后
  // 乐观开的透传窗口（shell integration 缺席时的兜底，见 termEvents）。
  return !!st && !!(st.cmdActive || st.cmdPending)
}

export function displayWidth(s) {
  let w = 0
  for (const ch of String(s)) {
    const c = ch.codePointAt(0)
    const wide = (c >= 0x1100 && c <= 0x115f) ||
      (c >= 0x2e80 && c <= 0xa4cf) ||
      (c >= 0xac00 && c <= 0xd7a3) ||
      (c >= 0xf900 && c <= 0xfaff) ||
      (c >= 0xfe30 && c <= 0xfe6f) ||
      (c >= 0xff00 && c <= 0xff60) ||
      (c >= 0xffe0 && c <= 0xffe6)
    w += wide ? 2 : 1
  }
  return w
}

export function eraseLocalLine(t, line) {
  if (!line) return
  const go = '\r\x1b[' + (t.startCol + 1) + 'G'
  t.term.write(go + '\x1b[0m' + ' '.repeat(displayWidth(line)) + '\x1b[0m' + go)
}

export function clearLocalEcho(t) {
  if (!t.line) return
  eraseLocalLine(t, t.line)
  t.line = ''
}

export function flushLocalToPty(t) {
  if (!t.line) return
  eraseLocalLine(t, t.line)
  writeTerminal(t.hostId, t.sessionId, textToBase64(t.line))
  t.line = ''
}
