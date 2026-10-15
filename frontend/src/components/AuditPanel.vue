<script setup>
import { computed, onMounted, ref } from 'vue'
import { push } from '../store'
import { t, i18n } from '../i18n'

const entries = ref([])
const size = ref(0)
const segments = ref(1)
const err = ref('')
const loading = ref(false)
const verifying = ref(false)
const verifyResult = ref(null)
const filter = ref('all')

// 筛选按钮（computed：语言切换时标签跟着换）。key 值与后端审计 kind 一致，不翻。
const kinds = computed(() => [
  { key: 'all', label: t('audit.kinds.all') },
  { key: 'tool', label: t('audit.kinds.tool') },
  { key: 'direct', label: t('audit.kinds.direct') },
  { key: 'terminal', label: t('audit.kinds.terminal') },
  { key: 'agent_run', label: t('audit.kinds.agentRun') },
  { key: 'host', label: t('audit.kinds.host') },
  { key: 'policy', label: t('audit.kinds.policy') },
  { key: 'llm', label: t('audit.kinds.llm') },
  { key: 'retention', label: t('audit.kinds.retention') },
  { key: 'system', label: t('audit.kinds.system') }
])

const KIND_KEYS = {
  agent_run: 'audit.kinds.agentRun',
  tool: 'audit.kinds.tool',
  direct: 'audit.kinds.direct',
  terminal: 'audit.kinds.terminal',
  host: 'audit.kinds.host',
  llm: 'audit.kinds.llm',
  policy: 'audit.kinds.policy',
  retention: 'audit.kinds.retention',
  system: 'audit.kinds.system'
}
const kindLabel = k => (KIND_KEYS[k] ? t(KIND_KEYS[k]) : k)

// 裁决徽章：allow/confirm/deny 与 timeline 域同一套语义，复用其键；human 是
// 审计页独有的（人工直接敲的命令）。值本身（allow/confirm/deny/human）是
// 后端字段，不翻。
const decisionLabel = d =>
  ({
    allow: t('tl.decision.allow'),
    confirm: t('tl.decision.confirm'),
    deny: t('tl.decision.deny'),
    human: t('audit.decision.human')
  }[d] || d)

const decisionClass = d =>
  ({ allow: 'ok', confirm: 'warn', deny: 'danger', human: 'neutral' }[d] || 'neutral')

const shown = computed(() =>
  filter.value === 'all' ? entries.value : entries.value.filter(e => e.kind === filter.value)
)

async function load() {
  loading.value = true
  err.value = ''
  try {
    const v = await window.go.main.App.ListAudit(500)
    if (v.error) {
      err.value = v.error
    } else {
      entries.value = v.entries || []
      size.value = v.size || 0
      segments.value = v.segments || 1
    }
  } catch (e) {
    err.value = String(e)
  } finally {
    loading.value = false
  }
}

async function verify() {
  verifying.value = true
  verifyResult.value = null
  try {
    verifyResult.value = await window.go.main.App.VerifyAudit()
  } catch (e) {
    verifyResult.value = { ok: false, message: String(e) }
  } finally {
    verifying.value = false
  }
}

async function exportPlain() {
  try {
    const dest = await window.go.main.App.ExportAudit()
    if (dest) {
      push({ kind: 'system', content: t('audit.exported', { dest }) })
      await load()
    }
  } catch (e) {
    push({ kind: 'error', content: String(e) })
  }
}

function fmtTime(s) {
  if (!s) return ''
  const d = new Date(s)
  if (isNaN(d.getTime())) return s
  return d.toLocaleString(i18n.locale === 'en' ? 'en-US' : 'zh-CN', { hour12: false })
}

function fmtSize(n) {
  if (!n) return '0 B'
  if (n < 1024) return n + ' B'
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB'
  return (n / 1024 / 1024).toFixed(1) + ' MB'
}

onMounted(load)
</script>

