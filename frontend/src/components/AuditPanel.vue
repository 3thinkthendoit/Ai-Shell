<script setup>
import { computed, onMounted, ref } from 'vue'
import { push } from '../store'

const entries = ref([])
const size = ref(0)
const segments = ref(1)
const err = ref('')
const loading = ref(false)
const verifying = ref(false)
const verifyResult = ref(null)
const filter = ref('all')

const kinds = [
  { key: 'all', label: '全部' },
  { key: 'tool', label: '工具调用' },
  { key: 'direct', label: '人工命令' },
  { key: 'terminal', label: '交互终端' },
  { key: 'agent_run', label: '会话' },
  { key: 'host', label: '主机变更' },
  { key: 'policy', label: '策略变更' },
  { key: 'llm', label: 'LLM 配置' },
  { key: 'retention', label: '日志轮转' },
  { key: 'system', label: '系统' }
]

const kindLabel = k =>
  ({
    agent_run: '会话',
    tool: '工具调用',
    direct: '人工命令',
    terminal: '交互终端',
    host: '主机变更',
    llm: 'LLM 配置',
    policy: '策略变更',
    retention: '日志轮转',
    system: '系统'
  }[k] || k)

const decisionLabel = d =>
  ({ allow: '自动放行', confirm: '需确认', deny: '硬拒绝', human: '人工操作' }[d] || d)

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
      push({ kind: 'system', content: `审计日志已导出到 ${dest}` })
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
  return d.toLocaleString('zh-CN', { hour12: false })
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
    <header class="panel-head">
      <div>
        <h2>审计日志</h2>
        <p class="muted">
          每一次对远端主机的操作、每一次人工批准或拒绝、每一次配置变更都在这里留下记录。
          日志逐条用主密钥加密落盘，并以哈希链串联 —— 任何删改都会被检出。
          达到体积上限后自动轮转归档，最旧的段按保留策略丢弃，丢弃动作本身也会记入链中。
        </p>
      </div>
      <div class="head-actions">
        <button :disabled="loading" @click="load">{{ loading ? '刷新中…' : '刷新' }}</button>
        <button :disabled="verifying" @click="verify">
          {{ verifying ? '校验中…' : '校验完整性' }}
        </button>
        <button @click="exportPlain">导出明文</button>
      </div>
    </header>

    <div v-if="err" class="card error-card">{{ err }}</div>

    <div v-if="verifyResult" class="card verify" :class="verifyResult.ok ? 'ok' : 'bad'">
      <b>{{ verifyResult.ok ? '哈希链完整' : '校验未通过' }}</b>
      <span>{{ verifyResult.message }}</span>
      <div v-if="verifyResult.truncated" class="verify-note">
        注意：校验起点是第 {{ verifyResult.startSeq }} 条，更早的记录已不在。
        若下方能看到「日志轮转」记录，说明是本程序的保留策略丢弃；
        但看不到也不能据此认定被篡改 —— 那条声明本身也可能已被后续轮转丢弃。
      </div>
    </div>

    <div class="meta">
      <span class="muted">共 {{ entries.length }} 条 · {{ fmtSize(size) }}</span>
      <span v-if="segments > 1" class="muted">
        （已轮转为 {{ segments }} 个段，最旧的段会按保留策略丢弃）
      </span>
      <span v-else class="muted">（单段，达到上限后自动轮转归档）</span>
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

    <div v-if="!shown.length && !err" class="card muted">暂无记录。</div>

    <div v-else class="list">
      <div v-for="e in shown.slice().reverse()" :key="e.seq" class="row">
        <div class="row-top">
          <span class="seq mono">#{{ e.seq }}</span>
          <span class="badge neutral">{{ kindLabel(e.kind) }}</span>
          <span v-if="e.decision" class="badge" :class="decisionClass(e.decision)">
            {{ decisionLabel(e.decision) }}
          </span>
          <span v-if="e.approved === false" class="badge danger">已拒绝</span>
          <span v-if="e.approved === true" class="badge ok">已批准</span>
          <span v-if="e.injection && e.injection.length" class="badge danger">
            注入 {{ e.injection.length }}
          </span>
          <span v-if="e.redacted" class="badge warn">脱敏 {{ e.redacted }}</span>
          <span class="time muted">{{ fmtTime(e.time) }}</span>
        </div>

        <div class="row-body">
          <span v-if="e.hostName" class="host mono">{{ e.hostName }}</span>
          <span v-if="e.tool" class="tool mono">{{ e.tool }}</span>
          <span v-if="e.command" class="cmd mono">{{ e.command }}</span>
        </div>

        <div class="row-foot">
          <span v-if="e.exitCode !== undefined && e.exitCode !== null" class="muted">
            退出码 {{ e.exitCode }}
          </span>
          <span v-if="e.durationMs" class="muted">{{ e.durationMs }}ms</span>
          <span v-if="e.rule" class="muted mono">规则 {{ e.rule }}</span>
          <span v-if="e.note" class="note">{{ e.note }}</span>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.panel { padding: 22px 26px; overflow-y: auto; height: 100%; }
.panel-head { display: flex; justify-content: space-between; align-items: flex-start; gap: 20px; margin-bottom: 16px; }
.panel-head h2 { font-size: 15px; margin: 0 0 6px; }
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

.meta { display: flex; gap: 10px; font-size: 12px; margin-bottom: 10px; }

.filters { display: flex; flex-wrap: wrap; gap: 6px; margin-bottom: 14px; }
.filters .on { background: var(--accent-bg); color: var(--accent); border-color: var(--accent); font-weight: 500; }

.list { display: flex; flex-direction: column; gap: 8px; }

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
