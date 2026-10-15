import { reactive } from 'vue'
import * as paint from './termPaint'
import { t } from './i18n'
import {
  bindTerminalEvents,
  registerTermSink, unregisterTermSink, registerLLMHint,
  surfaceWrite, fireLLMHint, afterTermQuiet, armCmdPending
} from './terminal/termEvents.js'

const api = () => window.go?.main?.App
const rt = () => window.runtime

// 后端绑定对象（Wails 注入 window.go.main.App）。
// 导出给组件用，是为了让「要调后端」的判断不绕过 store 直接摸全局 ——
// 一处退化（浏览器裸跑 dev server 时 window.go 为空）只在一个地方处理。
export const backendAPI = api

// ---- 会话时间线的桶键 ----
//
// 桶键 = 主机 ID + \x00 + 会话 ID。
//
// 为什么用 \x00 而不是 ':' 这类看着顺眼的分隔符：主机 ID 与会话 ID
// 都可能含分隔符本身，拼接就会**撞键** —— 主机 "a" + 会话 "b:c"
// 与主机 "a:b" + 会话 "c" 会拼出同一个字符串。撞键的表现是
// 「两台主机的对话混在一条时间线上」，而且只在特定的 ID 组合下出现，
// 极难复现。\x00 不可能出现在任何一个 ID 里（主机 ID 是 uuid，
// 会话 ID 由后端随机生成）。
const SEP = '\u0000'

// DEFAULT_SESSION 必须与后端的 agent.DefaultSessionID 一致。
//
// 这里刻意把空会话 ID 归一化成它，而不是让两种写法各占一个桶：
// 后端在 Ask / ClearSession / CompactSession 里都把空串当成默认会话，
// 若前端用空串建桶、用 'default' 读桶，那么「会话列表还没拉回来时
// 产生的那条提示」会落进一个谁也读不到的桶里 —— 用户看不到任何报错。
// 两边的值由 contract_test.go 钉住，不会悄悄漂移。
const DEFAULT_SESSION = 'default'

export function bucketKey(hostId, sessionId) {
  return `${hostId || ''}${SEP}${sessionId || DEFAULT_SESSION}`
}

// 终端字体默认值：与 xterm 实例创建时的历史默认完全一致 —— 升级后老用户
// 观感不变，也作为字体偏好坏数据时的回退。
export const DEFAULT_TERM_FONT = {
  family: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
  size: 12.5
}

export const store = reactive({
  ready: false,
  error: '',
  // auditError 是「审计日志写入失败」的持久告警，与 error 分开：
  // error 表示启动/调用失败，auditError 表示审计轨迹可能已不完整，
  // 后者不会因为一次成功调用而消失，必须用户显式确认。
  auditError: '',
  // 主题：'light' | 'dark'。持久化在 localStorage（纯前端偏好，不进加密库）。
  theme: 'light',
  // 终端字体偏好（family/fontSize），与主题同款 localStorage 持久化。
  termFont: { ...DEFAULT_TERM_FONT },
  posture: { keyProtection: '', configDir: '', hostCount: 0, degraded: false },
  hosts: [],
  llm: { baseUrl: '', model: '', hasApiKey: false },
  // llmProfiles 是全部 LLM 配置方案（每项含 active 标记，不含密钥）。
  // 当前使用哪一套以每项的 active 为准，不另存副本 —— 两份数据迟早不一致。
  llmProfiles: [],
  // selectedProfileId 是「下一轮对话要用的 LLM 方案」。
  //
  // 下拉框一改就**同步**写它，请求也直接带它（见 ask）—— 于是「界面上选中的模型」
  // 和「本轮请求实际带的模型」是同一个值。这样切换是「随请求下发」的即时生效，
  // 不再依赖「先改全局激活指针、下一轮才读它」那条间接路径：既不会在切换的
  // 后端回程还没跑完时把请求发到上一套方案上，也省掉了「切了却没换」的观感。
  selectedProfileId: '',
  // pendingSessionId 是「切主机后想直达的那条会话」：侧栏分组视图里点击
  // 其它主机的任务时先记下意图再切主机（currentHostId 一改 ConsolePanel 的
  // watch 会开始整套切换流程），流程里的 refreshSessions 完成后若这条会话
  // 确实存在就落到它 —— 用完即清，普通切主机时它始终是空串。
  pendingSessionId: '',

  // contextUsage 是最近一次请求的上下文用量（估算值，见 internal/agent/tokenbudget.go）。
  // window 为 0 表示后端认不出该模型的上下文窗口 —— 此时只显示 token 数，不显示占比。
  contextUsage: { tokens: 0, window: 0, messages: 0, model: '' },

  // agentTransfer 是**模型发起**的文件传输进度（download_remote_file 工具）。
  //
  // 与 fileManager.progress 分开：后者是人在弹窗里点下载，前者是模型在拉文件，
  // 两者可能同时进行。共用一个字段会让进度条张冠李戴 —— 显示 A 的速度，
  // 实际在传 B。null 表示当前没有进行中的传输。
  agentTransfer: null,

  // 会话上下文的三项上限与后端默认值保持一致（8 / 8KiB / 256KiB）。
  // 这里只是 Bootstrap 回来之前的占位值，真实值以 Bootstrap 为准。
  policy: {
    mode: 'manual',
    whitelist: [],
    redactOutput: true,
    maxOutput: 32768,
    maxSteps: 12,
    maxSessionTurns: 8,
    maxStoredToolBytes: 8192,
    maxSessionBytes: 262144,
    // 跨主机执行默认禁止：一条会话的工具只能打它所属的主机。
    allowCrossHost: false
  },
  currentHostId: '',

  // draftHostId：新建任务首页底栏「预选」的主机。
  // 还没发对话 / 开终端前不写入 currentHostId，避免侧栏切到该主机、
  // 首页被会话列表拖走。真正开始任务时再 commitDraftHost()。
  draftHostId: '',

  // freshTaskPending：处于「新建任务」欢迎态。发对话 / 开终端时要先
  // createSession，避免落到该主机已有上下文的默认会话上串线。
  // 点侧栏已有任务进入时清掉。
  freshTaskPending: false,

  // sessionBound：任务已真正开始（新建会话成功 / 侧栏进入已有任务）。
  // 与 hasMessages 一起锁定底栏主机：CreateSession 后、首条气泡前也不能换机。
  sessionBound: false,

  // currentSessionId 是当前选中的会话，空串表示默认会话。
  //
  // 敢用「空串表示默认」，是因为后端守住了一条不变式：**每台主机
  // 永远至少有一条默认会话**（见 internal/agent 的 DefaultSessionID）。
  // 于是这里永远不必处理「这台机器一条会话都没有」这种状态 ——
  // 那种状态一旦出现，每个跟会话有关的地方都要多一个分支，
  // 漏掉任何一处都会表现成「点了没反应」。
  currentSessionId: '',

  // sessions 是每台主机的会话列表（只含元信息，不含历史正文）。
  sessions: {},

  // terms 是每台主机交互终端的连接状态：hostId → { status, error, exitReason }。
  //
  // 只存**状态**，不存内容：终端的画面归 xterm 实例自己管（那是它的本职），
  // 这里再抄一份只会多出一份迟早会不一致的副本。
  // status 取值：idle（没开）/ opening（正在开）/ open / closed（远端退出）/ error。
  terms: {},

  // termCwd 记录每块终端表面（bucketKey）的远端当前目录，来自后端的
  // term:cwd 事件（OSC 7 上报）。文件管理弹窗默认定位到它。
  // 拿不到时的兜底由组件负责（要家目录）—— 后端 ListRemoteDir 收空路径
  // 会自动回落到家目录，所以这里存空串是安全的。
  termCwd: {},

  // fileManager 是文件管理弹窗的状态。
  // 刻意放在 store 而不是组件内部 ref：终端在用户 cd 之后会推 term:cwd，
  // store 的事件处理需要知道「弹窗是否开着、看着哪块表面」才能决定要不要
  // 跟着更新目录 —— 状态在组件里的话，事件处理器够不着它。
  fileManager: {
    open: false,
    key: '',        // 当前服务的那块终端表面（bucketKey）
    hostId: '',
    sessionId: '',
    cwd: '',        // 当前浏览的目录（绝对路径）
    parent: '',     // 上一级目录（根目录时等于 cwd）
    entries: [],    // 目录条目
    loading: false,
    error: '',
    // busy 表示有一次上传/下载/删除正在进行：期间禁用按钮，
    // 避免同一个文件被并发删两次之类。
    busy: false,
    notice: '',     // 操作成功的短暂提示
    // progress 是传输进度，只在上传/下载期间非空。
    //
    // 字段：
    //   phase  'reading'（本地读文件并 base64）/ 'sending'（发给后端）
    //          / 'downloading'（后端正在分块拉取并落盘）/ 'done'
    //   loaded 已处理的**原始字节数**（不是 base64 长度 —— 用户看到的
    //          应该是文件本身的进度，而 base64 会膨胀约 33%）
    //   total  原始文件总字节（0 表示未知）
    //   name   正在传的文件名
    //   id     传输 id（仅下载）：后端分块下载的事件里带回来，取消时
    //          原样传给 CancelFileTransfer
    //   bps    累计平均速度（字节/秒，仅下载）
    //
    // 为什么要有 total 之外还留 loaded：FileReader 的 progress 事件
    // 在文件很小时可能一次都不触发（直接 onload），所以 UI 必须能处理
    // 「有名字有阶段但没有百分比」的状态，而不是除以 0 显示 NaN%。
    progress: null
  },

  // bySession 是「每台主机的每条会话各一条独立时间线」：桶键 → 对话记录。
  //
  // 必须按 (主机, 会话) 分开 —— LLM 的上下文就是按这个粒度分的
  // （见 internal/agent 的 sessions）。若界面仍用一条全局时间线，
  // 用户在一段对话里聊完切到另一条会话，屏幕上却还是上一段对话，
  // 他会以为模型也还记着那些话；而实际上那条会话是空的。
  // 两条时间线对不上，用户就没法推断模型到底知道什么。
  //
  // 键一律用 bucketKey 生成，别自己拼字符串。
  bySession: {},

  // activeHostId / activeSessionId 是**正在运行的那一轮**属于哪条时间线，
  // 在 ask() 里记下。
  //
  // 为什么不用 currentHostId / currentSessionId：后端事件流
  // （delta / tool / result）里不带这两个 ID，而用户完全可以在一轮
  // 跑到一半时切主机或切会话 —— 那时 current* 已经变了，按它去落事件
  // 就会把这一轮的输出写进另一条时间线，看起来像是「答到了别处」。
  activeHostId: '',
  activeSessionId: '',

  // newTaskIntent：主导航「新建任务」点一下就 +1；ConsolePanel 回欢迎首页。
  // 用计数而不是布尔：连续点两次也要各触发一次。
  newTaskIntent: 0,

  // chatHomeIntent：删光侧栏任务等场景 +1；ConsolePanel 切回对话欢迎首页。
  chatHomeIntent: 0,

  // hostPickerIntent：需要用户先选主机时 +1；ChatPanel 监听后弹出主机列表。
  hostPickerIntent: 0,

  pending: null,
  running: false,
  busy: false,

  // compacting 表示「正在让模型压缩会话」。
  //
  // 与 busy 分开：busy 是「后端在跑一轮对话」，compacting 是「在等模型写摘要」。
  // 两者都会持续若干秒，但用户能做的操作不同 —— 压缩期间不该再点一次压缩，
  // 而中断按钮对压缩没有意义（它拦不住一次已经在飞的 HTTP 请求）。
  compacting: false,

  // entries 是当前会话的对话记录（只读视图）。
  //
  // 刻意做成派生视图而不是又存一份数组：一旦有两份，
  // 「清空当前会话」就得记得同时清两份，迟早漏一处。
  get entries() {
    return entriesFor(this.currentHostId, this.currentSessionId)
  },

  // currentSessions 是当前主机的会话列表（只读视图）。
  get currentSessions() {
    return this.sessions[this.currentHostId] || NO_SESSIONS
  }
})

