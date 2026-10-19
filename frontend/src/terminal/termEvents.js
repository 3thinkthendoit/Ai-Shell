import { providerTermBadge } from '../providerLogos.js'
import * as paint from '../termPaint.js'
import { t } from '../i18n'
import {
  store,
  push,
  targetKey,
  targetEntries,
  freezeStreams,
  sealAssistantStreams,
  upsertTool,
  liveAssistant,
  writeTerminal,
  textToBase64,
  base64ToBytes,
  bucketKey,
  termState,
  takeStagedUserImages,
  clearStagedUserImages
} from '../store.js'
import { estimateTokens } from '../tokenEstimate.js'

const EV_NAMES = [
  'agent:delta', 'agent:message', 'agent:reasoning', 'agent:tool', 'agent:toolResult',
  'agent:injection', 'agent:approval', 'approval:expired', 'agent:status', 'agent:error',
  'agent:plan',
  'audit:error', 'agent:done', 'term:data', 'term:exit', 'term:tui', 'term:cmd', 'term:cwd', 'agent:snapshot',
  'fm:transfer', 'agent:transfer'
]

// 终端表面（host+session 键）→ 往它的 xterm 实例里写字节的登记表。
// 刻意**不放进 reactive**：它存的是函数，被 Vue 代理一遍没有任何意义。
const termSinks = new Map()

// 每块表面一个「画 LLM 提示占位」的回调，由 ConsolePanel 在创建实例时登记。
const llmHintListeners = new Map()

// 思考占位状态：{ timer, next, width }
const thinkingDots = new Map()
const THINKING_INTERVAL_MS = 400
const THINKING_MAX_DOTS = 4

// 每轮助手正文一个 MD→ANSI 流。刻意不放进 reactive 时间线条目（避免被 Vue 代理，
// 也避免序列化/克隆夹带不可克隆对象）。键：hostSessionKey + 步号。
const MD_SEP = '\x01'
const mdStreams = new Map()

function mdStreamKey(hostKey, step) {
  return `${hostKey}${MD_SEP}${Number(step)}`
}

function ensureMdStream(hostKey, step) {
  const k = mdStreamKey(hostKey, step)
  let s = mdStreams.get(k)
  if (!s) {
    s = paint.createMdAnsiStream()
    mdStreams.set(k, s)
  }
  return s
}

function takeMdStream(hostKey, step) {
  const k = mdStreamKey(hostKey, step)
  const s = mdStreams.get(k)
  if (s) mdStreams.delete(k)
  return s
}

/** @param {string|null|undefined} [hostKey] 缺省 = 当前 paintHost */
function flushMdStreamsForHost(hostKey) {
  const host = hostKey || paintHost()
  if (!host && host !== '') return
  const w = surfaceWrite(host)
  const prefix = `${host}${MD_SEP}`
  for (const k of [...mdStreams.keys()]) {
    if (!k.startsWith(prefix)) continue
    const md = mdStreams.get(k)
    mdStreams.delete(k)
    paint.paintAssistantEnd(w, md)
  }
}

// 输出静默检测：某块表面输出停下来之后再执行回调。
const TERM_QUIET_MS = 400
const quietTimers = new Map()
const quietWaiters = new Map()

export function registerTermSink(hostId, sessionId, fn) {
  termSinks.set(bucketKey(hostId, sessionId), fn)
}

export function unregisterTermSink(hostId, sessionId, fn) {
  const key = bucketKey(hostId, sessionId)
  if (termSinks.get(key) === fn) termSinks.delete(key)
}

export function registerLLMHint(hostId, sessionId, fn) {
  const key = bucketKey(hostId, sessionId)
  llmHintListeners.set(key, fn)
  return () => { if (llmHintListeners.get(key) === fn) llmHintListeners.delete(key) }
}

export function fireLLMHint(hostId, sessionId) {
  const fn = llmHintListeners.get(bucketKey(hostId, sessionId))
  if (fn) { try { fn() } catch { /* 绘制失败不影响命令结果展示 */ } }
}

