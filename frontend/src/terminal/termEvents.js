import { providerTermBadge } from '../providerLogos.js'
import * as paint from '../termPaint.js'
import {
  store,
  push,
  targetKey,
  targetEntries,
  freezeStreams,
  upsertTool,
  liveAssistant,
  writeTerminal,
  textToBase64,
  base64ToBytes,
  bucketKey,
  termState
} from '../store.js'

const EV_NAMES = [
  'agent:delta', 'agent:message', 'agent:reasoning', 'agent:tool', 'agent:toolResult',
  'agent:injection', 'agent:approval', 'approval:expired', 'agent:status', 'agent:error',
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

function isHumanShellApproval(p) {
  return !!(p && p.name === 'shell')
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
      paint.paintAssistantDelta(w, d.text)
      return
    }
    const badge = currentModelBadge()
    push({ kind: 'assistant', content: d.text, stream: d.step, streaming: true, badge })
    paint.paintAssistantHeader(w, badge)
    paint.paintAssistantDelta(w, d.text)
  })

  runtime.EventsOn('agent:reasoning', d => {
    const host = paintHost()
    const w = surfaceWrite(host)
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

  runtime.EventsOn('agent:message', m => {
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
      live.content = m.content
      if (m.model) live.model = m.model
      live.streaming = false
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
    push({ kind: 'system', content: `${(d && d.reason) || '审批已失效'}，Agent 不会执行该命令。` })
  })

  runtime.EventsOn('agent:status', s => {
    store.running = s.status === 'thinking'
    if (store.running) startThinking(paintHost())
  })

  runtime.EventsOn('agent:error', e => {
    store.running = false
    stopThinking(paintHost())
    if (!isHumanShellApproval(store.pending)) store.pending = null
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
    if (!isHumanShellApproval(store.pending)) store.pending = null
    freezeStreams()
  })

  runtime.EventsOn('term:data', d => {
    const k = bucketKey(d && d.hostId, d && d.sessionId)
    const sink = termSinks.get(k)
    if (!sink) return
    stopThinking(k)
    try {
      sink(base64ToBytes(d.data))
      armQuietTimer(k)
    } catch (e) {
      push({ kind: 'error', content: '终端输出解码失败：' + String(e && e.message ? e.message : e) })
    }
  })

  runtime.EventsOn('term:tui', d => {
    const st = termState(d && d.hostId, d && d.sessionId)
    if (st) st.tuiActive = !!(d && d.active)
  })

  runtime.EventsOn('term:cmd', d => {
    const st = termState(d && d.hostId, d && d.sessionId)
    if (st) st.cmdActive = !!(d && d.active)
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
    st.exitReason = (d && d.reason) || '终端已结束'
    st.cmdActive = false
    st.tuiActive = false
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
