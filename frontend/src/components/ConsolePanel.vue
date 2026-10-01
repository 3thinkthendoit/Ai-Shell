<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import {
  store, ask, approve, stop, clearLog, clearSession, compactSession, push,
  createSession, refreshSessions, runShellInTerminal, switchProfile, syncWindowTitle,
  openTerminal, writeTerminal, resizeTerminal, closeTerminal,
  registerTermSink, unregisterTermSink, termState, textToBase64, reportActiveSession,
  bucketKey
} from '../store'
import { classifyInput } from '../inputRoute'
import { providerBadge } from '../providerLogos'
import { TERM_THEMES, currentTermTheme } from '../termTheme'
import * as paint from '../termPaint'
import UiSelect from './UiSelect.vue'

// 控制台 = 一块整幅 xterm，附着当前主机的常驻 shell PTY（阿里云 Workbench 同款）。
// 人可以直接在表面里敲（原始按键进 PTY，不过策略）；底部 composer 走分流：
// 中文/? → Agent，shell 行 → 策略闸门 → 写进同一片 PTY。top/vim 原生全屏接管。
// 「对话归档」开关把主区切成只读时间线（复用 store.entries 渲染），表面用 v-show
// 常驻不销毁 —— xterm 实例一销毁滚动回放就没了。

// ---- 常驻终端表面（xterm 实例生命周期，从 InteractiveTerminal 折叠进来）----
// 表面按「任务」组织：一块表面对应一条 (host, session)，键用 store.bucketKey。
// 只保留当前聚焦任务的 shell：切换任务时关闭上一任务的表面/PTY，为目标任务
// 新开一条全新 xterm（回来也是全新的，旧现场丢弃，历史仍在「对话归档」里）。
const rootEl = ref(null)          // ResizeObserver 的观察目标（.surface）
const openedKeys = ref([])        // 已建过容器的表面键（bucketKey）
const surfaceEls = new Map()      // surfaceKey -> 容器元素
const terms = new Map()           // surfaceKey -> { term, fit, sink, hostId, sessionId, ... }
let attachedKey = ''              // 当前附着的那块表面键（切任务时据此关闭旧的）
let ro = null

// 对话归档：只读时间线开关。false = 活表面，true = 归档视图。
const showArchive = ref(false)

const logEl = ref(null)

const currentHost = computed(() => store.hosts.find(h => h.id === store.currentHostId))
// 当前聚焦任务的表面键：未选主机时无表面。
const currentKey = computed(() =>
  store.currentHostId ? bucketKey(store.currentHostId, store.currentSessionId) : ''
)
const st = computed(() => (currentKey.value ? store.terms[currentKey.value] : null))

// 终端状态行：只在「打开中 / 已结束 / 失败」时出现，给出原因与重试入口。
const statusLine = computed(() => {
  const s = st.value
  if (!s) return ''
  if (s.status === 'opening') return '正在打开终端…'
  if (s.status === 'closed') return s.exitReason || '终端已结束'
  if (s.status === 'error') return '打开失败：' + s.error
  return ''
})
const canRetry = computed(() => !!st.value && (st.value.status === 'closed' || st.value.status === 'error'))
const canClose = computed(() => !!st.value && (st.value.status === 'open' || st.value.status === 'opening'))

function setSurfaceEl(key, el) {
  if (el) surfaceEls.set(key, el)
  else surfaceEls.delete(key)
}

// 终端配色跟随亮/暗主题；切换时同步所有已打开的实例。
watch(() => store.theme, t => {
  const theme = TERM_THEMES[t] || TERM_THEMES.light
  for (const { term } of terms.values()) term.options.theme = theme
})