// 该主机还没有记录时的返回值。共享且只读 —— 只用于渲染，不会被写入
// （push 一律先建桶再写，所以落到这里的只有读操作）。
const NO_ENTRIES = []

// 会话列表还没拉回来时的返回值。同上，共享且只读。
const NO_SESSIONS = []

// 空串也是一个合法的桶：没有选中主机时（比如「请先添加一台主机」这类提示）
// 记录就落在它里面，而此时 currentHostId 也是空串 —— 两边正好对上。
function entriesFor(hostId, sessionId) {
  return store.bySession[bucketKey(hostId, sessionId)] || NO_ENTRIES
}

// 事件该落到哪条时间线上。
//
// 优先级：显式指定 > 正在运行的那一轮 > 当前选中的主机/会话。
// 事件流（delta / tool / result / 审批结果）都属于「正在跑的那一轮」，
// 所以它们走 active*；界面主动发的提示（如清空结果）才显式传。
//
// 显式给了主机却不给会话时落到**默认会话**：这是刻意的取舍 ——
// 调用方要么两个都给，要么都不给；只给主机多半是「跟会话无关的
// 主机级提示」，放默认会话至少是可见的，好过凭空造一条谁也找不到的时间线。
export function targetKey(explicitHostId, explicitSessionId) {
  if (explicitHostId) return bucketKey(explicitHostId, explicitSessionId)
  if (store.activeHostId) return bucketKey(store.activeHostId, store.activeSessionId)
  return bucketKey(store.currentHostId, store.currentSessionId)
}

// targetEntries 是「正在跑那一轮」所在的那条时间线（没有在跑时就是当前会话）。
//
// 与 store.entries 的区别必须分清：后者是**用户此刻在看的**那条，
// 前者是**输出该去**的那条。用户跑到一半切走时两者会不同 ——
// 流式增量、工具状态更新都必须用前者，否则内容会写进用户切过去的那条会话。
export function targetEntries() {
  return store.bySession[targetKey()] || NO_ENTRIES
}

// hasAgentTurn 判断某条会话是否「已经和 Agent 对话过」：时间线里存在 assistant
// （Agent 回复）或 tool（工具调用）条目即视为处于 agent 对话中。
//
// 供输入分流使用（termSubmit）：agent 对话中回车一律当「回复」交给 Agent，
// 不再做命令识别/执行 —— 回复 Agent 的话（哪怕首词恰好像命令）绝不该被当 shell 命令跑掉。
export function hasAgentTurn(hostId, sessionId) {
  const list = store.bySession[bucketKey(hostId, sessionId)]
  if (!list) return false
  return list.some(e => e.kind === 'assistant' || e.kind === 'tool')
}

// dismissAuditError 由界面在用户确认告警后调用。
// 注意只清本地状态：审计日志里的空洞是永久的，重新校验仍会发现。
export function dismissAuditError() {
  store.auditError = ''
}

let seq = 0

// push 追加一条记录。
//
// hostId / sessionId 省略时按 targetKey 的优先级落到「正在跑那一轮」的时间线上。
// 只有界面自己发的、跟运行无关的提示才需要显式指定。
export function push(entry, hostId, sessionId) {
  const key = targetKey(hostId, sessionId)
  if (!store.bySession[key]) store.bySession[key] = []
  const list = store.bySession[key]
  list.push({ id: ++seq, ts: Date.now(), ...entry })
  if (list.length > 500) list.splice(0, list.length - 500)
}

// clearLog 清掉**当前会话**屏幕上的记录。
//
// 它不动 LLM 的会话上下文 —— 后者要用 clearSession。
// 两件事必须分开：屏幕上还留着对话但模型已经忘了，是「清空上下文」的正常结果；
// 反过来若一个按钮把两者都清了，用户就永远学不到「这两件事不是一回事」。
export function clearLog() {
  store.bySession[bucketKey(store.currentHostId, store.currentSessionId)] = []
  store.pending = null
}

export function upsertTool(v) {
  const list = targetEntries()
  const found = list.find(e => e.kind === 'tool' && e.toolId === v.id)
  if (found) {
    Object.assign(found, { tool: v })
  } else {
    push({ kind: 'tool', toolId: v.id, tool: v })
  }
  if (v.status === 'running') store.running = true
}

// ---- 窗口标题 ----
// 把系统标题栏（Wails Title）同步为「Ai-Shell · 任务名」，
// 用户切任务时窗口标题跟着变；拿不到 runtime 或未选任务时回退静态标题。
export function syncWindowTitle(taskName) {
  const r = rt()
  if (!r || typeof r.WindowSetTitle !== 'function') return
  const base = 'Ai-Shell · Linux 智能运维台'
  r.WindowSetTitle(taskName ? `${base} — ${taskName}` : base)
}

// ---- 主题 ----
const THEME_KEY = 'aishell.theme'

// initTheme 在应用挂载前调用：恢复上次选择并把 data-theme 落到 <html> 上。
// 注意必须在首帧渲染前执行，否则暗色用户会先看到一帧白屏。
export function initTheme() {
  // 局部变量/参数不能叫 t —— 它会遮蔽 i18n 的翻译函数 t()。
  let v = 'light'
  try {
    v = localStorage.getItem(THEME_KEY) === 'dark' ? 'dark' : 'light'
  } catch { /* localStorage 不可用时用默认值 */ }
  store.theme = v
  document.documentElement.dataset.theme = v
}

export function setTheme(v) {
  if (v !== 'light' && v !== 'dark') return
  store.theme = v
  document.documentElement.dataset.theme = v
  try { localStorage.setItem(THEME_KEY, v) } catch { /* 忽略持久化失败 */ }
}

// ---- 终端字体 ----
const TERMFONT_KEY = 'aishell.termfont'

// initTermFont 在应用挂载前调用（main.js）：恢复上次的字体偏好。坏数据
// （非法 JSON、越界字号、空字体栈）一律当没有，回退默认。
export function initTermFont() {
  try {
    const raw = localStorage.getItem(TERMFONT_KEY)
    if (!raw) return
    const v = JSON.parse(raw)
    const size = Number(v && v.size)
    store.termFont = {
      family: typeof v.family === 'string' && v.family.trim() ? v.family : DEFAULT_TERM_FONT.family,
      size: Number.isFinite(size) && size >= 8 && size <= 28 ? size : DEFAULT_TERM_FONT.size
    }
  } catch { /* 坏数据当没有，用默认 */ }
}

// setTermFont 更新字体偏好（patch 可只带 family 或只带 size），夹紧字号到
// 8~28、步进 0.5。只改 store：已开终端实例的热更新由 ConsolePanel 的
// watcher 调 termSession.setTermFont 完成 —— store 不依赖任何渲染模块。
export function setTermFont(patch = {}) {
  const family = typeof patch.family === 'string' && patch.family.trim()
    ? patch.family
    : store.termFont.family
  let size = Number(patch.size)
  size = Number.isFinite(size)
    ? Math.min(28, Math.max(8, Math.round(size * 2) / 2))
    : store.termFont.size
  store.termFont = { family, size }
  try { localStorage.setItem(TERMFONT_KEY, JSON.stringify(store.termFont)) } catch { /* 忽略持久化失败 */ }
}

