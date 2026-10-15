<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import {
  store, ask, stop, approve, loadSessionTimeline, currentSessionHasTurns,
  switchProfile, push, openFileManager,
  commitDraftHost, ensureFreshTaskSession,
  noteConversationActivity, onFocusSessionChanged
} from '../store'
import { providerBadge } from '../providerLogos'
import { formatHostLabel, maskHostAddr } from '../hostDisplay'
import { renderMarkdown } from '../markdown'
import { assistantTokenStats, formatTokenCount } from '../tokenEstimate'
import { i18n, t } from '../i18n'
import logoSrc from '../assets/logo.svg'

// Agent 对话页：首页对话框 + 气泡时间线（思考过程 / 工具调用）。
// 终端页仍保留原有 Agent+Shell 分流；本组件是对话优先的第二条入口。

const emit = defineEmits(['page', 'term', 'cmd-detail'])

const draft = ref('')
const inputEl = ref(null)
const logEl = ref(null)
const showHostPicker = ref(false)
const showModelPicker = ref(false)
const showPolicyPicker = ref(false)
// 仅「发送时因缺模型被拦住」后选模型才自动发出；任务中途换模型绝不偷发草稿。
const sendAfterModelPick = ref(false)
const nowTick = ref(Date.now())
let tickTimer = null
const modelMenuRoot = ref(null)
const hostMenuRoot = ref(null)
const policyMenuRoot = ref(null)
const chipScroll = ref(null)
const policySaving = ref(false)

const TABS = [
  { id: 'ops', labelKey: 'chat.tab.ops' },
  { id: 'config', labelKey: 'chat.tab.config' },
  { id: 'inspect', labelKey: 'chat.tab.inspect' }
]
const activeTab = ref('ops')

const CHIP_KEYS = {
  ops: ['logs', 'explain', 'status', 'restart', 'disk'],
  config: ['script', 'diff', 'restart'],
  inspect: ['summary', 'status', 'disk']
}

const chips = computed(() =>
  (CHIP_KEYS[activeTab.value] || CHIP_KEYS.ops).map(key => ({
    key,
    labelKey: `chat.chip.${key}`,
    promptKey: `chat.prompt.${key}`
  }))
)

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

const openGroups = reactive({})
function toggleGroup(id) { openGroups[id] = !openGroups[id] }

const hasMessages = computed(() => store.entries.length > 0)
// 侧栏点进已绑定任务：有轮次或已加载气泡 → 对话页（勿闪新建欢迎）。
// 仅预选主机（sessionBound=false）仍留欢迎首页，即使默认会话历史上有 turns。
// 新建任务态必须无视其它会话残留的 entries / 审批 / 思考，否则会把别的任务
// 正在流式输出的思考原文盖在欢迎页上。
const sessionHasHistory = computed(() => {
  if (store.freshTaskPending) return false
  return hasMessages.value || (store.sessionBound && currentSessionHasTurns())
})
const showHomeHero = computed(() => !sessionHasHistory.value)
const homeDocked = computed(() => sessionHasHistory.value)
// 新建欢迎页可换主机；任务已绑定（CreateSession / 侧栏进入）或已有气泡后锁定。
const hostLocked = computed(() => sessionHasHistory.value || store.sessionBound)
watch(() => store.entries.length, noteConversationActivity, { immediate: true })
watch(
  () => [store.currentHostId, store.currentSessionId],
  ([h, s]) => {
    onFocusSessionChanged()
    // 焦点会话变化时拉历史（selectSession 也会拉；切主机 pending 路径靠这里）。
    if (h) void loadSessionTimeline(h, s)
  }
)
watch(() => store.hostPickerIntent, () => {
  if (!store.hostPickerIntent) return
  // 对话已开始则不可换机；新建首页可随时弹出列表（含预选后重选）。
  if (hostLocked.value) return
  showModelPicker.value = false
  showPolicyPicker.value = false
  showHostPicker.value = true
})
watch(hostLocked, locked => {
  if (locked) showHostPicker.value = false
})
const streamingNow = computed(() => store.entries.some(e => e.streaming))
// 仅当「当前正在看的」就是正在跑的那一轮时，才显示思考占位。
// 否则别的任务在跑、点了新建任务/切到别的会话，欢迎页会被全局 running 误刷出「思考中」。
const viewingActiveRun = computed(() => {
  if (!store.running) return false
  if (store.freshTaskPending) return false
  if (!store.currentHostId || !store.activeHostId) return false
  if (store.currentHostId !== store.activeHostId) return false
  // 与 bucketKey 一致：空串与 default 是同一会话。
  const cur = store.currentSessionId || 'default'
  const act = store.activeSessionId || 'default'
  return cur === act
})
const showThinkingPlaceholder = computed(() =>
  viewingActiveRun.value && !store.pending && !streamingNow.value
)
// 审批卡也只出现在「正在看的那一轮」，避免新建页被其它任务的批准条撑开。
const showApprovalHere = computed(() =>
  !!store.pending && viewingActiveRun.value
)

const decisionLabel = d => ({ allow: t('tl.decision.allow'), confirm: t('tl.decision.confirm'), deny: t('tl.decision.deny') }[d] || d)
const decisionClass = d => ({ allow: 'ok', confirm: 'warn', deny: 'danger' }[d] || '')
const statusLabel = s => ({
  pending: t('console.toolStatus.pending'), running: t('console.toolStatus.running'),
  done: t('console.toolStatus.done'), denied: t('console.toolStatus.denied'),
  error: t('console.toolStatus.error')
}[s] || s)

const activeProfileId = computed(() =>
  store.selectedProfileId || (store.llmProfiles.find(p => p.active) || {}).id || ''
)
const activeProfile = computed(() =>
  store.llmProfiles.find(p => p.id === activeProfileId.value) || null
)
const modelLabel = computed(() => {
  const p = activeProfile.value
  if (!p) return t('chat.noModel')
  // 与控制台一致：方案名 · 模型 ID，便于一眼认出实际在用的模型
  const name = String(p.name || '').trim()
  const model = String(p.model || '').trim()
  if (name && model) return `${name} · ${model}`
  return name || model || p.id
})
const modelBadge = computed(() => {
  const p = activeProfile.value
  if (!p) return null
  try { return providerBadge(p) } catch { return null }
})
const profileRows = computed(() =>
  store.llmProfiles.map(p => ({
    id: p.id,
    name: p.name || p.model || p.id,
    model: p.model || '',
    badge: providerBadge(p),
    selected: p.id === activeProfileId.value
  }))
)

function badgeStyle(b) {
  if (!b) return {}
  return b.logo ? { color: b.color } : { background: b.color, color: '#fff' }
}

function closeMetaMenus() {
  showModelPicker.value = false
  showHostPicker.value = false
  showPolicyPicker.value = false
}

function toggleModelMenu() {
  // 运行中也允许换模型（选中后会先中断本轮），任务内切换下一轮即用新模型。
  const next = !showModelPicker.value
  if (next) sendAfterModelPick.value = false
  closeMetaMenus()
  showModelPicker.value = next
}

function toggleHostMenu() {
  // 进入对话框后主机已绑定，不可再换。
  if (hostLocked.value) return
  const next = !showHostPicker.value
  closeMetaMenus()
  showHostPicker.value = next
}

function togglePolicyMenu() {
  const next = !showPolicyPicker.value
  closeMetaMenus()
  showPolicyPicker.value = next
}

function onDocMousedown(e) {
  if (showModelPicker.value && modelMenuRoot.value && !modelMenuRoot.value.contains(e.target)) {
    showModelPicker.value = false
  }
  if (showHostPicker.value && hostMenuRoot.value && !hostMenuRoot.value.contains(e.target)) {
    showHostPicker.value = false
  }
  if (showPolicyPicker.value && policyMenuRoot.value && !policyMenuRoot.value.contains(e.target)) {
    showPolicyPicker.value = false
  }
}
function onDocKeydown(e) {
  if (e.key !== 'Escape') return
  closeMetaMenus()
}