// doFit 只在容器**确实有尺寸**时才测量。隐藏时（切到归档、或不是当前任务）
// 量到 0x0，fit 会算出 0 列 0 行报给远端，排版彻底乱掉且界面无异常可见。
function doFit(key) {
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

// focusTerm 把键盘焦点交给该片终端（输入已整体在终端内，composer 输入框已移除）。
// xterm 替身可能没有 focus，包一层 try 免得主流程被带偏。
function focusTerm(key) {
  const t = terms.get(key)
  if (!t) return
  try {
    t.term.focus()
  } catch {
    // 忽略：实例已销毁或替身无 focus
  }
}

function disposeTerm(key) {
  const t = terms.get(key)
  if (t) {
    unregisterTermSink(t.hostId, t.sessionId, t.sink)
    try {
      t.term.dispose()
    } catch {
      // 已经销毁过就算了
    }
    terms.delete(key)
  }
  openedKeys.value = openedKeys.value.filter(x => x !== key)
  surfaceEls.delete(key)
}

async function ensureTerm(hostId, sessionId) {
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
      return
    }
  }

  // 先把容器渲染出来再 open：xterm 在 open 那一刻就要测量容器、建渲染层。
  // 容器不存在时量到 0x0，画布按错尺寸建，之后再 fit 也修不回来。
  if (!openedKeys.value.includes(key)) openedKeys.value.push(key)
  await nextTick()

  const el = surfaceEls.get(key)
  if (!el) return

  const term = new Terminal({
    cursorBlink: true,
    // 回放行数：给足但不无限。终端的价值一半在「翻回去看」，每行都占内存。
    scrollback: 5000,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
    fontSize: 12.5,
    theme: currentTermTheme(store.theme)
  })
  const fit = new FitAddon()
  term.loadAddon(fit)
  term.open(el)
  // 表面不再预写任何提示行：输入文案已在顶部 banner 常驻，重连也不留装饰性
  // 空行 —— 让终端直接落到远端 shell 的提示符。

  const sink = bytes => {
    // 本地行编辑期间收到外部 PTY 输出 = 环境不安静（后台打印/提示符重绘），
    // 此刻 startCol 已不可信：把本地缓冲 flush 给 PTY 并降级透传，避免擦错位置。
    const st = terms.get(key)
    if (st && st.line && !st.passthrough) {
      flushLocalToPty(st)
      st.passthrough = true
    }
    term.write(bytes)
  }
  // 顺序不能反：先登记落点再 OpenTerminal。远端 shell 一启动就打印提示符，
  // 登记晚了那一段就落进虚空了。
  registerTermSink(hostId, sessionId, sink)
  terms.set(key, { term, fit, sink, hostId, sessionId, line: '', startCol: 0, passthrough: false })

  // 用户按键 → 本地行编辑（见 onTermData）：常态下可打印字符本地回显、
  // 回车时分类；全屏程序接管或控制键则原样透传。不再是无脑逐字符进 PTY。
  term.onData(d => onTermData(key, d))
  // 尺寸变化 → 后端。少了这一步远端程序会一直按初始尺寸排版。
  term.onResize(({ cols, rows }) => {
    resizeTerminal(hostId, sessionId, cols, rows)
  })

  doFit(key)
  // 布局可能在 open 之后才稳定（状态行出现、工具条换行、字体度量就绪）：
  // 再补一帧重测量，否则行数停留在偏小值，视口底部留一大段用不上的空白。
  requestAnimationFrame(() => doFit(key))
  await openTerminal(hostId, sessionId, term.cols, term.rows)
}

// ---- 终端内直接输入：在提示符下就地编辑、回车分类 ----
//
// 真 PTY 逐字符透传的话，自然语言会被 bash 当命令执行而报错。所以把
// 「shell 提示符下等待输入一条命令」这一常态改成本地行编辑：可打印字符
// 本地回显、退格本地删，回车时 classifyInput 决定交给 Agent 还是发回 PTY。
//
// 代价：提示符下的 readline 原生历史/补全改由本地接管。因此凡是控制键/转义
// 序列（Tab、方向键、Ctrl-C、Ctrl-R、Esc…）一律原样透传给 shell；
// 全屏程序接管（备用屏幕，vim/top/htop）时更是整体透传，绝不抢键。

function isFullscreen(key) {
  const t = terms.get(key)
  const buf = t && t.term.buffer && t.term.buffer.active
  if (buf && buf.type === 'alternate') return true
  // xterm 的 buffer 只看得到「切了备用屏幕」的全屏程序（vim/htop）。
  // procps 系的 top 走的是原地重绘、不发备用屏幕，靠后端 Screen.InTUI()
  // 经 term:tui 推来的 tuiActive 才能识别 —— 两条都得认，否则 top 里敲
  // q 会被本地行编辑吞掉。
  return !!t && !!termState(t.hostId, t.sessionId).tuiActive
}

function cursorCol(t) {
  const buf = t.term.buffer && t.term.buffer.active
  return buf && typeof buf.cursorX === 'number' ? buf.cursorX : 0
}

