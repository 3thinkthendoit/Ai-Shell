<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import {
  store, approve, stop, clearLog, clearSession, compactSession, push,
  createSession, refreshSessions, switchProfile, syncWindowTitle,
  closeTerminal, reportActiveSession,
  openFileManager, closeFileManager,
  bucketKey
} from '../store'
import { providerBadge, providerTermBadge } from '../providerLogos'
import {
  openedKeys, ensureTerm, disposeTerm, doFit, focusTerm, setTheme,
  setSurfaceEl, getTerm, paintWelcomeHintIfNeeded
} from '../terminal/termSession'
import { onTermData, paintInputPromptIfNeeded } from '../terminal/termInput'
import UiSelect from './UiSelect.vue'
import FileManagerModal from './FileManagerModal.vue'

// 控制台 = 一块整幅 xterm，附着当前主机的常驻 shell PTY（阿里云 Workbench 同款）。
// 人可以直接在表面里敲（原始按键进 PTY，不过策略）；底部 composer 走分流：
// 中文/? → Agent，shell 行 → 硬拒绝过滤 → 写进同一片 PTY。top/vim 原生全屏接管。
// 「对话归档」开关把主区切成只读时间线（复用 store.entries 渲染），表面用 v-show
// 常驻不销毁 —— xterm 实例一销毁滚动回放就没了。
//
// xterm 实例生命周期在 terminal/termSession.js，按键行编辑在 terminal/termInput.js，
// 提交分流在 terminal/termSubmit.js，后端事件在 terminal/termEvents.js（经 store 绑定）。
// 本组件只保留：布局/挂载协调、审批 UI、剪贴板接线与归档视图派生。

// ---- 常驻终端表面（xterm 实例生命周期，从 InteractiveTerminal 折叠进来）----
// 表面按「任务」组织：一块表面对应一条 (host, session)，键用 store.bucketKey。
// 只保留当前聚焦任务的 shell：切换任务时关闭上一任务的表面/PTY，为目标任务
// 新开一条全新 xterm（回来也是全新的，旧现场丢弃，历史仍在「对话归档」里）。
const rootEl = ref(null)          // ResizeObserver 的观察目标（.surface）
let attachedKey = ''              // 当前附着的那块表面键（切任务时据此关闭旧的）
let ro = null

// 对话归档：只读时间线开关。false = 活表面，true = 归档视图。
const showArchive = ref(false)

// 归档时间线：把连续的「工具调用 / 结果」折成一个可展开的组（`N 次工具调用`），
// 其余条目原样。这样一段对话不会被一串命令 / 退出码刷屏。
const timeline = computed(() => {
  const out = []
  let group = null
  for (const e of store.entries) {
    if (e.kind === 'tool' || e.kind === 'result') {
      if (!group) {
        group = { kind: 'toolGroup', id: 'tg-' + e.id, calls: [] }
        out.push(group)
      }
      if (e.kind === 'tool') {
        group.calls.push({ key: 'c-' + e.id, tool: e, result: null })
      } else {
        const hit = group.calls.find(c => c.tool && c.tool.toolId === e.toolId)
        if (hit) hit.result = e
        else group.calls.push({ key: 'r-' + e.id, tool: null, result: e })
      }
      continue
    }
    group = null
    out.push(e)
  }
  return out
})

// 工具组的展开态（默认收起：只显示一行 `N 次工具调用`）。
const openGroups = reactive({})
function toggleGroup(id) { openGroups[id] = !openGroups[id] }

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

// 终端配色跟随亮/暗主题：切换时转交 termSession.setTheme（它同步所有已打开实例）。
watch(() => store.theme, t => setTheme(t))

// disposeSurface：销毁本地 xterm 实例（termSession.disposeTerm 负责），并
// 顺带清掉 copy-on-select 防抖。
function disposeSurface(key) {
  cancelCopyOnSelect(key)
  disposeTerm(key)
}

// ensureSurface：在 termSession 建好/复用实例之后，接上本组件负责的用户交互
// 回调（onData 本地行编辑、粘贴/复制按键接管、选区复制）。
async function ensureSurface(hostId, sessionId) {
  await ensureTerm(hostId, sessionId, store.theme)
  attachHandlers(bucketKey(hostId, sessionId))
}