onMounted(() => {
  document.addEventListener('mousedown', onDocMousedown)
  document.addEventListener('keydown', onDocKeydown)
})
onBeforeUnmount(() => {
  document.removeEventListener('mousedown', onDocMousedown)
  document.removeEventListener('keydown', onDocKeydown)
  if (tickTimer) {
    clearInterval(tickTimer)
    tickTimer = null
  }
  if (copiedTimer) {
    clearTimeout(copiedTimer)
    copiedTimer = null
  }
})

// 新建首页用 draftHostId 预选；对话已开始用 currentHostId（已锁定）。
const composerHostId = computed(() =>
  hostLocked.value ? store.currentHostId : (store.draftHostId || store.currentHostId)
)
const currentHost = computed(() => store.hosts.find(h => h.id === composerHostId.value))
const workspaceLabel = computed(() => {
  const h = currentHost.value
  if (!h) return t('chat.workspace')
  return formatHostLabel(h) || t('chat.workspace')
})
const workspaceTitle = computed(() => {
  const h = currentHost.value
  if (!h) return t('chat.workspace')
  const ip = maskHostAddr(h.addr)
  return `${h.name} · ${h.user}@${ip}`
})

const policyLabel = computed(() =>
  store.policy.mode === 'whitelist' ? t('chat.policy.whitelist') : t('chat.policy.manual')
)
const policyTitle = computed(() => `${t('chat.policy')} · ${policyLabel.value}`)

// 上下文圆环：模型 pill 左侧常显图标；文案 tip 仅悬停时出现。
const CTX_RING_R = 9
const CTX_RING_C = 2 * Math.PI * CTX_RING_R

const ctxUsage = computed(() => {
  const { tokens, window: win, messages } = store.contextUsage
  if (!tokens) return { tip: '', level: '', title: '', pct: 0, show: false }
  const used = fmtTokens(tokens)
  if (!win) {
    return {
      tip: t('console.ctx.tipNoWin', { used }),
      level: 'unknown',
      title: t('console.ctx.unknown'),
      pct: 0,
      show: true
    }
  }
  const pctNum = tokens * 100 / win
  const pct = pctNum.toFixed(1)
  const level = pctNum >= 95 ? 'danger' : pctNum >= 80 ? 'warn' : pctNum >= 60 ? 'mid' : 'ok'
  return {
    tip: t('console.ctx.tip', { pct, used, win: fmtTokens(win) }),
    level,
    title: t('console.ctx.title', {
      tokens: tokens.toLocaleString(),
      win: win.toLocaleString(),
      pct: Math.round(pctNum),
      messages
    }),
    pct: Math.min(100, Math.max(0, pctNum)),
    show: true
  }
})

const ctxRingStyle = computed(() => {
  const pct = ctxUsage.value.pct || 0
  return {
    strokeDasharray: String(CTX_RING_C),
    strokeDashoffset: String(CTX_RING_C * (1 - pct / 100))
  }
})

function fmtTokens(n) {
  if (n >= 1000) {
    const k = n / 1000
    return (k >= 100 ? Math.round(k).toFixed(1) : (Math.round(k * 10) / 10).toFixed(1)) + 'K'
  }
  return String(n)
}

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

async function onSwitchModel(id) {
  if (!id || id === activeProfileId.value) return
  // 本轮还在跑 / busy 时先中断，否则 lane 占着，切换后点发送会一直「上一轮仍在执行」。
  if (store.running || store.busy) {
    await stop()
  }
  const prev = store.selectedProfileId
  store.selectedProfileId = id
  try {
    await switchProfile(id)
  } catch {
    store.selectedProfileId = prev
  }
}

function applyChip(chip) {
  draft.value = t(chip.promptKey)
  nextTick(() => inputEl.value?.focus())
}

async function onAttach() {
  if (hostLocked.value) {
    if (!store.currentHostId) {
      showHostPicker.value = true
      return
    }
    openFileManager(store.currentHostId, store.currentSessionId)
    return
  }
  if (!store.draftHostId && !store.currentHostId) {
    showHostPicker.value = true
    return
  }
  // 附件需要真实主机：预选先提交；新建态则先开新会话再开文件管理。
  if (!(await commitDraftHost()) || !(await ensureFreshTaskSession())) {
    showHostPicker.value = true
    return
  }
  openFileManager(store.currentHostId, store.currentSessionId)
}

async function ensureReadyToSend() {
  if (!store.draftHostId && !store.currentHostId) {
    showHostPicker.value = true
    return false
  }
  if (!activeProfileId.value) {
    sendAfterModelPick.value = true
    showModelPicker.value = true
    return false
  }
  // 预选主机在发送时才落到 currentHostId；新建态再静默开新会话。
  if (!(await commitDraftHost())) {
    showHostPicker.value = true
    return false
  }
  if (!(await ensureFreshTaskSession())) return false
  return true
}

async function doSend() {
  const text = draft.value.trim()
  if (!text) return
  if (!(await ensureReadyToSend())) return
  // 仅在 ask 真正发起成功后清草稿；busy / 校验失败 / 同步拒绝都保留输入。
  const ok = await ask(text)
  if (ok) draft.value = ''
  scrollLog()
}

function onSendClick() {
  void doSend()
}

// ---- 消息操作（WorkBuddy 风：时间 + 复制 / 编辑 / 重发）----
const copiedMsgId = ref(null)
let copiedTimer = null

function pad2(n) {
  return String(n).padStart(2, '0')
}

function formatMsgTime(ts) {
  if (!ts) return ''
  const d = new Date(ts)
  if (Number.isNaN(d.getTime())) return ''
  if (i18n.locale === 'zh') {
    return `${d.getMonth() + 1}月${d.getDate()}日 ${pad2(d.getHours())}:${pad2(d.getMinutes())}`
  }
  return d.toLocaleString('en-US', {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit'
  })
}

async function writeClipboard(text) {
  if (!text) return false
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch { /* 走兜底 */ }
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

async function copyMessage(it) {
  const text = String(it?.content || '').trim()
  if (!text) return
  const ok = await writeClipboard(text)
  if (!ok) return
  copiedMsgId.value = it.id
  if (copiedTimer) clearTimeout(copiedTimer)
  copiedTimer = setTimeout(() => {
    if (copiedMsgId.value === it.id) copiedMsgId.value = null
  }, 1500)
}

function editMessage(it) {
  const text = String(it?.content || '')
  if (!text) return
  draft.value = text
  nextTick(() => {
    inputEl.value?.focus()
    const el = inputEl.value
    if (el && typeof el.setSelectionRange === 'function') {
      const n = text.length
      el.setSelectionRange(n, n)
    }
  })
}

async function resendMessage(it) {
  const text = String(it?.content || '').trim()
  if (!text || store.running || store.busy) return
  draft.value = text
  await doSend()
}

const lastUserMsgId = computed(() => {
  for (let i = store.entries.length - 1; i >= 0; i--) {
    if (store.entries[i].kind === 'user') return store.entries[i].id
  }
  return null
})

function assistantTokensLabel(it) {
  const st = assistantTokenStats(it)
  if (!st) return ''
  const out = formatTokenCount(st.out)
  if (st.in > 0) {
    return t('chat.msg.tokensIO', { in: formatTokenCount(st.in), out })
  }
  return t('chat.msg.tokensOut', { n: out })
}

function onComposerKeydown(e) {
  if (e.isComposing || e.keyCode === 229) return
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault()
    void doSend()
  }
}

