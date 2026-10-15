<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import {
  store, ask, stop, approve, clearLog, clearSession, compactSession,
  switchProfile, push, openFileManager,
  noteConversationActivity, onFocusSessionChanged, shouldShowSessionChrome
} from '../store'
import { providerBadge, providerTermBadge } from '../providerLogos'
import { t } from '../i18n'

// Agent 对话页：首页对话框 + 气泡时间线（思考过程 / 工具调用）。
// 终端页仍保留原有 Agent+Shell 分流；本组件是对话优先的第二条入口。

const emit = defineEmits(['page', 'term', 'cmd-detail'])

const draft = ref('')
const inputEl = ref(null)
const logEl = ref(null)
const showHostPicker = ref(false)
const showModelPicker = ref(false)
const showPolicyPicker = ref(false)
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
// 营销首页只在「未选主机且无气泡」时出现。已选主机（含从终端切回）时
// 用贴底工作区，避免终端敲过命令却没有 chat entries 时又掉回欢迎首页。
const showHomeHero = computed(() => !hasMessages.value && !store.currentHostId)
const homeDocked = computed(() => hasMessages.value || !!store.currentHostId)
// 工具条：见过对话后清空记录仍保留；空首页仅选主机不因旧 turns 冒出来。
const showChatChrome = computed(() => shouldShowSessionChrome())
watch(() => store.entries.length, noteConversationActivity, { immediate: true })
watch(() => [store.currentHostId, store.currentSessionId], onFocusSessionChanged)
const streamingNow = computed(() => store.entries.some(e => e.streaming))

const decisionLabel = d => ({ allow: t('tl.decision.allow'), confirm: t('tl.decision.confirm'), deny: t('tl.decision.deny') }[d] || d)
const decisionClass = d => ({ allow: 'ok', confirm: 'warn', deny: 'danger' }[d] || '')
const statusLabel = s => ({
  pending: t('console.toolStatus.pending'), running: t('console.toolStatus.running'),
  done: t('console.toolStatus.done'), denied: t('console.toolStatus.denied'),
  error: t('console.toolStatus.error')
}[s] || s)

function archiveBadge(e) {
  if (e && e.badge) return e.badge
  if (e && e.model) {
    try { return providerTermBadge({ model: e.model, name: e.model }) } catch { return null }
  }
  return null
}

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
  if (store.running) return
  const next = !showModelPicker.value
  closeMetaMenus()
  showModelPicker.value = next
}

function toggleHostMenu() {
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
})

const currentHost = computed(() => store.hosts.find(h => h.id === store.currentHostId))
const workspaceLabel = computed(() => {
  const h = currentHost.value
  if (!h) return t('chat.workspace')
  return h.name
})
const workspaceTitle = computed(() => {
  const h = currentHost.value
  if (!h) return t('chat.workspace')
  return `${h.name} · ${h.user}@${h.addr}`
})

const policyLabel = computed(() =>
  store.policy.mode === 'whitelist' ? t('chat.policy.whitelist') : t('chat.policy.manual')
)
const policyTitle = computed(() => `${t('chat.policy')} · ${policyLabel.value}`)

const ctxUsage = computed(() => {
  const { tokens, window: win, messages } = store.contextUsage
  if (!tokens) return { tip: '', level: '', title: '' }
  const used = fmtTokens(tokens)
  if (!win) {
    return {
      tip: t('console.ctx.tipNoWin', { used }),
      level: 'unknown',
      title: t('console.ctx.unknown')
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
    })
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
  const p = store.llmProfiles.find(x => x.id === id)
  store.selectedProfileId = id
  try {
    await switchProfile(id)
  } catch {
    return
  }
  push({
    kind: 'system',
    content: t('console.modelSwitched', { name: p ? p.name : id })
  })
}

function applyChip(chip) {
  draft.value = t(chip.promptKey)
  nextTick(() => inputEl.value?.focus())
}

function onAttach() {
  if (!store.currentHostId) {
    showHostPicker.value = true
    return
  }
  openFileManager(store.currentHostId, store.currentSessionId)
}

function ensureReadyToSend() {
  if (!store.currentHostId) {
    showHostPicker.value = true
    return false
  }
  if (!activeProfileId.value) {
    showModelPicker.value = true
    return false
  }
  return true
}

async function doSend() {
  const text = draft.value.trim()
  if (!text) return
  if (!ensureReadyToSend()) return
  // 仅在 ask 真正发起成功后清草稿；busy / 校验失败 / 同步拒绝都保留输入。
  const ok = await ask(text)
  if (ok) draft.value = ''
  scrollLog()
}

function onSendClick() {
  void doSend()
}

function onComposerKeydown(e) {
  if (e.isComposing || e.keyCode === 229) return
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault()
    void doSend()
  }
}