// displayWidth 近似 wcwidth：CJK/全角占 2 列，其余 1 列。
// 擦除本地回显时要按**显示宽度**补空格，中文按 1 算会擦不干净。
function displayWidth(s) {
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

// eraseLocalLine 用等宽空格覆盖本地回显的字符（空格与回显同起点、同宽度，
// wrap 路径一致，因此跨行折返也擦得净），再把光标移回输入起点。
// 比「定位列 + \x1b[K 清到行尾」更能处理超长输入折行的情况。
function eraseLocalLine(t, line) {
  if (!line) return
  const go = '\r\x1b[' + (t.startCol + 1) + 'G'
  // 覆盖空格前后各补一次 SGR 复位：远端若残留黑底/反视频（某些程序退出时
  // 不复位），裸空格会继承那个背景，擦完留下一条黑带 —— 必须强制回默认色。
  t.term.write(go + '\x1b[0m' + ' '.repeat(displayWidth(line)) + '\x1b[0m' + go)
}

// clearLocalEcho 只擦掉本地回显并清空缓冲，**不**发给 PTY。
// 用于全屏接管：此时 PTY 在跑 vim/top，把半截字符发过去会当成程序输入。
function clearLocalEcho(t) {
  if (!t.line) return
  eraseLocalLine(t, t.line)
  t.line = ''
}

// flushLocalToPty 擦掉本地回显后把缓冲发给 PTY（shell 空闲等输入，安全），
// 用于控制键打断、或本地编辑期间检测到外部输出需要降级透传。
function flushLocalToPty(t) {
  if (!t.line) return
  eraseLocalLine(t, t.line)
  writeTerminal(t.hostId, t.sessionId, textToBase64(t.line))
  t.line = ''
}

// submitQueue 让同一块表面（同一任务）的提交串行：onData 是同步回调没法 await，
// 连敲回车会让多个 runShellInTerminal 并发（store.busy 交错、PTY 输出互踩）。
const submitQueue = new Map()
function enqueueSubmit(key, line) {
  const prev = submitQueue.get(key) || Promise.resolve()
  const next = prev.then(() => submitLine(key, line)).catch(() => {})
  submitQueue.set(key, next)
  return next
}

async function submitLine(key, line) {
  const t = terms.get(key)
  if (!t) return
  const { hostId, sessionId } = t
  const text = (line || '').trim()
  if (!text) {
    // 空回车：与 composer 同形，只给 PTY 一个换行让 shell 回显新提示符。
    writeTerminal(hostId, sessionId, textToBase64('\n'))
    return
  }
  const route = classifyInput(text)
  if (route.kind === 'agent') {
    if (!route.text) {
      paint.paintSystem(surfaceWrite(key), '输入要问 Agent 的内容，或直接敲一条 shell 命令。')
      return
    }
    ask(route.text)
    return
  }
  // shell：交回常驻终端通道（策略闸门 + 写进 PTY，PTY 自己回显执行）。
  await runShellInTerminal(hostId, sessionId, route.text)
}

function onTermData(key, d) {
  const t = terms.get(key)
  if (!t) return
  const send = s => writeTerminal(t.hostId, t.sessionId, s)

  // 已降级透传（本地编辑期间来过外部输出，或本地缓冲已被控制键 flush 给远端）：
  // 原样送 PTY（多字符也整体送）。回车（这一行交回 shell 处理完）或 Ctrl-C
  // （中断、回到空提示符）后恢复本地编辑。
  if (t.passthrough) {
    send(textToBase64(d))
    if (/[\r\n\x03]/.test(d)) t.passthrough = false
    return
  }

  // 多字符输入分三类：含换行=多行粘贴，逐字符回放让 \r 走提交分支；
  // 转义/控制序列（方向键、功能键）=整体透传，绝不能拆散；
  // 纯可打印（粘贴一段命令）=整体本地缓冲。
  if (d.length > 1) {
    if (/[\r\n]/.test(d)) {
      for (const ch of d) onTermData(key, ch)
      return
    }
    if (/[\x00-\x1f\x7f]/.test(d)) {
      flushLocalToPty(t)
      send(textToBase64(d))
      // 整段控制序列（方向键/粘贴含控制符等）同样把这一行交回 shell 接管：
      // 缓冲已 flush 给远端，不置透传则后续退格会因本地缓冲已空而被吞掉。
      t.passthrough = !d.includes('\x03')
      return
    }
    if (!t.line) t.startCol = cursorCol(t)
    t.line += d
    t.term.write(d)
    return
  }

  // 全屏程序接管：擦掉残留本地回显后整体透传，绝不抢键、也不污染程序输入。
  if (isFullscreen(key)) {
    clearLocalEcho(t)
    send(textToBase64(d))
    return
  }

  // 回车：提交本地缓冲的这一行（串行化，避免连敲并发）。
  if (d === '\r' || d === '\n') {
    const line = t.line
    t.line = ''
    eraseLocalLine(t, line)
    enqueueSubmit(key, line)
    return
  }

  // 退格：本地删一个字符。必须按**码点**删（代理对算一个字符），并按它的
  // **显示宽度**回退擦除 —— 中文/全角占 2 列，只回退擦 1 列会留下右半、
  // 光标错位（下一字符的 startCol 跟着错），表现为「输入自然语言时删不干净」。
  if (d === '\x7f') {
    if (t.line) {
      const chars = Array.from(t.line)
      const last = chars.pop()
      t.line = chars.join('')
      t.term.write('\b \b'.repeat(displayWidth(last)))
    }
    return
  }

  // 其它控制字符 / 转义序列（Tab、方向键、Ctrl-C、Ctrl-R、Esc…）：
  // 放弃本地行编辑，把缓冲 flush 给 PTY 后透传该键 —— 从这一下起这一行交给
  // shell，后续按键（含退格、方向键）原样进 PTY，直到回车再回到本地行编辑。
  // 关键：必须置 passthrough，否则退格会因本地缓冲已被 flush 清空而被吞掉，
  // 表现为「按方向键/Tab 编辑一行后删不动」。Ctrl-C 例外：它中断并清空该行，
  // 远端回到空提示符，故留在本地编辑态。
  if (/[\x00-\x1f\x7f]/.test(d)) {
    flushLocalToPty(t)
    send(textToBase64(d))
    t.passthrough = !d.includes('\x03')
    return
  }

  // 普通可打印字符：首次进入本行记下起始列（供擦除定位），本地回显 + 缓冲。
  if (!t.line) t.startCol = cursorCol(t)
  t.line += d
  t.term.write(d)
}

// surfaceWrite 返回往某块表面（按 host+session 键）写字符串的落点，供 termPaint 画 agent 事件。
// 实例还没建（或已销毁）时返回 undefined —— termPaint 对 undefined 落点安全 no-op。
function surfaceWrite(key) {
  const t = terms.get(key)
  return t ? (s => t.term.write(s)) : undefined
}

async function onClose() {
  const hostId = store.currentHostId
  if (!hostId) return
  const key = currentKey.value
  await closeTerminal(hostId, store.currentSessionId)
  disposeTerm(key)
  if (attachedKey === key) attachedKey = ''
}

function onRetry() {
  ensureTerm(store.currentHostId, store.currentSessionId)
}

// 从归档切回表面时重新测量：隐藏期间容器是 0x0，fit 一直跳过。
watch(showArchive, async on => {
  if (on) {
    scrollLog()
    return
  }
  await nextTick()
  doFit(currentKey.value)
})

// 状态行出现/消失会改 surface-body 的高度，但 .surface 自身尺寸不变 ——
// ResizeObserver 观测不到这种「内部重新分配」，补一次重测量。
watch(statusLine, async () => {
  await nextTick()
  doFit(currentKey.value)
})

// ---- 归档视图派生 ----

const decisionLabel = d => ({ allow: '自动放行', confirm: '需确认', deny: '硬拒绝' }[d] || d)
const decisionClass = d => ({ allow: 'ok', confirm: 'warn', deny: 'danger' }[d] || '')
const statusLabel = s => ({
  pending: '待批准', running: '执行中', done: '已完成', denied: '已拒绝', error: '出错'
}[s] || s)

const streamingNow = computed(() => store.entries.some(e => e.streaming))

function scrollLog() {
  nextTick(() => {
    if (logEl.value) logEl.value.scrollTop = logEl.value.scrollHeight
  })
}

const scrollKey = computed(() => {
  const n = store.entries.length
  if (!n) return '0'
  const last = store.entries[n - 1]
  return `${n}:${last.content ? last.content.length : 0}:${last.pending ? 1 : 0}`
})
watch(scrollKey, () => scrollLog())

// ---- 主机 / 任务切换：只留当前任务的 shell（切走关闭、回来重开）----

let hostSwitching = false

// detachSurface 关闭某块表面：先关远端 PTY（closeTerminal）再销毁本地 xterm。
// 落实「只留当前任务」：切换任务时上一任务的现场就此丢弃（回来是全新 shell）。
async function detachSurface(key) {
  if (!key) return
  const t = terms.get(key)
  if (!t) return
  await closeTerminal(t.hostId, t.sessionId)
  disposeTerm(key)
}

// attachSurface 为目标任务拉起一条全新 shell：关掉当前附着着的其它表面，
// 新建本任务专属表面（ensureTerm → openTerminal），并把「现在看着哪条会话」
// 报给后端（Ask 的 Agent 上下文仍需）。不再画会话分隔线 —— 表面不再共享。
async function attachSurface(hostId, sessionId) {
  if (!hostId) return
  const key = bucketKey(hostId, sessionId)
  if (attachedKey && attachedKey !== key) await detachSurface(attachedKey)
  await ensureTerm(hostId, sessionId)
  attachedKey = key
  reportActiveSession(hostId, sessionId)
  await nextTick()
  focusTerm(key)
}

watch(() => store.currentHostId, async id => {
  hostSwitching = true
  try {
    store.currentSessionId = ''
    await refreshSessions(id)
    if (id) await attachSurface(id, store.currentSessionId)
    scrollLog()
  } finally {
    hostSwitching = false
  }
})

// 侧边栏切任务（主机不变）：关掉上一任务的表面、为目标任务重开全新 shell。
// 主机切换过程中会重置会话，那条路径由上面的 watcher 统一处理，这里用
// hostSwitching 让路避免重复。
watch(() => store.currentSessionId, async () => {
  if (hostSwitching) return
  const hostId = store.currentHostId
  if (!hostId) return
  await attachSurface(hostId, store.currentSessionId)
  scrollLog()
})

// ---- 顶栏：任务名 / 窗口标题 / 模型 ----

const currentSession = computed(
  () => store.currentSessions.find(s => s.id === store.currentSessionId) || null
)
const currentSessionName = computed(() => {
  const s = currentSession.value
  if (!s) return ''
  if (s.name) return s.name
  if (s.isDefault) {
    const h = store.hosts.find(x => x.id === store.currentHostId)
    if (h) return `${h.name} (${h.user}@${h.addr})`
  }
  return '未命名任务'
})
const hostOptions = computed(() =>
  store.hosts.map(h => ({ value: h.id, label: `${h.name} — ${h.user}@${h.addr}` }))
)
watch(currentSessionName, name => syncWindowTitle(name), { immediate: true })

const activeProfileId = computed(() => store.llmProfiles.find(p => p.active)?.id || '')
const modelOptions = computed(() =>
  store.llmProfiles.map(p => ({
    value: p.id,
    label: `${p.name} · ${p.model}`,
    badge: providerBadge(p)
  }))
)
async function onSwitchModel(id) {
  if (!id || id === activeProfileId.value) return
  const p = store.llmProfiles.find(x => x.id === id)
  try {
    await switchProfile(id)
  } catch {
    return // 失败提示已由 switchProfile 推送，成功消息不能照发
  }
  push({
    kind: 'system',
    content: `已切换模型到「${p ? p.name : id}」，本会话上下文保留，下一轮对话生效。`
  })
}

async function onClearSession() {
  if (!store.currentHostId) {
    push({ kind: 'error', content: '请先选择一台主机。' })
    return
  }
  await clearSession(store.currentHostId, store.currentSessionId)
}

async function onCompactSession() {
  if (!store.currentHostId) {
    push({ kind: 'error', content: '请先选择一台主机。' })
    return
  }
  await compactSession(store.currentHostId, store.currentSessionId)
}

// ---- 弹窗：新建任务 / 命令详细 ----

const showNewModal = ref(false)
const newName = ref('')
const newEl = ref(null)

function startNewSession() {
  if (!store.currentHostId) {
    push({ kind: 'error', content: '请先选择一台主机。' })
    return
  }
  newName.value = ''
  showNewModal.value = true
  nextTick(() => { if (newEl.value) newEl.value.focus() })
}

async function commitNewSession() {
  const name = newName.value.trim()
  showNewModal.value = false
  newName.value = ''
  if (!name) return
  await createSession(name)
}

function onNewKeydown(e) {
  if (e.isComposing || e.keyCode === 229) return
  if (e.key === 'Enter') {
    e.preventDefault()
    commitNewSession()
  } else if (e.key === 'Escape') {
    e.preventDefault()
    showNewModal.value = false
  }
}

const showCmdDetail = ref(false)
const cmdCopied = ref(false)
watch(showCmdDetail, v => { if (!v) cmdCopied.value = false })
watch(() => store.pending, v => { if (!v) showCmdDetail.value = false })

const pendingCmd = computed(() => {
  const p = store.pending
  if (!p) return ''
  const raw = p.command || p.args
  return typeof raw === 'string' ? raw : JSON.stringify(raw ?? '', null, 2)
})

async function copyPendingCmd() {
  const text = pendingCmd.value
  if (!text) return
  let ok = false
  try {
    await navigator.clipboard.writeText(text)
    ok = true
  } catch { ok = false }
  if (!ok) {
    // WKWebView 等环境下 clipboard API 可能不可用，退回 execCommand。
    const ta = document.createElement('textarea')
    ta.value = text
    ta.style.position = 'fixed'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.select()
    try { ok = document.execCommand('copy') } catch { ok = false }
    ta.remove()
  }
  if (ok) {
    cmdCopied.value = true
    setTimeout(() => { cmdCopied.value = false }, 1500)
  }
}

function onWinKeydown(e) {
  // 新建任务弹窗在顶层时这里让路，避免一次 Esc 同时关掉两层。
  if (showNewModal.value) return
  if (e.key === 'Escape' && showCmdDetail.value) showCmdDetail.value = false
}

onMounted(async () => {
  window.addEventListener('keydown', onWinKeydown)
  // jsdom 里没有 ResizeObserver，测试环境不该因为这一点炸掉。
  if (typeof ResizeObserver !== 'undefined' && rootEl.value) {
    ro = new ResizeObserver(() => doFit(currentKey.value))
    ro.observe(rootEl.value)
  }
  // 挂载时若已选中主机（App 里 ConsolePanel 常驻、bootstrap 已填好 currentHostId），
  // watcher 不会因「无变化」触发，这里显式把当前任务的表面拉起来。
  if (store.currentHostId) await attachSurface(store.currentHostId, store.currentSessionId)
  focusTerm(currentKey.value)
})

onBeforeUnmount(() => {
  if (ro) {
    ro.disconnect()
    ro = null
  }
  window.removeEventListener('keydown', onWinKeydown)
  // 销毁本地实例（DOM 已经没了），但**不关远端终端**：卸载往往只是换视图，
  // 用户希望切回来时 shell 还在原目录。真正的清理走「关闭终端」与切换任务；
  // 应用退出时后端 shutdown 会关掉所有终端。
  for (const key of [...terms.keys()]) disposeTerm(key)
  attachedKey = ''
})
</script>

<template>
  <div class="console">
    <header class="bar">
      <div class="row grow">
        <label class="inline-label">目标主机</label>
        <UiSelect
          v-model="store.currentHostId"
          class="host-select"
          :options="hostOptions"
          placeholder="（暂无主机，请先到「主机管理」添加）"
        />
        <button
          class="sm tonal new-session-btn"
          :disabled="!store.currentHostId"
          title="为当前主机新建一条任务"
          @click="startNewSession"
        >＋ 新建任务</button>
      </div>

      <div class="seg">
        <button :class="{ on: !showArchive }" @click="showArchive = false">终端</button>
        <button :class="{ on: showArchive }" @click="showArchive = true">对话归档</button>
      </div>

      <!-- 新建任务弹窗（应用内，WKWebView 上原生 prompt 不弹） -->
      <div v-if="showNewModal" class="overlay" @click.self="showNewModal = false">
        <div class="modal">
          <h3>新建任务</h3>
          <input
            ref="newEl"
            v-model="newName"
            placeholder="任务名称，如「nginx 排查」"
            @keydown="onNewKeydown"
          />
          <div class="modal-hint">Enter 确定 · Esc 取消 · 留空则不创建</div>
          <div class="modal-actions">
            <button class="sm" @click="showNewModal = false">取消</button>
            <button class="sm primary" @click="commitNewSession">创建</button>
          </div>
        </div>
      </div>
    </header>

    <!-- 能力/风险横幅：说清两条输入路径的不同风险等级 -->
    <div class="sess-banner">
      <span>
        控制台<span v-if="currentHost"> · <b>{{ currentHost.user }}@{{ currentHost.addr }}</b></span>
        ｜ 在这里可直接敲 <b>Linux 命令</b>执行（先过策略，高危会请你确认），
        也能用<b>自然语言</b>（中文 / <span class="mono">?</span> 开头）把需求交给 <b>LLM</b>。
        top/vim 这类程序原生全屏接管，退出后最后一帧定格进模型上下文与审计。
      </span>
      <button v-if="canClose" class="sm" @click="onClose">关闭终端</button>
    </div>

    <!-- 活表面：整幅 xterm，附着当前主机的常驻 shell PTY -->
    <div class="surface" ref="rootEl" v-show="!showArchive">
      <div v-if="!store.currentHostId" class="surface-empty">请先选择一台主机。</div>
      <template v-else>
        <div v-if="statusLine" class="surface-status" :class="{ bad: st && st.status === 'error' }">
          <span>{{ statusLine }}</span>
          <button v-if="canRetry" class="sm" @click="onRetry">重新打开</button>
        </div>
        <div class="surface-body">
          <div
            v-for="key in openedKeys"
            :key="key"
            class="surface-host"
            :class="{ hidden: key !== currentKey }"
            :ref="el => setSurfaceEl(key, el)"
          ></div>
        </div>
      </template>
    </div>

    <!-- 对话归档：只读时间线（复用 store.entries 渲染，不接活事件、无交互按钮） -->
    <div class="log archive" ref="logEl" v-show="showArchive">
      <div v-for="e in store.entries" :key="e.id" class="entry" :class="'entry-' + e.kind">
        <!-- 屏幕快照：模型读的那份净化文本，人看同一份，审计才对得上 -->
        <div v-if="e.kind === 'snapshot'" class="result">
          <div class="result-head">
            <span v-if="e.final" class="badge warn">终端结束画面 · 进模型上下文</span>
            <span v-else class="badge warn">终端屏幕快照 · 进模型上下文</span>
            <span v-if="e.redacted" class="badge warn">已脱敏 {{ e.redacted }} 处</span>
            <span class="muted tiny">{{ e.final ? '全屏 TUI 退出/终端结束时定格的最后一屏（净化文本）' : '常驻终端静止后的可见屏幕（净化文本）' }}</span>
          </div>
          <pre>{{ e.text }}</pre>
          <div v-if="e.injection && e.injection.length" class="tool-reason">
            检测到疑似提示注入：{{ e.injection.join('、') }}（内容已标记为不可信）
          </div>
        </div>

        <div v-else-if="e.kind === 'user'" class="msg user">
          <div class="msg-role">你</div>
          <div class="msg-body">{{ e.content }}</div>
        </div>

        <div v-else-if="e.kind === 'assistant'" class="msg assistant">
          <div class="msg-role">LLM</div>
          <div class="msg-body">{{ e.content }}<span v-if="e.streaming" class="caret"></span><span v-if="!e.content && !e.streaming" class="muted tiny">（模型返回了空回复）</span></div>
        </div>

        <div v-else-if="e.kind === 'tool'" class="tool">
          <div class="tool-head">
            <span class="tool-name mono">{{ e.tool.name }}</span>
            <span class="badge" :class="decisionClass(e.tool.decision)">
              {{ decisionLabel(e.tool.decision) }}
            </span>
            <span class="badge neutral">{{ statusLabel(e.tool.status) }}</span>
            <span class="muted" v-if="e.tool.hostName">@{{ e.tool.hostName }}</span>
            <span class="muted right" v-if="e.tool.durationMs">{{ e.tool.durationMs }}ms</span>
          </div>
          <pre v-if="e.tool.command" class="tool-cmd">{{ e.tool.command }}</pre>
          <div v-if="e.tool.reason" class="tool-reason">{{ e.tool.reason }}</div>
        </div>

        <div v-else-if="e.kind === 'result'" class="result">
          <div class="result-head">
            <span class="muted">退出码 {{ e.exitCode }}</span>
            <span v-if="e.redacted" class="badge warn">已脱敏 {{ e.redacted }} 处</span>
            <span class="muted tiny">原始输出 · 发给 LLM 前已脱敏并标记为不可信</span>
          </div>
          <pre>{{ e.content }}</pre>
        </div>

        <div v-else-if="e.kind === 'injection'" class="injection">
          <div class="injection-title">远端输出中检测到疑似提示注入</div>
          <div class="injection-body">命中话术：{{ e.findings.join('、') }}</div>
          <pre v-if="e.command" class="injection-cmd">{{ e.command }}</pre>
          <div class="injection-hint">
            该内容来自 <span class="mono">{{ e.hostName }}</span> 的执行结果，已被标记为不可信数据，
            不会作为指令执行。建议排查该主机上这段文本的来源。
          </div>
        </div>

        <div v-else-if="e.kind === 'error'" class="msg error">
          <div class="msg-role">错误</div>
          <div class="msg-body">{{ e.content }}</div>
        </div>

        <div v-else class="msg system">
          <div class="msg-body">{{ e.content }}</div>
        </div>
      </div>

      <div v-if="!store.entries.length" class="archive-empty">
        这条会话还没有归档记录。切到「终端」直接输入提问 / 敲命令。
      </div>
    </div>

    <!-- 审批条：overlay 在表面之上，交互入口照旧 -->
    <div v-if="store.pending" class="approval">
      <div class="approval-head">
        <span class="badge warn">需要你的批准</span>
        <span class="mono">{{ store.pending.name }}</span>
        <span class="muted" v-if="store.pending.hostName">@{{ store.pending.hostName }}</span>
        <button class="sm right" @click="showCmdDetail = true">详细</button>
      </div>
      <pre class="approval-cmd">{{ store.pending.command || store.pending.args }}</pre>
      <div class="approval-reason">{{ store.pending.reason }}</div>
      <div class="row" style="margin-top: 10px">
        <button class="ok" @click="approve(true)">批准执行</button>
        <button class="danger" @click="approve(false)">拒绝</button>
      </div>
    </div>

    <!-- 底部 composer：输入框 + 工具条 -->
    <div class="composer-bar">
      <div class="composer-tools">
        <span
          v-if="store.running && !store.pending && !streamingNow"
          class="thinking-inline"
          aria-live="polite"
        >
          <svg class="thinking-icon" width="14" height="14" viewBox="0 0 24 24" aria-hidden="true">
            <g fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round">
              <path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8" />
              <path d="M21 3v5h-5" />
              <path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16" />
              <path d="M8 16H3v5" />
            </g>
            <path class="thinking-spark" d="M19 1.6l.7 1.9 1.9.7-1.9.7-.7 1.9-.7-1.9-1.9-.7 1.9-.7z" fill="currentColor" stroke="none" />
          </svg>
          Thinking
        </span>
        <UiSelect
          class="model-select"
          :model-value="activeProfileId"
          :options="modelOptions"
          :disabled="store.running || !store.llmProfiles.length"
          placeholder="（暂无方案，请到「设置」添加）"
          @change="onSwitchModel"
        />
        <button
          class="sm"
          :disabled="store.compacting"
          :title="store.compacting ? '' : '让 LLM 把全部历史重写成一段摘要（花一次 API 调用）。日常使用无需手动压缩：旧的对话轮次已自动移出上下文。'"
          @click="onCompactSession"
        >
          {{ store.compacting ? '压缩中…' : '压缩上下文' }}
        </button>
        <button class="sm" @click="onClearSession">清空上下文</button>
        <span class="composer-sep"></span>
        <button class="sm" @click="clearLog" :disabled="store.running || store.busy">清空记录</button>
        <button v-if="store.running || store.busy" class="sm danger" @click="stop">中断</button>
      </div>
    </div>

    <!-- 命令详细弹窗：审批卡内只做预览，超长命令在这里看全文 -->
    <div v-if="showCmdDetail" class="overlay" @click.self="showCmdDetail = false">
      <div class="modal cmd-modal">
        <h3>待执行命令详细</h3>
        <pre class="cmd-detail">{{ pendingCmd }}</pre>
        <div class="modal-actions">
          <button class="sm" @click="copyPendingCmd">{{ cmdCopied ? '已复制' : '复制' }}</button>
          <button class="sm primary" @click="showCmdDetail = false">关闭</button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.console {
  display: flex;
  flex-direction: column;
  height: 100%;
  overflow: hidden;
  position: relative;
}

.bar {
  display: flex;
  align-items: center;
  gap: 14px;
  padding: 12px 18px;
  border-bottom: 1px solid var(--border);
  background: var(--surface);
  flex-shrink: 0;
}
.inline-label { font-size: 12px; color: var(--text-2); margin: 0; white-space: nowrap; }
.host-select { max-width: 420px; }
.new-session-btn { flex-shrink: 0; white-space: nowrap; font-size: 13px; }

/* 弹窗（应用内，与侧边栏删除确认同风格） */
.overlay {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.35);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 200;
}
.modal {
  background: var(--surface);
  border: 1px solid var(--border-strong);
  border-radius: var(--radius);
  padding: 18px 20px;
  width: min(400px, calc(100% - 60px));
  box-shadow: var(--shadow);
}
.modal h3 { margin: 0 0 12px; font-size: 14px; }
.modal-hint { font-size: 11px; color: var(--text-3); margin-top: 6px; }
.modal-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 14px; }