function pickHost(id) {
  if (hostLocked.value) return
  // 只预选：不写 currentHostId，不自动发送；用户再点发送才进入任务。
  store.draftHostId = id
  showHostPicker.value = false
  nextTick(() => inputEl.value?.focus())
}

async function onGotoTerm() {
  // 开终端 = 真正开始任务：有预选则提交，新建态再静默开新会话。
  if (store.draftHostId || store.currentHostId) {
    if (!(await commitDraftHost())) return
    if (!(await ensureFreshTaskSession())) return
  }
  emit('term')
}

function pickModel(id) {
  showModelPicker.value = false
  const autoSend = sendAfterModelPick.value
  sendAfterModelPick.value = false
  void onSwitchModel(id).then(() => {
    // 只有「点发送时因缺模型被拦住」才自动续发；任务中途换模型保留草稿，由用户再发。
    if (autoSend && draft.value.trim() && (store.currentHostId || store.draftHostId)) {
      void doSend()
    }
  })
}

function goHosts() {
  showHostPicker.value = false
  emit('page', 'hosts')
}

function goSettings() {
  showModelPicker.value = false
  emit('page', 'settings')
}

async function pickPolicyMode(mode) {
  if (policySaving.value) return
  if (store.policy.mode === mode) {
    showPolicyPicker.value = false
    return
  }
  policySaving.value = true
  try {
    const payload = { ...store.policy, mode }
    const api = window.go?.main?.App
    if (!api || typeof api.SavePolicy !== 'function') throw new Error('backend unavailable')
    await api.SavePolicy(payload)
    store.policy = payload
    showPolicyPicker.value = false
    // 底栏 pill 已显示当前模式，不再往对话里灌「已切换为…」系统气泡。
  } catch (e) {
    push({ kind: 'error', content: t('chat.policy.fail', { err: e && e.message ? e.message : e }) })
  } finally {
    policySaving.value = false
  }
}

function goPolicyPage() {
  showPolicyPicker.value = false
  emit('page', 'policy')
}

function scrollChips(dir) {
  const el = chipScroll.value
  if (!el) return
  el.scrollBy({ left: dir * 180, behavior: 'smooth' })
}

// ---- 对话页审批卡：进消息区（不浮在输入框上）----
const approvalRemainSec = computed(() => {
  const dl = store.pending && store.pending.deadlineMs
  if (!dl) return null
  const left = Math.ceil((dl - nowTick.value) / 1000)
  return left > 0 ? left : 0
})
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
    tickTimer = setInterval(() => { nowTick.value = Date.now() }, 1000)
  },
  { immediate: true }
)
watch(() => store.pending, v => {
  if (v) nextTick(scrollLog)
})
watch(showThinkingPlaceholder, v => {
  if (v) scrollLog()
})
</script>