<template>
  <div class="panel">
    <div class="panel-inner">
      <header class="panel-head">
        <div>
          <h2>{{ t('audit.title') }}</h2>
          <p class="muted">
            {{ t('audit.headHint') }}
          </p>
        </div>
        <div class="head-actions">
          <button :disabled="loading" @click="load">{{ loading ? t('audit.refreshing') : t('audit.refresh') }}</button>
          <button :disabled="verifying" @click="verify">
            {{ verifying ? t('audit.verifying') : t('audit.verify') }}
          </button>
          <button @click="exportPlain">{{ t('audit.export') }}</button>
        </div>
      </header>

      <div v-if="err" class="card error-card">{{ err }}</div>

      <div v-if="verifyResult" class="card verify" :class="verifyResult.ok ? 'ok' : 'bad'">
        <b>{{ verifyResult.ok ? t('audit.verify.ok') : t('audit.verify.bad') }}</b>
        <span>{{ verifyResult.message }}</span>
        <div v-if="verifyResult.truncated" class="verify-note">
          {{ t('audit.verify.note.a', { seq: verifyResult.startSeq }) }}
          {{ t('audit.verify.note.b') }}
          {{ t('audit.verify.note.c') }}
        </div>
      </div>

      <!-- 概览行 + 筛选器合成一条工具栏：左边是规模，右边是筛选。 -->
      <div class="toolbar">
        <div class="meta">
          <span class="muted">{{ t('audit.meta.total', { n: entries.length }) }} · {{ fmtSize(size) }}</span>
          <span v-if="segments > 1" class="muted">
            {{ t('audit.meta.segments', { n: segments }) }}
          </span>
          <span v-else class="muted">{{ t('audit.meta.single') }}</span>
        </div>
        <div class="filters">
          <button
            v-for="k in kinds"
            :key="k.key"
            class="sm"
            :class="{ on: filter === k.key }"
            @click="filter = k.key"
          >
            {{ k.label }}
          </button>
        </div>
      </div>

      <div v-if="!shown.length && !err" class="card muted empty">{{ t('audit.empty') }}</div>

      <div v-else class="list">
        <div v-for="e in shown.slice().reverse()" :key="e.seq" class="row">
          <div class="row-top">
            <span class="seq mono">#{{ e.seq }}</span>
            <span class="badge neutral">{{ kindLabel(e.kind) }}</span>
            <span v-if="e.decision" class="badge" :class="decisionClass(e.decision)">
              {{ decisionLabel(e.decision) }}
            </span>
            <span v-if="e.approved === false" class="badge danger">{{ t('audit.rejected') }}</span>
            <span v-if="e.approved === true" class="badge ok">{{ t('audit.approved') }}</span>
            <span v-if="e.injection && e.injection.length" class="badge danger">
              {{ t('audit.injection', { n: e.injection.length }) }}
            </span>
            <span v-if="e.redacted" class="badge warn">{{ t('audit.redacted', { n: e.redacted }) }}</span>
            <span class="time muted">{{ fmtTime(e.time) }}</span>
          </div>

          <div class="row-body">
            <span v-if="e.hostName" class="host mono">{{ e.hostName }}</span>
            <span v-if="e.tool" class="tool mono">{{ e.tool }}</span>
            <span v-if="e.command" class="cmd mono">{{ e.command }}</span>
          </div>

          <div class="row-foot">
            <span v-if="e.exitCode !== undefined && e.exitCode !== null" class="muted">
              {{ t('console.exitCode', { n: e.exitCode }) }}
            </span>
            <span v-if="e.durationMs" class="muted">{{ e.durationMs }}ms</span>
            <span v-if="e.rule" class="muted mono">{{ t('audit.rule', { r: e.rule }) }}</span>
            <span v-if="e.note" class="note">{{ e.note }}</span>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.panel { padding: 24px 28px 36px; overflow-y: auto; height: 100%; }
/* 内容列：窗口再宽也不把卡片 / 命令块拉成一条 —— 保持稳定的阅读宽度。 */
.panel-inner { max-width: 900px; }

.panel-head { display: flex; justify-content: space-between; align-items: flex-start; gap: 20px; margin-bottom: 18px; }
.panel-head h2 { font-size: 16px; margin: 0 0 6px; }
.panel-head p { margin: 0; font-size: 12px; line-height: 1.7; max-width: 620px; }
.head-actions { display: flex; gap: 8px; flex-shrink: 0; }

.error-card { color: var(--danger); background: var(--danger-bg); border-color: var(--danger); margin-bottom: 12px; }

.verify { margin-bottom: 12px; display: flex; gap: 10px; align-items: baseline; font-size: 12px; flex-wrap: wrap; }
.verify.ok { background: var(--ok-bg); border-color: var(--ok); color: var(--ok); }
.verify.bad { background: var(--danger-bg); border-color: var(--danger); color: var(--danger); }
.verify-note {
  flex-basis: 100%;
  margin-top: 6px;
  padding-top: 6px;
  border-top: 1px solid currentColor;
  opacity: 0.85;
  line-height: 1.65;
}

/* 工具栏：规模信息在左、筛选器在右，一行放下；放不下时筛选器整段换行。 */
.toolbar { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; margin-bottom: 14px; }
.meta { display: flex; gap: 4px; font-size: 12px; }
.filters { display: flex; flex-wrap: wrap; gap: 6px; margin-left: auto; }
.filters .on { background: var(--accent-bg); color: var(--accent); border-color: var(--accent); font-weight: 500; }

.list { display: flex; flex-direction: column; gap: 8px; }
.empty { padding: 28px 16px; text-align: center; }

.row {
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 10px 12px;
  background: var(--surface);
}
.row-top { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.seq { font-size: 11px; color: var(--text-3); }
.time { margin-left: auto; font-size: 11px; }

.row-body { display: flex; gap: 10px; flex-wrap: wrap; margin-top: 7px; align-items: baseline; }
.host { font-size: 12px; color: var(--purple); }
.tool { font-size: 12px; color: var(--text-2); }
.cmd {
  font-size: 12px;
  background: var(--surface-2);
  border-radius: 6px;
  padding: 2px 7px;
  word-break: break-all;
}

.row-foot { display: flex; gap: 12px; flex-wrap: wrap; margin-top: 6px; font-size: 11px; }
.note { color: var(--text-3); }

.badge { font-size: 11px; padding: 1px 7px; border-radius: 20px; }
.badge.ok { background: var(--ok-bg); color: var(--ok); }
.badge.warn { background: var(--warn-bg); color: var(--warn); }
.badge.danger { background: var(--danger-bg); color: var(--danger); }
.badge.neutral { background: var(--surface-2); color: var(--text-2); }
</style>