export function surfaceWrite(key) {
  return termSinks.get(key)
}

export function paintHost() {
  return targetKey()
}

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

function startThinking(key) {
  if (!key) return
  if (thinkingDots.has(key)) return
  const w = surfaceWrite(key)
  if (!w) return
  const state = { timer: null, next: 0, width: 0 }
  const tick = () => {
    const frame = state.next
    state.next = frame >= THINKING_MAX_DOTS ? 0 : frame + 1
    state.width = paint.paintThinking(w, frame, state.width) || state.width
  }
  state.timer = setInterval(tick, THINKING_INTERVAL_MS)
  thinkingDots.set(key, state)
  tick()
}

function stopThinking(key) {
  const state = thinkingDots.get(key)
  if (!state) return
  clearInterval(state.timer)
  thinkingDots.delete(key)
  paint.clearThinking(surfaceWrite(key), state.width)
}

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

export function afterTermQuiet(hostId, sessionId, fn) {
  const key = bucketKey(hostId, sessionId)
  quietWaiters.set(key, fn)
  armQuietTimer(key)
}

// ---- 提交 shell 命令后的乐观透传窗口（cmdPending）----
//
// 为什么需要：控制台把「在提示符下敲命令」这一常态改成了本地行编辑，只有
// 拿到「前台程序接管了输入」的正向信号时才让键直达 PTY。这些信号是 OSC 133
// 的 cmdActive 与备用屏 tuiActive —— 但两者都可能缺席：
//   - shell integration 没装上（非 bash/zsh，或阿里云 ECS 上安全 agent 把
//     bash 的 DEBUG trap 冲掉，133;C 再也不发）→ cmdActive 恒假；
//   - 程序在**切备用屏之前**就等输入（vim 打开有 .swp 冲突的文件时，
//     E325 交换文件提示 / "Press ENTER or type command to continue" 打在
//     主屏上；less/read/菜单同理）→ tuiActive 也假。
// 三个信号全假时，用户敲的选项字母被本地行编辑吞进缓冲、送不到程序，
// 表现为「执行 vi 这类命令直接卡死」。
//
// 人刚提交一条 shell 命令，接下来屏幕上必然是这条命令的输出/交互，而不是
// 一个能打字问 LLM 的新提示符 —— 所以这一刻乐观地开一个透传窗口：期间按键
// 一律直达 PTY。窗口由权威信号及时关闭（收到 term:cmd / term:tui 说明真信号
// 接管了，交还给它们）；没有 shell integration 时靠「输出静默
// CMD_PENDING_IDLE_MS 秒」兜底关闭，自动回到本地行编辑。持续重绘的程序（top）
// 每帧都重置静默计时，于是它运行期间窗口一直开着、退出后自动关。
export const CMD_PENDING_IDLE_MS = 6000
const cmdPendingTimers = new Map()

function clearCmdPendingTimer(key) {
  const timer = cmdPendingTimers.get(key)
  if (timer) {
    clearTimeout(timer)
    cmdPendingTimers.delete(key)
  }
}

function armCmdPendingTimer(key, hostId, sessionId) {
  clearCmdPendingTimer(key)
  const timer = setTimeout(() => {
    cmdPendingTimers.delete(key)
    const st = termState(hostId, sessionId)
    if (st) st.cmdPending = false
  }, CMD_PENDING_IDLE_MS)
  cmdPendingTimers.set(key, timer)
}

// armCmdPending 在提交一条 shell 命令时开启透传窗口（由 store.runShellInTerminal 调）。
export function armCmdPending(hostId, sessionId) {
  const key = bucketKey(hostId, sessionId)
  const st = termState(hostId, sessionId)
  if (!st) return
  st.cmdPending = true
  armCmdPendingTimer(key, hostId, sessionId)
}