export async function bootstrap() {
  try {
    const info = await api().Bootstrap()
    if (info.error) {
      store.error = info.error
      return
    }
    store.posture = info.posture
    store.hosts = info.hosts || []
    store.llm = info.llm || store.llm
    store.llmProfiles = info.llmProfiles || []
    // 选中的方案若已不存在（被删）或还没定过，落到当前激活的那套上。
    if (!store.llmProfiles.some(p => p.id === store.selectedProfileId)) {
      store.selectedProfileId = (store.llmProfiles.find(p => p.active) || {}).id || ''
    }
    store.policy = info.policy || store.policy
    // 自动选中只在「只有一台主机」时发生：猜对率 100% 且省一次点击。
    // 多台时不猜 —— 选错的主机会被立即建立 SSH 连接（远端登录、审计痕迹），
    // 而用户启动后未必先用终端（可能只想看审计/改设置）。选中动作交给用户，
    // 连接时机随之从「启动时」移到「用户选择时」。
    if (!store.currentHostId && store.hosts.length === 1) store.currentHostId = store.hosts[0].id
    store.ready = true
  } catch (e) {
    store.error = String(e && e.message ? e.message : e)
  }
  // 会话列表在 ready 之后拉，且失败不影响启动 —— 它只是让选择器有内容，
  // 拉不到时界面退化成「只有默认会话」，仍然能用。
  await refreshSessions(store.currentHostId)
}

export async function refreshHosts() {
  store.hosts = await api().ListHosts()
  if (!store.hosts.some(h => h.id === store.currentHostId)) {
    store.currentHostId = store.hosts.length ? store.hosts[0].id : ''
  }
  store.posture.hostCount = store.hosts.length

  // 删除主机后，bySession 里那几条记录就成死条目了。每桶最多 256KB，
  // 删得勤也不会爆内存 —— 但留着一堆空条目也没意义，顺手清掉。
  //
  // 键是「主机 + \x00 + 会话」，所以取主机部分要按分隔符切，
  // 不能用 startsWith 猜 —— 主机 'a' 与主机 'ab' 会互相误判。
  // 注意空主机桶是「没有选中主机」时的兜底，不能清。
  const alive = new Set(store.hosts.map(h => h.id))
  for (const key of Object.keys(store.bySession)) {
    const host = key.split(SEP)[0]
    if (host && !alive.has(host)) delete store.bySession[key]
  }
  // 会话列表也是跟着主机走的，主机没了它的列表也该清掉。
  for (const id of Object.keys(store.sessions)) {
    if (!alive.has(id)) delete store.sessions[id]
  }
  // 删了之后发现 activeHostId 已经指向不存在的机器，重置回当前主机。
  if (store.activeHostId && !alive.has(store.activeHostId)) {
    store.activeHostId = store.currentHostId
    store.activeSessionId = store.currentSessionId
  }
  // 当前主机的会话列表要重新拉：上面可能刚把 currentHostId 换掉了。
  await refreshSessions(store.currentHostId)
}

// 会话列表拉取代数：并发 refresh（切主机 watch + createSession）时，
// 晚到的旧 ListSessions 不得覆盖新列表、更不得把刚选中的会话打回默认。
const sessionListEpoch = Object.create(null)

// refreshSessions 拉取某台主机的会话列表，并把「当前选中的会话」修正到有效值。
//
// 这一步不能省。列表是用户操作（新建/改名/删除）之后唯一的事实来源，
// 而 currentSessionId 可能指向一条**已经被删掉**的会话 ——
// 不修正的话，用户会看到一个空白的对话区，而他明明没有删过任何东西。
export async function refreshSessions(hostId) {
  if (!hostId) {
    store.currentSessionId = ''
    return
  }
  const epoch = (sessionListEpoch[hostId] = (sessionListEpoch[hostId] || 0) + 1)
  let list
  try {
    list = (await api().ListSessions(hostId)) || []
  } catch (e) {
    // 列表拉不到不该把界面清空：保留上一次的结果，用户至少还能继续用。
    // 但要说出来 —— 否则他会以为自己的会话全没了。
    push({ kind: 'error', content: t('msg.sessionsFail', { err: e && e.message ? e.message : e }) }, hostId)
    return
  }
  // 已被更新的拉取取代：丢弃这份过期结果，避免冲掉刚 CreateSession 的列表。
  if (epoch !== sessionListEpoch[hostId]) return

  store.sessions = { ...store.sessions, [hostId]: list }

  // 选中的会话可能指向一条**已经被删掉**的会话，也可能还是初始的空串。
  // 两种情况都必须落到**默认会话**上，而且要用后端报回来的那个 ID，
  // 不能写死成某个字符串。
  //
  // 这条修正不是可选的：选择器的每个 <option> 的 value 都是真实 ID，
  // 若 currentSessionId 停在空串上，就没有任何一个 option 与之匹配 ——
  // 界面表现为「选择器是空的」，而用户其实有会话可用。
  // （记录本身不会丢：bucketKey 会把空串归一化成默认会话，
  //   所以这只影响选择器的显示，不影响消息落进哪个桶。）
  if (!list.some(s => s.id === store.currentSessionId)) {
    const def = list.find(s => s.isDefault)
    store.currentSessionId = def ? def.id : ''
  }
  pruneTimelines(hostId, list)
}

// refreshAllSessionLists 把**未选中主机**的会话列表拉齐，只填充 store.sessions、
// 不碰 currentSessionId / currentHostId —— 与 refreshSessions 的区别在于后者
// 还承担「把选中会话修正到有效值」的选中语义。
//
// 用途是侧栏的「按主机分组」视图：还没有选中主机时（欢迎页），侧栏也要能把
// 每台主机的任务列出来供直达。当前选中的主机被跳过 —— 它的列表由切换与
// 操作流程里的 refreshSessions 维护，这里再拉一遍等于用旧数据覆盖新数据
// （重命名/新建刚落盘就被冲掉）。单台失败只让它缺席（该组显示为空），不挡其它主机。
export async function refreshAllSessionLists() {
  await Promise.all(
    store.hosts
      .filter(h => h.id !== store.currentHostId)
      .map(async h => {
        try {
          const list = (await api().ListSessions(h.id)) || []
          store.sessions = { ...store.sessions, [h.id]: list }
        } catch { /* 拉不到就不显示这台的任务，下次主机列表变化时会再试 */ }
      })
  )
}

// pruneTimelines 清掉某台主机下「会话已不存在」的时间线。
//
// 不清的话，用户删掉一条会话、再新建一条恰好拿到同一个 ID 时，
// 屏幕上会突然冒出上一次的对话 —— 后端是干净的，只有界面在骗人。
function pruneTimelines(hostId, list) {
  const alive = new Set(list.map(s => s.id))
  // 前缀必须是「主机 + 分隔符」，**不能**写成 bucketKey(hostId, '')。
  // 后者会把空会话 ID 归一化成默认会话的 ID，于是前缀变成
  // 'h1\x00default' —— 而 'h1\x00s2' 并不以它开头，结果一条也剪不掉。
  // （分隔符本身也顺带解决了主机 ID 互为前缀的问题：
  //   'h11\x00s' 不以 'h1\x00' 开头。）
  const prefix = hostId + SEP
  for (const key of Object.keys(store.bySession)) {
    if (!key.startsWith(prefix)) continue
    const sessionId = key.slice(prefix.length)
    if (!alive.has(sessionId)) delete store.bySession[key]
  }
}

// liveAssistant 找出某一轮正在流式生成的那条 LLM 消息。
//
// 用 step（后端给的第几轮）做标识，而不是「最后一条 assistant」——
// 多轮工具调用里会有多条 assistant 消息，认错目标就会把内容写到错误的回答上。
// step 缺失时必须返回 undefined：否则 e.stream === undefined 会命中所有非流式消息。
export function liveAssistant(step) {
  if (step === undefined || step === null) return undefined
  // 认的是「正在跑那一轮」的时间线，不是当前选中的会话 ——
  // 跑到一半切了主机或会话，流式消息仍在原来那条时间线上。
  // step 统一成 Number：Wails/JSON 偶发字符串，严格 === 会对不上、思考增量无法挂载。
  const want = Number(step)
  if (Number.isNaN(want)) return undefined
  return targetEntries().find(e => e.kind === 'assistant' && Number(e.stream) === want)
}

// bindEvents 注册所有后端事件。必须可安全重复调用：
// Wails v2 的 EventsOn 对同名事件是**追加**监听器而不是替换，
// 热重载或组件重新挂载后再绑一遍，每条事件就会被处理 N 次 ——
// 表现是终端里「ls 变成 lllsss」、对话消息重复三份。
// 因此每次先 EventsOff 清掉同名旧监听，再重新注册：调用多少次都只挂一份。
// 事件处理的具体逻辑已拆到 terminal/termEvents.js。
export function bindEvents() {
  const r = rt()
  if (!r) return
  bindTerminalEvents(r)
}

// freezeStreams 收尾所有还在「流式中」的消息。
// 中断或出错时流不会再有定稿消息，不清掉标记就会留下一个永远闪烁的光标。
export function freezeStreams() {
  for (const e of targetEntries()) {
    if (e.streaming) e.streaming = false
  }
}

// 本轮待挂到用户气泡上的附图（仅 UI；后端 agent:message 不含图）。
// ask 成功发起后暂存，等 agent:message(role=user) 消费；失败 / done / error 清掉。
let stagedUserImages = null