// attachHandlers 给某块表面接上交互回调。幂等标记挂在**实例登记项**上（t.wired）
// 而不是组件里的模块级 Set：ensureTerm 在 status 变为 closed/error 后会不经
// disposeSurface 直接 disposeTerm+重建（「重新打开」按钮就是这么走的），按 key
// 去重的 Set 会残留旧标记，新实例永远接不上 onData —— 表现为画面正常渲染、
// 按键全部石沉大海的假死（进入 vim 后无法操作的一个直接原因）。
// 标记随登记项生灭：复用活实例时不重复挂（否则按键派发两次），重建后自动重接。
function attachHandlers(key) {
  const t = getTerm(key)
  if (!t || t.wired) return
  t.wired = true
  const term = t.term

  // 用户按键 → 本地行编辑（见 termInput.onTermData）。
  term.onData(d => onTermData(key, d))

  // 粘贴接线：xterm 本身**不处理剪贴板** —— Ctrl+V 会被浏览器/WebView2 先截走，
  // 结果就是一个字节都进不了 onData。所以这里必须自己接管按键：拦下键事件
  // （返回 false 阻止 xterm 再处理），异步读剪贴板后把整段文本喂进 onTermData。
  term.attachCustomKeyEventHandler(e => {
    if (e.type !== 'keydown') return true
    // e.key 在合成事件/极端环境下可能缺失，统一取小写再比较，避免直接
    // toLowerCase() 抛 TypeError（回调里抛异常会让整个按键链路失效）。
    const k = (e.key || '').toLowerCase()
    // 复制（优先级最高）：Ctrl+Shift+C 与 Ctrl+Insert。返回 false 阻止 xterm
    // 继续把按键当输入处理，否则 Ctrl+Insert 会被解析成转义序列打进 PTY。
    const isCopy = (e.ctrlKey && e.shiftKey && k === 'c') ||
                   (e.ctrlKey && k === 'insert')
    if (isCopy) {
      e.preventDefault()
      void copySelection(key)
      return false
    }
    // 裸 Ctrl+C 的语义修正：**有选区时复制，无选区时才发 SIGINT**。
    // 无选区时返回 true 交给原有链路，\x03 照旧透传给 PTY，中断能力不受影响。
    if (e.ctrlKey && !e.shiftKey && !e.altKey && k === 'c') {
      const sel = term.getSelection ? term.getSelection() : ''
      if (sel) {
        e.preventDefault()
        void copySelection(key)
        return false
      }
      return true
    }
    const isPaste = (e.ctrlKey && !e.altKey && k === 'v') ||
                    (e.shiftKey && k === 'insert')
    if (!isPaste) return true
    e.preventDefault()
    // WebView2 下 readText 可能因权限被拒：静默失败，不要弹出未捕获的 rejection。
    navigator.clipboard?.readText?.()
      .then(text => { if (text) onTermData(key, text) })
      .catch(() => {})
    return false
  })

  // copy-on-select：拖选一松开就把选区写进剪贴板（防抖 60ms，避开拖动中途的
  // 中间态）。选中后再按 Ctrl+C / Ctrl+Shift+C 也能复制，两条路并行不冲突。
  if (term.onSelectionChange) {
    term.onSelectionChange(() => {
      const sel = term.getSelection ? term.getSelection() : ''
      if (!sel) { cancelCopyOnSelect(key); return }
      scheduleCopyOnSelect(key)
    })
  }
}

// doFit/focusTerm/disposeTerm/reportResize/ensureTerm 的具体实现已拆到
// terminal/termSession.js；本组件通过上面的 ensureSurface/disposeSurface 封装调用。

// 欢迎引导绘制与光标/擦除/透传判定工具（paintWelcomeHintIfNeeded、isFullscreen、
// isCmdRunning、cursorCol、displayWidth、eraseLocalLine、clearLocalEcho、
// flushLocalToPty）都已拆到 terminal/termSession.js；本地行编辑归类见
// terminal/termInput.js，提交分流见 terminal/termSubmit.js，本组件不再自带。

// 提交队列（enqueueSubmit/submitLine/dispatchLine）、本地行编辑路由（onTermData）、
// 落点写入（surfaceWrite）与占位符擦除（clearWelcomeHint/clearLLMHint）已全部
// 拆到 terminal/termSubmit.js 与 terminal/termSession.js。

async function onClose() {
  const hostId = store.currentHostId
  if (!hostId) return
  const key = currentKey.value
  await closeTerminal(hostId, store.currentSessionId)
  disposeSurface(key)
  if (attachedKey === key) attachedKey = ''
}

function onRetry() {
  ensureSurface(store.currentHostId, store.currentSessionId)
}

// 从归档切回表面时重新测量：隐藏期间容器是 0x0，fit 一直跳过。
// 顺带重画一次欢迎引导：用户切走又回来，视线重新落到这个 shell 上，
// 此刻提示「这里能说人话」正是有效时机（whyHintShown 的「每次回来都提示」
// 语义就落在这一句上）。
watch(showArchive, async on => {
  if (on) {
    scrollLog()
    return
  }
  await nextTick()
  doFit(currentKey.value)
  paintWelcomeHintIfNeeded(currentKey.value, true)
})