// resetCmdPending：窗口内每收到一次 PTY 输出就续期 —— 程序还在动，别急着关。
function resetCmdPending(hostId, sessionId) {
  const st = termState(hostId, sessionId)
  if (!st || !st.cmdPending) return
  armCmdPendingTimer(bucketKey(hostId, sessionId), hostId, sessionId)
}

// endCmdPending：拿到权威接管信号（term:cmd / term:tui）或终端退出时收掉窗口，
// 交还给 cmdActive / tuiActive（或回到本地行编辑）。
// 也供 termInput 在「乐观窗口里敲了中文」时主动关掉：中文是自然语言，
// 不属于 vim/菜单选项，不应直灌 bash。
export function endCmdPending(hostId, sessionId) {
  clearCmdPendingTimer(bucketKey(hostId, sessionId))
  const st = termState(hostId, sessionId)
  if (st) st.cmdPending = false
}

export function bindTerminalEvents(runtime) {
  if (!runtime) return
  if (typeof runtime.EventsOff === 'function') runtime.EventsOff(...EV_NAMES)

  runtime.EventsOn('agent:delta', d => {
    const host = paintHost()
    const w = surfaceWrite(host)
    stopThinking(host)
    const live = liveAssistant(d.step)
    if (live) {
      live.content += d.text
      if (!live.contentPainted) {
        live.contentPainted = true
        if (live.reasoningPainted) {
          paint.paintAssistantEnd(w)
          paint.paintAssistantHeader(w, live.badge)
        }
      }
      paint.paintAssistantDelta(w, d.text, ensureMdStream(host, d.step))
      return
    }
    const badge = currentModelBadge()
    // 同 step 可能残留「上次未定稿/未 done」的渲染器（painted/buf 脏）。
    // 新开气泡必须换新流，否则半截散文后缀会把 `## 标题` 当原文软吐出去。
    takeMdStream(host, d.step)
    const mdAnsi = ensureMdStream(host, d.step)
    push({
      kind: 'assistant',
      content: d.text,
      stream: d.step,
      streaming: true,
      badge,
      model: store.contextUsage.model || ''
    })
    paint.paintAssistantHeader(w, badge)
    paint.paintAssistantDelta(w, d.text, mdAnsi)
  })

  runtime.EventsOn('agent:reasoning', d => {
    const host = paintHost()
    const w = surfaceWrite(host)
    stopThinking(host)
    let live = liveAssistant(d.step)
    if (!live) {
      const badge = currentModelBadge()
      push({
        kind: 'assistant',
        content: '',
        stream: d.step,
        streaming: true,
        reasoning: '',
        reasoningOpen: true,
        badge,
        model: store.contextUsage.model || ''
      })
      live = liveAssistant(d.step)
      paint.paintAssistantHeader(w, badge)
    }
    live.reasoning = (live.reasoning || '') + d.text
    live.reasoningOpen = true
    live.reasoningPainted = true
    paint.paintAssistantReasoning(w, d.text)
  })

  runtime.EventsOn('agent:message', m => {
    const host = paintHost()
    const w = surfaceWrite(host)
    if (m.role !== 'assistant') {
      // 新的一问从 step 0 重新计。先冲掉上一问未定稿 MD、封住气泡，再落用户行。
      flushMdStreamsForHost(host)
      sealAssistantStreams()
      const images = takeStagedUserImages()
      push({
        kind: 'user',
        content: m.content,
        ...(images.length ? { images } : {})
      })
      paint.paintUser(w, m.content)
      return
    }
    stopThinking(host)
    const live = liveAssistant(m.step)
    if (live) {
      live.content = m.content
      if (m.model) live.model = m.model
      live.reasoningOpen = false
      // 定稿前先取出 MD 流（步号还在）；再清 step，下一问复用同一序号时不会写回这条。
      const md = takeMdStream(host, m.step)
      live.streaming = false
      live.stream = undefined
      // 定稿时记下本步上下文估算 + 本条输出估算，供气泡底栏展示。
      live.tokensIn = store.contextUsage.tokens || live.tokensIn || 0
      live.tokensOut = estimateTokens(`${live.reasoning || ''}${m.content || ''}`)
      paint.paintAssistantEnd(w, md)
      return
    }
    const badge = m.model ? providerTermBadge({ model: m.model, name: m.model }) : currentModelBadge()
    const tokensOut = estimateTokens(m.content || '')
    // 无 step 的整包消息：用完即弃的临时流，不进 mdStreams 表。
    const mdAnsi = paint.createMdAnsiStream()
    push({
      kind: 'assistant',
      content: m.content,
      model: m.model,
      badge,
      tokensIn: store.contextUsage.tokens || 0,
      tokensOut
    })
    paint.paintAssistantHeader(w, badge)
    paint.paintAssistantDelta(w, m.content, mdAnsi)
    paint.paintAssistantEnd(w, mdAnsi)
  })

  runtime.EventsOn('agent:plan', p => {
    stopThinking(paintHost())
    push({
      kind: 'plan',
      reason: (p && p.reason) || '',
      items: (p && p.items) || []
    })
    paint.paintPlan(surfaceWrite(paintHost()), p)
  })

  runtime.EventsOn('agent:tool', v => {
    stopThinking(paintHost())
    upsertTool(v)
    paint.paintToolCall(surfaceWrite(paintHost()), v)
  })

  runtime.EventsOn('agent:toolResult', res => {
    stopThinking(paintHost())
    push({
      kind: 'result',
      toolId: res.id,
      exitCode: res.exitCode,
      content: res.content,
      redacted: res.redacted || 0,
      injection: res.injection || []
    })
    paint.paintToolResultLine(surfaceWrite(paintHost()), res)
    if (res.tty && res.command && res.hostId) {
      writeTerminal(res.hostId, res.sessionId, textToBase64(res.command + '\n'))
    }
  })

  runtime.EventsOn('agent:context', d => {
    if (!d) return
    store.contextUsage.tokens = d.tokens || 0
    store.contextUsage.window = d.window || 0
    store.contextUsage.messages = d.messages || 0
    if (d.model) store.contextUsage.model = d.model
    // 挂到当前流式助手气泡：用量 + 本轮真实 model（切方案后立刻可见）。
    const live = targetEntries().find(e => e.kind === 'assistant' && e.streaming)
    if (live) {
      live.tokensIn = d.tokens || 0
      if (d.model) live.model = d.model
    }
  })

  runtime.EventsOn('agent:injection', e => {
    stopThinking(paintHost())
    push({
      kind: 'injection',
      findings: e.findings || [],
      command: e.command || '',
      hostName: e.hostName || ''
    })
    paint.paintInjection(surfaceWrite(paintHost()), e)
  })

  runtime.EventsOn('agent:approval', v => {
    stopThinking(paintHost())
    store.pending = v
    paint.paintApproval(surfaceWrite(paintHost()), v)
  })

  runtime.EventsOn('approval:expired', d => {
    const id = d && d.id
    if (!id || !store.pending || store.pending.id !== id) return
    store.pending = null
    push({ kind: 'system', content: t('console.approval.staleLine', { reason: (d && d.reason) || t('console.approval.gone') }) })
  })

  runtime.EventsOn('agent:status', s => {
    store.running = s.status === 'thinking'
    if (store.running) startThinking(paintHost())
  })

  runtime.EventsOn('agent:error', e => {
    store.running = false
    stopThinking(paintHost())
    store.pending = null
    clearStagedUserImages()
    flushMdStreamsForHost(paintHost())
    freezeStreams()
    push({ kind: 'error', content: e.message })
    paint.paintError(surfaceWrite(paintHost()), e.message)
  })

  runtime.EventsOn('audit:error', e => {
    store.auditError = String(e && e.message ? e.message : e)
    push({ kind: 'error', content: store.auditError })
  })

  runtime.EventsOn('agent:done', () => {
    store.running = false
    stopThinking(paintHost())
    store.pending = null
    // 正常路径已在 user message 消费；此处兜底避免异常时芯片挂到下一轮。
    clearStagedUserImages()
    // 中断时可能没有定稿 message：把 MD 渲染器里憋着的尾行冲掉。
    flushMdStreamsForHost(paintHost())
    freezeStreams()
  })

  runtime.EventsOn('term:data', d => {
    const k = bucketKey(d && d.hostId, d && d.sessionId)
    // 有输出 = 前台程序还在动：为乐观透传窗口续期（仅当窗口开着）。
    resetCmdPending(d && d.hostId, d && d.sessionId)
    const sink = termSinks.get(k)
    if (!sink) return
    stopThinking(k)
    try {
      sink(base64ToBytes(d.data))
      armQuietTimer(k)
    } catch (e) {
      push({ kind: 'error', content: t('console.decodeFail', { err: String(e && e.message ? e.message : e) }) })
    }
  })

  runtime.EventsOn('term:tui', d => {
    const st = termState(d && d.hostId, d && d.sessionId)
    if (st) st.tuiActive = !!(d && d.active)
    // 备用屏接管/让出都是权威信号：接管时 isFullscreen 会兜住，让出时程序
    // 已退出 —— 两种情况都该把乐观窗口收掉。
    endCmdPending(d && d.hostId, d && d.sessionId)
  })

  runtime.EventsOn('term:cmd', d => {
    const st = termState(d && d.hostId, d && d.sessionId)
    const active = !!(d && d.active)
    if (st) st.cmdActive = active
    // 命令边界（OSC 133）是权威信号：运行时 cmdActive 自己管透传，结束时
    // 回到提示符 —— 两者都不再需要乐观窗口。
    endCmdPending(d && d.hostId, d && d.sessionId)
  })

  runtime.EventsOn('term:cwd', d => {
    const k = bucketKey(d && d.hostId, d && d.sessionId)
    if (!d || !d.cwd) return
    store.termCwd[k] = d.cwd
    if (store.fileManager.open && store.fileManager.key === k) {
      store.fileManager.cwd = d.cwd
    }
  })

  runtime.EventsOn('fm:transfer', d => {
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

  runtime.EventsOn('agent:transfer', d => {
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

  runtime.EventsOn('term:exit', d => {
    const st = store.terms[bucketKey(d && d.hostId, d && d.sessionId)]
    if (!st || (st.status !== 'open' && st.status !== 'opening')) return
    st.status = 'closed'
    st.exitReason = (d && d.reason) || t('console.termExited')
    st.cmdActive = false
    st.tuiActive = false
    endCmdPending(d && d.hostId, d && d.sessionId)
    // 断连必须**画在屏幕上**：全屏程序（vim/top）运行中断连时画面冻结在
    // 最后一帧，输入态已归零、后续按键也不再透传 —— 用户盯着一动不动的
    // 画面，既不知道连接断了，也不知道按键为什么全部失效（表现为假死）。
    // 状态横幅在表面上方，盯着终端的人很容易漏看；在冻结的帧上补一行字，
    // 让「发生了什么」在事发地点可见。
    paint.paintSystem(surfaceWrite(bucketKey(d && d.hostId, d && d.sessionId)), t('console.termDisconnected', { reason: st.exitReason }))
  })

  runtime.EventsOn('agent:snapshot', d => {
    const snapKey = (d && d.hostId) ? bucketKey(d.hostId, d.sessionId) : paintHost()
    stopThinking(snapKey)
    push({
      kind: 'snapshot',
      termId: (d && d.termId) || '',
      text: (d && d.text) || '',
      redacted: (d && d.redacted) || 0,
      injection: (d && d.injection) || [],
      final: !!(d && d.final)
    }, (d && d.hostId) || undefined, (d && d.sessionId) || undefined)
    paint.paintSnapshotNote(surfaceWrite(snapKey), !!(d && d.final))
  })
}