<template>
  <div class="chat" :class="{ empty: showHomeHero }">
    <!-- 消息区：气泡时间线 + 待审批卡（审批进消息流，不盖在输入框上） -->
    <div
      v-if="!store.freshTaskPending && (sessionHasHistory || showApprovalHere || showThinkingPlaceholder)"
      class="log archive"
      ref="logEl"
    >
      <div class="log-col">
      <div v-for="it in timeline" :key="it.id" class="entry" :class="'entry-' + it.kind">
        <div v-if="it.kind === 'toolGroup'" class="tool-group">
          <div class="tool-group-head" role="button" tabindex="0" @click="toggleGroup(it.id)" @keydown.enter="toggleGroup(it.id)">
            <span class="tg-arrow">{{ openGroups[it.id] ? '▾' : '▸' }}</span>
            <span class="tg-label">{{ t('console.toolGroup', { n: it.calls.length }) }}</span>
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
                  <span class="muted">{{ t('console.exitCode', { n: c.result.exitCode }) }}</span>
                  <span v-if="c.result.redacted" class="badge warn">{{ t('console.redacted', { n: c.result.redacted }) }}</span>
                  <span class="muted tiny">{{ t('console.rawOutput') }}</span>
                </div>
                <pre>{{ c.result.content }}</pre>
              </div>
            </div>
          </div>
        </div>

        <div v-else-if="it.kind === 'snapshot'" class="result">
          <div class="result-head">
            <span v-if="it.final" class="badge warn">{{ t('console.snapshot.final') }}</span>
            <span v-else class="badge warn">{{ t('console.snapshot.live') }}</span>
            <span v-if="it.redacted" class="badge warn">{{ t('console.redacted', { n: it.redacted }) }}</span>
            <span class="muted tiny">{{ it.final ? t('console.snapshot.finalHint') : t('console.snapshot.liveHint') }}</span>
          </div>
          <pre>{{ it.text }}</pre>
          <div v-if="it.injection && it.injection.length" class="tool-reason">
            {{ t('console.injection.found', { findings: it.injection.join('、') }) }}
          </div>
        </div>

        <div v-else-if="it.kind === 'user'" class="msg user">
          <div class="msg-col">
            <div class="bubble">{{ it.content }}</div>
            <div class="msg-actions" :class="{ revealed: copiedMsgId === it.id }">
              <span v-if="it.ts" class="msg-time">{{ formatMsgTime(it.ts) }}</span>
              <button
                type="button"
                class="msg-act"
                :title="copiedMsgId === it.id ? t('chat.msg.copied') : t('chat.msg.copy')"
                @click="copyMessage(it)"
              >
                <svg width="14" height="14" viewBox="0 0 24 24" aria-hidden="true">
                  <rect x="9" y="9" width="11" height="11" rx="2" fill="none" stroke="currentColor" stroke-width="1.7" />
                  <path d="M5 15V5a2 2 0 0 1 2-2h10" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" />
                </svg>
              </button>
              <button
                type="button"
                class="msg-act"
                :title="t('chat.msg.resend')"
                :disabled="store.running || store.busy || it.id !== lastUserMsgId"
                @click="resendMessage(it)"
              >
                <svg width="14" height="14" viewBox="0 0 24 24" aria-hidden="true">
                  <path d="M3 12a9 9 0 1 0 3-6.7" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" />
                  <path d="M3 4v5h5" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" />
                </svg>
              </button>
              <button type="button" class="msg-act" :title="t('chat.msg.edit')" @click="editMessage(it)">
                <svg width="14" height="14" viewBox="0 0 24 24" aria-hidden="true">
                  <path d="M4 20h4L18 10l-4-4L4 16v4z M14 6l4 4" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" />
                </svg>
              </button>
            </div>
          </div>
        </div>

        <div v-else-if="it.kind === 'assistant'" class="msg assistant">
          <div class="asst-head">
            <img class="asst-avatar" :src="logoSrc" alt="Ai-Shell" title="Ai-Shell" />
            <div class="asst-meta">
              <span class="asst-name">Ai-Shell</span>
              <span v-if="it.streaming && it.content" class="asst-status">{{ t('console.reasoning.writing') }}</span>
              <span v-else-if="it.model" class="asst-status">{{ it.model }}</span>
            </div>
          </div>
          <div class="asst-body">
            <!-- 思考阶段或已有思考内容：始终给出「思考过程」区块（推理模型会往里灌增量）。 -->
            <div v-if="it.reasoning || (it.streaming && !it.content)" class="reasoning">
              <div class="reasoning-head" @click="it.reasoningOpen = !it.reasoningOpen">
                {{ it.streaming && !it.content ? t('console.reasoning.thinking') : t('console.reasoning.label') }}
                <span class="reasoning-arrow">{{ it.reasoningOpen !== false ? '▾' : '▸' }}</span>
              </div>
              <div v-show="it.reasoningOpen !== false" class="reasoning-body">
                <template v-if="it.reasoning">{{ it.reasoning }}</template>
                <span v-else class="reasoning-wait">{{ t('console.reasoning.wait') }}</span>
              </div>
            </div>
            <div class="asst-text">
              <div v-if="it.content" class="md-body" v-html="renderMarkdown(it.content)"></div>
              <span v-if="it.streaming && it.content" class="caret"></span>
              <span v-if="!it.content && !it.reasoning && !it.streaming" class="muted tiny">{{ t('console.emptyReply') }}</span>
            </div>
            <div v-if="(it.content || it.reasoning) && !it.streaming" class="msg-footer asst-footer">
              <span v-if="assistantTokensLabel(it)" class="msg-tokens">{{ assistantTokensLabel(it) }}</span>
              <div class="msg-actions" :class="{ revealed: copiedMsgId === it.id }">
                <span v-if="it.ts" class="msg-time">{{ formatMsgTime(it.ts) }}</span>
                <button
                  type="button"
                  class="msg-act"
                  :title="copiedMsgId === it.id ? t('chat.msg.copied') : t('chat.msg.copy')"
                  @click="copyMessage(it)"
                >
                  <svg width="14" height="14" viewBox="0 0 24 24" aria-hidden="true">
                    <rect x="9" y="9" width="11" height="11" rx="2" fill="none" stroke="currentColor" stroke-width="1.7" />
                    <path d="M5 15V5a2 2 0 0 1 2-2h10" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" />
                  </svg>
                </button>
              </div>
            </div>
          </div>
        </div>

        <div v-else-if="it.kind === 'injection'" class="injection">
          <div class="injection-title">{{ t('console.injection.title') }}</div>
          <div class="injection-body">{{ t('console.injection.hits', { findings: it.findings.join('、') }) }}</div>
          <pre v-if="it.command" class="injection-cmd">{{ it.command }}</pre>
          <div class="injection-hint">
            {{ t('console.injection.p1') }}<span class="mono">{{ it.hostName }}</span>{{ t('console.injection.p2') }}{{ t('console.injection.p3') }}
          </div>
        </div>

        <div v-else-if="it.kind === 'error'" class="msg error">
          <div class="asst-text error-text">{{ it.content }}</div>
        </div>

        <div v-else class="msg system">
          <div class="system-text">{{ it.content }}</div>
        </div>
      </div>

      <div v-if="showApprovalHere" class="approval in-feed">
        <div class="approval-head">
          <span class="badge warn">{{ t('console.approval.title') }}</span>
          <span class="mono">{{ store.pending.name }}</span>
          <span class="muted" v-if="store.pending.hostName">@{{ store.pending.hostName }}</span>
          <span
            v-if="approvalCountdown"
            class="muted approval-countdown"
            :class="{ urgent: approvalRemainSec !== null && approvalRemainSec <= 30 }"
          >{{ t('console.approval.expiry', { time: approvalCountdown }) }}</span>
          <button type="button" class="sm right" @click="emit('cmd-detail')">{{ t('console.approval.detail') }}</button>
        </div>
        <pre class="approval-cmd">{{ store.pending.command || store.pending.args }}</pre>
        <div class="approval-reason">{{ store.pending.reason }}</div>
        <div class="row approval-actions">
          <button type="button" class="ok" @click="approve(true)">{{ t('console.approval.approve') }}</button>
          <button type="button" class="danger" @click="approve(false)">{{ t('console.approval.deny') }}</button>
          <button type="button" class="sm" @click="approve(false)">{{ t('console.approval.skip') }}</button>
        </div>
      </div>

      <!-- 首包前占位：同款气泡 + 展开的思考过程区（有推理增量后由正式 assistant 条目接替）。 -->
      <div
        v-if="showThinkingPlaceholder"
        class="msg assistant thinking-placeholder"
        aria-live="polite"
      >
        <div class="asst-head">
          <img class="asst-avatar" :src="logoSrc" alt="Ai-Shell" title="Ai-Shell" />
          <div class="asst-meta">
            <span class="asst-name">Ai-Shell</span>
          </div>
        </div>
        <div class="asst-body">
          <div class="reasoning">
            <div class="reasoning-head">
              {{ t('console.reasoning.thinking') }}
              <span class="reasoning-arrow">▾</span>
            </div>
            <div class="reasoning-body">
              <span class="reasoning-wait">
                {{ t('console.reasoning.wait') }}<span class="thinking-dots" aria-hidden="true"><i>.</i><i>.</i><i>.</i></span>
              </span>
            </div>
          </div>
        </div>
      </div>
      </div>
    </div>

    <!-- 空态：分类+快捷；对话中：只留输入框（参考 WorkBuddy 会话页） -->
    <div class="home" :class="{ docked: homeDocked, conversing: sessionHasHistory }">
      <template v-if="showHomeHero">
        <h1 class="home-title">{{ t('chat.title') }}</h1>
        <p class="home-sub">{{ t('chat.subtitle') }}</p>
        <button type="button" class="home-term-link" @click="onGotoTerm">
          {{ t('chat.gotoTerm') }}
        </button>
      </template>

      <template v-if="!sessionHasHistory">
        <div class="tabs">
          <button
            v-for="tab in TABS"
            :key="tab.id"
            type="button"
            class="tab"
            :class="{ on: activeTab === tab.id }"
            @click="activeTab = tab.id"
          >{{ t(tab.labelKey) }}</button>
        </div>

        <div class="chips-row">
          <div class="chips" ref="chipScroll">
            <button
              v-for="c in chips"
              :key="c.key"
              type="button"
              class="chip"
              @click="applyChip(c)"
            >{{ t(c.labelKey) }}</button>
          </div>
          <button type="button" class="chip-more" :title="t('chat.chip.more')" @click="scrollChips(1)">›</button>
        </div>
      </template>

      <!-- WorkBuddy 风：单卡片 = 输入区 + 底栏（+ / 主机 / 权限 | 模型 / 发送） -->
      <div class="composer-shell">
        <div class="composer-card">
          <textarea
            ref="inputEl"
            v-model="draft"
            class="composer-input"
            rows="3"
            :placeholder="t('chat.placeholder')"
            :disabled="store.running || store.busy"
            @keydown="onComposerKeydown"
          />
          <div class="composer-toolbar">
            <div class="composer-left">
              <button type="button" class="tool-icon" :title="t('console.files.title')" @click="onAttach">+</button>

              <!-- 新建任务首页可换主机；发出第一条消息后锁定。 -->
              <div ref="hostMenuRoot" class="host-menu-wrap">
                <button
                  v-if="!hostLocked"
                  type="button"
                  class="tool-pill meta-btn"
                  :class="{ open: showHostPicker }"
                  :title="workspaceTitle"
                  @click="toggleHostMenu"
                >
                  <svg class="meta-ico" width="14" height="14" viewBox="0 0 24 24" aria-hidden="true">
                    <path d="M4 4h16v6H4z M4 14h16v6H4z M7 7h.01 M7 17h.01"
                          fill="none" stroke="currentColor" stroke-width="1.7"
                          stroke-linecap="round" stroke-linejoin="round" />
                  </svg>
                  <span class="meta-label">{{ workspaceLabel }}</span>
                  <span class="caret-down">{{ showHostPicker ? '▴' : '▾' }}</span>
                </button>
                <span
                  v-else
                  class="tool-pill meta-btn host-bound"
                  :title="workspaceTitle"
                >
                  <svg class="meta-ico" width="14" height="14" viewBox="0 0 24 24" aria-hidden="true">
                    <path d="M4 4h16v6H4z M4 14h16v6H4z M7 7h.01 M7 17h.01"
                          fill="none" stroke="currentColor" stroke-width="1.7"
                          stroke-linecap="round" stroke-linejoin="round" />
                  </svg>
                  <span class="meta-label">{{ workspaceLabel }}</span>
                </span>
                <div v-if="showHostPicker && !hostLocked" class="host-menu" role="listbox">
                  <div v-if="store.hosts.length" class="model-menu-list">
                    <button
                      v-for="h in store.hosts"
                      :key="h.id"
                      type="button"
                      class="model-menu-item"
                      role="option"
                      :aria-selected="h.id === composerHostId"
                      :class="{ on: h.id === composerHostId }"
                      @click="pickHost(h.id)"
                    >
                      <span class="model-menu-text">
                        <span class="model-menu-name">{{ h.name }}</span>
                        <span class="model-menu-sub">{{ h.user }}@{{ maskHostAddr(h.addr) }}</span>
                      </span>
                      <span v-if="h.id === composerHostId" class="model-menu-check" aria-hidden="true">✓</span>
                    </button>
                  </div>
                  <p v-else class="model-menu-empty">{{ t('chat.pickHost.empty') }}</p>
                  <button type="button" class="model-menu-foot" @click="goHosts">
                    <svg width="14" height="14" viewBox="0 0 24 24" aria-hidden="true">
                      <path d="M12 5v14 M5 12h14" fill="none" stroke="currentColor" stroke-width="1.7"
                            stroke-linecap="round" stroke-linejoin="round" />
                    </svg>
                    {{ t('chat.pickHost.add') }}
                  </button>
                </div>
              </div>

              <div ref="policyMenuRoot" class="policy-menu-wrap">
                <button
                  type="button"
                  class="tool-pill meta-btn"
                  :class="{ open: showPolicyPicker }"
                  :disabled="policySaving"
                  :title="policyTitle"
                  @click="togglePolicyMenu"
                >
                  <svg class="meta-ico" width="14" height="14" viewBox="0 0 24 24" aria-hidden="true">
                    <path d="M12 3l7 3v5c0 5-3 8-7 10-4-2-7-5-7-10V6l7-3z"
                          fill="none" stroke="currentColor" stroke-width="1.7"
                          stroke-linejoin="round" />
                  </svg>
                  <span class="meta-label">{{ policyLabel }}</span>
                  <span class="caret-down">{{ showPolicyPicker ? '▴' : '▾' }}</span>
                </button>
                <div v-if="showPolicyPicker" class="policy-menu" role="listbox">
                  <div class="model-menu-list">
                    <button
                      type="button"
                      class="model-menu-item"
                      role="option"
                      :aria-selected="store.policy.mode === 'manual'"
                      :class="{ on: store.policy.mode === 'manual' }"
                      :disabled="policySaving"
                      @click="pickPolicyMode('manual')"
                    >
                      <span class="model-menu-text">
                        <span class="model-menu-name">{{ t('chat.policy.manual') }}</span>
                        <span class="model-menu-sub">{{ t('chat.policy.manualHint') }}</span>
                      </span>
                      <span v-if="store.policy.mode === 'manual'" class="model-menu-check" aria-hidden="true">✓</span>
                    </button>
                    <button
                      type="button"
                      class="model-menu-item"
                      role="option"
                      :aria-selected="store.policy.mode === 'whitelist'"
                      :class="{ on: store.policy.mode === 'whitelist' }"
                      :disabled="policySaving"
                      @click="pickPolicyMode('whitelist')"
                    >
                      <span class="model-menu-text">
                        <span class="model-menu-name">{{ t('chat.policy.whitelist') }}</span>
                        <span class="model-menu-sub">{{ t('chat.policy.whitelistHint') }}</span>
                      </span>
                      <span v-if="store.policy.mode === 'whitelist'" class="model-menu-check" aria-hidden="true">✓</span>
                    </button>
                  </div>
                  <button type="button" class="model-menu-foot" @click="goPolicyPage">
                    {{ t('chat.policy.more') }}
                  </button>
                </div>
              </div>
            </div>

            <div class="composer-right">
              <!-- 上下文圆环在模型左侧；悬停才显示用量 tip -->
              <div v-if="ctxUsage.show" class="ctx-ring-wrap">
                <button
                  type="button"
                  class="ctx-ring-btn"
                  :class="ctxUsage.level"
                  :title="ctxUsage.title"
                  :aria-label="ctxUsage.tip"
                >
                  <svg class="ctx-ring" viewBox="0 0 24 24" aria-hidden="true">
                    <circle class="ctx-ring-track" cx="12" cy="12" :r="CTX_RING_R" />
                    <circle
                      class="ctx-ring-fill"
                      cx="12"
                      cy="12"
                      :r="CTX_RING_R"
                      :style="ctxRingStyle"
                    />
                  </svg>
                </button>
                <div
                  v-if="ctxUsage.tip && !showModelPicker"
                  class="ctx-tip"
                  :class="ctxUsage.level"
                >{{ ctxUsage.tip }}</div>
              </div>
              <div ref="modelMenuRoot" class="model-menu-wrap">
                <button
                  type="button"
                  class="model-pill"
                  :class="{ open: showModelPicker }"
                  @click="toggleModelMenu"
                >
                  <span
                    v-if="modelBadge"
                    class="model-badge-ico"
                    :class="{ 'is-logo': !!modelBadge.logo }"
                    :style="badgeStyle(modelBadge)"
                    v-html="modelBadge.logo || modelBadge.mark"
                  ></span>
                  <span v-else class="model-dot" aria-hidden="true"></span>
                  <span class="model-pill-label">{{ modelLabel }}</span>
                  <span class="caret-down">{{ showModelPicker ? '▴' : '▾' }}</span>
                </button>
                <div v-if="showModelPicker" class="model-menu" role="listbox">
                  <div v-if="profileRows.length" class="model-menu-list">
                    <button
                      v-for="row in profileRows"
                      :key="row.id"
                      type="button"
                      class="model-menu-item"
                      role="option"
                      :aria-selected="row.selected"
                      :class="{ on: row.selected }"
                      @click="pickModel(row.id)"
                    >
                      <span
                        class="model-badge-ico"
                        :class="{ 'is-logo': !!row.badge.logo }"
                        :style="badgeStyle(row.badge)"
                        v-html="row.badge.logo || row.badge.mark"
                      ></span>
                      <span class="model-menu-text">
                        <span class="model-menu-name">{{ row.name }}</span>
                        <span v-if="row.model && row.model !== row.name" class="model-menu-sub">{{ row.model }}</span>
                      </span>
                      <span v-if="row.selected" class="model-menu-check" aria-hidden="true">✓</span>
                    </button>
                  </div>
                  <p v-else class="model-menu-empty">{{ t('chat.pickModel.empty') }}</p>
                  <button type="button" class="model-menu-foot" @click="goSettings">
                    <svg width="14" height="14" viewBox="0 0 24 24" aria-hidden="true">
                      <path d="M4 20h4L18 10l-4-4L4 16v4z M14 6l4 4"
                            fill="none" stroke="currentColor" stroke-width="1.7"
                            stroke-linecap="round" stroke-linejoin="round" />
                    </svg>
                    {{ t('chat.pickModel.configure') }}
                  </button>
                </div>
              </div>
              <!-- 运行中：发送钮变中断；「思考中」在消息流里（logo + Ai-Shell）。 -->
              <button
                v-if="store.running || store.busy"
                type="button"
                class="send-btn stop-btn"
                :title="t('console.interrupt')"
                @click="stop"
              >
                <span class="stop-ico" aria-hidden="true"></span>
              </button>
              <button
                v-else
                type="button"
                class="send-btn"
                :disabled="!draft.trim()"
                :title="t('chat.send')"
                @click="onSendClick"
              >
                <svg class="send-ico" width="18" height="18" viewBox="0 0 24 24" aria-hidden="true">
                  <path
                    d="M12 19V5M12 5l-6 6M12 5l6 6"
                    fill="none"
                    stroke="currentColor"
                    stroke-width="2.2"
                    stroke-linecap="round"
                    stroke-linejoin="round"
                  />
                </svg>
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>

  </div>
