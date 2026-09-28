<script setup>
import { computed, nextTick, reactive, ref, watch } from 'vue'
import {
  store, ask, approve, stop, clearLog, clearSession, compactSession, push,
  selectSession, createSession, renameSession, deleteSession, refreshSessions, runShell
} from '../store'
import { classifyInput } from '../inputRoute'
import { parseBareCd, extractCwd, promptParts } from '../term'
import InteractiveTerminal from './InteractiveTerminal.vue'

// agent = Agent会话（终端风：自然语言→Agent，shell→策略后 Exec）
// terminal = 完整交互终端（PTY，绕过策略）
const mode = ref('agent')
const draft = ref('')
const logEl = ref(null)
const inputEl = ref(null)

// 每台主机独立跟踪 cwd（Exec 无状态）。切主机再切回来目录还在。
const cwds = reactive({})
const currentHost = computed(() => store.hosts.find(h => h.id === store.currentHostId))
const cwd = computed(() => cwds[store.currentHostId] || '~')
const prompt = computed(() => promptParts(currentHost.value, cwd.value))

const sessionLocked = computed(() => store.running || store.busy || !!store.pending)

watch(() => store.currentHostId, async id => {
  store.currentSessionId = ''
  await refreshSessions(id)
  await nextTick()
  if (logEl.value) logEl.value.scrollTop = logEl.value.scrollHeight
  focusInput()
})

watch(() => store.currentSessionId, async () => {
  await nextTick()
  if (logEl.value) logEl.value.scrollTop = logEl.value.scrollHeight
})

const streamingNow = computed(() => store.entries.some(e => e.streaming))

const scrollKey = computed(() => {
  const n = store.entries.length
  if (!n) return '0'
  const last = store.entries[n - 1]
  return `${n}:${last.content ? last.content.length : 0}:${last.pending ? 1 : 0}`
})

const decisionLabel = d => ({ allow: '自动放行', confirm: '需确认', deny: '硬拒绝' }[d] || d)
const decisionClass = d => ({ allow: 'ok', confirm: 'warn', deny: 'danger' }[d] || '')

const statusLabel = s => ({
  pending: '待批准', running: '执行中', done: '已完成', denied: '已拒绝', error: '出错'
}[s] || s)

watch(scrollKey, async () => {
  await nextTick()
  if (logEl.value) logEl.value.scrollTop = logEl.value.scrollHeight
})

watch(() => store.pending, async () => {
  await nextTick()
  if (logEl.value) logEl.value.scrollTop = logEl.value.scrollHeight
})

function focusInput() {
  nextTick(() => {
    if (inputEl.value && !sessionLocked.value) inputEl.value.focus()
  })
}

async function send() {
  const text = draft.value.trim()
  if (!text || sessionLocked.value) return
  if (!store.currentHostId) {
    push({ kind: 'error', content: '请先选择一台主机。' })
    return
  }

  const route = classifyInput(text)
  draft.value = ''

  if (route.kind === 'agent') {
    if (!route.text) {
      push({ kind: 'error', content: '请输入要问 Agent 的内容（或直接敲一条 shell 命令）。' })
      return
    }
    // ask() 内部会再 push user；这里不重复。
    ask(route.text)
    focusInput()
    return
  }

  await execShellLine(route.text)
}

async function execShellLine(c) {
  const hostId = store.currentHostId
  const p = prompt.value
  const bare = parseBareCd(c)

  if (c === 'clear') {
    clearLog()
    focusInput()
    return
  }

  // push 会展开成普通对象放进时间线；必须改时间线里那一份，才能触发渲染。
  push({
    kind: 'shell',
    cmd: c,
    who: p.who,
    path: p.path,
    sym: p.sym,
    pending: true,
    stdout: '',
    stderr: '',
    exitCode: null,
    durationMs: 0,
    error: '',
    hint: '',
    truncated: false,
    decision: '',
    reason: '',
    status: 'running'
  })
  const block = store.entries[store.entries.length - 1]

  try {
    const res = await runShell(hostId, c, cwd.value, 60)
    let stdout = res.stdout || ''
    if (bare !== null && res.status === 'done') {
      const ex = extractCwd(stdout)
      stdout = ex.stdout
      if (ex.cwd && (res.exitCode ?? 0) === 0) cwds[hostId] = ex.cwd
    }
    Object.assign(block, {
      pending: false,
      stdout,
      stderr: res.stderr || '',
      exitCode: res.exitCode ?? 0,
      durationMs: res.durationMs || 0,
      error: res.error || '',
      hint: res.hint || '',
      truncated: !!res.truncated,
      decision: res.decision || '',
      reason: res.reason || '',
      status: res.status || 'done'
    })
    if (res.status === 'denied' || res.status === 'cancelled' || res.status === 'missing') {
      block.error = block.error || res.reason || res.error || '未执行'
    }
  } catch (e) {
    Object.assign(block, {
      pending: false,
      status: 'error',
      error: String(e && e.message ? e.message : e)
    })
  }
  focusInput()
}