function imagePreviewURL(img) {
  if (!img) return ''
  if (img.preview) return img.preview
  const data = String(img.data || '')
  if (data.startsWith('data:')) return data
  if (data && img.mime) return `data:${img.mime};base64,${data}`
  return ''
}

function normalizeStagedImages(images) {
  const list = Array.isArray(images) ? images.filter(Boolean) : []
  return list.map((img, i) => {
    const mime = String(img.mime || 'image/png')
    const ext = mime.includes('jpeg') || mime.includes('jpg')
      ? 'jpg'
      : mime.includes('webp')
        ? 'webp'
        : mime.includes('gif')
          ? 'gif'
          : 'png'
    return {
      mime,
      data: img.data || '',
      preview: imagePreviewURL(img),
      name: String(img.name || `Screenshot.${ext}`).trim() || `Screenshot.${ext}`
    }
  })
}

/** 取出并清空本轮暂存附图（给 agent:message 用户气泡用）。 */
export function takeStagedUserImages() {
  const imgs = stagedUserImages
  stagedUserImages = null
  return imgs && imgs.length ? imgs : []
}

export function clearStagedUserImages() {
  stagedUserImages = null
}

// ask 返回是否已成功发起本轮（true）—— 调用方据此决定要不要清草稿。
// 校验失败 / busy / 后端同步拒绝一律 false，避免「输入丢了但没发出去」。
// images: 可选 [{mime, data, preview?, name?}]；仅 mime+data 发给模型，其余只进气泡芯片。
export async function ask(prompt, images = []) {
  if (!store.currentHostId) {
    push({ kind: 'error', content: t('msg.needHost') })
    return false
  }
  // 对话页发送前已用弹层拦过；终端分流仍可能直接 ask —— 这里再守一道：
  // 没有当前选中的 LLM 方案就不要开跑，避免跑到一半才 agent:error。
  let profileId = store.selectedProfileId ||
    (store.llmProfiles.find(p => p.active) || {}).id || ''
  if (!profileId) {
    push({ kind: 'error', content: t('msg.needModel') })
    return false
  }
  const imgs = Array.isArray(images) ? images.filter(Boolean) : []
  if (imgs.length) {
    const profile = store.llmProfiles.find(p => p.id === profileId)
    if (!profile || !profile.supportsVision) {
      push({ kind: 'error', content: t('msg.needVision') })
      return false
    }
  }
  if (!String(prompt || '').trim() && !imgs.length) {
    return false
  }
  // 若界面尚未写 selectedProfileId（仅后端 active），对齐一下，避免下拉与请求不一致。
  if (!store.selectedProfileId) store.selectedProfileId = profileId
  // 防重入：这个不变量由 store 自己守住，不指望每个调用方都记得先查 running。
  // 否则第二次调用会被后端以「已有会话正在运行」拒绝，给用户弹一个莫名其妙的报错。
  // 不能静默 return：那会让用户以为「发出去没反应」，必须给出可见提示。
  if (store.running || store.busy) {
    push({ kind: 'system', content: t('msg.busyRound') })
    return false
  }
  // 不在开跑时清零 tokens/window：圆环要一直显示上一轮用量，等本轮 agent:context 再覆盖。
  // 只清 model，避免思考占位短暂挂着上轮模型名。
  store.contextUsage.model = ''
  store.running = true
  try {
    // 未命名的空会话：用首条用户消息当任务标题（与欢迎首页「先聊再起名」一致）。
    await maybeAutoTitleSession(prompt || t('msg.imageAskTitle'))
    // 会话可能刚被 createSession / 自动起名确定：在 Ask 前再钉 active*，
    // 避免事件落到旧桶。
    store.activeHostId = store.currentHostId
    store.activeSessionId = store.currentSessionId
    // 本轮 PTY 写入标记从零计数：上一轮结束时已消费，期间的零星写入
    // （如空回车让 shell 打新提示符）不代表「本轮 shell 自己打印过提示符」。
    ptyDirty.delete(bucketKey(store.activeHostId, store.activeSessionId))
    // Ask 前再读一次：自动起名等待期间用户可能刚切了模型。
    profileId = store.selectedProfileId || profileId
    // 附图：后端只要 mime+data；name/preview 留给用户气泡芯片悬停预览。
    stagedUserImages = imgs.length ? normalizeStagedImages(imgs) : null
    const imagesJSON = imgs.length
      ? JSON.stringify(imgs.map(({ mime, data }) => ({ mime, data })))
      : ''
    await api().Ask(store.currentHostId, store.currentSessionId, prompt || '', profileId, imagesJSON)
    return true
  } catch (e) {
    store.running = false
    clearStagedUserImages()
    push({ kind: 'error', content: String(e) })
    return false
  }
}

// clearSession 清空**某条会话的 LLM 上下文**。
//
// 与 clearLog 的区别必须让用户看得见：清屏不动模型的记忆，
// 清上下文不动画面上的文字。后端返回轮数，界面据此区分
// 「清掉了 N 轮」和「本来就没有上下文」—— 后者能解释「为什么点了没反应」，
// 不然用户只会以为按钮坏了。
//
// 粒度是**一条会话**而不是一台主机：用户完全可能想留住「nginx 排查」
// 那段上下文，只把另一条试验性的对话清掉。
export async function clearSession(hostId, sessionId) {
  // 无主机守卫：组件按钮已隐藏该入口，这里再兜一道 —— 未来新调用方不必记得先判空。
  if (!hostId) {
    push({ kind: 'error', content: t('msg.pickHost') })
    return
  }
  try {
    const res = await api().ClearSession(hostId, sessionId)
    if (res && res.busy) {
      push({
        kind: 'error',
        content: t('msg.clearRunning')
      }, hostId, sessionId)
      return
    }
    const n = (res && res.cleared) || 0
    // 上下文已清空：用量归零，否则工具条上还挂着上一轮的「87%」——
    // 用户会以为清空没生效。
    store.contextUsage.tokens = 0
    store.contextUsage.window = 0
    store.contextUsage.messages = 0
    store.contextUsage.model = ''
    push({
      kind: 'system',
      content: n > 0 ? t('msg.clearDone', { n }) : t('msg.clearEmpty')
    }, hostId, sessionId)
  } catch (e) {
    push({
      kind: 'error',
      content: t('msg.clearFail', { err: e && e.message ? e.message : e })
    }, hostId, sessionId)
  }
}

// compactSession 让模型把**某条会话**的历史压缩成一段摘要。
//
// 与 clearSession 的分工：清空是**丢掉**记忆，压缩是**换一种更省的方式留着**。
// 排查链路很长时压缩比清空合适得多 —— 用户既想省 token，又不想让模型失忆。
//
// 后端刻意用「结果对象 + error」区分两类情况：
//   - 预期内的状态（正忙、没得压）走结果对象，以 system 消息提示；
//   - 真故障（没配 Key、模型调用失败）走 error，以 error 消息提示。
// 把「没有可压缩的内容」渲染成红色报错是误导 —— 用户会以为功能坏了。
export async function compactSession(hostId, sessionId) {
  // 无主机守卫：同 clearSession，兜住绕过组件按钮的调用方。
  if (!hostId) {
    push({ kind: 'error', content: t('msg.pickHost') })
    return
  }
  if (store.compacting) return
  store.compacting = true
  try {
    const res = await api().CompactSession(hostId, sessionId)
    // 后端已经把话说全了（含被拒绝的原因），照搬即可 ——
    // 在前端重写一遍措辞，两处迟早会漂移。
    push({ kind: 'system', content: (res && res.message) || t('msg.compactDone') }, hostId, sessionId)
  } catch (e) {
    push({
      kind: 'error',
      content: t('msg.compactFail', { err: e && e.message ? e.message : e })
    }, hostId, sessionId)
  } finally {
    store.compacting = false
  }
}

// ---- 会话的增删改查 ----

// loadSessionTimeline：侧栏点进任务时，把后端历史灌进本地 bySession。
// 已有气泡则跳过（当场听过的事件优先，避免冲掉流式中的半条助手消息）。
export async function loadSessionTimeline(hostId, sessionId) {
  if (!api() || !hostId) return
  const key = bucketKey(hostId, sessionId)
  const existing = store.bySession[key]
  if (existing && existing.length > 0) return
  let rows
  try {
    rows = (await api().GetSessionTimeline(hostId, sessionId || '')) || []
  } catch {
    return
  }
  // 异步回来时焦点可能已切走：只写目标桶，不碰 current*。
  if (store.bySession[key] && store.bySession[key].length > 0) return
  const list = []
  for (const r of rows) {
    if (!r || !r.kind) continue
    if (r.kind === 'tool') {
      list.push({
        id: ++seq,
        ts: Date.now(),
        kind: 'tool',
        toolId: r.toolId || '',
        tool: {
          id: r.toolId || '',
          name: r.name || 'tool',
          command: r.content || '',
          status: 'done',
          decision: 'allow'
        }
      })
      continue
    }
    if (r.kind === 'result') {
      list.push({
        id: ++seq,
        ts: Date.now(),
        kind: 'result',
        toolId: r.toolId || '',
        exitCode: 0,
        content: r.content || '',
        redacted: 0,
        injection: []
      })
      continue
    }
    list.push({
      id: ++seq,
      ts: Date.now(),
      kind: r.kind,
      content: r.content || '',
      reasoning: r.reasoning || '',
      reasoningOpen: false
    })
  }
  store.bySession[key] = list
  if (hostId === store.currentHostId &&
      (sessionId || '') === (store.currentSessionId || '') &&
      list.length > 0) {
    noteConversationActivity()
  }
}