.cmd-modal { width: min(760px, calc(100% - 60px)); }
.cmd-detail {
  margin: 0;
  max-height: 55vh;
  overflow: auto;
  background: var(--inset-bg);
  border-radius: 7px;
  padding: 10px 12px;
  color: var(--text);
  font-family: var(--mono);
  font-size: 12.5px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
}

.seg { display: flex; gap: 0; border: 1px solid var(--border); border-radius: 8px; overflow: hidden; }
.seg button {
  border: none;
  border-radius: 0;
  min-height: 34px;
  height: 34px;
  padding: 0 16px;
  margin: 0;
  background: var(--surface);
  color: var(--text-2);
}
.seg button.on { background: var(--accent-bg); color: var(--accent); font-weight: 500; }
.model-select { flex: 0 1 300px; min-width: 200px; }

.sess-banner {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 8px 16px;
  background: var(--warn-bg);
  color: var(--warn);
  font-size: 12.5px;
  line-height: 1.55;
  border-bottom: 1px solid var(--border);
}
.sess-banner .sm { flex-shrink: 0; }

/* ---- 活表面 ---- */
.surface {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
  background: var(--bg);
}
.surface-empty { padding: 24px 16px; color: var(--text-3); font-size: 13px; }
.surface-status {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 6px 14px;
  font-size: 12.5px;
  color: var(--text-2);
  background: var(--surface-2);
  flex-shrink: 0;
}
.surface-status.bad { color: var(--danger); background: var(--danger-bg); }
.surface-body { position: relative; flex: 1; min-height: 0; }
/* 每个主机一个容器，全部叠在同一处，只显示当前主机那个。容器必须一直留在
   DOM 里：xterm 实例绑在它的元素上，元素被移除实例就废了，滚动回放也跟着没。 */