function onKeydown(e) {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault()
    send()
  }
}

const sessionId = computed({
  get: () => store.currentSessionId,
  set: v => selectSession(v)
})

const editing = ref('')
const editName = ref('')
const editEl = ref(null)

const currentSession = computed(
  () => store.currentSessions.find(s => s.id === store.currentSessionId) || null
)

const isDefaultSession = computed(() => !!(currentSession.value && currentSession.value.isDefault))

function sessionLabel(s) {
  const base = s.name || (s.isDefault ? '默认会话' : '未命名会话')
  return s.turns > 0 ? `${base}（${s.turns} 轮）` : base
}

function startNewSession() {
  editing.value = 'new'
  editName.value = ''
  nextTick(() => { if (editEl.value) editEl.value.focus() })
}

function startRenameSession() {
  if (!currentSession.value) return
  editing.value = 'rename'
  editName.value = currentSession.value.name || ''
  nextTick(() => { if (editEl.value) editEl.value.focus() })
}

function cancelEdit() {
  editing.value = ''
  editName.value = ''
}

async function commitEdit() {
  const name = editName.value.trim()
  if (!name) {
    push({ kind: 'error', content: '会话名不能为空。' })
    return
  }
  const what = editing.value
  editing.value = ''
  editName.value = ''
  if (what === 'new') await createSession(name)
  else if (what === 'rename') await renameSession(store.currentSessionId, name)
}

async function onDeleteSession() {
  if (!store.currentSessionId) return
  const label = currentSession.value ? sessionLabel(currentSession.value) : '这条会话'
  if (!confirm(`确认删除会话「${label}」？该会话的上下文会被永久丢弃，此操作不可撤销。`)) return
  await deleteSession(store.currentSessionId)
}

async function onClearSession() {
  if (!store.currentHostId) {
    push({ kind: 'error', content: '请先选择一台主机。' })
    return
  }
  await clearSession(store.currentHostId, store.currentSessionId)
}

async function onCompactSession() {
  if (!store.currentHostId) {
    push({ kind: 'error', content: '请先选择一台主机。' })
    return
  }
  await compactSession(store.currentHostId, store.currentSessionId)
}
</script>