// selectSession 切换当前会话。
//
// 刻意**不**在运行中禁止切换：事件按 activeSessionId 归档（见 targetKey），
// 切过去之后这一轮的输出仍然落在原来那条会话上，不会写错地方 ——
// 与切主机的行为一致。禁掉反而多一个「为什么点不动」的解释负担。
export async function selectSession(sessionId) {
  bindSession()
  store.currentSessionId = sessionId || ''
  // 告诉后端「这台主机现在看着哪条会话」：之后常驻终端定格的快照要落到这条上。
  reportActiveSession(store.currentHostId, store.currentSessionId)
  await loadSessionTimeline(store.currentHostId, store.currentSessionId)
}

// requestNewTask 供主导航调用：切到对话欢迎首页（真正开任务等发消息/开终端）。
export function requestNewTask() {
  store.newTaskIntent++
}

// requestHostPicker 弹出对话页主机列表。
export function requestHostPicker() {
  store.hostPickerIntent++
}

// beginFreshTask：回到新建欢迎态（可预选主机，尚未绑定任务）。
export function beginFreshTask() {
  store.freshTaskPending = true
  store.sessionBound = false
}

export function clearFreshTask() {
  store.freshTaskPending = false
}

// bindSession：任务已落地（侧栏进入 / CreateSession 成功），锁定底栏主机。
export function bindSession() {
  store.freshTaskPending = false
  store.sessionBound = true
}

export function unbindSession() {
  store.sessionBound = false
}

// commitDraftHost：把底栏预选主机落到 currentHostId（发起对话 / 开终端时调用）。
// 已选中同一台则只清草稿；返回是否已有可用主机。
export async function commitDraftHost() {
  const id = store.draftHostId || store.currentHostId
  if (!id) return false
  store.draftHostId = ''
  if (store.currentHostId !== id) {
    store.currentHostId = id
  }
  // 与 ConsolePanel 的 host watch 并行时多拉一次无害；保证 ask 前会话列表已就绪。
  await refreshSessions(id)
  return true
}

// ensureFreshTaskSession：新建任务真正开始时静默开一条新会话，
// 避免落到该主机默认会话上串旧上下文。
// 失败时退回「仅预选」：currentHostId 清空、draftHostId 恢复，便于重试。
export async function ensureFreshTaskSession() {
  if (!store.freshTaskPending) {
    // 已有主机会话（侧栏进入 / 非新建）：视为已绑定。
    if (store.currentHostId) bindSession()
    return true
  }
  const hostId = store.currentHostId
  if (!hostId) return false
  const info = await createSession(t('msg.untitledTask'), { silent: true })
  if (!info) {
    store.draftHostId = hostId
    store.currentHostId = ''
    store.currentSessionId = ''
    // freshTaskPending 保持 true，允许再次发送/开终端。
    return false
  }
  bindSession()
  return true
}

// currentSessionHasTurns：当前会话是否已有对话轮次（后端元信息）。
// 清空屏幕记录后 entries 为空，但 turns 仍可能 >0 —— 顶栏/工具条应继续显示。
export function currentSessionHasTurns() {
  const list = store.currentSessions || []
  const cur = list.find(s => s.id === store.currentSessionId) || list.find(s => s.isDefault)
  return !!(cur && cur.turns > 0)
}

// 本机 UI 是否已经在「当前焦点会话」里见过对话气泡。
// 不能单靠后端 turns：空首页点「选择主机」会 refreshSessions，
// 旧会话 turns>0 会把压缩/清空工具条和顶栏突然顶出来，但屏幕仍是空首页。
let sawConversation = false

export function noteConversationActivity() {
  if (store.entries.length > 0) sawConversation = true
}

export function onFocusSessionChanged() {
  sawConversation = store.entries.length > 0
}

export function resetSessionChromeSeen() {
  sawConversation = false
}

// 顶栏 / 对话工具条：有气泡，或本页见过对话且清空记录后 turns 仍在。
export function shouldShowSessionChrome() {
  return store.entries.length > 0 || (sawConversation && currentSessionHasTurns())
}

// 与后端 maxSessionNameRunes 对齐：按 Unicode 字符截断，避免中文被按字节切坏。
const MAX_SESSION_NAME_RUNES = 40

export function titleFromPrompt(prompt) {
  const s = String(prompt || '').replace(/\s+/g, ' ').trim()
  if (!s) return ''
  const chars = Array.from(s)
  if (chars.length <= MAX_SESSION_NAME_RUNES) return s
  // 超长留省略号（占 1 个字符位），与后端 maxSessionNameRunes 对齐。
  return chars.slice(0, MAX_SESSION_NAME_RUNES - 1).join('') + '…'
}

function isUntitledSession(s) {
  if (!s) return false
  const name = String(s.name || '').trim()
  if (!name) return true
  return name === t('msg.untitledTask') || name === t('ss.unnamedTask') || name === t('console.unnamed')
}

// maybeAutoTitleSession：空的未命名会话用首条用户消息起名（静默，不灌系统气泡）。
export async function maybeAutoTitleSession(prompt) {
  const list = store.currentSessions || []
  const sid = store.currentSessionId || DEFAULT_SESSION
  const cur = list.find(s => s.id === sid)
  if (!cur) {
    // 列表里没有当前会话：只允许给「尚未物化」的默认槽起名。
    // 其它 id 缺失多半是测试/瞬时态，乱 rename→refresh 会把选中冲回默认。
    if (sid !== DEFAULT_SESSION) return
  } else if (cur.turns > 0 || !isUntitledSession(cur)) {
    return
  }
  const title = titleFromPrompt(prompt)
  if (!title) return
  await renameSession(sid, title, { silent: true })
}

// createSession 在当前主机下新建一条会话，并立刻切过去。
//
// 新建后必须切过去：用户的意图就是「开一段新对话」，
// 建完还停在旧会话上，他会以为新建失败了。
// opts.silent：不往时间线塞「已新建」—— 欢迎首页选主机/新建后仍留在当前界面。
export async function createSession(name, opts = {}) {
  const hostId = opts.hostId || store.currentHostId
  if (!hostId) {
    push({ kind: 'error', content: t('msg.pickHost') })
    return null
  }
  try {
    const info = await api().CreateSession(hostId, name)
    // 先钉选中再拉列表：避免并发 refreshSessions 用「建之前」的旧列表
    // 把 currentSessionId 从空/默认「修正」走，首条消息自动起名就会打到错会话。
    store.currentSessionId = info.id
    await refreshSessions(hostId)
    store.currentSessionId = info.id
    // 并发旧列表若仍缺这条，本地补上，保证 maybeAutoTitleSession 能认出未命名会话。
    const list = store.sessions[hostId] || []
    if (!list.some(s => s.id === info.id)) {
      store.sessions = {
        ...store.sessions,
        [hostId]: list.concat([{
          id: info.id,
          name: info.name || '',
          turns: info.turns || 0,
          archivedTurns: info.archivedTurns || 0,
          isDefault: !!info.isDefault
        }])
      }
    }
    if (!opts.silent) {
      push({ kind: 'system', content: t('msg.taskCreated', { name: info.name }) }, hostId, info.id)
    }
    return info
  } catch (e) {
    push({
      kind: 'error',
      content: t('msg.taskCreateFail', { err: e && e.message ? e.message : e })
    }, hostId)
    return null
  }
}

// renameSession 改会话名。改名**不**影响会话的身份 —— 它是由 ID 认定的。
// opts.silent：首条消息自动起名时不灌系统气泡、失败也不弹错（避免欢迎页被刷）。
// opts.hostId：侧栏跨主机操作时显式指定，避免为改名而切走当前主区。
export async function renameSession(sessionId, name, opts = {}) {
  const hostId = opts.hostId || store.currentHostId
  if (!hostId) {
    push({ kind: 'error', content: t('msg.pickHost') })
    return
  }
  try {
    await api().RenameSession(hostId, sessionId, name)
    await refreshSessions(hostId)
    if (!opts.silent) {
      push({ kind: 'system', content: t('msg.taskRenamed', { name }) }, hostId, sessionId)
    }
  } catch (e) {
    if (opts.silent) return
    push({
      kind: 'error',
      content: t('msg.renameFail', { err: e && e.message ? e.message : e })
    }, hostId, sessionId)
  }
}

// 与侧栏 visibleTasks 同口径：默认空会话（turns=0）不算「最近任务」。
function visibleTaskCount(list) {
  return (list || []).filter(s => !s.isDefault || s.turns > 0).length
}

function returnToNewTaskHome() {
  store.currentHostId = ''
  store.currentSessionId = ''
  store.draftHostId = ''
  beginFreshTask()
  resetSessionChromeSeen()
  store.chatHomeIntent++
}