.surface-host { position: absolute; inset: 8px; }
.surface-host.hidden { display: none; }
.surface-host :deep(.xterm) { height: 100%; }

/* ---- 对话归档（只读时间线）---- */
.log {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 12px 16px 8px;
  display: flex;
  flex-direction: column;
  gap: 10px;
  background: var(--bg);
  font-family: var(--mono);
  font-size: 12.5px;
  line-height: 1.6;
}
.archive-empty { color: var(--text-3); font-size: 12.5px; padding: 8px 2px; }

.msg { display: flex; gap: 10px; font-family: var(--font, inherit); }
.msg-role {
  flex-shrink: 0;
  width: 44px;
  font-size: 11px;
  color: var(--text-3);
  padding-top: 3px;
  text-align: right;
}
.msg-body { flex: 1; line-height: 1.7; white-space: pre-wrap; word-break: break-word; }
.msg.user .msg-body {
  background: var(--accent-bg);
  border-radius: var(--radius);
  padding: 9px 12px;
  color: var(--accent);
}
.msg.assistant .msg-body { padding: 3px 0; }
.msg.error .msg-body {
  background: var(--danger-bg);
  color: var(--danger);
  border-radius: 8px;
  padding: 9px 12px;
}
.msg.system .msg-body { color: var(--text-3); font-size: 12px; }