<template>
  <div class="console">
    <header class="bar">
      <div class="row grow">
        <label class="inline-label">目标主机</label>
        <select v-model="store.currentHostId" class="host-select">
          <option v-if="!store.hosts.length" value="">（暂无主机，请先到「主机管理」添加）</option>
          <option v-for="h in store.hosts" :key="h.id" :value="h.id">
            {{ h.name }} — {{ h.user }}@{{ h.addr }}
          </option>
        </select>
      </div>

      <div class="seg">
        <button :class="{ on: mode === 'agent' }" @click="mode = 'agent'">Agent会话</button>
        <button :class="{ on: mode === 'terminal' }" @click="mode = 'terminal'">交互终端</button>
      </div>
    </header>

    <template v-if="mode === 'agent'">
      <div class="ctx-bar">
        <label class="inline-label">会话</label>
        <select
          v-model="sessionId"
          class="session-select"
          :disabled="!store.currentHostId"
        >
          <option v-for="s in store.currentSessions" :key="s.id" :value="s.id">
            {{ sessionLabel(s) }}
          </option>
        </select>
        <button class="sm" :disabled="!store.currentHostId" @click="startNewSession">新建</button>
        <button class="sm" :disabled="!currentSession" @click="startRenameSession">改名</button>
        <button
          class="sm"
          :disabled="!currentSession || isDefaultSession"
          :title="isDefaultSession ? '默认会话不能删除，用「清空上下文」清空它即可' : ''"
          @click="onDeleteSession"
        >删除</button>

        <span class="muted tiny ctx-note">shell 直跑（高危确认）· 自然语言走 <b>LLM</b> · 会话按主机独立加密</span>

        <button class="sm" :disabled="store.compacting" @click="onCompactSession">
          {{ store.compacting ? '压缩中…' : '压缩上下文' }}
        </button>
        <button class="sm" @click="onClearSession">清空上下文</button>
      </div>

      <div v-if="editing" class="session-edit">
        <input
          ref="editEl"
          v-model="editName"
          :placeholder="editing === 'new' ? '给这条会话起个名字，例如「nginx 排查」' : '新的会话名'"
          @keydown.enter.prevent="commitEdit"
          @keydown.esc.prevent="cancelEdit"
        />
        <button class="sm primary" @click="commitEdit">确定</button>
        <button class="sm" @click="cancelEdit">取消</button>
        <span class="muted tiny">Enter 确定，Esc 取消</span>
      </div>

      <!-- 风险/能力横幅：与交互终端对称，标明本模式走策略 -->
      <div class="sess-banner">
        <span>
          Agent会话<span v-if="currentHost"> · <b>{{ currentHost.user }}@{{ currentHost.addr }}</b></span>
          ：自然语言交给 <b>LLM</b>；shell 命令直跑（仅高危需确认）；未知命令会先查本机是否存在。
          需要 vim/top 等交互程序请切到「交互终端」。
        </span>
      </div>

      <div class="log term-log" ref="logEl" @click="focusInput">
        <div v-if="!store.entries.length" class="empty">
          <p>敲 shell 命令直接执行（高危才确认）；用中文描述问题或前缀 <span class="mono">?</span> 则交给 Agent。</p>
          <p class="muted">例如：<span class="mono">ls -la</span>　或　「nginx 起不来了，帮我看看为什么」</p>
          <p class="muted">会话按「主机 × 会话」分开保存。</p>
        </div>

        <div v-for="e in store.entries" :key="e.id" class="entry">
          <!-- 人工 shell 回显 -->
          <div v-if="e.kind === 'shell'" class="shell-block">
            <div class="term-line">
              <span class="p-who">{{ e.who }}</span><span class="p-sep">:</span><span class="p-path">{{ e.path }}</span><span class="p-sym">{{ e.sym }}</span><span class="p-cmd">{{ e.cmd }}</span>
            </div>
            <pre v-if="e.stdout" class="term-out">{{ e.stdout }}</pre>
            <pre v-if="e.stderr" class="term-err">{{ e.stderr }}</pre>
            <div v-if="e.error" class="term-err">{{ e.error }}</div>
            <div v-if="e.truncated" class="term-hint">输出过长，只保留了开头部分。可用 head / tail / grep 缩小范围后再看。</div>
            <div v-if="e.hint" class="term-hint">{{ e.hint }}</div>
            <div v-if="e.pending" class="term-busy"><span class="caret"></span></div>
            <div v-else-if="e.exitCode" class="term-fail">退出码 {{ e.exitCode }} · {{ e.durationMs }}ms</div>
            <div v-if="e.decision && e.decision !== 'allow'" class="term-meta">
              <span class="badge" :class="decisionClass(e.decision)">{{ decisionLabel(e.decision) }}</span>
              <span v-if="e.reason" class="muted tiny">{{ e.reason }}</span>
            </div>
          </div>

          <div v-else-if="e.kind === 'user'" class="msg user">
            <div class="msg-role">你</div>
            <div class="msg-body">{{ e.content }}</div>
          </div>

          <div v-else-if="e.kind === 'assistant'" class="msg assistant">
            <div class="msg-role">LLM</div>
            <div class="msg-body">{{ e.content }}<span v-if="e.streaming" class="caret"></span></div>
          </div>

          <div v-else-if="e.kind === 'tool'" class="tool">
            <div class="tool-head">
              <span class="tool-name mono">{{ e.tool.name }}</span>
              <span class="badge" :class="decisionClass(e.tool.decision)">
                {{ decisionLabel(e.tool.decision) }}
              </span>
              <span class="badge neutral">{{ statusLabel(e.tool.status) }}</span>
              <span class="muted" v-if="e.tool.hostName">@{{ e.tool.hostName }}</span>
              <span class="muted right" v-if="e.tool.durationMs">{{ e.tool.durationMs }}ms</span>
            </div>
            <pre v-if="e.tool.command" class="tool-cmd">{{ e.tool.command }}</pre>
            <div v-if="e.tool.reason" class="tool-reason">{{ e.tool.reason }}</div>
          </div>

          <div v-else-if="e.kind === 'result'" class="result">
            <div class="result-head">
              <span class="muted">退出码 {{ e.exitCode }}</span>
              <span v-if="e.redacted" class="badge warn">已脱敏 {{ e.redacted }} 处</span>
              <span class="muted tiny">原始输出 · 发给 LLM 前已脱敏并标记为不可信</span>
            </div>
            <pre>{{ e.content }}</pre>
          </div>

          <div v-else-if="e.kind === 'injection'" class="injection">
            <div class="injection-title">远端输出中检测到疑似提示注入</div>
            <div class="injection-body">命中话术：{{ e.findings.join('、') }}</div>
            <pre v-if="e.command" class="injection-cmd">{{ e.command }}</pre>
            <div class="injection-hint">
              该内容来自 <span class="mono">{{ e.hostName }}</span> 的执行结果，已被标记为不可信数据，
              不会作为指令执行。建议排查该主机上这段文本的来源。
            </div>
          </div>

          <div v-else-if="e.kind === 'error'" class="msg error">
            <div class="msg-role">错误</div>
            <div class="msg-body">{{ e.content }}</div>
          </div>

          <div v-else class="msg system">
            <div class="msg-body">{{ e.content }}</div>
          </div>
        </div>

        <div v-if="store.running && !store.pending && !streamingNow" class="thinking">LLM 正在思考…</div>

        <!-- 内联提示符：像终端一样停在时间线末尾 -->
        <div class="term-line term-live">
          <span class="p-who">{{ prompt.who }}</span><span class="p-sep">:</span><span class="p-path">{{ prompt.path }}</span><span class="p-sym">{{ prompt.sym }}</span><input
            ref="inputEl"
            v-model="draft"
            class="term-input"
            :disabled="!store.currentHostId || sessionLocked"
            :placeholder="store.currentHostId ? (sessionLocked ? '等待中…' : 'shell 命令，或中文/? 问 Agent') : '请先选择一台主机'"
            spellcheck="false"
            autocomplete="off"
            @keydown="onKeydown"
          />
        </div>
      </div>

      <div v-if="store.pending" class="approval">
        <div class="approval-head">
          <span class="badge warn">需要你的批准</span>
          <span class="mono">{{ store.pending.name }}</span>
          <span class="muted" v-if="store.pending.hostName">@{{ store.pending.hostName }}</span>
        </div>
        <pre class="approval-cmd">{{ store.pending.command || store.pending.args }}</pre>
        <div class="approval-reason">{{ store.pending.reason }}</div>
        <div class="row" style="margin-top: 10px">
          <button class="ok" @click="approve(true)">批准执行</button>
          <button class="danger" @click="approve(false)">拒绝</button>
        </div>
      </div>

      <div class="composer-bar">
        <button class="sm" @click="clearLog" :disabled="store.running || store.busy">清空记录</button>
        <button v-if="store.running || store.busy" class="sm danger" @click="stop">中断</button>
        <span class="muted tiny grow-hint">Enter 发送 · Shift+Enter 不适用（单行输入）· <span class="mono">?</span> 前缀强制问 Agent</span>
      </div>
    </template>

    <InteractiveTerminal v-show="mode === 'terminal'" :active="mode === 'terminal'" />
  </div>