</template>

<style scoped>
.chat {
  /* 首页 80%；有对话后消息列 90% */
  --home-col: 80%;
  --chat-col: 90%;
  /* 头像 28 + 间距 8：正文 / 思考 / 工具调用共用左缘 */
  --asst-indent: 36px;
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
  background: var(--bg);
}
.chat.empty {
  justify-content: center;
}

.log.archive {
  flex: 1;
  min-height: 0;
  overflow: auto;
  padding: 18px 20px 8px;
}
/* 与底部 composer / chips / tools 同宽居中，避免气泡通栏、输入框收窄错位 */
.log-col {
  width: var(--chat-col);
  max-width: 100%;
  margin: 0 auto;
}

.home {
  display: flex;
  flex-direction: column;
  align-items: center;
  padding: 24px 20px 28px;
  gap: 14px;
}
.home.docked {
  margin-top: auto;
  padding-top: 8px;
  border-top: 1px solid var(--border);
  background: var(--surface);
}
.home.conversing {
  border-top: 0;
  background: transparent;
  padding-top: 4px;
  padding-bottom: 16px;
}

.home-title {
  margin: 0;
  font-size: 28px;
  font-weight: 700;
  letter-spacing: 0.02em;
  color: var(--text);
}
.home-sub {
  margin: -4px 0 4px;
  font-size: 13px;
  color: var(--text-3);
}
.home-term-link {
  border: 0;
  background: transparent;
  color: var(--accent);
  font-size: 12.5px;
  cursor: pointer;
  padding: 0;
  margin: -6px 0 2px;
  text-decoration: underline;
  text-underline-offset: 3px;
}
.home-term-link:hover { opacity: 0.85; }