.caret {
  display: inline-block;
  width: 2px;
  height: 1em;
  margin-left: 2px;
  vertical-align: text-bottom;
  background: var(--text-2);
  animation: caret-blink 1s steps(2, start) infinite;
}
@keyframes caret-blink { to { visibility: hidden; } }
@media (prefers-reduced-motion: reduce) { .caret { animation: none; } }

.tool {
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 10px 12px;
  background: var(--surface);
  font-family: var(--font, inherit);
}
.tool-head { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.tool-name { font-weight: 500; color: var(--purple); }
.right { margin-left: auto; }
.tool-cmd { margin-top: 8px; background: var(--surface-2); border-radius: 7px; padding: 8px 10px; }
.tool-reason { margin-top: 6px; font-size: 12px; color: var(--text-3); }

.badge {
  font-size: 11px;
  padding: 1px 7px;
  border-radius: 20px;
  border: 1px solid transparent;
}
.badge.ok { background: var(--ok-bg); color: var(--ok); }
.badge.warn { background: var(--warn-bg); color: var(--warn); }
.badge.danger { background: var(--danger-bg); color: var(--danger); }
.badge.neutral { background: var(--surface-2); color: var(--text-2); }

.result { border-left: 2px solid var(--border); padding-left: 12px; font-family: var(--font, inherit); }
.result-head { display: flex; align-items: center; gap: 8px; margin-bottom: 5px; }
.result pre { color: var(--text-2); white-space: pre-wrap; word-break: break-word; }
.tiny { font-size: 11px; }

.injection {
  border: 1px solid var(--danger);
  background: var(--danger-bg);
  border-radius: var(--radius);
  padding: 11px 13px;
  font-family: var(--font, inherit);
}
.injection-title { font-weight: 500; color: var(--danger); }
.injection-body { font-size: 12px; color: var(--danger); margin-top: 5px; }
.injection-cmd {
  margin-top: 7px;
  background: var(--inset-bg);
  border-radius: 7px;
  padding: 7px 9px;
  color: var(--danger);
}
.injection-hint { font-size: 12px; color: var(--text-2); margin-top: 7px; line-height: 1.65; }

.approval {
  margin: 0 18px 10px;
  border: 1px solid var(--warn);
  background: var(--warn-bg);
  border-radius: var(--radius);
  padding: 12px 14px;
  flex-shrink: 0;
  max-height: 70%;
  display: flex;
  flex-direction: column;
}
.approval-head { display: flex; align-items: center; gap: 8px; margin-bottom: 8px; flex-shrink: 0; }
.approval-cmd {
  background: var(--inset-bg);
  border-radius: 7px;
  padding: 8px 10px;
  color: var(--text);
  max-height: 168px;
  overflow: hidden;
}
.approval-reason { font-size: 12px; color: var(--warn); margin-top: 6px; flex-shrink: 0; }
.approval .row { flex-shrink: 0; }

.composer-bar {
  flex-shrink: 0;
  border-top: 1px solid var(--border);
  background: var(--surface);
  padding: 8px 18px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.thinking-inline {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  color: var(--text-3);
  font-size: 12px;
  white-space: nowrap;
  flex-shrink: 0;
  animation: thinking-breathe 1.8s ease-in-out infinite;
}
.thinking-icon { flex-shrink: 0; display: block; }
.thinking-spark { transform-origin: 19px 5px; animation: thinking-spark 1.8s ease-in-out infinite; }
@keyframes thinking-breathe { 0%, 100% { opacity: .55; } 50% { opacity: 1; } }
@keyframes thinking-spark { 0%, 100% { opacity: .35; } 50% { opacity: 1; } }
@media (prefers-reduced-motion: reduce) {
  .thinking-inline, .thinking-spark { animation: none; opacity: .8; }
}

.composer-tools { display: flex; align-items: center; gap: 8px; }
.composer-sep { width: 1px; height: 18px; background: var(--border); margin: 0 4px; flex-shrink: 0; }
.grow-hint { margin-left: auto; }
</style>
