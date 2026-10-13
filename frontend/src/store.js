import { reactive } from 'vue'
import * as paint from './termPaint'
import { providerTermBadge } from './providerLogos'

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

export const store = reactive({
  ready: false,
  error: '',
  // auditError 是「审计日志写入失败」的持久告警，与 error 分开：
  // error 表示启动/调用失败，auditError 表示审计轨迹可能已不完整，
  // 后者不会因为一次成功调用而消失，必须用户显式确认。
  auditError: '',
  // 主题：'light' | 'dark'。持久化在 localStorage（纯前端偏好，不进加密库）。
  theme: 'light',
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

  // contextUsage 是最近一次请求的上下文用量（估算值，见 internal/agent/tokenbudget.go）。
  // window 为 0 表示后端认不出该模型的上下文窗口 —— 此时只显示 token 数，不显示占比。
  contextUsage: { tokens: 0, window: 0, messages: 0 },

  // agentTransfer 是**模型发起**的文件传输进度（download_remote_file 工具）。
  //
  // 与 fileManager.progress 分开：后者是人在弹窗里点下载，前者是模型在拉文件，
  // 两者可能同时进行。共用一个字段会让进度条张冠李戴 —— 显示 A 的速度，
  // 实际在传 B。null 表示当前没有进行中的传输。
  agentTransfer: null,

  // maxTransferBytes 是文件管理上传/下载的大小上限，来自 Bootstrap。
  //
  // 与 policy 那几个占位值同一个模式：这里只是 Bootstrap 回来之前的默认值，
  // 真实值以后端为准。前端**不自己定义**这个数 —— 它同时决定后端的校验
  // 与拒绝文案，两处各写一份迟早漂移成「界面放行、后端拒绝」。
  maxTransferBytes: 32 * 1024 * 1024,
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
function targetKey(explicitHostId, explicitSessionId) {
  if (explicitHostId) return bucketKey(explicitHostId, explicitSessionId)
  if (store.activeHostId) return bucketKey(store.activeHostId, store.activeSessionId)
  return bucketKey(store.currentHostId, store.currentSessionId)
}

// targetEntries 是「正在跑那一轮」所在的那条时间线（没有在跑时就是当前会话）。
//
// 与 store.entries 的区别必须分清：后者是**用户此刻在看的**那条，
// 前者是**输出该去**的那条。用户跑到一半切走时两者会不同 ——
// 流式增量、工具状态更新都必须用前者，否则内容会写进用户切过去的那条会话。
function targetEntries() {
  return store.bySession[targetKey()] || NO_ENTRIES
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

// isHumanShellApproval 判断挂起的审批是否是「人敲命令」的审批
// （composer/终端里敲的 shell 行经策略闸门弹出的确认）。
// 它与 agent 工具审批共用 store.pending，但生命周期不同：
// agent 工具审批随回合收场；人敲审批由后端 shellGate 单独等待，
// 只属于它自己（批准 / 拒绝 / 5 分钟超时 / 中断）。
function isHumanShellApproval(p) {
  return !!(p && p.name === 'shell')
}

function upsertTool(v) {
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
  let t = 'light'
  try {
    t = localStorage.getItem(THEME_KEY) === 'dark' ? 'dark' : 'light'
  } catch { /* localStorage 不可用时用默认值 */ }
  store.theme = t
  document.documentElement.dataset.theme = t
}

export function setTheme(t) {
  if (t !== 'light' && t !== 'dark') return
  store.theme = t
  document.documentElement.dataset.theme = t
  try { localStorage.setItem(THEME_KEY, t) } catch { /* 忽略持久化失败 */ }
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
    if (info.maxTransferBytes > 0) store.maxTransferBytes = info.maxTransferBytes
    if (!store.currentHostId && store.hosts.length) store.currentHostId = store.hosts[0].id
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
  let list
  try {
    list = (await api().ListSessions(hostId)) || []
  } catch (e) {
    // 列表拉不到不该把界面清空：保留上一次的结果，用户至少还能继续用。
    // 但要说出来 —— 否则他会以为自己的会话全没了。
    push({ kind: 'error', content: `读取会话列表失败：${e && e.message ? e.message : e}` }, hostId)
    return
  }
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
function liveAssistant(step) {
  if (step === undefined || step === null) return undefined
  // 认的是「正在跑那一轮」的时间线，不是当前选中的会话 ——
  // 跑到一半切了主机或会话，流式消息仍在原来那条时间线上。
  return targetEntries().find(e => e.kind === 'assistant' && e.stream === step)
}

// bindEvents 注册所有后端事件。必须可安全重复调用：
// Wails v2 的 EventsOn 对同名事件是**追加**监听器而不是替换，
// 热重载或组件重新挂载后再绑一遍，每条事件就会被处理 N 次 ——
// 表现是终端里「ls 变成 lllsss」、对话消息重复三份。
// 因此每次先 EventsOff 清掉同名旧监听，再重新注册：调用多少次都只挂一份。
const EV_NAMES = [
  'agent:delta', 'agent:message', 'agent:reasoning', 'agent:tool', 'agent:toolResult',
  'agent:injection', 'agent:approval', 'approval:expired', 'agent:status', 'agent:error',
  'audit:error', 'agent:done', 'term:data', 'term:exit', 'term:tui', 'term:cmd', 'term:cwd', 'agent:snapshot',
  'fm:transfer', 'agent:transfer'
]

// surfaceWrite 返回往某块终端表面（按 host+session 键）写字符串的落点（就是它
// xterm 实例的 write）。没有实例（还没打开/已卸载）时返回 undefined —— termPaint 的
// 每个函数都对 undefined 落点安全 no-op。
function surfaceWrite(key) {
  return termSinks.get(key)
}

// 事件该画到哪片表面：正在跑那一轮的任务（activeHost+activeSession），
// 其次当前任务。与 targetKey 同源 —— 用户中途切任务也不会把输出画错地方。
function paintHost() {
  return targetKey()
}

// ---- 思考占位（「……」动画）----

// currentModelBadge 取「这一轮实际在用的模型方案」的终端徽标信息。
//
// 优先用 selectedProfileId（下拉框刚选中的那套，与请求实际带的是同一个值 ——
// 见 ask），回退到后端标记 active 的那套。拿不到任何方案时返回 null，
// 让 paintAssistantHeader 走「綠点」兜底。
function currentModelBadge() {
  const id = store.selectedProfileId
  const p = store.llmProfiles.find(x => x.id === id) ||
    store.llmProfiles.find(x => x.active) || null
  if (!p) return null
  try {
    return providerTermBadge(p)
  } catch {
    return null
  }
}

// thinkingDots 记录每块表面当前的思考占位状态：{ timer, frame, width }。
// 用非响应式 Map（存的是定时器与函数，进响应式毫无意义）。
const thinkingDots = new Map()

const THINKING_INTERVAL_MS = 400
// 点数在 0..4 之间循环：`Working` → `Working.` → `Working..` → `Working...`
// → `Working....` → 回到 `Working`。循环才是「还在转」的通用符号，也不至于
// 把行撑长。
const THINKING_MAX_DOTS = 4

// startThinking 在表面上开始滚动 `Working....` 占位。幂等：已在跑就不重开
// （后端每个 step 都会重发 status=thinking，不能因此叠出多个定时器）。
function startThinking(key) {
  if (!key) return
  if (thinkingDots.has(key)) return
  const w = surfaceWrite(key)
  if (!w) return
  // next 记录「下一个要画的点数」，0..THINKING_MAX_DOTS 循环。
  const state = { timer: null, next: 0, width: 0 }
  const tick = () => {
    const frame = state.next
    state.next = frame >= THINKING_MAX_DOTS ? 0 : frame + 1
    state.width = paint.paintThinking(w, frame, state.width) || state.width
  }
  state.timer = setInterval(tick, THINKING_INTERVAL_MS)
  thinkingDots.set(key, state)
  tick() // 立刻画第一帧，别等一个周期（否则提问后有一瞬空白）
}

// stopThinking 停掉并擦除某块表面的思考占位。
function stopThinking(key) {
  const state = thinkingDots.get(key)
  if (!state) return
  clearInterval(state.timer)
  thinkingDots.delete(key)
  paint.clearThinking(surfaceWrite(key), state.width)
}

export function bindEvents() {
  const r = rt()
  if (!r) return
  // 测试用的运行时 mock 可能没有 EventsOff，此时跳过清理直接注册。
  if (typeof r.EventsOff === 'function') r.EventsOff(...EV_NAMES)

  // 流式增量：逐字累加到「活」消息上
  r.EventsOn('agent:delta', d => {
    const host = paintHost()
    const w = surfaceWrite(host)
    // 正文开始流出：思考占位使命完成，先擦掉再画抬头/正文，避免两者叠在一行。
    stopThinking(host)
    const live = liveAssistant(d.step)
    if (live) {
      live.content += d.text
      // 思考已先画到表面上：正文另起一行再落一个●抬头，别和思考挤在同一行
      if (!live.contentPainted) {
        live.contentPainted = true
        if (live.reasoningPainted) {
          paint.paintAssistantEnd(w)
          paint.paintAssistantHeader(w, live.badge)
        }
      }
      paint.paintAssistantDelta(w, d.text)
      return
    }
    const badge = currentModelBadge()
    push({ kind: 'assistant', content: d.text, stream: d.step, streaming: true, badge })
    paint.paintAssistantHeader(w, badge)
    paint.paintAssistantDelta(w, d.text)
  })

  // 推理型模型的思考增量：挂到同一条「活」消息的思考区（归档视图里可折叠），
  // 同时以弱化灰字画进终端表面——那是主视图，思考过程必须当场可见。
  // 思考先于正文到达，往往还要先由它建出这条消息。
  r.EventsOn('agent:reasoning', d => {
    const host = paintHost()
    const w = surfaceWrite(host)
    // 思考内容开始到达：占位的「......」该让位给真正的思考文本了。
    stopThinking(host)
    let live = liveAssistant(d.step)
    if (!live) {
      const badge = currentModelBadge()
      push({ kind: 'assistant', content: '', stream: d.step, streaming: true, reasoning: '', reasoningOpen: true, badge })
      live = liveAssistant(d.step)
      paint.paintAssistantHeader(w, badge)
    }
    live.reasoning = (live.reasoning || '') + d.text
    live.reasoningOpen = true
    live.reasoningPainted = true
    paint.paintAssistantReasoning(w, d.text)
  })

  r.EventsOn('agent:message', m => {
    const host = paintHost()
    const w = surfaceWrite(host)
    if (m.role !== 'assistant') {
      push({ kind: 'user', content: m.content })
      paint.paintUser(w, m.content)
      return
    }
    stopThinking(host)
    const live = liveAssistant(m.step)
    if (live) {
      // 定稿：用后端给的完整内容覆盖一次，纠正任何在传输中丢掉的尾巴。
      // 表面上 delta 已经流式画过了，这里只补一个换行收尾，不重画全文。
      live.content = m.content
      // 记下这条回答是哪个模型给的：界面据此标注，让「选中的模型」与
      // 「实际回答的模型」对得上，不必靠模型自述去猜。
      if (m.model) live.model = m.model
      live.streaming = false
      // 回答已开始：默认收起思考区，长思考不该一直把正文顶出屏幕，需要时可手动展开
      live.reasoningOpen = false
      paint.paintAssistantEnd(w)
      return
    }
    const badge = m.model ? providerTermBadge({ model: m.model, name: m.model }) : currentModelBadge()
    push({ kind: 'assistant', content: m.content, model: m.model, badge })
    paint.paintAssistantHeader(w, badge)
    paint.paintAssistantDelta(w, m.content)
    paint.paintAssistantEnd(w)
  })

  r.EventsOn('agent:tool', v => {
    // 模型已经产出工具调用 = 不再是「还在思考」：先收掉滚动占位。
    // **实时**画一行（非 shell 风格），让人当场看到 agent 在调什么工具。
    stopThinking(paintHost())
    upsertTool(v)
    paint.paintToolCall(surfaceWrite(paintHost()), v)
  })

  r.EventsOn('agent:toolResult', res => {
    // 同上：工具结果是一大段会往表面写的输出，必须先结束占位，避免两者交错。
    stopThinking(paintHost())
    push({
      kind: 'result',
      toolId: res.id,
      exitCode: res.exitCode,
      content: res.content,
      redacted: res.redacted || 0,
      injection: res.injection || []
    })
    // 一行紧凑状态（完成 / 失败）；输出与详情进「对话归档」。
    // TTY 交接还要把命令写进常驻终端 —— 那是「把交互式程序交给用户操作」。
    paint.paintToolResultLine(surfaceWrite(paintHost()), res)
    if (res.tty && res.command && res.hostId) {
      writeTerminal(res.hostId, res.sessionId, textToBase64(res.command + '\n'))
    }
  })

  // agent:context 是「本次请求的上下文用量」：后端每步请求前推一次
  // （见 agent.EvContext）。放进 store 而不是组件本地 state ——
  // 归档视图将来也要显示它，而事件只应有一个消费者入口（bindEvents）。
  r.EventsOn('agent:context', d => {
    if (!d) return
    store.contextUsage.tokens = d.tokens || 0
    store.contextUsage.window = d.window || 0
    store.contextUsage.messages = d.messages || 0
  })

  r.EventsOn('agent:injection', e => {
    stopThinking(paintHost())
    push({
      kind: 'injection',
      findings: e.findings || [],
      command: e.command || '',
      hostName: e.hostName || ''
    })
    paint.paintInjection(surfaceWrite(paintHost()), e)
  })

  r.EventsOn('agent:approval', v => {
    stopThinking(paintHost())
    store.pending = v
    // 审批条/弹窗是交互入口；表面上只补一行注记说明「为什么停住了」。
    paint.paintApproval(surfaceWrite(paintHost()), v)
  })

  // 审批失效（后端超时或被中断）。必须按 id 比对再清 —— 事件可能在
  // 「上一张已失效、下一张已经来了」之后才被派发，无条件清会把新的一条也抹掉。
  r.EventsOn('approval:expired', d => {
    const id = d && d.id
    if (!id || !store.pending || store.pending.id !== id) return
    store.pending = null
    const reason = (d && d.reason) || '已失效'
    push({ kind: 'system', content: `审批${reason}，Agent 不会执行该命令。` })
  })

  r.EventsOn('agent:status', s => {
    store.running = s.status === 'thinking'
    // 后端发出 status=thinking 即「用户提问已被受理、模型开始思考」——
    // 这正是要显示「......」的时刻（见需求：用户发完自然语言就转圈）。
    if (store.running) startThinking(paintHost())
  })

  r.EventsOn('agent:error', e => {
    store.running = false
    stopThinking(paintHost())
    // 出错即本轮已终止，agent 自己的工具审批条要一并清掉，否则输入框锁死。
    // 但人敲命令的审批（name=shell）不属于 agent 回合：它由 shellGate 在后端
    // 单独等待（见 app_shell.go），清掉会让审批条凭空消失、后端还在等
    // （最长 5 分钟），期间回车提交全部排队 —— 表现为「控制台无法输入」。
    if (!isHumanShellApproval(store.pending)) store.pending = null
    freezeStreams()
    push({ kind: 'error', content: e.message })
    paint.paintError(surfaceWrite(paintHost()), e.message)
  })

  // 审计写入失败。这不是「本次任务失败」，而是「审计轨迹可能已不完整」，
  // 因此不塞进对话流（会被后续输出冲走），而是挂在 store 上做持久横幅，
  // 直到用户显式关掉 —— 用户必须知道审计日志不再可信。
  r.EventsOn('audit:error', e => {
    store.auditError = String(e && e.message ? e.message : e)
    push({ kind: 'error', content: store.auditError })
  })

  r.EventsOn('agent:done', () => {
    store.running = false
    stopThinking(paintHost())
    // 同 agent:error：人敲命令的审批（name=shell）不随 agent 回合收场。
    // agent 自己的工具审批在回合结束时必须清掉（回合已终止，批准无意义）。
    if (!isHumanShellApproval(store.pending)) store.pending = null
    freezeStreams()
  })

  // 交互终端的输出。解成字节后直接交给 xterm ——
  // 让它的 UTF-8 解码器去处理跨块的续字节（这正是它存在的意义）。
  r.EventsOn('term:data', d => {
    const k = bucketKey(d && d.hostId, d && d.sessionId)
    const sink = termSinks.get(k)
    // 没有落点就直接丢掉：那是「界面还没建好实例」的那一小段，
    // 而终端只在用户真的打开它之后才会有输出。
    if (!sink) return
    // 远端真的吐字节了：光标即将被这段输出推走，思考占位不再安全 —— 立即收掉。
    // 不先收的话，占位那一行会被 PTY 输出顶到别处、`\r` 再也回不去，残留成幽灵
    // `Working`（用户截图里 `Working..` 和 shell 提示符挤在同一行就是这个）。
    stopThinking(k)
    try {
      sink(base64ToBytes(d.data))
      // 有人等着「这轮输出画完」：续期静默计时器（见 afterTermQuiet）。
      armQuietTimer(k)
    } catch (e) {
      push({ kind: 'error', content: '终端输出解码失败：' + String(e && e.message ? e.message : e) })
    }
  })

  // 远端是否被全屏程序接管（vim/top/htop）。后端只在状态**变化**时发。
  // 前端拿它给「终端内直接输入」让路：接管期间整体透传按键，绝不抢键。
  r.EventsOn('term:tui', d => {
    const st = termState(d && d.hostId, d && d.sessionId)
    if (st) st.tuiActive = !!(d && d.active)
  })

  // 远端是否有一条命令正在跑（OSC 133;C..D 之间）。后端只在状态**变化**时发。
  //
  // 前端据此把按键让给正在等输入的程序：bt 菜单、read、REPL、sudo 口令这类
  // **非全屏**的交互程序没有别的让路信号，只能靠这一档 —— 漏掉时用户答菜单
  // 的 `14` 会被本地行编辑按「数字编号回复」路由给 LLM（截图 bug：表面画出
  // `Ai-Shell > 14` + `Working...`，而菜单一直等不到输入）。
  r.EventsOn('term:cmd', d => {
    const st = termState(d && d.hostId, d && d.sessionId)
    if (st) st.cmdActive = !!(d && d.active)
  })

  // 远端 shell 的当前目录（来自 OSC 7）。后端只在目录**变化**时发。
  //
  // 这是文件管理弹窗「默认定位到当前控制台目录」的数据来源：目录状态住在
  // 远端那条 shell 里，前端只看得见渲染好的字符，猜不出用户 cd 去了哪。
  // 按 host+session 分别记 —— 每块终端表面可能停在不同目录，混用一个
  // 全局值会让「切到另一条任务再打开文件管理」定位到别人的目录。
  r.EventsOn('term:cwd', d => {
    const k = bucketKey(d && d.hostId, d && d.sessionId)
    if (!d || !d.cwd) return
    store.termCwd[k] = d.cwd
    // 弹窗正开着且看的就是这块表面：跟着目录走，否则用户在终端里 cd
    // 之后弹窗还停在旧目录，看起来像没刷新。
    if (store.fileManager.open && store.fileManager.key === k) {
      store.fileManager.cwd = d.cwd
    }
  })

  // 文件传输进度（下载）。后端每落盘一块就推一次（app_files.go 的
  // downloadTo → a.emit），这里把它翻译成 fm.progress 的 downloading 相位，
  // 与上传共用同一个进度条渲染。上传的进度仍由前端自己驱动（读本地文件），
  // 走不到这条事件。
  r.EventsOn('fm:transfer', d => {
    if (!d || d.kind !== 'download') return
    store.fileManager.progress = {
      phase: 'downloading',
      id: d.id,
      loaded: d.done,
      total: d.total,
      name: d.name,
      bps: d.bps
    }
  })

  // agent 侧文件传输进度（download_remote_file 工具）。
  //
  // 与上面的 fm:transfer 刻意分开存：那条是「人在弹窗里点下载」，
  // 这条是「模型发起的传输」。两者可能同时进行（用户下载一个包，
  // 模型同时把一份日志拉回来），共用一个进度字段会让进度条
  // 张冠李戴 —— 显示 A 的速度，实际在传 B。
  //
  // 落在 store 而不是组件本地 state：控制台将来要在跑长传输时
  // 显示「正在拉取 xxx」，而事件只应有一个消费者入口（bindEvents）。
  r.EventsOn('agent:transfer', d => {
    if (!d || d.kind !== 'download') return
    store.agentTransfer = {
      remote: d.remote,
      name: d.name,
      dest: d.dest,
      done: d.done,
      total: d.total,
      bps: d.bps
    }
  })

  r.EventsOn('term:exit', d => {
    const st = store.terms[bucketKey(d && d.hostId, d && d.sessionId)]
    // 只在「开着」的时候才认这条退出：用户自己点关闭时状态已经置回 idle，
    // 那一次退出不该再弹一条消息。
    if (!st || (st.status !== 'open' && st.status !== 'opening')) return
    st.status = 'closed'
    st.exitReason = (d && d.reason) || '终端已结束'
    // 终端已经没了，输入态必须跟着归零：后端此后不再有任何屏幕事件，
    // 残留的 cmdActive/tuiActive 会把按键继续透传给一条死掉的通道
    // （表现为「敲什么都没反应、也不报错」）。
    st.cmdActive = false
    st.tuiActive = false
  })

  // 终端屏幕快照：模型看的那份净化后文本，原样落一条给人看 ——
  // 人和模型看到同一份东西，审计才对得上。落点用后端给的
  // hostId/sessionId 显式指定：快照属于开块那一轮，不属于「此刻在看」的会话。
  r.EventsOn('agent:snapshot', d => {
    stopThinking((d && d.hostId) ? bucketKey(d.hostId, d.sessionId) : paintHost())
    push({
      kind: 'snapshot',
      termId: (d && d.termId) || '',
      text: (d && d.text) || '',
      redacted: (d && d.redacted) || 0,
      injection: (d && d.injection) || [],
      final: !!(d && d.final)
    }, (d && d.hostId) || undefined, (d && d.sessionId) || undefined)
    // 表面上只落一行注记，不重画那一帧：定格画面本来就在终端的 scrollback 里，
    // 重画反而会打乱滚动位置。注记让人和审计对得上「这一屏已进模型上下文」。
    // 落点按后端给的 hostId/sessionId 组合成表面键：快照属于开块那一轮的任务，
    // 不属于「此刻在看」的会话；那塊表面已不在（切走已关）时 sink 为空，自然 no-op。
    const snapKey = (d && d.hostId) ? bucketKey(d.hostId, d.sessionId) : paintHost()
    paint.paintSnapshotNote(surfaceWrite(snapKey), !!(d && d.final))
  })
}

// freezeStreams 收尾所有还在「流式中」的消息。
// 中断或出错时流不会再有定稿消息，不清掉标记就会留下一个永远闪烁的光标。
function freezeStreams() {
  for (const e of targetEntries()) {
    if (e.streaming) e.streaming = false
  }
}

export async function ask(prompt) {
  if (!store.currentHostId) {
    push({ kind: 'error', content: '请先在「主机管理」里添加一台主机。' })
    return
  }
  // 防重入：这个不变量由 store 自己守住，不指望每个调用方都记得先查 running。
  // 否则第二次调用会被后端以「已有会话正在运行」拒绝，给用户弹一个莫名其妙的报错。
  // 不能静默 return：那会让用户以为「发出去没反应」，必须给出可见提示。
  if (store.running || store.busy) {
    push({ kind: 'system', content: '上一轮任务仍在执行中，请等待完成或点「中断」。' })
    return
  }
  // 记下这一轮属于哪条时间线。后续所有事件都按它归档 ——
  // 用户中途切了主机或会话也不会把输出写错地方。
  store.activeHostId = store.currentHostId
  store.activeSessionId = store.currentSessionId
  // 本轮 PTY 写入标记从零计数：上一轮结束时已消费，期间的零星写入
  // （如空回车让 shell 打新提示符）不代表「本轮 shell 自己打印过提示符」。
  ptyDirty.delete(bucketKey(store.activeHostId, store.activeSessionId))
  store.contextUsage.tokens = 0
  store.contextUsage.window = 0
  store.contextUsage.messages = 0
  store.running = true
  try {
    // 带上「这一轮用哪套方案」：切换随请求即时生效，不再依赖全局激活指针。
    await api().Ask(store.currentHostId, store.currentSessionId, prompt, store.selectedProfileId)
  } catch (e) {
    store.running = false
    push({ kind: 'error', content: String(e) })
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
  try {
    const res = await api().ClearSession(hostId, sessionId)
    if (res && res.busy) {
      push({
        kind: 'error',
        content: '有会话正在运行，无法清空上下文。等这一轮结束或点「中断」后再试。'
      }, hostId, sessionId)
      return
    }
    const n = (res && res.cleared) || 0
  // 上下文已清空：用量归零，否则工具条上还挂着上一轮的「87%」——
  // 用户会以为清空没生效。
  store.contextUsage.tokens = 0
  store.contextUsage.window = 0
  store.contextUsage.messages = 0
    push({
      kind: 'system',
      content: n > 0
        ? `已清空这条会话的 ${n} 轮对话上下文，LLM 不再记得之前的对话。` +
          '界面上的文字保留 —— 清的是模型的记忆，不是屏幕上的记录。'
        : '这条会话本来就没有对话上下文。'
    }, hostId, sessionId)
  } catch (e) {
    push({
      kind: 'error',
      content: `清空上下文失败：${e && e.message ? e.message : e}`
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
  if (store.compacting) return
  store.compacting = true
  try {
    const res = await api().CompactSession(hostId, sessionId)
    // 后端已经把话说全了（含被拒绝的原因），照搬即可 ——
    // 在前端重写一遍措辞，两处迟早会漂移。
    push({ kind: 'system', content: (res && res.message) || '压缩完成。' }, hostId, sessionId)
  } catch (e) {
    push({
      kind: 'error',
      content: `压缩上下文失败：${e && e.message ? e.message : e}。原历史未被改动。`
    }, hostId, sessionId)
  } finally {
    store.compacting = false
  }
}

// ---- 会话的增删改查 ----

// selectSession 切换当前会话。
//
// 刻意**不**在运行中禁止切换：事件按 activeSessionId 归档（见 targetKey），
// 切过去之后这一轮的输出仍然落在原来那条会话上，不会写错地方 ——
// 与切主机的行为一致。禁掉反而多一个「为什么点不动」的解释负担。
export function selectSession(sessionId) {
  store.currentSessionId = sessionId || ''
  // 告诉后端「这台主机现在看着哪条会话」：之后常驻终端定格的快照要落到这条上。
  reportActiveSession(store.currentHostId, store.currentSessionId)
}

// createSession 在当前主机下新建一条会话，并立刻切过去。
//
// 新建后必须切过去：用户的意图就是「开一段新对话」，
// 建完还停在旧会话上，他会以为新建失败了。
export async function createSession(name) {
  const hostId = store.currentHostId
  if (!hostId) {
    push({ kind: 'error', content: '请先选择一台主机。' })
    return
  }
  try {
    const info = await api().CreateSession(hostId, name)
    await refreshSessions(hostId)
    store.currentSessionId = info.id
    push({ kind: 'system', content: `已新建任务「${info.name}」。` }, hostId, info.id)
  } catch (e) {
    push({
      kind: 'error',
      content: `新建任务失败：${e && e.message ? e.message : e}`
    }, hostId)
  }
}

// renameSession 改会话名。改名**不**影响会话的身份 —— 它是由 ID 认定的。
export async function renameSession(sessionId, name) {
  const hostId = store.currentHostId
  if (!hostId) {
    push({ kind: 'error', content: '请先选择一台主机。' })
    return
  }
  try {
    await api().RenameSession(hostId, sessionId, name)
    await refreshSessions(hostId)
    push({ kind: 'system', content: `任务已改名为「${name}」。` }, hostId, sessionId)
  } catch (e) {
    push({
      kind: 'error',
      content: `改名失败：${e && e.message ? e.message : e}`
    }, hostId, sessionId)
  }
}

// deleteSession 删掉一条会话。
//
// 后端会拒绝删除默认会话（它是「不带会话 ID 跑一轮」的落点），
// 这里不预先判一遍 —— 让后端说那句话，前端只管把原因显示出来。
// 两处各判一次的话，规则迟早会分叉成「界面允许删、后端拒绝」。
export async function deleteSession(sessionId) {
  const hostId = store.currentHostId
  if (!hostId) {
    push({ kind: 'error', content: '请先选择一台主机。' })
    return
  }
  try {
    await api().DeleteSession(hostId, sessionId)
    // 删掉的正好是当前选中的那条：先切回默认会话，再刷新列表。
    // 顺序不能反 —— 先刷新的话，refreshSessions 里的兜底也会把它置空，
    // 结果相同但意图看不出来。
    if (store.currentSessionId === sessionId) store.currentSessionId = ''
    await refreshSessions(hostId)
    push({ kind: 'system', content: '已删除该会话。' }, hostId)
  } catch (e) {
    push({
      kind: 'error',
      content: `删除会话失败：${e && e.message ? e.message : e}`
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
    push({ kind: 'error', content: `审批提交失败：${e && e.message ? e.message : e}` })
    return
  }

  // 后端 Resolve() 对已超时（默认 5 分钟）或已中断的审批 id 返回 false，且不抛错。
  // 这种情况下命令根本不会执行，所以绝不能显示「已批准执行」——那是在骗用户。
  if (!applied) {
    push({
      kind: 'error',
      content: '这次审批已失效（可能已超时或被中断），Agent 不会执行该命令。'
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
    const msg = `切换模型失败：${e && e.message ? e.message : e}`
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

export async function stop() {
  try {
    await api().Stop()
  } catch (e) {
    push({ kind: 'error', content: `中断失败：${e && e.message ? e.message : e}` })
  } finally {
    // 无论后端是否响应，本地都必须回到可交互状态，否则界面会永久卡在「运行中」。
    store.running = false
    store.busy = false
    store.pending = null
    freezeStreams()
  }
}

// ---- 终端输出静默检测 ----

// afterTermQuiet 在「某块终端的输出安静下来」之后调 fn。
//
// 为什么需要它：把一条命令写进 PTY 是**不等它跑完**的 —— 后端写完就返回，
// 回显、命令输出、新提示符都在之后陆续从网络上飘回来。调用方若拿到返回值就
// 往屏幕上画东西，那东西会插在命令输出**前面**，看起来就像「命令没执行」：
//
//	root@host:~$   提示：这段输出可交给 LLM 分析。   ← 提示抢先画了
//	install.sh mysql.php tttt.text      ← 真正的输出被挤到下面
//	root@host:~$
//
// 所以凡是「命令跑完之后才该出现」的内容（LLM 提示、裁决说明），都要挂在这里。
// 判据是输出流静默：不再有新字节到达即认为命令画完了。
//
// 静默时长取 400ms：本地环回上 ls 的输出常在几十毫秒内结束，400ms 不会让人
// 察觉延迟；而慢命令的字节流会不断续期，不会被误判成「已跑完」。
const TERM_QUIET_MS = 400
const quietTimers = new Map()   // 表面键 → 静默计时器
const quietWaiters = new Map()  // 表面键 → 等到静默后要执行的回调

function armQuietTimer(key) {
  const prev = quietTimers.get(key)
  if (prev) clearTimeout(prev)
  const timer = setTimeout(() => {
    quietTimers.delete(key)
    const fn = quietWaiters.get(key)
    if (fn) {
      quietWaiters.delete(key)
      fn()
    }
  }, TERM_QUIET_MS)
  quietTimers.set(key, timer)
}

// afterTermQuiet 登记「静默后执行」。同一块表面同时只保留一个待执行回调：
// 用户连敲两条命令时，前一条的提示没必要还追着画（屏幕早滚过去了）。
export function afterTermQuiet(hostId, sessionId, fn) {
  const key = bucketKey(hostId, sessionId)
  quietWaiters.set(key, fn)
  armQuietTimer(key)
}

// runShellInTerminal：composer 里人敲的 shell 命令，走**常驻终端**通道 ——
// 策略闸门裁决（高危弹审批）后把这一行写进常驻 PTY，提示符回显、输出、
// top/vim 接管全发生在同一片终端表面上，与人亲手在键盘上敲完全同形。
//
// 与已退役的 runShell（有界 Exec，输出喂 LLM）的差别：这里**不捕获**输出文本，
// 模型看到的是命令结束时定格的屏幕快照（见 agent:snapshot）。
// busy 覆盖「等审批 + 写入」整段，避免用户连敲把后端打成并发。
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
    push({ kind: 'error', content: '命令执行失败：' + msg })
    return { status: 'error', error: msg }
  } finally {
    store.busy = false
    // 审批条由 agent:approval 挂上；命令结束后必须清掉，
    // 否则拒绝/超时返回后界面还挂着一张过期审批。
    store.pending = null
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

// termSinks 是「终端表面（host+session 键）→ 往它的 xterm 实例里写字节」的登记表。
//
// 事件是后端推的，而 xterm 实例住在组件里，两者需要一个会合点。
// 刻意**不放进 reactive**：它存的是函数，被 Vue 代理一遍没有任何意义，
// 反而会让每次写入都走一遍响应式系统。
const termSinks = new Map()

// llmHintListeners：每块表面一个「画 LLM 提示占位」的回调，由 ConsolePanel
// 在创建实例时登记。store 的 runShellInTerminal 在命令画完后触发它 ——
// 提示占位的起点列只有组件读得到（xterm 光标位置），所以画这件事归组件。
const llmHintListeners = new Map()

// registerLLMHint 由终端组件登记回调；返回反登记函数。
export function registerLLMHint(hostId, sessionId, fn) {
  const key = bucketKey(hostId, sessionId)
  llmHintListeners.set(key, fn)
  return () => { if (llmHintListeners.get(key) === fn) llmHintListeners.delete(key) }
}

// fireLLMHint 触发占位提示绘制（内部用；组件不在场时静默跳过）。
function fireLLMHint(hostId, sessionId) {
  const fn = llmHintListeners.get(bucketKey(hostId, sessionId))
  if (fn) { try { fn() } catch { /* 绘制失败不影响命令结果展示 */ } }
}

// registerTermSink 由终端组件在**创建实例之后、调用 OpenTerminal 之前**登记。
// 顺序不能反：远端 shell 一启动就会打印提示符，登记晚了那一段就落进虚空了。
export function registerTermSink(hostId, sessionId, fn) {
  termSinks.set(bucketKey(hostId, sessionId), fn)
}

// unregisterTermSink 只在当前登记的仍是自己时才删。
// 同一块表面可能被重新挂载，晚挂上的那个不该被早先那次卸载顺手清掉。
export function unregisterTermSink(hostId, sessionId, fn) {
  const key = bucketKey(hostId, sessionId)
  if (termSinks.get(key) === fn) termSinks.delete(key)
}

export function termState(hostId, sessionId) {
  const key = bucketKey(hostId, sessionId)
  if (!store.terms[key]) {
    // tuiActive / cmdActive 是两块「输入让路」信号（全屏接管 / 命令运行中），
    // 由 term:tui / term:cmd 事件更新。初值必须在场：读到它们的地方不该
    // 依赖 undefined 的 falsy 语义。
    store.terms[key] = {
      status: 'idle', error: '', exitReason: '',
      tuiActive: false, cmdActive: false
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
// 看得见、留得下历史、也能被审批闸门拦住的路径。把不可逆的远端删除做成
// 列表里的一个按钮，误点成本太高（尤其是当列表里混着目录时）。
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
  // 上限以后端下发的为准（见 store.maxTransferBytes）。
  const limit = store.maxTransferBytes
  if (limit > 0 && file.size > limit) {
    fm.error = `文件超过上限（${fmtBytes(limit)}），当前 ${fmtBytes(file.size)}`
    return
  }
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
      fm.notice = res.message || `已上传 ${file.name}`
      await loadDir(fm.cwd)
    } else {
      fm.error = (res && res.error) || '上传失败'
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
      fm.notice = res.message ? `已保存到 ${res.message}` : `已下载 ${name || ''}`
    } else {
      fm.error = (res && res.error) || '下载失败'
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
const B64_CHUNK = 1024 * 1024 // 每次读 1 MiB 原始字节

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
      fr.onerror = () => reject(fr.error || new Error('读取本地文件失败'))
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
    if (!/没有打开|已关闭/.test(msg)) {
      push({ kind: 'error', content: '终端输入失败：' + msg })
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

