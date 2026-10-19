<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import {
  store, approve, stop, push,
  refreshSessions, switchProfile, syncWindowTitle,
  closeTerminal, reportActiveSession, loadSessionTimeline,
  openFileManager, closeFileManager,
  bucketKey, noteConversationActivity, onFocusSessionChanged, shouldShowSessionChrome,
  resetSessionChromeSeen, beginFreshTask, ensureFreshTaskSession
} from '../store'
import { formatHostLabel, maskHostAddr } from '../hostDisplay'
import { providerBadge } from '../providerLogos'

// 未选主机的欢迎页按钮会向上切页签（App.vue 接住后改 tab），script 里也要
// 主动调（新建任务引导去主机管理），所以提成常量而不是模板里用 $emit。
const emit = defineEmits(['page'])
import {
  openedKeys, ensureTerm, disposeTerm, doFit, focusTerm, setTheme, setTermFont,
  setSurfaceEl, getTerm, paintWelcomeHintIfNeeded
} from '../terminal/termSession'
import { onTermData, paintInputPromptIfNeeded } from '../terminal/termInput'
import { t } from '../i18n'
import UiSelect from './UiSelect.vue'
import FileManagerModal from './FileManagerModal.vue'
import ChatPanel from './ChatPanel.vue'

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

// 顶栏视图：false = 终端表面（仍支持 Agent+Shell 分流），true = 对话页（对话框首页）。
// 默认进对话页：打开软件即见对话框；需要敲命令时切回「终端」。
const showArchive = ref(true)

// 顶栏：
// - 终端视图始终显示（要切回对话 / 关终端）
// - 已选主机时对话页也显示：切「对话」= 进当前任务对话框，不是新建；
//   顶栏保留主机与「对话/终端」切换，任务名用「未命名任务」兜底。
// - 未选主机的空欢迎首页仍藏顶栏。
const showTopBar = computed(() =>
  !showArchive.value || shouldShowSessionChrome() || !!store.currentHostId
)
watch(() => store.entries.length, noteConversationActivity, { immediate: true })
watch(() => [store.currentHostId, store.currentSessionId], onFocusSessionChanged)

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
  if (s.status === 'opening') return t('console.status.opening')
  if (s.status === 'closed') return s.exitReason || t('console.termExited')
  if (s.status === 'error') return t('console.status.openFail', { err: s.error })
  return ''
})
const canRetry = computed(() => !!st.value && (st.value.status === 'closed' || st.value.status === 'error'))
const canClose = computed(() => !!st.value && (st.value.status === 'open' || st.value.status === 'opening'))

// 终端配色跟随亮/暗主题：切换时转交 termSession.setTheme（它同步所有已打开实例）。
// 注意参数名用 v：导入的 t() 是翻译函数，不能被遮蔽。
watch(() => store.theme, v => setTheme(v))
// 终端字体同理：字号/字体族变化时热更新所有已开实例并重测行列（见 setTermFont）。
watch(() => store.termFont, f => setTermFont(f))

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
  // 局部变量不能叫 t —— 它会遮蔽 i18n 的翻译函数 t()（同款问题在 termSubmit.js 炸过）。
  const state = getTerm(key)
  if (!state || state.wired) return
  state.wired = true
  const term = state.term

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
  // 关终端 = 断 SSH + 回对话；再点「终端」由 goTermView 重建连接。
  showArchive.value = true
}

function goTermView() {
  showArchive.value = false
}

function onRetry() {
  ensureSurface(store.currentHostId, store.currentSessionId)
}

