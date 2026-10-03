import { reactive } from 'vue'
import * as paint from './termPaint'

const api = () => window.go?.main?.App
const rt = () => window.runtime

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
    store.policy = info.policy || store.policy
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
  'agent:injection', 'agent:approval', 'agent:status', 'agent:error',
  'audit:error', 'agent:done', 'term:data', 'term:exit', 'term:tui', 'agent:snapshot'
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

export function bindEvents() {
  const r = rt()
  if (!r) return
  // 测试用的运行时 mock 可能没有 EventsOff，此时跳过清理直接注册。
  if (typeof r.EventsOff === 'function') r.EventsOff(...EV_NAMES)

  // 流式增量：逐字累加到「活」消息上
  r.EventsOn('agent:delta', d => {
    const w = surfaceWrite(paintHost())
    const live = liveAssistant(d.step)
    if (live) {
      live.content += d.text
      // 思考已先画到表面上：正文另起一行再落一个●抬头，别和思考挤在同一行
      if (!live.contentPainted) {
        live.contentPainted = true
        if (live.reasoningPainted) {
          paint.paintAssistantEnd(w)
          paint.paintAssistantHeader(w)
        }
      }
      paint.paintAssistantDelta(w, d.text)
      return
    }
    push({ kind: 'assistant', content: d.text, stream: d.step, streaming: true })
    paint.paintAssistantHeader(w)
    paint.paintAssistantDelta(w, d.text)
  })

  // 推理型模型的思考增量：挂到同一条「活」消息的思考区（归档视图里可折叠），
  // 同时以弱化灰字画进终端表面——那是主视图，思考过程必须当场可见。
  // 思考先于正文到达，往往还要先由它建出这条消息。
  r.EventsOn('agent:reasoning', d => {
    const w = surfaceWrite(paintHost())
    let live = liveAssistant(d.step)
    if (!live) {
      push({ kind: 'assistant', content: '', stream: d.step, streaming: true, reasoning: '', reasoningOpen: true })
      live = liveAssistant(d.step)
      paint.paintAssistantHeader(w)
    }
    live.reasoning = (live.reasoning || '') + d.text
    live.reasoningOpen = true
    live.reasoningPainted = true
    paint.paintAssistantReasoning(w, d.text)
  })

  r.EventsOn('agent:message', m => {
    const w = surfaceWrite(paintHost())
    if (m.role !== 'assistant') {
      push({ kind: 'user', content: m.content })
      paint.paintUser(w, m.content)
      return
    }
    const live = liveAssistant(m.step)
    if (live) {
      // 定稿：用后端给的完整内容覆盖一次，纠正任何在传输中丢掉的尾巴。
      // 表面上 delta 已经流式画过了，这里只补一个换行收尾，不重画全文。
      live.content = m.content
      live.streaming = false
      // 回答已开始：默认收起思考区，长思考不该一直把正文顶出屏幕，需要时可手动展开
      live.reasoningOpen = false
      paint.paintAssistantEnd(w)
      return
    }
    push({ kind: 'assistant', content: m.content })
    paint.paintAssistantHeader(w)
    paint.paintAssistantDelta(w, m.content)
    paint.paintAssistantEnd(w)
  })

  r.EventsOn('agent:tool', v => {
    upsertTool(v)
    paint.paintTool(surfaceWrite(paintHost()), v)
  })

  r.EventsOn('agent:toolResult', res => {
    push({
      kind: 'result',
      toolId: res.id,
      exitCode: res.exitCode,
      content: res.content,
      redacted: res.redacted || 0,
      injection: res.injection || []
    })
    // 双写：结果进归档时间线（上面 push）之外，也在终端表面上画一行退出码 +
    // 截断输出，让人在同一片流里看到 agent 这条命令跑成了什么样。
    const w = surfaceWrite(paintHost())
    paint.paintToolResult(w, res)
    // 命中 TTY 特征（top/vim 这类）：把命令**交接**到这条任务自己的常驻终端
    // 表面执行 —— 与人亲手在键盘上敲同形，画面实时可见；模型看到的仍是命令
    // 退出时定格的屏幕快照（见 agent:snapshot）。
    if (res.tty && res.command && res.hostId) {
      writeTerminal(res.hostId, res.sessionId, textToBase64(res.command + '\n'))
    }
  })

  r.EventsOn('agent:injection', e => {
    push({
      kind: 'injection',
      findings: e.findings || [],
      command: e.command || '',
      hostName: e.hostName || ''
    })
    paint.paintInjection(surfaceWrite(paintHost()), e)
  })

  r.EventsOn('agent:approval', v => {
    store.pending = v
    // 审批条/弹窗是交互入口；表面上只补一行注记说明「为什么停住了」。
    paint.paintApproval(surfaceWrite(paintHost()), v)
  })

  r.EventsOn('agent:status', s => {
    store.running = s.status === 'thinking'
  })

  r.EventsOn('agent:error', e => {
    store.running = false
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
    // 同 agent:error：人敲命令的审批（name=shell）不随 agent 回合收场。
    // agent 自己的工具审批在回合结束时必须清掉（回合已终止，批准无意义）。
    if (!isHumanShellApproval(store.pending)) store.pending = null
    freezeStreams()
  })

  // 交互终端的输出。解成字节后直接交给 xterm ——
  // 让它的 UTF-8 解码器去处理跨块的续字节（这正是它存在的意义）。
  r.EventsOn('term:data', d => {
    const sink = termSinks.get(bucketKey(d && d.hostId, d && d.sessionId))
    // 没有落点就直接丢掉：那是「界面还没建好实例」的那一小段，
    // 而终端只在用户真的打开它之后才会有输出。
    if (!sink) return
    try {
      sink(base64ToBytes(d.data))
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

  r.EventsOn('term:exit', d => {
    const st = store.terms[bucketKey(d && d.hostId, d && d.sessionId)]
    // 只在「开着」的时候才认这条退出：用户自己点关闭时状态已经置回 idle，
    // 那一次退出不该再弹一条消息。
    if (!st || (st.status !== 'open' && st.status !== 'opening')) return
    st.status = 'closed'
    st.exitReason = (d && d.reason) || '终端已结束'
  })

  // 终端屏幕快照：模型看的那份净化后文本，原样落一条给人看 ——
  // 人和模型看到同一份东西，审计才对得上。落点用后端给的
  // hostId/sessionId 显式指定：快照属于开块那一轮，不属于「此刻在看」的会话。
  r.EventsOn('agent:snapshot', d => {
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
  store.running = true
  try {
    await api().Ask(store.currentHostId, store.currentSessionId, prompt)
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
    // 放行时 PTY 自己会回显命令与输出；命令跑完提示一句「输出可以直接问 LLM」。
    // 被拒/取消/出错由 paintShellVerdict 说明原因，不再叠加提示。
    if (res.status === 'done') paint.paintLLMHint(surfaceWrite(key))
    paint.paintShellVerdict(surfaceWrite(key), res)
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
    store.terms[key] = { status: 'idle', error: '', exitReason: '' }
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
  }
  return ok
}