function pickHost(id) {
  store.currentHostId = id
  showHostPicker.value = false
  nextTick(() => {
    if (draft.value.trim() && activeProfileId.value) void doSend()
  })
}

function pickModel(id) {
  showModelPicker.value = false
  void onSwitchModel(id).then(() => {
    // 发送前因缺模型被拦住的：选完若草稿还在且已有主机，自动发出。
    if (draft.value.trim() && store.currentHostId) void doSend()
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
    push({
      kind: 'system',
      content: t('chat.policy.switched', {
        mode: mode === 'whitelist' ? t('chat.policy.whitelist') : t('chat.policy.manual')
      })
    })
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

async function onClearSession() {
  if (!store.currentHostId) {
    showHostPicker.value = true
    return
  }
  await clearSession(store.currentHostId, store.currentSessionId)
}

async function onCompactSession() {
  if (!store.currentHostId) {
    showHostPicker.value = true
    return
  }
  await compactSession(store.currentHostId, store.currentSessionId)
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
</script>

<template>
  <div class="chat" :class="{ empty: showHomeHero }">
    <!-- 消息区：气泡时间线 + 待审批卡（审批进消息流，不盖在输入框上） -->
    <div v-if="hasMessages || store.pending" class="log archive" ref="logEl">
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
          <div class="bubble">{{ it.content }}</div>
        </div>

        <div v-else-if="it.kind === 'assistant'" class="msg assistant">
          <div class="asst-head">
            <span
              v-if="archiveBadge(it)"
              class="asst-avatar"
              :style="{ color: archiveBadge(it).color, background: archiveBadge(it).color + '22' }"
              :title="archiveBadge(it).name"
            >{{ archiveBadge(it).mark }}</span>
            <span v-else class="asst-avatar fallback">AI</span>
            <div class="asst-meta">
              <span class="asst-name">{{ (archiveBadge(it) && archiveBadge(it).name) || it.model || 'Ai-Shell' }}</span>
              <span v-if="it.streaming" class="asst-status">{{ t('console.reasoning.thinking') }}</span>
              <span v-else-if="it.model && archiveBadge(it) && archiveBadge(it).name !== it.model" class="asst-status">{{ it.model }}</span>
            </div>
          </div>
          <div class="asst-body">
            <div v-if="it.reasoning" class="reasoning">
              <div class="reasoning-head" @click="it.reasoningOpen = !it.reasoningOpen">
                {{ it.streaming && !it.content ? t('console.reasoning.thinking') : t('console.reasoning.label') }}
                <span class="reasoning-arrow">{{ it.reasoningOpen ? '▾' : '▸' }}</span>
              </div>
              <div v-show="it.reasoningOpen" class="reasoning-body">{{ it.reasoning }}</div>
            </div>
            <div class="asst-text">
              {{ it.content }}<span v-if="it.streaming" class="caret"></span><span v-if="!it.content && !it.reasoning && !it.streaming" class="muted tiny">{{ t('console.emptyReply') }}</span>
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

      <div v-if="store.pending" class="approval in-feed">
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
      </div>
    </div>

    <!-- 空态：分类+快捷；对话中：只留输入框（参考 WorkBuddy 会话页） -->
    <div class="home" :class="{ docked: homeDocked, conversing: hasMessages }">
      <template v-if="showHomeHero">
        <h1 class="home-title">{{ t('chat.title') }}</h1>
        <p class="home-sub">{{ t('chat.subtitle') }}</p>
        <button type="button" class="home-term-link" @click="emit('term')">
          {{ t('chat.gotoTerm') }}
        </button>
      </template>

      <template v-if="!hasMessages">
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

              <div ref="hostMenuRoot" class="host-menu-wrap">
                <button
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
                <div v-if="showHostPicker" class="host-menu" role="listbox">
                  <div v-if="store.hosts.length" class="model-menu-list">
                    <button
                      v-for="h in store.hosts"
                      :key="h.id"
                      type="button"
                      class="model-menu-item"
                      role="option"
                      :aria-selected="h.id === store.currentHostId"
                      :class="{ on: h.id === store.currentHostId }"
                      @click="pickHost(h.id)"
                    >
                      <span class="model-menu-text">
                        <span class="model-menu-name">{{ h.name }}</span>
                        <span class="model-menu-sub">{{ h.user }}@{{ h.addr }}</span>
                      </span>
                      <span v-if="h.id === store.currentHostId" class="model-menu-check" aria-hidden="true">✓</span>
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
                  <span class="meta-label">{{ t('chat.policy') }}</span>
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
              <div ref="modelMenuRoot" class="model-menu-wrap">
                <!-- 上下文用量：浮在模型 pill 上方的深色 tip（WorkBuddy 同款） -->
                <div
                  v-if="ctxUsage.tip && !showModelPicker"
                  class="ctx-tip"
                  :class="ctxUsage.level"
                  :title="ctxUsage.title"
                >{{ ctxUsage.tip }}</div>
                <button
                  type="button"
                  class="model-pill"
                  :class="{ open: showModelPicker }"
                  :disabled="store.running"
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
              <button
                type="button"
                class="send-btn"
                :disabled="store.running || store.busy || !draft.trim()"
                :title="t('chat.send')"
                @click="onSendClick"
              >↑</button>
            </div>
          </div>
        </div>
      </div>

      <!-- 压缩/清空等：空首页藏起；见过对话后清空记录仍显示。用量已挂模型 pill 上方。 -->
      <div v-if="showChatChrome" class="chat-tools">
        <span
          v-if="store.running && !store.pending && !streamingNow"
          class="thinking-inline"
          aria-live="polite"
        >{{ t('chat.working') }}<span class="thinking-dots" aria-hidden="true"><i>.</i><i>.</i><i>.</i></span></span>
        <button class="sm" :disabled="store.compacting" :title="t('console.compact.title')" @click="onCompactSession">
          {{ store.compacting ? t('console.compact.busy') : t('console.compact.label') }}
        </button>
        <button class="sm" @click="onClearSession">{{ t('console.clearCtx') }}</button>
        <button class="sm" @click="clearLog" :disabled="store.running || store.busy">{{ t('console.clearLog') }}</button>
        <button v-if="store.running || store.busy" class="sm danger" @click="stop">{{ t('console.interrupt') }}</button>
      </div>
    </div>

  </div>
</template>

<style scoped>
.chat {
  /* 首页 80%；有对话后消息列 90% */
  --home-col: 80%;
  --chat-col: 90%;
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
.composer-input {
  width: 100%;
  border: 0;
  outline: none;
  resize: none;
  background: transparent;
  color: var(--text);
  font: inherit;
  font-size: 15px;
  line-height: 1.55;
  min-height: 64px;
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
.model-menu-wrap,
.host-menu-wrap,
.policy-menu-wrap {
  position: relative;
}
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
  bottom: calc(100% + 8px);
  width: min(320px, 78vw);
  max-height: min(360px, 50vh);
  display: flex;
  flex-direction: column;
  background: var(--surface);
  border: 1px solid var(--border-strong);
  border-radius: 12px;
  box-shadow: var(--shadow);
  z-index: 40;
  overflow: hidden;
}
.host-menu {
  left: 0;
  right: auto;
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
  width: 36px;
  height: 36px;
  border-radius: 50%;
  border: 0;
  background: var(--accent);
  color: #fff;
  font-size: 16px;
  cursor: pointer;
  line-height: 1;
  flex-shrink: 0;
}
.send-btn:disabled {
  opacity: 0.35;
  cursor: not-allowed;
  background: var(--text-3);
}

.meta-ico { flex-shrink: 0; }
.meta-label {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.chat-tools {
  width: var(--chat-col);
  max-width: 100%;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  margin-top: 4px;
}
.thinking-inline {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  font-size: 12px;
  color: var(--text-3);
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
.msg.user .bubble {
  max-width: min(85%, 520px);
  background: var(--surface-2);
  color: var(--text);
  border-radius: 16px 16px 4px 16px;
  padding: 10px 14px;
  font-size: 14px;
  line-height: 1.55;
  white-space: pre-wrap;
  word-break: break-word;
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
  border-radius: 50%;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-size: 10px;
  font-weight: 700;
  flex-shrink: 0;
  background: var(--surface-2);
  color: var(--accent);
}
.asst-avatar.fallback { font-size: 11px; }
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
  padding-left: 36px;
}
.asst-text {
  font-size: 14px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
  color: var(--text);
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
}
.tool-group {
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