</template>

<style scoped>
.console {
  display: flex;
  flex-direction: column;
  height: 100%;
  overflow: hidden;
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
.inline-label { font-size: 12px; color: var(--text-2); margin: 0; white-space: nowrap; }
.host-select { max-width: 420px; }

.seg { display: flex; gap: 0; border: 1px solid var(--border); border-radius: 8px; overflow: hidden; }
.seg button {
  border: none;
  border-radius: 0;
  padding: 6px 14px;
  background: var(--surface);
  color: var(--text-2);
}
.seg button.on { background: var(--accent-bg); color: var(--accent); font-weight: 500; }

.ctx-bar {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  padding: 6px 18px;
  border-bottom: 1px solid var(--border);
  background: var(--surface);
  flex-shrink: 0;
}
.session-select { max-width: 260px; }
.ctx-note { margin-left: auto; }

.session-edit {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 18px;
  border-bottom: 1px solid var(--border);
  background: var(--surface-2);
  flex-shrink: 0;
}
.session-edit input { max-width: 320px; }

.sess-banner {
  flex-shrink: 0;
  padding: 8px 16px;
  background: var(--warn-bg);
  color: var(--warn);
  font-size: 12.5px;
  line-height: 1.55;
  border-bottom: 1px solid var(--border);
}

.term-log {
  flex: 1;
  overflow-y: auto;
  padding: 12px 16px 8px;
  display: flex;
  flex-direction: column;
  gap: 10px;
  background: var(--bg);
  font-family: var(--mono);
  font-size: 12.5px;
  line-height: 1.6;
  cursor: text;
}

.empty { color: var(--text-2); padding: 20px 4px; line-height: 1.9; font-family: var(--font, inherit); }

.shell-block { margin-bottom: 2px; }
.term-line { white-space: pre-wrap; word-break: break-word; }
.p-who { color: var(--ok); }
.p-sep { color: var(--text-3); }
.p-path { color: var(--purple); }
.p-sym { color: var(--text-3); margin-right: 8px; }
.p-cmd { color: var(--text); }
.term-out { color: var(--text); margin: 0; white-space: pre-wrap; word-break: break-word; }
.term-err { color: var(--danger); margin: 0; white-space: pre-wrap; word-break: break-word; }
.term-busy { color: var(--text-3); }
.term-fail { color: var(--danger); font-size: 11.5px; }
.term-meta { margin-top: 4px; display: flex; align-items: center; gap: 8px; }
.term-hint {
  color: var(--warn);
  font-size: 11.5px;
  line-height: 1.6;
  margin: 2px 0;
  padding-left: 8px;
  border-left: 2px solid var(--warn);
  white-space: pre-wrap;
  word-break: break-word;
}

.term-live { display: flex; align-items: baseline; margin-top: 4px; }
.term-live > span { flex-shrink: 0; white-space: pre; }
.term-input {
  flex: 1;
  width: auto;
  min-width: 0;
  border: none;
  border-radius: 0;
  background: transparent;
  padding: 0;
  margin: 0;
  font-family: var(--mono);
  font-size: 12.5px;
  line-height: 1.6;
  color: var(--text);
  caret-color: var(--text);
}
.term-input:focus { border: none; outline: none; }
.term-input:disabled { background: transparent; opacity: 1; color: var(--text-3); }
.term-input::placeholder { color: var(--text-3); }

.msg { display: flex; gap: 10px; font-family: var(--font, inherit); }
.msg-role {
  flex-shrink: 0;
  width: 44px;
  font-size: 11px;
  color: var(--text-3);
  padding-top: 3px;
  text-align: right;
}
.msg-body {
  flex: 1;
  line-height: 1.7;
  white-space: pre-wrap;
  word-break: break-word;
}
.msg.user .msg-body {
  background: var(--accent-bg);
  border-radius: var(--radius);
  padding: 9px 12px;
  color: #0c447c;
}
.msg.assistant .msg-body { padding: 3px 0; }

.caret {
  display: inline-block;
  width: 2px;
  height: 1em;
  margin-left: 2px;
  vertical-align: text-bottom;
  background: var(--text-2);
  animation: caret-blink 1s steps(2, start) infinite;
}
@keyframes caret-blink {
  to { visibility: hidden; }
}
@media (prefers-reduced-motion: reduce) {
  .caret { animation: none; }
}
.msg.error .msg-body {
  background: var(--danger-bg);
  color: var(--danger);
  border-radius: 8px;
  padding: 9px 12px;
}
.msg.system .msg-body { color: var(--text-3); font-size: 12px; }

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
.tool-cmd {
  margin-top: 8px;
  background: var(--surface-2);
  border-radius: 7px;
  padding: 8px 10px;
}
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

.result {
  border-left: 2px solid var(--border);
  padding-left: 12px;
  font-family: var(--font, inherit);
}
.result-head { display: flex; align-items: center; gap: 8px; margin-bottom: 5px; }
.result pre { color: var(--text-2); }
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
  background: rgba(255, 255, 255, 0.7);
  border-radius: 7px;
  padding: 7px 9px;
  color: #501313;
}
.injection-hint { font-size: 12px; color: var(--text-2); margin-top: 7px; line-height: 1.65; }

.thinking { color: var(--text-3); font-size: 12px; padding-left: 54px; font-family: var(--font, inherit); }

.approval {
  margin: 0 18px 10px;
  border: 1px solid var(--warn);
  background: var(--warn-bg);
  border-radius: var(--radius);
  padding: 12px 14px;
  flex-shrink: 0;
}
.approval-head { display: flex; align-items: center; gap: 8px; margin-bottom: 8px; }
.approval-cmd {
  background: rgba(255, 255, 255, 0.7);
  border-radius: 7px;
  padding: 8px 10px;
  color: #4a1b0c;
}
.approval-reason { font-size: 12px; color: var(--warn); margin-top: 6px; }

.composer-bar {
  flex-shrink: 0;
  border-top: 1px solid var(--border);
  background: var(--surface);
  padding: 8px 18px;
  display: flex;
  align-items: center;
  gap: 8px;
}
.grow-hint { margin-left: auto; }
</style>