// deleteSession 删掉一条会话。
//
// 默认会话：后端清空内容但保留槽位（侧栏 turns>0 才显示）。本地时间线
// 也要清掉，否则列表没了、屏幕上还挂着旧气泡。
// 若删完后**当前正在看的那台主机**已无可见任务 → 回到新建欢迎态。
// opts.hostId：侧栏跨主机删除时显式指定，不切走主区焦点。
export async function deleteSession(sessionId, opts = {}) {
  const hostId = opts.hostId || store.currentHostId
  if (!hostId) {
    push({ kind: 'error', content: t('msg.pickHost') })
    return
  }
  try {
    await api().DeleteSession(hostId, sessionId)
    store.bySession[bucketKey(hostId, sessionId)] = []
    // 删掉的正好是当前选中的那条：先切回默认会话，再刷新列表。
    // 顺序不能反 —— 先刷新的话，refreshSessions 里的兜底也会把它置空，
    // 结果相同但意图看不出来。
    if (store.currentHostId === hostId && store.currentSessionId === sessionId) {
      store.currentSessionId = ''
    }
    await refreshSessions(hostId)
    if (store.currentHostId === hostId && visibleTaskCount(store.sessions[hostId]) === 0) {
      returnToNewTaskHome()
    }
    // 删除成功不灌系统气泡：侧栏列表变化已是反馈。
  } catch (e) {
    push({
      kind: 'error',
      content: t('msg.deleteSessionFail', { err: e && e.message ? e.message : e })
    }, hostId)
  }
}

export async function approve(ok) {  if (!store.pending) return
  const id = store.pending.id
  store.pending = null

  let applied = false
  try {
    applied = await api().Approve(id, ok)
  } catch (e) {
    push({ kind: 'error', content: t('msg.approveFail', { err: e && e.message ? e.message : e }) })
    return
  }

  // 后端 Resolve() 对已超时（默认 5 分钟）或已中断的审批 id 返回 false，且不抛错。
  // 这种情况下命令根本不会执行，所以绝不能显示「已批准执行」——那是在骗用户。
  if (!applied) {
    push({
      kind: 'error',
      content: t('msg.approvalStale')
    })
    return
  }

  push({ kind: 'system', content: ok ? '已批准执行' : '已拒绝执行' })
}

// switchProfile 切换当前使用的 LLM 方案（= 模型）。
// 会话历史按 (主机, 会话) 存储、与模型无关，且每轮对话开始时才读取
// 当前激活方案 —— 因此切换不丢上下文，下一轮对话即用新模型。
export async function switchProfile(id) {
  try {
    await api().ActivateLLMProfile(id)
  } catch (e) {
    const msg = t('msg.switchModelFail', { err: e && e.message ? e.message : e })
    push({ kind: 'error', content: msg })
    throw e // 让调用方知道失败，避免外层再推「已切换」的成功提示
  }
  const info = await api().Bootstrap()
  store.llm = info.llm || store.llm
  store.llmProfiles = info.llmProfiles || []
  // 选中项以用户这次的选择为准，而不是重新从 active 推一遍 ——
  // 保证「刚选的那套」就是下一轮请求带的那套。
  store.selectedProfileId = id
}

// stop 中断本轮。opts.silent=true 时不灌「已中断」系统条（换模型先停一轮用）。
export async function stop(opts = {}) {
  // 只有真的有任务在跑才落回执：后端 Stop() 对「本来就没在跑」是静默空操作
  // （agent.go：a.cancel 为 nil 就什么都不做），无条件 push 会在竞态窗口
  // （回合恰好在点击前一瞬正常收场）里留下一句「已中断」的谎话。
  const silent = !!(opts && opts.silent)
  const wasActive = store.running || store.busy
  try {
    await api().Stop()
    // 后端对用户主动中断**静默收场**（agent.go：ctx 取消不是故障，不发
    // agent:error）—— 否则被取消的 HTTP 请求会把「请求 LLM 失败: context
    // canceled」画成红字，等于把用户自己的操作报成错误。所以这句回执由
    // 前端补：灰字系统提示，进归档时间线；表面上的可见反馈是
    // 「Working 停止 + 输入提示符回来」（ConsolePanel 的 running watch）。
    if (wasActive && !silent) push({ kind: 'system', content: t('msg.stopped') })
  } catch (e) {
    push({ kind: 'error', content: t('msg.stopFail', { err: e && e.message ? e.message : e }) })
  } finally {
    // 无论后端是否响应，本地都必须回到可交互状态，否则界面会永久卡在「运行中」。
    store.running = false
    store.busy = false
    store.pending = null
    freezeStreams()
  }
}

// 终端输出静默检测（afterTermQuiet）已拆到 terminal/termEvents.js；
// 本模块通过顶部 import 使用它（见 runShellInTerminal）。

// runShellInTerminal：composer 里人敲的 shell 命令，走**常驻终端**通道 ——
// 仅硬拒绝不可逆/凭据命令，其余直接把这行写进常驻 PTY，提示符回显、输出、
// top/vim 接管全发生在同一片终端表面上，与人亲手在键盘上敲完全同形。
//
// 与已退役的 runShell（有界 Exec，输出喂 LLM）的差别：这里**不捕获**输出文本，
// 模型看到的是命令结束时定格的屏幕快照（见 agent:snapshot）。
// busy 覆盖「裁决 + 写入」整段，避免用户连敲把后端打成并发。
export async function runShellInTerminal(hostId, sessionId, command) {
  const key = bucketKey(hostId, sessionId)
  store.busy = true
  try {
    const res = await api().RunShellInTerminal(hostId, sessionId, command)
    // 放行时 PTY 自己会回显命令与输出；命令跑完补一句「输出可交给 LLM 分析」的
    // **提示**（纯文字，不进 agent —— 见 termPaint.paintLLMHint 的说明）。
    // 被拒/取消/出错由 paintShellVerdict 说明原因，不再叠加提示。
    //
    // 提示必须**等命令输出画完**再落：这个调用返回时命令才刚写进 PTY，
    // 回显与输出还在路上，此时画提示会插到输出前面（见 afterTermQuiet 注释）。
    // 裁决说明同理 —— 它说的是「这条命令的结果如何」，显然该排在结果后面。
    if (res.status === 'done') {
      // 命令已写进 PTY，接下来屏幕归它。开一个乐观透传窗口：期间的按键直达
      // PTY，兜住 shell integration 缺席、而程序又在切备用屏前就等输入的场
      // 景（vim 的 E325、read、菜单），避免按键被本地行编辑吞掉而“卡死”。
      armCmdPending(hostId, sessionId)
      // 「命令跑完」不能只看输出静默：交互式命令（bt 菜单/read/REPL）在
      // **等输入**时输出同样是停的 —— 静默一到就画，提示会插进程序界面里
      // （bt 菜单里会挂在 "Please enter command number:" 后面），长命令的
      // 输出间隙也会被误判成结束。命令还在跑（term:cmd，OSC 133;C..D）
      // 就再等一轮静默，直到真的回到提示符。
      const afterDone = () => {
        if (termState(hostId, sessionId).cmdActive) {
          afterTermQuiet(hostId, sessionId, afterDone)
          return
        }
        paint.paintShellVerdict(surfaceWrite(key), res)
        // 「可交给 LLM 分析」的提示由**组件**画：占位符要贴在提示符后面，
        // 起点列只有组件读得到（xterm 光标位置），store 只有 write 函数。
        fireLLMHint(hostId, sessionId)
      }
      afterTermQuiet(hostId, sessionId, afterDone)
    } else {
      paint.paintShellVerdict(surfaceWrite(key), res)
    }
    return res
  } catch (e) {
    const msg = String(e && e.message ? e.message : e)
    paint.paintError(surfaceWrite(key), msg)
    push({ kind: 'error', content: t('msg.cmdFail', { msg }) })
    return { status: 'error', error: msg }
  } finally {
    store.busy = false
  }
}

// reportActiveSession 告诉后端「这台主机现在正看着哪条会话」。
//
// 常驻终端的定格快照（TUI 命令结束、备用屏幕释放、PTY 退出）不带会话 ID，
// 后端 per-host 记住这里报的值，用它给快照路由到正确的时间线 ——
// 用户切了会话之后产生的快照，就该落进新的那条。
// 失败静默：报不上去顶多让快照落到上一次报的会话，不该打断界面。
export async function reportActiveSession(hostId, sessionId) {
  if (!api() || !hostId) return
  try {
    await api().ReportActiveSession(hostId, sessionId || '')
  } catch {
    // 忽略：后端没就绪、或这台主机还没开终端时都会失败，这不是用户能处理的错误。
  }
}

// ---- 交互终端 ----
//
// 后端推的是**原始字节**（base64 编码），不是字符串。原因见后端 OpenTerminal：
// 一次读取完全可能把一个多字节 UTF-8 字符切成两半，当成字符串发就会渲染成乱码。
// 所以这里一律在字节层转，绝不先解成字符串再编码回去。
//
// 顺带说一句：不能拿 btoa/atob 直接作用于含中文的字符串 —— 它们只认
// Latin-1，遇到中文直接抛 InvalidCharacterError。
//
// 另外：这里一律直接写「api() 后面紧跟方法名」，而不是先存进一个局部变量。
// 契约测试是靠正则扫源码找前端调用点的（它连注释一起扫），存进局部变量就
// 扫不到了 —— 那四个终端方法会变成「后端导出但前端从未调用」，
// 而真正的拼写错误也就跟着一起漏过去。这个约束不明显，所以写在这里。
// 反过来说，这段注释本身也不能出现那种形状的字面量，否则会被当成一次调用。

export function bytesToBase64(bytes) {
  let s = ''
  // 分块拼接：一次 apply 传几万个参数会爆栈，而终端的输出块完全可能很大。
  const CHUNK = 0x8000
  for (let i = 0; i < bytes.length; i += CHUNK) {
    s += String.fromCharCode.apply(null, bytes.subarray(i, i + CHUNK))
  }
  return btoa(s)
}

export function base64ToBytes(b64) {
  const s = atob(b64)
  const out = new Uint8Array(s.length)
  for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i)
  return out
}