.tabs {
  display: inline-flex;
  gap: 4px;
  padding: 4px;
  border-radius: 999px;
  background: var(--inset-bg);
  border: 1px solid var(--border);
}
.tab {
  border: 0;
  background: transparent;
  color: var(--text-2);
  font-size: 13px;
  padding: 7px 16px;
  border-radius: 999px;
  cursor: pointer;
}
.tab.on {
  background: var(--surface);
  color: var(--text);
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.08);
  font-weight: 600;
}

.chips-row {
  width: var(--home-col);
  max-width: 100%;
  display: flex;
  align-items: center;
  gap: 8px;
}
.chips {
  flex: 1;
  display: flex;
  gap: 8px;
  overflow-x: auto;
  scrollbar-width: none;
  padding: 2px 0;
}
.chips::-webkit-scrollbar { display: none; }
.chip {
  flex-shrink: 0;
  border: 1px solid var(--border);
  background: var(--surface-2);
  color: var(--text-2);
  border-radius: 999px;
  padding: 7px 14px;
  font-size: 12.5px;
  cursor: pointer;
}
.chip:hover { border-color: var(--accent); color: var(--text); }
.chip-more {
  flex-shrink: 0;
  width: 32px;
  height: 32px;
  border-radius: 50%;
  border: 1px solid var(--border);
  background: var(--surface);
  cursor: pointer;
  font-size: 18px;
  line-height: 1;
  color: var(--text-2);
}