// agent 回合结束 → 回到输入态。提问不走 PTY，远端 shell 不会打印新提示符；
// 若本轮 PTY 一次都没被写过（纯问答），表面补一个合成 ❯ 提示符，
// 否则光标悬在空行上，看起来像「回复完没回到命令输入行」。
// PTY 有写入（top 交接 / shell 命令）时 shell 自己会回到提示符，不补。
//
// 「回到控制台」必须是**必达**的，所以这里只保留两类真的不能补的情况：
//   - passthrough：本轮把控制权交给了远端程序（top/vim 等还在跑），
//     补 ❯ 会插进那个程序的界面里。
//   - isPtyDirty：本轮 shell 命令让远端自己打印了真提示符。
// 曾经还有一条 `t.line`（本地缓冲非空就跳过）—— 那是错的：运行期间敲的键
// 已经不再进缓冲（见 onTermData 的 running 分支），而任何残留的半截字符都不该
// 变成「所以不给你提示符」的理由。用户要的是每次答完都回到命令行。
watch(() => store.running, (v, ov) => {
  if (ov !== true || v) return
  // 补合成 ❯ 的判定（passthrough / isPtyDirty / 已有合成行）全部下沉到
  // termInput.paintInputPromptIfNeeded，这里只负责在回合结束时触发。
  paintInputPromptIfNeeded(currentKey.value)
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

// 上下文用量：后端每步请求前推一次 agent:context（见 agent.EvContext），
// 由 store 收进 store.contextUsage —— 事件只有一个消费者入口（bindEvents）。
//
// 为什么要显示：上下文保护靠的是三个字节上限「盲估」，真爆窗口时是服务端报错、
// 界面只能显示一条看不懂的 400。有了它，用户能提前看到「已经 87% 了」。
const ctxUsage = computed(() => {
  const { tokens, window: win, messages } = store.contextUsage
  if (!tokens) return { text: '', level: '', title: '' }
  const text = '约 ' + fmtTokens(tokens) + ' tokens'
  if (!win) {
    return { text, level: 'unknown', title: '未能识别该模型的上下文窗口，仅显示估算 token 数' }
  }
  const pct = Math.round(tokens * 100 / win)
  const level = pct >= 95 ? 'danger' : pct >= 80 ? 'warn' : pct >= 60 ? 'mid' : 'ok'
  return {
    text: `${text} · ${pct}%`,
    level,
    title: `本次请求约 ${tokens.toLocaleString()} tokens，` +
      `占模型窗口（${win.toLocaleString()}）的 ${pct}%\n` +
      `${messages} 条消息。接近上限时服务端会直接报错 —— ` +
      '可减少单次命令的输出量，或点「压缩上下文 / 清空上下文」。'
  }
})

// fmtTokens 把 token 数压成好读的形式：128000 → 128K，12345 → 12.3K。
function fmtTokens(n) {
  if (n >= 1000) {
    const k = n / 1000
    return (k >= 100 ? Math.round(k) : Math.round(k * 10) / 10) + 'K'
  }
  return String(n)
}

// archiveBadge 给归档里每条 assistant 消息算一个「模型图标 + 名字」的展示对象。
//
// **只认这条消息自己的信息**（e.badge 由 store 在推入时算好、或退一步用 e.model）——
// 刻意不做「回退到当前选中方案」：归档是历史，历史消息该显示它当时用的模型，
// 用此刻选中的方案去补历史消息会张冠李戴（也更容易与「这条回复是什么模型给的」
// 的语义冲突）。拿不到就返回 null，模板退化成角色名。
function archiveBadge(e) {
  if (e && e.badge) return e.badge
  if (e && e.model) {
    try { return providerTermBadge({ model: e.model, name: e.model }) } catch { return null }
  }
  return null
}

// ---- 审批倒计时 ----
//
// 后端把失效时刻（deadlineMs）随审批一起下发，这里只做**显示**：
// 每秒重算一次剩余秒数。判断「是否已失效」不在这里做 —— 后端超时后会发
// approval:expired，由 store 清掉这条。前端若自己判定过期就收起，
// 会因为时钟漂移出现「界面说没了、后端还在等」的错位（见 ToolCallView.DeadlineMs）。
const nowTick = ref(Date.now())
let tickTimer = null

// 剩余秒数；没有 deadline（0/缺省）时返回 null，界面就不显示倒计时。
const approvalRemainSec = computed(() => {
  const dl = store.pending && store.pending.deadlineMs
  if (!dl) return null
  const left = Math.ceil((dl - nowTick.value) / 1000)
  return left > 0 ? left : 0
})

// 倒计时文案：>1 分钟显示 m:ss（更好读），否则显示秒。
const approvalCountdown = computed(() => {
  const s = approvalRemainSec.value
  if (s === null) return ''
  if (s >= 60) {
    const m = Math.floor(s / 60)
    const r = s % 60
    return `${m}:${String(r).padStart(2, '0')}`
  }
  return `${s} 秒`
})

// 计时器只在有 pending 且带 deadline 时跑：没审批时白转一秒一次是浪费。
watch(
  () => {
    const p = store.pending
    return p && p.deadlineMs ? p.id : null
  },
  id => {
    if (tickTimer) {
      clearInterval(tickTimer)
      tickTimer = null
    }
    if (!id) return
    nowTick.value = Date.now()
    tickTimer = setInterval(() => {
      nowTick.value = Date.now()
    }, 1000)
  }
)

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
  const t = getTerm(key)
  if (!t) return
  await closeTerminal(t.hostId, t.sessionId)
  disposeSurface(key)
}

// attachSurface 为目标任务拉起一条全新 shell：关掉当前附着着的其它表面，
// 新建本任务专属表面（ensureTerm → openTerminal），并把「现在看着哪条会话」
// 报给后端（Ask 的 Agent 上下文仍需）。不再画会话分隔线 —— 表面不再共享。
async function attachSurface(hostId, sessionId) {
  if (!hostId) return
  const key = bucketKey(hostId, sessionId)
  if (attachedKey && attachedKey !== key) await detachSurface(attachedKey)
  await ensureSurface(hostId, sessionId)
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

// 选中的方案：优先用 store.selectedProfileId（用户刚点的那个，同步写入），
// 回退到后端标记的 active。前者保证「界面上显示选中的」与「本轮请求带的」一致。
const activeProfileId = computed(() =>
  store.selectedProfileId || (store.llmProfiles.find(p => p.active) || {}).id || ''
)
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
  // 同步生效：先把选中项写上，紧接着这一轮的请求就会带上它（见 store.ask）。
  store.selectedProfileId = id
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

// writeClipboard 往系统剪贴板写文本，返回是否成功。
//
// 为什么不能只用 navigator.clipboard：WebView2 / WKWebView 下它需要页面处于
// 「聚焦 + 安全上下文」，而终端场景里焦点在 xterm 的隐藏 textarea 上、事件又
// 常在鼠标松开那一刻触发，writeText 会被拒绝。所以统一退回 execCommand。
// 返回布尔值而不是吞掉异常：调用方要据此决定要不要给用户提示。
async function writeClipboard(text) {
  if (!text) return false
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    // 继续走下面的兜底
  }
  const ta = document.createElement('textarea')
  ta.value = text
  ta.setAttribute('readonly', '')
  ta.style.position = 'fixed'
  ta.style.left = '-9999px'
  ta.style.opacity = '0'
  document.body.appendChild(ta)
  ta.select()
  let ok = false
  try { ok = document.execCommand('copy') } catch { ok = false }
  ta.remove()
  return ok
}

async function copyPendingCmd() {
  const text = pendingCmd.value
  if (!text) return
  const ok = await writeClipboard(text)
  if (ok) {
    cmdCopied.value = true
    setTimeout(() => { cmdCopied.value = false }, 1500)
  }
}

// ---- 终端框选复制（copy-on-select / Ctrl+Shift+C / Ctrl+Insert）----
//
// 为什么要自己接：xterm v6 默认 Canvas 渲染，文本是画布像素，浏览器原生框选
// 选不到任何东西；Ctrl+C 又被 onTermData 当作 \x03 透传给 PTY（SIGINT）。
// 但 xterm 自己维护了一份选区模型 —— term.getSelection() 与 DOM 渲染无关，
// Canvas 下同样有效。所以拖选本身是**可用的**，缺的只是把选区接进剪贴板。
//
// copy-on-select 用防抖：拖动过程中选区每次变化都会触发 onSelectionChange，
// 逐次写剪贴板会（a）狂调系统 API，（b）把中间态覆盖进剪贴板。抖完再写。

const COPY_SELECT_DEBOUNCE_MS = 60
const copyTimers = new Map()   // key -> timer

// copySelection 把当前选区写进系统剪贴板，返回是否成功。
//
// 刻意**不往终端流里写任何反馈**：终端表面是命令流，插一行「已复制」会
//   (a) 污染回滚内容（那行文字不是命令输出，却混在里面）；
//   (b) 更糟 —— paintSystem 会先写 \r\n 把光标推到新行，而本地行编辑的
//       t.line / t.startCol 并不知道光标被挪走了。用户打了一半命令时去复制，
//       接着输入字符落在新行、退格却按旧列号定位，表现为擦错位置、字符乱跳。
// 复制是高频动作，静默完成最稳妥；真要提示应走 UI 层（toast），不碰终端。
async function copySelection(key) {
  const t = getTerm(key)
  if (!t) return false
  const sel = t.term.getSelection ? t.term.getSelection() : ''
  if (!sel) return false
  return writeClipboard(sel)
}

function scheduleCopyOnSelect(key) {
  const prev = copyTimers.get(key)
  if (prev) clearTimeout(prev)
  const timer = setTimeout(() => {
    copyTimers.delete(key)
    void copySelection(key)
  }, COPY_SELECT_DEBOUNCE_MS)
  copyTimers.set(key, timer)
}

function cancelCopyOnSelect(key) {
  const prev = copyTimers.get(key)
  if (prev) {
    clearTimeout(prev)
    copyTimers.delete(key)
  }
}

// openFiles 打开文件管理弹窗，定位到当前终端表面所在的远端目录。
//
// 传的是「这块表面」而不是「当前主机」：一台主机可以有多条任务，
// 各自停在不同目录（每任务一条独立 shell），用主机作键会打开到别人的目录。
function openFiles() {
  if (!store.currentHostId) {
    push({ kind: 'error', content: '请先选择一台主机。' })
    return
  }
  openFileManager(store.currentHostId, store.currentSessionId)
}

function onWinKeydown(e) {
  // 新建任务弹窗在顶层时这里让路，避免一次 Esc 同时关掉两层。
  if (showNewModal.value) return
  // 文件管理弹窗最外层优先：它盖住整个界面，Esc 应该先关它。
  if (e.key === 'Escape' && store.fileManager.open) {
    closeFileManager()
    return
  }
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
  // 倒计时计时器必须显式停：它挂在组件外的 setInterval 上，
  // 不停就是卸载后每秒还在改一个已销毁组件的 ref。
  if (tickTimer) {
    clearInterval(tickTimer)
    tickTimer = null
  }
  window.removeEventListener('keydown', onWinKeydown)
  // 销毁本地实例（DOM 已经没了），但**不关远端终端**：卸载往往只是换视图，
  // 用户希望切回来时 shell 还在原目录。真正的清理走「关闭终端」与切换任务；
  // 应用退出时后端 shutdown 会关掉所有终端。实例登记表归 termSession，这里
  // 遍历当前存活的表面键逐个销毁（disposeSurface 顺带清接线标记）。
  for (const key of [...openedKeys.value]) disposeSurface(key)
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
        <!-- 文件管理：默认打开当前终端所在的远端目录（目录由那条 shell
             通过 OSC 7 上报，见 store 的 term:cwd 处理）。 -->
        <button
          class="sm new-session-btn"
          :disabled="!store.currentHostId"
          title="浏览当前终端所在目录，支持上传/下载/删除"
          @click="openFiles"
        >文件</button>
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
        ｜ 在这里可直接敲 <b>Linux 命令</b>执行（以你的身份直接运行、不经审批；仅 <span class="mono">rm -rf /</span> 这类不可逆操作被硬拒绝），
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
      <div v-for="it in timeline" :key="it.id" class="entry" :class="'entry-' + it.kind">
        <!-- 工具调用组：连续的「调用 / 结果」折成一行 `N 次工具调用`，点开看详情 -->
        <div v-if="it.kind === 'toolGroup'" class="tool-group">
          <div class="tool-group-head" role="button" tabindex="0" @click="toggleGroup(it.id)">
            <span class="tg-arrow">{{ openGroups[it.id] ? '▾' : '▸' }}</span>
            <span class="tg-label">{{ it.calls.length }} 次工具调用</span>
          </div>
          <div v-show="openGroups[it.id]" class="tool-group-body">
            <div v-for="c in it.calls" :key="c.key" class="tool-call">
              <div v-if="c.tool" class="tool">
                <div class="tool-head">
                  <span class="tool-name mono">{{ c.tool.tool.name }}</span>
                  <span class="badge" :class="decisionClass(c.tool.tool.decision)">
                    {{ decisionLabel(c.tool.tool.decision) }}
                  </span>
                  <span class="badge neutral">{{ statusLabel(c.tool.tool.status) }}</span>
                  <span class="muted" v-if="c.tool.tool.hostName">@{{ c.tool.tool.hostName }}</span>
                  <span class="muted right" v-if="c.tool.tool.durationMs">{{ c.tool.tool.durationMs }}ms</span>
                </div>
                <pre v-if="c.tool.tool.command" class="tool-cmd">{{ c.tool.tool.command }}</pre>
                <div v-if="c.tool.tool.reason" class="tool-reason">{{ c.tool.tool.reason }}</div>
              </div>
              <div v-if="c.result" class="result">
                <div class="result-head">
                  <span class="muted">退出码 {{ c.result.exitCode }}</span>
                  <span v-if="c.result.redacted" class="badge warn">已脱敏 {{ c.result.redacted }} 处</span>
                  <span class="muted tiny">原始输出 · 发给 LLM 前已脱敏并标记为不可信</span>
                </div>
                <pre>{{ c.result.content }}</pre>
              </div>
            </div>
          </div>
        </div>

        <!-- 屏幕快照：模型读的那份净化文本，人看同一份，审计才对得上 -->
        <div v-else-if="it.kind === 'snapshot'" class="result">
          <div class="result-head">
            <span v-if="it.final" class="badge warn">终端结束画面 · 进模型上下文</span>
            <span v-else class="badge warn">终端屏幕快照 · 进模型上下文</span>
            <span v-if="it.redacted" class="badge warn">已脱敏 {{ it.redacted }} 处</span>
            <span class="muted tiny">{{ it.final ? '全屏 TUI 退出/终端结束时定格的最后一屏（净化文本）' : '常驻终端静止后的可见屏幕（净化文本）' }}</span>
          </div>
          <pre>{{ it.text }}</pre>
          <div v-if="it.injection && it.injection.length" class="tool-reason">
            检测到疑似提示注入：{{ it.injection.join('、') }}（内容已标记为不可信）
          </div>
        </div>

        <div v-else-if="it.kind === 'user'" class="msg user">
          <div class="msg-role msg-logo" title="Ai-Shell">Ai-Shell &gt;</div>
          <div class="msg-body">{{ it.content }}</div>
        </div>

        <div v-else-if="it.kind === 'assistant'" class="msg assistant">
          <div class="msg-role">
            <!-- 与终端表面同一套语法：`厂商 >` 表示「这是模型在回答」 -->
            <span
              v-if="archiveBadge(it)"
              class="model-badge"
              :style="{ color: archiveBadge(it).color }"
              :title="archiveBadge(it).name"
            >{{ archiveBadge(it).mark }}</span>
            <span v-else class="model-mark-fallback">LLM</span>
            <span class="model-chevron">&gt;</span>
          </div>
          <div class="msg-body">
            <div v-if="archiveBadge(it) && archiveBadge(it).name" class="msg-model" title="这条回答实际使用的模型">
              {{ archiveBadge(it).name }}<span v-if="it.model && archiveBadge(it).name !== it.model" class="msg-model-id"> · {{ it.model }}</span>
            </div>
            <div v-else-if="it.model" class="msg-model" title="这条回答实际使用的模型">{{ it.model }}</div>
            <div v-if="it.reasoning" class="reasoning">
              <div class="reasoning-head" @click="it.reasoningOpen = !it.reasoningOpen">
                {{ it.streaming && !it.content ? '思考中…' : '思考过程' }}
                <span class="reasoning-arrow">{{ it.reasoningOpen ? '▾' : '▸' }}</span>
              </div>
              <div v-show="it.reasoningOpen" class="reasoning-body">{{ it.reasoning }}</div>
            </div>
            {{ it.content }}<span v-if="it.streaming" class="caret"></span><span v-if="!it.content && !it.reasoning && !it.streaming" class="muted tiny">（模型返回了空回复）</span>
          </div>
        </div>

        <div v-else-if="it.kind === 'injection'" class="injection">
          <div class="injection-title">远端输出中检测到疑似提示注入</div>
          <div class="injection-body">命中话术：{{ it.findings.join('、') }}</div>
          <pre v-if="it.command" class="injection-cmd">{{ it.command }}</pre>
          <div class="injection-hint">
            该内容来自 <span class="mono">{{ it.hostName }}</span> 的执行结果，已被标记为不可信数据，
            不会作为指令执行。建议排查该主机上这段文本的来源。
          </div>
        </div>

        <div v-else-if="it.kind === 'error'" class="msg error">
          <div class="msg-role">错误</div>
          <div class="msg-body">{{ it.content }}</div>
        </div>

        <div v-else class="msg system">
          <div class="msg-body">{{ it.content }}</div>
        </div>
      </div>

      <div v-if="!store.entries.length" class="archive-empty">
        这条会话还没有归档记录。切到「终端」直接输入提问 / 敲命令。
      </div>
    </div>

    <!-- 底部 composer：审批浮层 + 工具条 -->
    <div class="composer-bar">
      <!-- 审批条：必须是**浮层**（绝对定位、不占布局）。
           一旦它占布局，出现/消失就会改变 .surface 的高度 → 触发终端 resize →
           远端 shell 收到 SIGWINCH 重绘提示符 → 看起来像「agent 干活干到一半
           突然回到了命令行」。实测：审批出现与消失各触发一次重绘。 -->
      <div v-if="store.pending" class="approval">
        <div class="approval-head">
          <span class="badge warn">需要你的批准</span>
          <span class="mono">{{ store.pending.name }}</span>
          <span class="muted" v-if="store.pending.hostName">@{{ store.pending.hostName }}</span>
          <!-- 倒计时：告诉用户「不回答的代价」是什么。没有它，用户以为可以
               无限期挂起，实际上后端 5 分钟后就放弃了。 -->
          <span
            v-if="approvalCountdown"
            class="muted approval-countdown"
            :class="{ urgent: approvalRemainSec !== null && approvalRemainSec <= 30 }"
          >{{ approvalCountdown }} 后失效</span>
          <button class="sm right" @click="showCmdDetail = true">详细</button>
        </div>
        <pre class="approval-cmd">{{ store.pending.command || store.pending.args }}</pre>
        <div class="approval-reason">{{ store.pending.reason }}</div>
        <div class="row" style="margin-top: 10px">
          <button class="ok" @click="approve(true)">批准执行</button>
          <!-- 拒绝与跳过在后端是同一个动作（不执行），差别只在**语义**：
               「拒绝」读起来像在否决 Agent 的判断，用户不确定拒绝了要不要
               给个理由；「跳过」是中性词，不想回答这条就直接跳过。
               两个按钮并存是为了让「不想回答」有一个心理成本最低的出口。 -->
          <button class="danger" @click="approve(false)">拒绝</button>
          <button class="sm" @click="approve(false)">跳过</button>
        </div>
      </div>
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
          Working<span class="thinking-dots" aria-hidden="true"><i>.</i><i>.</i><i>.</i><i>.</i></span>
        </span>
        <!-- 上下文用量：本轮请求约多少 token、占模型窗口几成。
             没有它，用户只能等服务端返回 400 才知道自己撑爆了窗口。 -->
        <span
          v-if="ctxUsage.text"
          class="ctx-usage"
          :class="ctxUsage.level"
          :title="ctxUsage.title"
        >{{ ctxUsage.text }}</span>
        <UiSelect
          class="model-select"
          :model-value="activeProfileId"
          :options="modelOptions"
          :disabled="store.running || !store.llmProfiles.length"
          placeholder="（暂无服务商，请到「设置」添加）"
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

    <!-- 文件管理弹窗。状态在 store 里（见 fileManager），组件只负责渲染 ——
         term:cwd 事件到达时 store 要能直接更新它，状态在组件内部就够不着。 -->
    <FileManagerModal />
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
/* 模型选择器：宽度随内容自适应（不再固定 300px，否则名字短时右侧会空一大截）。
   class="model-select" 会透传到 UiSelect 的根元素（就是 .uis），所以这里直接生效：
   width:max-content 让按钮"文字多宽、按钮多宽"；max-width 兜住超长名字。 */
.model-select { flex: 0 1 auto; width: max-content; max-width: 340px; }

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

/* 用户行左侧：Ai-Shell 软件 wordmark（与终端表面同款文字语法 `Ai-Shell >`） */
.msg-logo {
  display: inline-flex;
  align-items: center;
  justify-content: flex-end;
  padding-top: 3px;
  font-family: var(--mono, ui-monospace, monospace);
  font-size: 11px;
  font-weight: 700;
  line-height: 1;
}
/* `Ai-Shell` 青 + `>` 紫 —— 与 termPaint.userPromptMark 同色 */
.msg-logo { color: #22d3ee; }
.msg-logo::after { content: ' >'; color: #8b5cf6; }

/* assistant 行左侧：模型图标 + 紫色 `>`（与终端表面 paintAssistantHeader 同语法） */
.model-badge {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  min-width: 20px;
  height: 18px;
  padding: 0 4px;
  border-radius: 5px;
  border: 1px solid currentColor;
  font-family: var(--mono, ui-monospace, monospace);
  font-size: 10px;
  font-weight: 700;
  line-height: 1;
}
.model-mark-fallback { color: var(--text-3); font-size: 10px; font-weight: 700; }
.model-chevron { color: #8b5cf6; font-weight: 700; font-family: var(--mono, ui-monospace, monospace); }
.msg-model-id { color: var(--text-3); }
/* 这条回答实际使用的模型：让「选中的模型」和「回答的模型」对得上，不必靠模型自述 */
.msg-model {
  font-size: 11px;
  color: var(--text-3);
  font-family: var(--font-mono, ui-monospace, monospace);
  margin-bottom: 4px;
}

/* 工具调用组：折叠成一行 `N 次工具调用`，点开看每条命令 / 结果 */
.tool-group { margin: 6px 0; }
.tool-group-head {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: var(--text-3);
  cursor: pointer;
  user-select: none;
}
.tool-group-head:hover { color: var(--text-2); }
.tg-arrow { font-size: 10px; }
.tg-label { font-family: var(--font-mono, ui-monospace, monospace); }
.tool-group-body {
  margin: 6px 0 0 14px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.tool-call { display: flex; flex-direction: column; gap: 4px; }

/* 思考过程：弱化的折叠块，思考中自动展开，回答开始后收起 */
.reasoning { margin: 2px 0 6px; border-left: 2px solid var(--border); padding-left: 8px; }
.reasoning-head {
  font-size: 11px;
  color: var(--text-3);
  cursor: pointer;
  user-select: none;
  line-height: 1.8;
}
.reasoning-head:hover { color: var(--text-2); }
.reasoning-arrow { font-size: 10px; margin-left: 4px; }
.reasoning-body {
  font-size: 12px;
  color: var(--text-3);
  line-height: 1.65;
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 240px;
  overflow-y: auto;
}
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

/* 审批浮层：bottom 锚在 composer 上方，**不占布局**。
   占布局的代价见模板注释：出现/消失会 resize 终端 → SIGWINCH → 远端重绘提示符。 */
.approval {
  position: absolute;
  left: 18px;
  right: 18px;
  bottom: calc(100% + 10px);
  z-index: 50; /* 低于 modal 的 200，高于表面内容 */
  border: 1px solid var(--warn);
  background: var(--warn-bg);
  border-radius: var(--radius);
  padding: 12px 14px;
  max-height: 60vh;
  box-shadow: 0 -8px 28px rgba(0, 0, 0, 0.14);
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
/* 倒计时靠右推到头（右邻的「详细」按钮自带 right 推开，这里只需跟它并行不挤压）。
   紧迫态（≤30 秒）用警示色加粗：时间不多了这件事必须跳出来。 */
.approval-countdown { font-size: 12px; white-space: nowrap; }
.approval-countdown.urgent { color: var(--warn); font-weight: 600; }

.composer-bar {
  flex-shrink: 0;
  position: relative; /* 审批浮层的锚：bottom:100% 即浮在工具条正上方 */
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
.thinking-dots { display: inline-block; margin-left: 1px; }
.thinking-dots i {
  font-style: normal;
  animation: thinking-dot 1.4s ease-in-out infinite;
  opacity: 0.25;
}
.thinking-dots i:nth-child(1) { animation-delay: 0s; }
.thinking-dots i:nth-child(2) { animation-delay: 0.15s; }
.thinking-dots i:nth-child(3) { animation-delay: 0.3s; }
.thinking-dots i:nth-child(4) { animation-delay: 0.45s; }
@keyframes thinking-dot { 0%, 100% { opacity: 0.25; } 50% { opacity: 1; } }
@keyframes thinking-breathe { 0%, 100% { opacity: .55; } 50% { opacity: 1; } }
@keyframes thinking-spark { 0%, 100% { opacity: .35; } 50% { opacity: 1; } }
@media (prefers-reduced-motion: reduce) {
  .thinking-inline, .thinking-spark, .thinking-dots i { animation: none; opacity: .8; }
  .thinking-dots i { opacity: 1; }
}

.composer-tools { display: flex; align-items: center; gap: 8px; }
/* 上下文用量指示：常驻工具条，颜色随占用率升级（ok → mid → warn → danger）。
   认不出模型窗口时用中性色（unknown），不吓人也不装作知道。 */
.ctx-usage {
  flex-shrink: 0;
  font-size: 11px;
  font-family: var(--mono, ui-monospace, monospace);
  color: var(--text-3);
  padding: 2px 7px;
  border-radius: 999px;
  background: var(--surface-2);
  cursor: help;
  white-space: nowrap;
}
.ctx-usage.ok { color: var(--text-2); }
.ctx-usage.mid { color: var(--warn); background: var(--warn-bg); }
.ctx-usage.warn { color: var(--warn); background: var(--warn-bg); font-weight: 600; }
.ctx-usage.danger { color: var(--danger); background: var(--danger-bg); font-weight: 600; }
.ctx-usage.unknown { color: var(--text-3); }
.composer-sep { width: 1px; height: 18px; background: var(--border); margin: 0 4px; flex-shrink: 0; }
.grow-hint { margin-left: auto; }
</style>