// textToBase64 把 xterm 给的按键字符串编成 UTF-8 字节再 base64。
// 中文输入法、粘贴进来的内容都要按原始字节送，中间任何一次
// 「字符串 ↔ 字节」的随手转换都可能把用户敲的字变成问号。
const utf8 = new TextEncoder()
export function textToBase64(s) {
  return bytesToBase64(utf8.encode(s))
}

// 终端 sink 登记表、LLM 提示回调与静默检测都已拆到 terminal/termEvents.js。
// 这里 re-export 三个注册函数，保持测试与旧调用面的导入不变。
export { registerTermSink, unregisterTermSink, registerLLMHint }

export function termState(hostId, sessionId) {
  const key = bucketKey(hostId, sessionId)
  if (!store.terms[key]) {
    // tuiActive / cmdActive 是两块「输入让路」信号（全屏接管 / 命令运行中），
    // 由 term:tui / term:cmd 事件更新。cmdPending 是提交 shell 命令后乐观开
    // 的透传窗口（见 termEvents 的 armCmdPending）：兜住 shell integration 缺席、
    // 程序又在切备用屏前就等输入的场景（vim 的 E325、read、菜单）。初值必须在场。
    store.terms[key] = {
      status: 'idle', error: '', exitReason: '',
      tuiActive: false, cmdActive: false, cmdPending: false
    }
  }
  return store.terms[key]
}

export async function openTerminal(hostId, sessionId, cols, rows) {
  const st = termState(hostId, sessionId)
  if (!api()) {
    st.status = 'error'
    st.error = '后端未就绪'
    return st
  }
  st.status = 'opening'
  st.error = ''
  st.exitReason = ''
  try {
    await api().OpenTerminal(hostId, sessionId, cols || 80, rows || 24)
    // 只在还停在 opening 时才改成 open：等待期间远端可能已经退出了，
    // 那一次 term:exit 把状态改成了 closed，这里再覆盖就把退出消息吞了。
    if (st.status === 'opening') st.status = 'open'
  } catch (e) {
    st.status = 'error'
    st.error = String(e && e.message ? e.message : e)
  }
  return st
}

// ptyDirty 记录「哪块表面在本轮 agent 运行期间被写过 PTY 输入」。
// 纯问答轮（提问没走 PTY）结束时远端 shell 不会打印新提示符，
// 界面要补一个本地输入提示符；而 top 交接、shell 命令这类写入会让
// shell 自己回到提示符，就不该再补。见 agent:done 处理与 ConsolePanel。
const ptyDirty = new Set()

// ---- 文件管理 ----

// 文件管理弹窗同时只服务一块终端表面：它不是「全局文件浏览器」，
// 而是「看这块终端所在目录」的窗口。打开新的会把旧的状态覆盖，
// 因为界面上也只可能有一个弹窗。
function fmState() {
  return store.fileManager
}

// fmLoadSeq 是「当前有效的那次列目录请求」的序号。
//
// 列目录是异步的，而用户点目录的速度可以比远端回答得快：连点 a（大目录、慢）
// 再点 b（小目录、快），b 先返回并渲染，随后 a 才返回 —— 没有这道序号，
// a 的结果会覆盖 b，列表"自己跳回去"，而路径栏显示的却是 a。
//
// 同样重要的是**关闭/切换弹窗**：那时在途请求返回后不该再写任何状态，
// 否则会把上一个任务的目录写进新任务的 termCwd，下次打开就定位错了目录。
// 因此关闭、打开新弹窗都会 ++ 让旧请求作废。
let fmLoadSeq = 0

// openFileManager 打开弹窗并定位到该终端表面的当前目录。
//
// 目录来源优先级：term:cwd 事件（OSC 7，最准）＞ 后端回落的家目录。
// 传空路径给后端是**有意为之**：让「拿不到目录」这件事只有一个处理点
// （后端 ListRemoteDir），而不是前后端各写一份回落逻辑。
export async function openFileManager(hostId, sessionId) {
  const key = bucketKey(hostId, sessionId)
  const fm = fmState()
  // 作废上一个弹窗（如果有）在途的请求：它属于别的终端表面，
  // 回来后会写错 key 的目录。
  fmLoadSeq++
  fm.open = true
  fm.key = key
  fm.hostId = hostId
  fm.sessionId = sessionId
  fm.entries = []
  fm.error = ''
  fm.notice = ''
  // 目录来源优先级：① 已收过的 term:cwd 事件；② 直接问后端该终端的实时 cwd
  // （term:cwd 只在「目录变化」时推一次，前端刷新/重启后本地是空的、或弹窗开得
  // 比 OSC 7 第一次送达还早时，事件靠不住 —— 后端 screen 一直在喂字节，cwd 永远新鲜）。
  let cwd = store.termCwd[key] || ''
  if (!cwd && api() && typeof api().CurrentTermCwd === 'function') {
    try {
      cwd = (await api().CurrentTermCwd(hostId, sessionId)) || ''
    } catch {
      cwd = '' // 后端不可用或终端未开：退回到「让后端列目录时落家目录」
    }
  }
  // await 后端查询期间用户可能已关掉弹窗（或改开了另一块表面）：
  // 再发 ListRemoteDir 只会是一次注定被 seq 守卫丢弃的请求，直接放弃。
  if (!fm.open || fm.key !== key) return
  await loadDir(cwd)
}

export function closeFileManager() {
  // 作废在途请求：关闭之后再写状态会让已经清空的弹窗突然"复活"出内容，
  // 或把 key 已经变空的 termCwd 写脏。
  fmLoadSeq++
  const fm = fmState()
  fm.open = false
  fm.key = ''
  fm.entries = []
  fm.error = ''
  fm.notice = ''
  fm.busy = false
  fm.loading = false
}

// loadDir 列出一个目录并更新弹窗状态。path 传空表示「让后端决定」（家目录）。
//
// 每次调用领一个序号；await 回来后若序号已不是最新，说明期间用户又导航到了
// 别处（或关掉了弹窗），这次结果直接丢弃 —— 不能写进 fm，也不能写 termCwd。
export async function loadDir(path) {
  const fm = fmState()
  if (!api()) {
    fm.error = '后端未就绪'
    return
  }
  const seq = ++fmLoadSeq
  // 记下这次请求是为哪块表面发的：await 期间 fm.key 可能已经变了。
  const key = fm.key
  fm.loading = true
  fm.error = ''
  try {
    const res = await api().ListRemoteDir(fm.hostId, path || '')
    if (seq !== fmLoadSeq) return // 已被更新的导航/关闭取代
    if (res && res.ok) {
      fm.cwd = res.path
      fm.parent = res.parent
      fm.entries = res.entries || []
      // 把实际落到的目录回写进 termCwd：用户可能是在拿不到 OSC 7 时
      // 打开弹窗、被回落到家目录，该记住的是**实际列出的**那个目录。
      // 用请求发出时记下的 key，而不是此刻的 fm.key。
      if (key) store.termCwd[key] = res.path
    } else {
      fm.error = (res && res.error) || '列目录失败'
    }
  } catch (e) {
    if (seq !== fmLoadSeq) return
    fm.error = String(e && e.message ? e.message : e)
  } finally {
    // 只有仍是最新那次请求才复位 loading：否则旧请求的 finally 会把
    // 新请求正在显示的「读取中」提前关掉，界面看起来像卡住。
    if (seq === fmLoadSeq) fm.loading = false
  }
}

export async function fmEnter(entryPath) {
  return loadDir(entryPath)
}

export async function fmGoParent() {
  const fm = fmState()
  if (!fm.parent || fm.parent === fm.cwd) return
  return loadDir(fm.parent)
}

// 注意：这里**没有** fmDelete。
//
// 弹窗只提供「浏览 / 上传 / 下载」，删除要用户走终端自己敲 rm —— 那是一条
// 看得见、留得下审计、且需显式敲命令因而不会误点的路径。注意人工 shell 现在
// 只硬拒绝不可逆破坏（`rm -rf /` 一类），普通 `rm` 会直接执行 —— 正因如此更
// 不能把删除做成列表里一个可误点的按钮（尤其当列表里混着目录时）。
//
// 后端 App.DeleteRemotePath 仍然保留：它是通用能力（终端、自动化都能用），
// 只是这个前端入口不再暴露它。

// fmUpload 把本地 File 内容 base64 后上传到当前目录（二进制安全，带进度）。
//
// 走的是和下载同一条路：整个文件 base64 后一次性交给后端，后端再
// base64 -d 落到远端。所以「二进制」这件事是端到端成立的 —— 上传的不是
// 文本，而是原始字节的 base64 表示，`\x00` 与无效 UTF-8 都能原样过去。
//
// 关于进度：Wails 的绑定调用是**一次性**的（参数就是一个大字符串），
// 中间没有可观测的传输事件。所以进度只能分两段报：
//
//   1. 'reading' —— 本地把文件读成 base64。这一段是真实分块的（FileReader
//      的 progress 事件），也是大文件耗时最长的部分，进度是真的。
//   2. 'sending' —— 调后端。这一段只能显示"不定进度"（UI 上做条纹动画），
//      因为拿不到回调。硬要显示一个假百分比，在大文件上会长时间停着不动，
//      比诚实的"发送中"更让人心慌。
//
// 两段都报**原始字节数**，不是 base64 长度：base64 会膨胀约 33%，
// 直接用它当分子会让"读完了"显示成 75% 那种莫名其妙的数字。
export async function fmUpload(file) {
  const fm = fmState()
  if (!api() || !file) return
  // 上传不设大小上限（按需求去掉）：任何大小的文件都直接分块读、整体交给后端。
  fm.busy = true
  fm.error = ''
  fm.notice = ''
  fm.progress = { phase: 'reading', loaded: 0, total: file.size, name: file.name }
  try {
    const pure = await fileToBase64Paced(file, loaded => {
      // 读阶段：loaded 是已读的原始字节。
      fm.progress = { phase: 'reading', loaded, total: file.size, name: file.name }
    })
    // 读完之后切到"发送中"。这一段的百分比是未知的，UI 会显示不定进度。
    fm.progress = { phase: 'sending', loaded: file.size, total: file.size, name: file.name }
    const res = await api().UploadRemoteFile(fm.hostId, fm.cwd, file.name, pure)
    if (res && res.ok) {
      fm.progress = { phase: 'done', loaded: file.size, total: file.size, name: file.name }
      fm.notice = res.message || t('msg.uploaded', { name: file.name })
      await loadDir(fm.cwd)
    } else {
      fm.error = (res && res.error) || t('msg.uploadFail')
    }
  } catch (e) {
    fm.error = String(e && e.message ? e.message : e)
  } finally {
    // 无论成败都要清掉进度：留着会让下次打开弹窗看到一个停在 60% 的
    // 陈旧进度条，用户以为还在传。
    fm.progress = null
    fm.busy = false
  }
}