.composer-shell {
  width: var(--home-col);
  max-width: 100%;
  display: flex;
  flex-direction: column;
}
.home.conversing .composer-shell {
  width: var(--chat-col);
}
.composer-card {
  border: 1px solid var(--border);
  border-radius: 22px;
  background: var(--surface);
  box-shadow: 0 2px 12px rgba(0, 0, 0, 0.04);
  padding: 16px 16px 12px;
}
/* 盖掉全局 textarea 的边框/内边距/聚焦描边，只留外层 composer-card 一圈。 */
.composer-input {
  width: 100%;
  border: 0;
  border-radius: 0;
  outline: none;
  box-shadow: none;
  resize: none;
  background: transparent;
  padding: 0;
  color: var(--text);
  font: inherit;
  font-family: inherit;
  font-size: 15px;
  line-height: 1.55;
  min-height: 64px;
}
.composer-input:focus {
  border: 0;
  outline: none;
  box-shadow: none;
}
.composer-input::placeholder { color: var(--text-3); }
.composer-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  margin-top: 10px;
  min-width: 0;
}
.composer-left,
.composer-right {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}
.composer-left { flex: 1 1 auto; }
.composer-right { flex-shrink: 0; }
.tool-icon {
  width: 32px;
  height: 32px;
  border-radius: 50%;
  border: 0;
  background: transparent;
  cursor: pointer;
  font-size: 20px;
  line-height: 1;
  color: var(--text-2);
  flex-shrink: 0;
}
.tool-icon:hover { background: var(--surface-2); color: var(--text); }
.tool-pill {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  border: 0;
  background: transparent;
  color: var(--text-2);
  font-size: 12.5px;
  cursor: pointer;
  padding: 6px 10px;
  border-radius: 999px;
  max-width: 160px;
  min-width: 0;
}
.tool-pill:hover,
.tool-pill.open {
  background: var(--surface-2);
  color: var(--text);
}
/* 已绑定主机：只读标签，无悬停手型 / 下拉箭头 */
.tool-pill.host-bound {
  cursor: default;
  pointer-events: none;
}
.tool-pill.host-bound:hover {
  background: transparent;
  color: var(--text-2);
}
.model-menu-wrap,
.host-menu-wrap,
.policy-menu-wrap {
  position: relative;
}
.ctx-ring-wrap {
  position: relative;
  display: inline-flex;
  align-items: center;
  flex-shrink: 0;
}
.ctx-ring-btn {
  width: 28px;
  height: 28px;
  padding: 0;
  border: 0;
  border-radius: 50%;
  background: transparent;
  color: var(--text-3);
  cursor: default;
  display: inline-flex;
  align-items: center;
  justify-content: center;
}
.ctx-ring-btn:hover { background: var(--surface-2); color: var(--text-2); }
.ctx-ring {
  width: 18px;
  height: 18px;
  transform: rotate(-90deg);
  display: block;
}
.ctx-ring-track,
.ctx-ring-fill {
  fill: none;
  stroke-width: 2.2;
  stroke-linecap: round;
}
.ctx-ring-track { stroke: var(--border-strong); }
.ctx-ring-fill { stroke: currentColor; transition: stroke-dashoffset 0.25s ease; }
.ctx-ring-btn.ok { color: var(--accent); }
.ctx-ring-btn.mid { color: #ca8a04; }
.ctx-ring-btn.warn { color: #b45309; }
.ctx-ring-btn.danger { color: #b91c1c; }
.ctx-ring-btn.unknown { color: var(--text-3); }
.ctx-tip {
  position: absolute;
  left: 50%;
  bottom: calc(100% + 10px);
  transform: translateX(-50%);
  white-space: nowrap;
  padding: 6px 12px;
  border-radius: 8px;
  background: #3a3a3a;
  color: #fff;
  font-size: 12px;
  line-height: 1.3;
  z-index: 35;
  pointer-events: none;
  box-shadow: 0 4px 14px rgba(0, 0, 0, 0.18);
  opacity: 0;
  visibility: hidden;
  transition: opacity 0.12s ease;
}
.ctx-ring-wrap:hover .ctx-tip {
  opacity: 1;
  visibility: visible;
}
.ctx-tip::after {
  content: '';
  position: absolute;
  left: 50%;
  top: 100%;
  transform: translateX(-50%);
  border: 6px solid transparent;
  border-top-color: #3a3a3a;
}
.ctx-tip.warn { background: #b45309; }
.ctx-tip.warn::after { border-top-color: #b45309; }
.ctx-tip.danger { background: #b91c1c; }
.ctx-tip.danger::after { border-top-color: #b91c1c; }
.model-pill {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  border: 0;
  background: transparent;
  color: var(--text-2);
  font-size: 12.5px;
  cursor: pointer;
  padding: 6px 10px;
  border-radius: 999px;
  max-width: 340px;
}
.model-pill:hover,
.model-pill.open {
  background: var(--surface-2);
  color: var(--text);
}
.model-pill-label {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.model-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--accent);
  flex-shrink: 0;
}
.model-badge-ico {
  flex-shrink: 0;
  width: 20px;
  height: 20px;
  border-radius: 50%;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-size: 9px;
  font-weight: 700;
  line-height: 1;
  overflow: hidden;
}
.model-badge-ico.is-logo {
  background: transparent;
  border-radius: 0;
}
.model-badge-ico :deep(svg) {
  width: 16px;
  height: 16px;
  display: block;
}
.caret-down { font-size: 10px; opacity: 0.7; flex-shrink: 0; }

.model-menu,
.host-menu,
.policy-menu {
  position: absolute;
  right: 0;
  bottom: calc(100% + 6px);
  width: min(320px, 78vw);
  max-height: min(360px, 50vh);
  display: flex;
  flex-direction: column;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: 10px;
  box-shadow: 0 4px 16px rgba(0, 0, 0, 0.08);
  z-index: 40;
  overflow: hidden;
}
/* 主机菜单：轻量锚定，别做成居中对话框观感 */
.host-menu {
  left: 0;
  right: auto;
  width: min(260px, 72vw);
  max-height: min(240px, 36vh);
  border-radius: 8px;
  box-shadow: 0 2px 10px rgba(0, 0, 0, 0.06);
}
.host-menu .model-menu-item { padding: 6px 8px; gap: 8px; }
.host-menu .model-menu-name { font-size: 12.5px; }
.host-menu .model-menu-sub { font-size: 10.5px; }
.host-menu .model-menu-list { padding: 4px; }
.host-menu .model-menu-foot {
  padding: 7px 10px;
  font-size: 12px;
}
.policy-menu {
  left: 0;
  right: auto;
  width: min(300px, 78vw);
}
.model-menu-list {
  overflow: auto;
  padding: 6px;
  flex: 1;
  min-height: 0;
}
.model-menu-item {
  width: 100%;
  display: flex;
  align-items: center;
  gap: 10px;
  text-align: left;
  border: 0;
  background: transparent;
  color: var(--text);
  padding: 8px 10px;
  border-radius: 8px;
  cursor: pointer;
}
/* 不用 inset-bg：浅色主题里它几乎是白，叠在 surface 上看不见悬停 */
.model-menu-item:hover { background: var(--surface-2); }
.model-menu-item.on {
  background: var(--surface-2);
  box-shadow: inset 0 0 0 1px var(--border-strong);
}
.model-menu-text {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 1px;
}
.model-menu-name {
  font-size: 13px;
  font-weight: 500;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.model-menu-sub {
  font-size: 11px;
  color: var(--text-3);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.model-menu-check {
  color: #16a34a;
  font-size: 14px;
  font-weight: 700;
  flex-shrink: 0;
}
.model-menu-empty {
  margin: 0;
  padding: 16px 14px;
  font-size: 12.5px;
  color: var(--text-3);
}
.model-menu-foot {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  border: 0;
  border-top: 1px solid var(--border);
  background: var(--surface);
  color: var(--text-2);
  font-size: 12.5px;
  padding: 10px 14px;
  cursor: pointer;
  text-align: left;
}
.model-menu-foot:hover {
  background: var(--surface-2);
  color: var(--text);
}
.send-btn {
  width: 34px;
  height: 34px;
  border-radius: 10px;
  border: 0;
  background: var(--accent);
  color: #fff;
  cursor: pointer;
  line-height: 1;
  flex-shrink: 0;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  padding: 0;
}
.send-btn .send-ico {
  display: block;
}
.send-btn:disabled {
  opacity: 1;
  cursor: not-allowed;
  background: #c4c8ce;
  color: #fff;
}
.send-btn.stop-btn {
  background: var(--text);
}
.send-btn.stop-btn:hover {
  background: var(--danger);
}
.stop-ico {
  width: 11px;
  height: 11px;
  border-radius: 2px;
  background: #fff;
}

.meta-ico { flex-shrink: 0; }
.meta-label {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.thinking-placeholder {
  margin-top: 4px;
}
.thinking-placeholder .asst-status {
  display: inline-flex;
  align-items: center;
  gap: 1px;
}
.thinking-dots i {
  animation: blink 1.2s infinite;
  opacity: 0.2;
}
.thinking-dots i:nth-child(2) { animation-delay: 0.2s; }
.thinking-dots i:nth-child(3) { animation-delay: 0.4s; }
@keyframes blink {
  0%, 100% { opacity: 0.2; }
  50% { opacity: 1; }
}

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
  width: min(420px, calc(100% - 60px));
  box-shadow: var(--shadow);
}
.modal h3 { margin: 0 0 8px; font-size: 15px; }
.picker-hint { margin: 0 0 14px; font-size: 12px; color: var(--text-3); }
.picker-empty { font-size: 13px; color: var(--text-3); margin: 0 0 12px; }
.btn-group {
  display: flex;
  flex-direction: column;
  gap: 8px;
  max-height: 40vh;
  overflow: auto;
}
.pick-btn {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 2px;
  text-align: left;
  padding: 10px 12px;
  border-radius: 10px;
  border: 1px solid var(--border);
  background: var(--surface-2);
  cursor: pointer;
  color: var(--text);
}
.pick-btn b { font-size: 13px; }
.pick-btn span { font-size: 11px; color: var(--text-3); }
.pick-btn:hover { border-color: var(--accent); }
.pick-btn.on {
  border-color: var(--accent);
  background: color-mix(in srgb, var(--accent) 12%, var(--surface));
}
.modal-actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  margin-top: 14px;
}

/* 审批卡：落在消息列里（文档流），不是盖在输入框上的浮层 */
.approval.in-feed {
  margin: 8px 0 16px;
  border: 1px solid var(--warn);
  background: var(--warn-bg);
  border-radius: 12px;
  padding: 12px 14px;
  display: flex;
  flex-direction: column;
  gap: 0;
}
.approval-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
  flex-wrap: wrap;
}
.approval-cmd {
  background: var(--surface);
  border-radius: 7px;
  padding: 8px 10px;
  color: var(--text);
  max-height: 168px;
  overflow: auto;
  margin: 0;
  font-size: 12.5px;
  white-space: pre-wrap;
  word-break: break-word;
}
.approval-reason { font-size: 12px; color: var(--warn); margin-top: 6px; }
.approval-actions { display: flex; gap: 8px; flex-wrap: wrap; margin-top: 10px; }
.approval-countdown { font-size: 12px; white-space: nowrap; }
.approval-countdown.urgent { color: var(--warn); font-weight: 600; }

/* 会话时间线：用户右气泡 / 助手左列（WorkBuddy 风） */
.entry { margin-bottom: 18px; }
.msg.user {
  display: flex;
  justify-content: flex-end;
}
.msg.user .msg-col {
  display: flex;
  flex-direction: column;
  align-items: flex-end;
  gap: 6px;
  max-width: min(85%, 520px);
}
.msg.user .bubble {
  width: 100%;
  background: var(--surface-2);
  color: var(--text);
  border-radius: 16px 16px 4px 16px;
  padding: 10px 14px;
  font-size: 14px;
  line-height: 1.55;
  white-space: pre-wrap;
  word-break: break-word;
}
.msg-footer {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 4px 8px;
  min-height: 22px;
}
.asst-footer {
  margin-top: 6px;
}
.msg-actions {
  display: flex;
  align-items: center;
  gap: 2px;
  min-height: 22px;
  transition: opacity 0.12s;
}
/* 精细指针设备：默认隐藏时间和操作钮，悬停 / 焦点 / 刚复制时再显示。 */
@media (hover: hover) and (pointer: fine) {
  .msg-actions {
    opacity: 0;
  }
  .msg.user:hover .msg-actions,
  .msg.assistant:hover .msg-actions,
  .msg-actions:focus-within,
  .msg-actions.revealed {
    opacity: 1;
  }
}
.msg-time {
  font-size: 11px;
  color: var(--text-3);
  margin-right: 4px;
  font-variant-numeric: tabular-nums;
  user-select: none;
}
.msg-tokens {
  font-size: 11px;
  color: var(--text-3);
  font-variant-numeric: tabular-nums;
  user-select: none;
}
.msg-act {
  width: 26px;
  min-height: 26px;
  padding: 0;
  border: none;
  border-radius: 6px;
  background: transparent;
  color: var(--text-3);
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  justify-content: center;
}
.msg-act:hover:not(:disabled) {
  background: var(--hover-overlay);
  color: var(--text);
  border-color: transparent;
}
.msg-act:disabled {
  opacity: 0.28;
  cursor: not-allowed;
}
.msg.assistant {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 8px;
  max-width: 100%;
}
.asst-head {
  display: flex;
  align-items: center;
  gap: 8px;
}
.asst-avatar {
  width: 28px;
  height: 28px;
  border-radius: 8px;
  flex-shrink: 0;
  object-fit: cover;
  display: block;
  background: #0B1220;
}
.asst-meta {
  display: flex;
  flex-direction: column;
  gap: 1px;
  min-width: 0;
}
.asst-name {
  font-size: 13px;
  font-weight: 600;
  color: var(--text);
}
.asst-status {
  font-size: 11px;
  color: var(--text-3);
}
.asst-body {
  width: 100%;
  padding-left: var(--asst-indent);
  box-sizing: border-box;
}
.asst-text {
  font-size: 14px;
  line-height: 1.6;
  word-break: break-word;
  color: var(--text);
}
.asst-text .md-body > :first-child { margin-top: 0; }
.asst-text .md-body > :last-child { margin-bottom: 0; }
.asst-text .md-body p { margin: 0.55em 0; }
.asst-text .md-body h1,
.asst-text .md-body h2,
.asst-text .md-body h3,
.asst-text .md-body h4 {
  margin: 0.9em 0 0.4em;
  font-weight: 650;
  line-height: 1.35;
  color: var(--text);
}
.asst-text .md-body h1 { font-size: 1.25em; }
.asst-text .md-body h2 { font-size: 1.12em; }
.asst-text .md-body h3,
.asst-text .md-body h4 { font-size: 1.02em; }
.asst-text .md-body ul,
.asst-text .md-body ol {
  margin: 0.45em 0;
  padding-left: 1.35em;
}
.asst-text .md-body li { margin: 0.2em 0; }
.asst-text .md-body li > ul,
.asst-text .md-body li > ol { margin: 0.15em 0; }
.asst-text .md-body strong { font-weight: 650; }
.asst-text .md-body a {
  color: var(--accent);
  text-decoration: none;
}
.asst-text .md-body a:hover { text-decoration: underline; }
.asst-text .md-body code {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 0.9em;
  padding: 0.1em 0.35em;
  border-radius: 4px;
  background: var(--inset-bg);
  border: 1px solid var(--border);
}
.asst-text .md-body pre {
  margin: 0.55em 0;
  padding: 10px 12px;
  overflow-x: auto;
  border-radius: 8px;
  background: var(--inset-bg);
  border: 1px solid var(--border);
  font-size: 12.5px;
  line-height: 1.5;
}
.asst-text .md-body pre code {
  padding: 0;
  border: none;
  background: transparent;
  font-size: inherit;
}
.asst-text .md-body blockquote {
  margin: 0.55em 0;
  padding: 0.15em 0 0.15em 0.85em;
  border-left: 3px solid var(--border);
  color: var(--text-2);
}
.asst-text .md-body table {
  width: 100%;
  margin: 0.65em 0;
  border-collapse: collapse;
  font-size: 13px;
  display: block;
  overflow-x: auto;
}
.asst-text .md-body th,
.asst-text .md-body td {
  border: 1px solid var(--border);
  padding: 6px 10px;
  text-align: left;
  vertical-align: top;
}
.asst-text .md-body th {
  background: var(--inset-bg);
  font-weight: 600;
  white-space: nowrap;
}
.asst-text .md-body hr {
  margin: 0.85em 0;
  border: none;
  border-top: 1px solid var(--border);
}
.msg.error .error-text { color: #b91c1c; padding-left: 0; }
.msg.system {
  display: flex;
  justify-content: center;
}
.system-text {
  font-size: 12px;
  color: var(--text-3);
  text-align: center;
}
.caret {
  display: inline-block;
  width: 6px;
  height: 1em;
  background: var(--accent);
  margin-left: 2px;
  vertical-align: text-bottom;
  animation: blink 1s step-end infinite;
}
.reasoning {
  margin-bottom: 8px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--inset-bg);
  overflow: hidden;
}
.reasoning-head {
  padding: 6px 10px;
  font-size: 12px;
  color: var(--text-3);
  cursor: pointer;
  display: flex;
  justify-content: space-between;
  user-select: none;
}
.reasoning-body {
  padding: 0 10px 8px;
  font-size: 12px;
  color: var(--text-2);
  white-space: pre-wrap;
  line-height: 1.55;
}
.reasoning-wait {
  color: var(--text-3);
  font-style: italic;
}
.tool-group {
  margin-left: var(--asst-indent);
  border: 1px solid var(--border);
  border-radius: 10px;
  background: var(--surface);
  overflow: hidden;
}
.tool-group-head {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 12px;
  cursor: pointer;
  font-size: 12.5px;
  color: var(--text-2);
  user-select: none;
}
.tool-group-body { border-top: 1px solid var(--border); padding: 8px 12px; }
.tool { margin-bottom: 8px; }
.tool-head { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; font-size: 12px; }
.tool-name { font-weight: 600; }
.tool-cmd, .result pre, .injection-cmd {
  margin: 6px 0 0;
  padding: 8px 10px;
  background: var(--inset-bg);
  border-radius: 7px;
  font-size: 12px;
  overflow: auto;
  max-height: 240px;
}
.tool-reason { margin-top: 4px; font-size: 12px; color: var(--text-3); }
.result-head { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; font-size: 12px; }
.injection {
  border: 1px solid #f59e0b;
  background: color-mix(in srgb, #f59e0b 10%, var(--surface));
  border-radius: 10px;
  padding: 10px 12px;
}
.injection-title { font-weight: 600; font-size: 13px; margin-bottom: 4px; }
.injection-body, .injection-hint { font-size: 12px; color: var(--text-2); }
.muted { color: var(--text-3); }
.tiny { font-size: 11px; }
.right { margin-left: auto; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
.badge {
  display: inline-block;
  font-size: 11px;
  padding: 1px 7px;
  border-radius: 999px;
  background: var(--inset-bg);
  color: var(--text-2);
}
.badge.ok { background: color-mix(in srgb, #16a34a 16%, var(--surface)); color: #15803d; }
.badge.warn { background: color-mix(in srgb, #d97706 16%, var(--surface)); color: #b45309; }
.badge.danger { background: color-mix(in srgb, #dc2626 16%, var(--surface)); color: #b91c1c; }
.badge.neutral { opacity: 0.85; }
</style>