// 切回终端：对话页 v-show 隐藏时容器是 0x0，ensureTerm 会推迟 open；
// 这里等布局后再附着。若这一刻仍是 0x0，ResizeObserver 会在出尺寸时补建。
watch(showArchive, async on => {
  if (on) return
  await nextTick()
  if (store.currentHostId) {
    // 仍开着 → ensureTerm 只 fit；关过 / 曾推迟 → 此时再 OpenTerminal。
    await attachSurface(store.currentHostId, store.currentSessionId)
  }
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

const streamingNow = computed(() => store.entries.some(e => e.streaming))

// 上下文用量：后端每步请求前推一次 agent:context（见 agent.EvContext），
// 由 store 收进 store.contextUsage —— 事件只有一个消费者入口（bindEvents）。
//
// 为什么要显示：上下文保护靠的是三个字节上限「盲估」，真爆窗口时是服务端报错、
// 界面只能显示一条看不懂的 400。有了它，用户能提前看到「已经 87% 了」。
const ctxUsage = computed(() => {
  const { tokens, window: win, messages } = store.contextUsage
  // 与对话页一致：无用量时也占位常显，有上一轮数据则保持到下一轮覆盖。
  if (!tokens) {
    return { text: t('console.ctx.idle'), level: 'idle', title: t('console.ctx.idleTitle') }
  }
  const text = t('console.ctx.about', { n: fmtTokens(tokens) })
  if (!win) {
    return { text, level: 'unknown', title: t('console.ctx.unknown') }
  }
  const pct = Math.round(tokens * 100 / win)
  const level = pct >= 95 ? 'danger' : pct >= 80 ? 'warn' : pct >= 60 ? 'mid' : 'ok'
  return {
    text: t('console.ctx.usage', { text, pct }),
    level,
    title: t('console.ctx.title', {
      tokens: tokens.toLocaleString(),
      win: win.toLocaleString(),
      pct,
      messages
    })
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
  return t('console.ctx.seconds', { s })
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

// ---- 主机 / 任务切换：只留当前任务的 shell（切走关闭、回来重开）----

let hostSwitching = false

// detachSurface 关闭某块表面：先关远端 PTY（closeTerminal）再销毁本地 xterm。
// 落实「只留当前任务」：切换任务时上一任务的现场就此丢弃（回来是全新 shell）。
async function detachSurface(key) {
  if (!key) return
  const state = getTerm(key)
  if (!state) return
  await closeTerminal(state.hostId, state.sessionId)
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
    // 清主机：卸掉旧 PTY，回到新建首页（侧栏分组视图）。
    if (!id) {
      if (attachedKey) {
        await detachSurface(attachedKey)
        attachedKey = ''
      }
      store.currentSessionId = ''
      return
    }
    // 侧栏分组视图「点其它主机的某条任务直达」：意图记在 pendingSessionId，
    // 列表里确实存在才落选中（用完即清；普通切主机时它是空串，走默认兜底）。
    // 不再先把 currentSessionId 置空：refreshSessions 会把无效 id 兜到默认会话，
    // 避免与 commitDraftHost / ask 并发时把刚选好的会话冲掉。
    const want = store.pendingSessionId
    store.pendingSessionId = ''
    await refreshSessions(id)
    if (want && (store.sessions[id] || []).some(s => s.id === want)) {
      store.currentSessionId = want
    }
    // 侧栏直达任务：立刻还原该会话历史（勿等 ChatPanel watch）。
    if (!store.freshTaskPending) {
      await loadSessionTimeline(id, store.currentSessionId)
    }
    // 新建态刚 commit 主机：等 ensureFreshTaskSession 建完会话再附着，
    // 避免先对默认会话空开一趟 PTY。
    if (store.freshTaskPending) return
    await attachSurface(id, store.currentSessionId)
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
})

// ---- 顶栏：任务名 / 窗口标题 / 模型 ----

const currentSession = computed(
  () => store.currentSessions.find(s => s.id === store.currentSessionId) || null
)
const currentSessionName = computed(() => {
  const s = currentSession.value
  if (!s) return ''
  // 空名 / 默认会话：统一「未命名任务」。
  const name = String(s.name || '').trim()
  if (name) return name
  return t('ss.unnamedTask')
})
// 窗口标题 / 顶栏：主机名 + 脱敏 IP；中间两段用 ***（见 maskHostAddr）。
const hostTitle = computed(() => formatHostLabel(currentHost.value))
const windowTitleLabel = computed(() => {
  const host = hostTitle.value
  const task = currentSessionName.value
  if (host && task) return `${host} · ${task}`
  return host || task
})
const hostDetailTitle = computed(() => {
  const h = currentHost.value
  if (!h) return ''
  const ip = maskHostAddr(h.addr)
  const name = String(h.name || '').trim()
  const who = h.user ? `${h.user}@${ip}` : ip
  return name ? `${name} · ${who}` : who
})
watch(windowTitleLabel, name => syncWindowTitle(name), { immediate: true })

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
  // 任务中途换模型：先中断本轮，避免 lane 占着导致下一轮发不出去。
  if (store.running || store.busy) {
    await stop({ silent: true })
  }
  const prev = store.selectedProfileId
  store.selectedProfileId = id
  try {
    await switchProfile(id)
  } catch {
    store.selectedProfileId = prev
  }
}

async function pickTermHost(id) {
  if (!id) return
  store.currentHostId = id
  // 从新建首页进终端空态选主机 = 真正开始任务。
  await ensureFreshTaskSession()
}

// ---- 命令详细弹窗 ----

function goChatView() {
  // 纯视图切换：已有主机则停在当前会话对话框，绝不顺手新建任务。
  showArchive.value = true
}

function startNewSession() {
  showArchive.value = true
  // 新建任务 = 回欢迎首页：不立刻建会话、不占某一台主机。
  // 侧栏保持「按主机分组」；底栏预选主机用 draftHostId，发对话/开终端才真正开始。
  store.draftHostId = ''
  beginFreshTask()
  if (store.currentHostId) {
    store.currentHostId = ''
  }
  store.currentSessionId = ''
  resetSessionChromeSeen()
}

// 主导航「新建任务」：回对话欢迎首页，标记待开新会话。
watch(() => store.newTaskIntent, () => {
  if (!store.newTaskIntent) return
  startNewSession()
})

// 删光侧栏任务等：回到对话欢迎首页（新建任务态）。
watch(() => store.chatHomeIntent, () => {
  if (!store.chatHomeIntent) return
  showArchive.value = true
})

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
  // 局部变量不能叫 t —— 遮蔽翻译函数（见 attachHandlers 内注释）。
  const state = getTerm(key)
  if (!state) return false
  const sel = state.term.getSelection ? state.term.getSelection() : ''
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
    push({ kind: 'error', content: t('msg.pickHost') })
    return
  }
  openFileManager(store.currentHostId, store.currentSessionId)
}

function onWinKeydown(e) {
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
    ro = new ResizeObserver(() => {
      const key = currentKey.value
      // 对话页曾推迟 open：容器从 0x0 变为有尺寸时补建实例（只 fit 建不起来）。
      if (
        !showArchive.value &&
        store.currentHostId &&
        key &&
        !getTerm(key) &&
        openedKeys.value.includes(key)
      ) {
        void ensureSurface(store.currentHostId, store.currentSessionId)
        return
      }
      doFit(key)
    })
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
    <!-- 顶栏：文件 + 当前主机（只读）+ 对话/终端。主机不可在此切换。 -->
    <header v-if="showTopBar" class="bar">
      <div class="row grow">
        <!-- 文件管理：默认打开当前终端所在的远端目录（目录由那条 shell
             通过 OSC 7 上报，见 store 的 term:cwd 处理）。 -->
        <button
          class="sm new-session-btn"
          :disabled="!store.currentHostId"
          :title="t('console.files.title')"
          @click="openFiles"
        >{{ t('console.files.label') }}</button>
        <span
          v-if="hostTitle"
          class="bar-host"
          :title="hostDetailTitle"
        >{{ hostTitle }}</span>
      </div>

      <div class="seg">
        <!-- 切对话：只切视图，不建任务；落到当前会话（空名即「未命名任务」）。 -->
        <button :class="{ on: showArchive }" @click="goChatView">{{ t('console.view.chat') }}</button>
        <button :class="{ on: !showArchive }" @click="goTermView">{{ t('console.view.term') }}</button>
      </div>
    </header>

    <!-- 关闭终端入口：只在终端视图占布局。对话页选主机也会后台开 PTY，
         若这里仍插入横幅，居中首页会上下抖一下。 -->
    <div v-if="canClose && !showArchive" class="sess-banner">
      <button class="sm" @click="onClose">{{ t('console.term.close') }}</button>
    </div>

    <!-- 活表面：整幅 xterm，附着当前主机的常驻 shell PTY（终端内仍可 Agent+Shell 分流） -->
    <div class="surface" ref="rootEl" v-show="!showArchive">
      <div v-if="!store.currentHostId" class="surface-empty">
        <p class="term-empty-title">{{ t('console.term.pickHost') }}</p>
        <p class="term-empty-hint">{{ t('console.term.pickHostHint') }}</p>
        <div v-if="store.hosts.length" class="term-host-list" role="listbox">
          <button
            v-for="h in store.hosts"
            :key="h.id"
            type="button"
            class="term-host-item"
            role="option"
            @click="pickTermHost(h.id)"
          >
            <span class="term-host-name">{{ h.name }}</span>
            <span class="term-host-addr">{{ h.user }}@{{ maskHostAddr(h.addr) }}</span>
          </button>
        </div>
        <button type="button" class="sm term-host-add" @click="emit('page', 'hosts')">
          {{ t('console.term.addHost') }}
        </button>
      </div>
      <template v-else>
        <div v-if="statusLine" class="surface-status" :class="{ bad: st && st.status === 'error' }">
          <span>{{ statusLine }}</span>
          <button v-if="canRetry" class="sm" @click="onRetry">{{ t('console.term.reopen') }}</button>
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

    <!-- 对话页：对话框首页 + 气泡时间线（思考 / 工具调用）；与终端共用 store.entries -->
    <ChatPanel
      v-show="showArchive"
      @page="emit('page', $event)"
      @term="goTermView"
      @cmd-detail="showCmdDetail = true"
    />

    <!-- 底部 composer：仅终端视图。对话页的审批卡在 ChatPanel 消息区。
         终端视图审批仍必须是浮层（绝对定位、不占布局），否则出现/消失会
         resize 终端 → SIGWINCH → 远端重绘提示符。 -->
    <div v-if="store.currentHostId && !showArchive" class="composer-bar">
      <div v-if="store.pending" class="approval">
        <div class="approval-head">
          <span class="badge warn">{{ t('console.approval.title') }}</span>
          <span class="mono">{{ store.pending.name }}</span>
          <span class="muted" v-if="store.pending.hostName">@{{ store.pending.hostName }}</span>
          <!-- 倒计时：告诉用户「不回答的代价」是什么。没有它，用户以为可以
               无限期挂起，实际上后端 5 分钟后就放弃了。 -->
          <span
            v-if="approvalCountdown"
            class="muted approval-countdown"
            :class="{ urgent: approvalRemainSec !== null && approvalRemainSec <= 30 }"
          >{{ t('console.approval.expiry', { time: approvalCountdown }) }}</span>
          <button class="sm right" @click="showCmdDetail = true">{{ t('console.approval.detail') }}</button>
        </div>
        <pre class="approval-cmd">{{ store.pending.command || store.pending.args }}</pre>
        <div class="approval-reason">{{ store.pending.reason }}</div>
        <div class="row" style="margin-top: 10px">
          <button class="ok" @click="approve(true)">{{ t('console.approval.approve') }}</button>
          <!-- 拒绝与跳过在后端是同一个动作（不执行），差别只在**语义**：
               「拒绝」读起来像在否决 Agent 的判断，用户不确定拒绝了要不要
               给个理由；「跳过」是中性词，不想回答这条就直接跳过。
               两个按钮并存是为了让「不想回答」有一个心理成本最低的出口。 -->
          <button class="danger" @click="approve(false)">{{ t('console.approval.deny') }}</button>
          <button class="sm" @click="approve(false)">{{ t('console.approval.skip') }}</button>
        </div>
      </div>
      <div v-if="!showArchive" class="composer-tools">
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
          :disabled="!store.llmProfiles.length"
          :placeholder="t('console.noProviders')"
          @change="onSwitchModel"
        />
        <button v-if="store.running || store.busy" class="sm danger" @click="stop">{{ t('console.interrupt') }}</button>
      </div>
    </div>

    <!-- 命令详细弹窗：审批卡内只做预览，超长命令在这里看全文 -->
    <div v-if="showCmdDetail" class="overlay" @click.self="showCmdDetail = false">
      <div class="modal cmd-modal">
        <h3>{{ t('console.cmdDetail.title') }}</h3>
        <pre class="cmd-detail">{{ pendingCmd }}</pre>
        <div class="modal-actions">
          <button class="sm" @click="copyPendingCmd">{{ cmdCopied ? t('console.cmdDetail.copied') : t('console.cmdDetail.copy') }}</button>
          <button class="sm primary" @click="showCmdDetail = false">{{ t('console.cmdDetail.close') }}</button>
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
.new-session-btn { flex-shrink: 0; white-space: nowrap; font-size: 13px; }
.bar-host {
  min-width: 0;
  max-width: 280px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 13px;
  font-weight: 500;
  color: var(--text);
}
.bar-host::before {
  content: '·';
  margin-right: 8px;
  color: var(--text-3);
  font-weight: 400;
}

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
.seg button.on {
  background: var(--accent);
  color: #0a0c0a;
  font-weight: 600;
}
.seg button.on:hover:not(:disabled) {
  background: var(--accent);
  color: #0a0c0a;
  filter: brightness(1.06);
  border-color: transparent;
}
/* 模型选择器：宽度随内容自适应（不再固定 300px，否则名字短时右侧会空一大截）。
   class="model-select" 会透传到 UiSelect 的根元素（就是 .uis），所以这里直接生效：
   width:max-content 让按钮"文字多宽、按钮多宽"；max-width 兜住超长名字。 */
.model-select { flex: 0 1 auto; width: max-content; max-width: 340px; }

.sess-banner {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 12px;
  padding: 8px 16px;
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
/* 欢迎页：未选主机时的默认界面。居中卡片，介绍三条输入路径并引导选主机。 */
.welcome {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  height: 100%;
  text-align: center;
  padding: 32px 24px;
}
.welcome-title {
  margin: 0 0 6px;
  font-size: 17px;
  color: var(--text-1);
}
.welcome-sub {
  margin: 0 0 18px;
  color: var(--text-3);
  font-size: 12.5px;
}
.welcome-feats {
  display: flex;
  flex-direction: column;
  gap: 10px;
  max-width: 460px;
  text-align: left;
}
.welcome-feat {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 10px 14px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--surface-2);
}
.welcome-feat b {
  font-size: 13px;
  color: var(--text-1);
}
.welcome-feat span {
  font-size: 12px;
  color: var(--text-3);
  line-height: 1.55;
}
/* 快捷入口：三个竖排「图标 + 文字」按钮横排成一行，按配置顺序排列。 */
.welcome-actions {
  margin-top: 20px;
  display: flex;
  gap: 12px;
}
.welcome-btn {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 7px;
  min-width: 118px;
  padding: 13px 18px 11px;
  border: 1px solid var(--border);
  border-radius: 10px;
  background: var(--surface-2);
  color: var(--text-1);
  font-size: 12.5px;
  cursor: pointer;
  transition: border-color 0.12s, background 0.12s;
}
.welcome-btn svg { opacity: 0.8; }
.welcome-btn:hover:not(:disabled) { border-color: var(--accent); }
.welcome-btn.primary {
  background: var(--accent-bg);
  color: var(--accent);
  border-color: transparent;
  font-weight: 600;
}
.welcome-btn.primary:hover:not(:disabled) { border-color: var(--accent); }
.welcome-btn:disabled { opacity: 0.5; cursor: not-allowed; }
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

.surface-empty {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 10px;
  padding: 32px 20px;
}
.term-empty-title {
  margin: 0;
  font-size: 15px;
  font-weight: 600;
  color: var(--text);
}
.term-empty-hint {
  margin: 0 0 6px;
  font-size: 12.5px;
  color: var(--text-3);
}
.term-host-list {
  width: min(360px, 92%);
  max-height: min(280px, 50vh);
  overflow: auto;
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 6px;
  border: 1px solid var(--border);
  border-radius: 10px;
  background: var(--surface);
}
.term-host-item {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 2px;
  width: 100%;
  text-align: left;
  border: 0;
  background: transparent;
  color: var(--text);
  padding: 9px 11px;
  border-radius: 8px;
  cursor: pointer;
}
.term-host-item:hover { background: var(--surface-2); }
.term-host-name {
  font-size: 13px;
  font-weight: 500;
}
.term-host-addr {
  font-size: 11px;
  color: var(--text-3);
  font-family: var(--mono, ui-monospace, monospace);
}
.term-host-add { margin-top: 4px; }
.composer-bar.approval-only {
  border-top: 0;
  background: transparent;
  pointer-events: none;
}
.composer-bar.approval-only .approval {
  pointer-events: auto;
}
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
.ctx-usage.unknown,
.ctx-usage.idle { color: var(--text-3); }
.composer-sep { width: 1px; height: 18px; background: var(--border); margin: 0 4px; flex-shrink: 0; }
.grow-hint { margin-left: auto; }
</style>