// fmDownload 把远端文件下载到用户挑选的本地路径。
//
// 保存走 Wails 原生对话框、传输由后端分块落盘（app_files.go 的
// downloadTo）：前端只发一次请求，之后通过 fm:transfer 事件收进度与速度。
//
// 旧实现把整个文件 base64 塞回前端、用 <a download> 触发浏览器下载 ——
// 在应用窗口里这依赖 WebView 的下载行为，macOS 的 WKWebView 对它支持
// 很差（表现为点了没反应），而且传输期间没有任何进度可看。
export async function fmDownload(entryPath, name) {
  const fm = fmState()
  if (!api()) return
  fm.busy = true
  fm.error = ''
  fm.notice = ''
  try {
    const res = await api().DownloadRemoteFile(fm.hostId, entryPath)
    if (res && res.cancelled) {
      // 用户在保存对话框里按了取消：不是错误，界面什么都不该显示。
      return
    }
    if (res && res.ok) {
      fm.notice = res.message ? t('msg.savedTo', { path: res.message }) : t('msg.downloaded', { name: name || '' })
    } else {
      fm.error = (res && res.error) || t('msg.downloadFail')
    }
  } catch (e) {
    fm.error = String(e && e.message ? e.message : e)
  } finally {
    // 无论成败都要清掉进度：留着会让下次打开弹窗看到一个停在 60% 的
    // 陈旧进度条，用户以为还在传。
    fm.progress = null
    fm.busy = false
  }
}

// fmCancelTransfer 取消当前下载：把进度事件里带回来的 id 还给后端。
// id 为空说明第一块还没传完（事件还没来过），此时没有真正可取消的东西。
export function fmCancelTransfer() {
  const fm = fmState()
  const id = fm.progress && fm.progress.id
  if (id && api()) api().CancelFileTransfer(id)
}

// fmtBytes 把字节数变成人能读的大小，用于错误与提示文案。
export function fmtBytes(n) {
  if (!n || n < 0) return '0 B'
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`
  if (n < 1024 * 1024 * 1024) return `${(n / 1048576).toFixed(1)} MiB`
  return `${(n / 1073741824).toFixed(2)} GiB`
}

// fileToBase64Paced 把本地文件读成**纯** base64（不含 data URL 前缀），
// 并在读取过程中回调已处理的原始字节数。
//
// 为什么不直接用 FileReader.readAsDataURL 的 onload：
//
//  1. 那个结果是带 `data:...;base64,` 前缀的，调用方还得手动切一刀；
//  2. onprogress 给的是**整个 data URL 字符串**的进度，而 base64 比原始
//     字节多约 33%，拿它当进度会让百分比虚高（读到 75% 就显示 100%）；
//  3. 小文件时 onprogress 可能一次都不触发，只有 onload，
//     如果 UI 依赖进度事件，小文件会一直显示 0%。
//
// 所以这里自己按块读：每块是原始字节，先算好总块数再逐块推进，
// 保证**最后一块读完时 loaded 恰好等于 file.size** —— 进度条一定能走到 100%，
// 不会停在 97% 那种地方。
//
// 拼接用字符串而不是先合 Uint8Array 再转：几十 MB 的文件里，前者只是
// 字符串相加，后者会额外复制一份完整缓冲，内存峰值翻倍。
//
// B64_CHUNK 必须是 3 的倍数：每块各自 bytesToBase64 后直接字符串拼接，
// 若某块长度不是 3 的倍数，它的 base64 尾部会补 '='，下一块再接上去就让
// 整串在块边界出现非法字符 —— 后端 base64.DecodeString 报
// "illegal base64 data at input byte <块 base64 长度>"（>1 MiB 的文件必挂）。
// 取 3×349525=1048575（≈1 MiB）既满足约束、又保留原来的分块粒度与进度节奏。
const B64_CHUNK = 1048575 // =3×349525，≈1 MiB；必须 3 的倍数，见上

function fileToBase64Paced(file, onProgress) {
  return new Promise((resolve, reject) => {
    const size = file.size || 0
    if (!size) {
      // 空文件：没有可读的块，直接给空串。给进度回调一个 0/0 的终态，
      // 让 UI 能立刻显示"完成"而不是一直转圈。
      if (onProgress) onProgress(0)
      resolve('')
      return
    }

    const parts = []
    let offset = 0

    // 用 FileReaderSync 不行（它是 Worker 专用的），所以还是异步 FileReader，
    // 只是把"一次读整个文件"换成"顺序读若干块"。
    function readNext() {
      const end = Math.min(offset + B64_CHUNK, size)
      const fr = new FileReader()

      // onloadend 同时覆盖成功与失败：少写一个分支就少一处忘记推进的地方。
      // 真失败时 fr.result 为空、fr.error 非空。
      fr.onerror = () => reject(fr.error || new Error(t('msg.readFileFail')))
      fr.onload = () => {
        const buf = fr.result
        if (!buf) {
          reject(new Error('读取本地文件失败（内容为空）'))
          return
        }
        const bytes = new Uint8Array(buf)
        parts.push(bytesToBase64(bytes))
        offset = end
        // 报原始字节数：这是"文件传了多少"，和用户看到的文件大小同一个单位。
        if (onProgress) onProgress(offset)
        if (offset < size) {
          readNext()
        } else {
          resolve(parts.join(''))
        }
      }
      fr.readAsArrayBuffer(file.slice(offset, end))
    }

    readNext()
  })
}

// 注意：这里**不**再定义一份 bytesToBase64。上面已经有一个导出给终端用的
// 同名函数，实现完全一样（分块 + fromCharCode.apply，避开爆栈与 UCS-2 陷阱）。
// 复制一份出来只会多一处将来会漂移的实现 —— 二进制编码这件事只该有一个源头。

// isPtyDirty 供界面在回合结束时判断要不要补输入提示符。
export function isPtyDirty(hostId, sessionId) {
  return ptyDirty.has(bucketKey(hostId, sessionId))
}

export async function writeTerminal(hostId, sessionId, dataB64) {
  if (!api()) return
  ptyDirty.add(bucketKey(hostId, sessionId))
  try {
    await api().WriteTerminal(hostId, sessionId, dataB64)
  } catch (e) {
    const msg = String(e && e.message ? e.message : e)
    // 终端已经关掉时的写入失败是正常的（用户手快），不打扰用户；
    // 其它错误必须说出来 —— 否则表现为「按键没反应」，最难排查的那种。
    // 「已经关掉」以本地状态为准（closed/idle/error）：本地还认为 open 却
    // 写不进去，说明 PTY 在后端死了而 term:exit 还在路上（或丢了）——
    // 尤其全屏程序（vim/top）冻在最后一帧时，静默等于把「连接断了」伪装成
    // 「终端假死」，用户一个像素都看不到变化，必须报出来。
    const st = store.terms[bucketKey(hostId, sessionId)]
    const knownGone = !!st && (st.status === 'closed' || st.status === 'idle' || st.status === 'error')
    if (!knownGone || !/没有打开|已关闭/.test(msg)) {
      push({ kind: 'error', content: t('msg.termInputFail', { msg }) })
    }
  }
}

export async function resizeTerminal(hostId, sessionId, cols, rows) {
  if (!api()) return
  try {
    await api().ResizeTerminal(hostId, sessionId, cols, rows)
  } catch {
    // 尺寸上报失败刻意不提示：它随每次布局变化都会重发，
    // 弹报错只会刷屏；后端在终端没开时本来就静默忽略。
  }
}

export async function closeTerminal(hostId, sessionId) {
  const st = store.terms[bucketKey(hostId, sessionId)]
  let ok = false
  if (api()) {
    try {
      ok = await api().CloseTerminal(hostId, sessionId)
    } catch {
      ok = false
    }
  }
  // 先置回 idle：关闭会连带触发后端的 term:exit，
  // 而那是「用户自己关的」，不该再弹一条「已断开」让人以为关操作出了问题。
  if (st) {
    st.status = 'idle'
    st.exitReason = ''
    st.error = ''
    // 输入态一并归零。不能等 term:exit：那条事件到达时状态已是 idle，
    // 会被上面的守卫跳过 —— 残留的 cmdActive/tuiActive 会让重开后的按键
    // 一直走透传，本地行编辑「再也回不来」。
    st.cmdActive = false
    st.tuiActive = false
  }
  return ok
}

