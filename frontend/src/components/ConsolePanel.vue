<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import {
  store, ask, approve, stop, clearLog, clearSession, compactSession, push,
  createSession, refreshSessions, runShell, switchProfile, syncWindowTitle
} from '../store'
import { classifyInput } from '../inputRoute'
import { parseBareCd, extractCwd, promptParts } from '../term'
import InteractiveTerminal from './InteractiveTerminal.vue'
import UiSelect from './UiSelect.vue'

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
  if (!store.pending) showCmdDetail.value = false
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
  if (sessionLocked.value) return
  if (!store.currentHostId) {
    push({ kind: 'error', content: '请先选择一台主机。' })
    return
  }

  // 空命令：像真实终端一样，回车只在时间线里刷新一个提示符（本地 no-op，exit 0），
  // 不下发远端、不经策略引擎。空白字符同样视为空：回车即清空当前行。
  if (!text) {
    draft.value = ''
    const p = prompt.value
    push({
      kind: 'shell',
      cmd: '',
      who: p.who,
      path: p.path,
      sym: p.sym,
      pending: false,
      stdout: '',
      stderr: '',
      exitCode: 0,
      durationMs: 0,
      error: '',
      hint: '',
      truncated: false,
      decision: '',
      reason: '',
      status: 'done'
    })
    focusInput()
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
  // 输入法组词期间的 Enter 是确认候选词，不能当「发送」：
  // 此时 draft 还没同步（Vue 在组词期间不更新 model），
  // 误触发会把空命令/旧草稿发出去，时间线里插进多余一行。
  // keyCode 229 是 WKWebView 等老内核上 isComposing 缺失时的兜底。
  if (e.isComposing || e.keyCode === 229) return
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault()
    send()
  }
}

// 会话的切换/新建/重命名/删除都在侧边栏（SessionsSidebar.vue）完成。
const currentSession = computed(
  () => store.currentSessions.find(s => s.id === store.currentSessionId) || null
)

// 顶栏展示当前任务名，让用户随时知道自己在哪条任务里。
// 默认任务没有名字：用「主机名 (user@addr)」顶替，与侧边栏列表一致。
const currentSessionName = computed(() => {
  const s = currentSession.value
  if (!s) return ''
  if (s.name) return s.name
  if (s.isDefault) {
    const h = store.hosts.find(x => x.id === store.currentHostId)
    if (h) return `${h.name} (${h.user}@${h.addr})`
  }
  return '未命名任务'
})

const hostOptions = computed(() =>
  store.hosts.map(h => ({ value: h.id, label: `${h.name} — ${h.user}@${h.addr}` }))
)

// 窗口标题跟随当前任务：切任务/改名后标题栏立即更新。
watch(currentSessionName, name => syncWindowTitle(name), { immediate: true })

// 模型（= LLM 方案）选择。激活项以后端 active 标记为准；
// 会话历史与模型无关，切换不丢上下文。
const activeProfileId = computed(() =>
  store.llmProfiles.find(p => p.active)?.id || ''
)
const modelOptions = computed(() =>
  store.llmProfiles.map(p => ({ value: p.id, label: `${p.name} · ${p.model}` }))
)
async function onSwitchModel(id) {
  if (!id || id === activeProfileId.value) return
  const p = store.llmProfiles.find(x => x.id === id)
  try {
    await switchProfile(id)
  } catch {
    return // 失败提示已由 switchProfile 推送，成功消息不能照发
  }
  push({
    kind: 'system',
    content: `已切换模型到「${p ? p.name : id}」，本会话上下文保留，下一轮对话生效。`
  })
}

// 会话的新建/重命名/删除已移至侧边栏（SessionsSidebar.vue），
// 这里只保留作用于「当前会话」的两个操作：压缩与清空。

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

// 新建会话（顶栏入口，参考 WorkBuddy 把主操作放在最顺手的位置）。
// 弹应用内输入框确认 —— WKWebView 上原生 prompt/confirm 都不弹，不能用。
const showNewModal = ref(false)

// 审批命令详细弹窗：卡片内只做限高预览，全文（复制/细看）放弹窗
const showCmdDetail = ref(false)
const cmdCopied = ref(false)
// 关闭就重置复制标记：下次打开不该残留「已复制」的假状态
watch(showCmdDetail, v => { if (!v) cmdCopied.value = false })
const pendingCmd = computed(() => {
  const p = store.pending
  if (!p) return ''
  const raw = p.command || p.args
  return typeof raw === 'string' ? raw : JSON.stringify(raw ?? '', null, 2)
})
const newName = ref('')
const newEl = ref(null)

function startNewSession() {
  if (!store.currentHostId) {
    push({ kind: 'error', content: '请先选择一台主机。' })
    return
  }
  newName.value = ''
  showNewModal.value = true
  nextTick(() => { if (newEl.value) newEl.value.focus() })
}

async function commitNewSession() {
  const name = newName.value.trim()
  showNewModal.value = false
  newName.value = ''
  if (!name) return
  await createSession(name)
}

// 弹窗输入框的 Enter/Esc 与主输入框同理：输入法组词期间要让路，
// 否则确认中文任务名候选词的那次 Enter 会把弹窗直接关掉。
function onNewKeydown(e) {
  if (e.isComposing || e.keyCode === 229) return
  if (e.key === 'Enter') {
    e.preventDefault()
    commitNewSession()
  } else if (e.key === 'Escape') {
    e.preventDefault()
    showNewModal.value = false
  }
}

async function copyPendingCmd() {
  const text = pendingCmd.value
  if (!text) return
  let ok = false
  try {
    await navigator.clipboard.writeText(text)
    ok = true
  } catch { ok = false }
  if (!ok) {
    // WKWebView 等环境下 clipboard API 可能不可用，退回 execCommand
    const ta = document.createElement('textarea')
    ta.value = text
    ta.style.position = 'fixed'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.select()
    try { ok = document.execCommand('copy') } catch { ok = false }
    ta.remove()
  }
  if (ok) {
    cmdCopied.value = true
    setTimeout(() => { cmdCopied.value = false }, 1500)
  }
}

function onWinKeydown(e) {
  // 新建任务弹窗的输入框有自己的 Esc 处理：它在顶层时这里让路，
  // 避免一次 Esc 同时关掉两层、或把用户正编辑的任务名弹窗误关。
  if (showNewModal.value) return
  if (e.key === 'Escape' && showCmdDetail.value) showCmdDetail.value = false
}
onMounted(() => window.addEventListener('keydown', onWinKeydown))
onBeforeUnmount(() => window.removeEventListener('keydown', onWinKeydown))
</script>

<template>
  <div class="console">
    <header class="bar">
      <div class="row grow">
        <label class="inline-label">目标主机</label>
        <UiSelect
          v-model="store.currentHostId"
          class="host-select"
          :options="hostOptions"
          placeholder="（暂无主机，请先到「主机管理」添加）"
        />
        <button
          class="sm tonal new-session-btn"
          :disabled="!store.currentHostId"
          title="为当前主机新建一条任务"
          @click="startNewSession"
        >＋ 新建任务</button>
      </div>

      <div class="seg">
        <button :class="{ on: mode === 'agent' }" @click="mode = 'agent'">Agent会话</button>
        <button :class="{ on: mode === 'terminal' }" @click="mode = 'terminal'">交互终端</button>
      </div>

      <!-- 新建任务弹窗（应用内，WKWebView 上原生 prompt 不弹） -->
      <div v-if="showNewModal" class="overlay" @click.self="showNewModal = false">
        <div class="modal">
          <h3>新建任务</h3>
          <input
            ref="newEl"
            v-model="newName"
            placeholder="任务名称，如「nginx 排查」"
            @keydown="onNewKeydown"
          />
          <div class="modal-hint">Enter 确定 · Esc 取消 · 留空则不创建</div>
          <div class="modal-actions">
            <button class="sm" @click="showNewModal = false">取消</button>
            <button class="sm primary" @click="commitNewSession">创建</button>
          </div>
        </div>
      </div>
    </header>

    <template v-if="mode === 'agent'">
      <!-- 风险/能力横幅：与交互终端对称，标明本模式走策略 -->
      <div class="sess-banner">
        <span>
          Agent会话<span v-if="currentHost"> · <b>{{ currentHost.user }}@{{ currentHost.addr }}</b></span>
          ｜ 中文提问交给 <b>LLM</b>，shell 命令直接执行，高危命令会先请你确认。
          vim/top 等交互程序请用「交互终端」。
        </span>
      </div>

      <div class="log term-log" ref="logEl" @click="focusInput">
        <div v-for="e in store.entries" :key="e.id" class="entry" :class="'entry-' + e.kind">
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
            <div class="msg-body">{{ e.content }}<span v-if="e.streaming" class="caret"></span><span v-if="!e.content && !e.streaming" class="muted tiny">（模型返回了空回复）</span></div>
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
          <button class="sm right" @click="showCmdDetail = true">详细</button>
        </div>
        <pre class="approval-cmd">{{ store.pending.command || store.pending.args }}</pre>
        <div class="approval-reason">{{ store.pending.reason }}</div>
        <div class="row" style="margin-top: 10px">
          <button class="ok" @click="approve(true)">批准执行</button>
          <button class="danger" @click="approve(false)">拒绝</button>
        </div>
      </div>

      <div class="composer-bar">
        <label class="inline-label">模型</label>
        <UiSelect
          class="model-select"
          :model-value="activeProfileId"
          :options="modelOptions"
          :disabled="store.running || !store.llmProfiles.length"
          placeholder="（暂无方案，请到「设置」添加）"
          @change="onSwitchModel"
        />
        <button
          class="sm"
          :disabled="store.compacting"
          :title="store.compacting ? '' : '让 LLM 把全部历史重写成一段摘要（花一次 API 调用）。日常使用无需手动压缩：旧的对话轮次已自动移出上下文。'"
          @click="onCompactSession"
        >
          {{ store.compacting ? '压缩中…' : '压缩上下文' }}
        </button>
        <button class="sm" @click="onClearSession">清空上下文</button>
        <span class="composer-sep"></span>
        <button class="sm" @click="clearLog" :disabled="store.running || store.busy">清空记录</button>
        <button v-if="store.running || store.busy" class="sm danger" @click="stop">中断</button>
        <span class="muted tiny grow-hint">Enter 发送 · 加 <span class="mono">?</span> 开头强制由 Agent 回答</span>
      </div>
    </template>

    <InteractiveTerminal v-show="mode === 'terminal'" :active="mode === 'terminal'" />

    <!-- 命令详细弹窗：审批卡内只做预览，超长命令在这里看全文 -->
    <div v-if="showCmdDetail" class="overlay" @click.self="showCmdDetail = false">
      <div class="modal cmd-modal">
        <h3>待执行命令详细</h3>
        <pre class="cmd-detail">{{ pendingCmd }}</pre>
        <div class="modal-actions">
          <button class="sm" @click="copyPendingCmd">{{ cmdCopied ? '已复制' : '复制' }}</button>
          <button class="sm primary" @click="showCmdDetail = false">关闭</button>
        </div>
      </div>
    </div>
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
.new-session-btn { flex-shrink: 0; white-space: nowrap; font-size: 13px; }

/* 新建会话弹窗（应用内，与侧边栏删除确认同风格） */
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
  width: min(400px, calc(100% - 60px));
  box-shadow: var(--shadow);
}
.modal h3 { margin: 0 0 12px; font-size: 14px; }
.modal-hint { font-size: 11px; color: var(--text-3); margin-top: 6px; }
.modal-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 14px; }

/* 命令详细弹窗：宽一些，命令块内部滚动 */
.cmd-modal { width: min(760px, calc(100% - 60px)); }
.cmd-detail {
  margin: 0;
  max-height: 55vh;
  overflow: auto;
  background: var(--inset-bg);
  border-radius: 7px;
  padding: 10px 12px;
  color: var(--text);
  font-family: var(--mono);
  font-size: 12.5px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
}

.seg { display: flex; gap: 0; border: 1px solid var(--border); border-radius: 8px; overflow: hidden; }
.seg button {
  flex: 1;
  border: none;
  border-radius: 0;
  min-height: 34px;
  height: 34px;
  padding: 0 16px;
  margin: 0;
  background: var(--surface);
  color: var(--text-2);
}
.seg button.on { background: var(--accent-bg); color: var(--accent); font-weight: 500; }
.model-select { flex: 0 1 280px; min-width: 190px; }

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
  min-height: 0;
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

.shell-block { margin-bottom: 2px; }
/* 连续的终端回显行要像真实终端一样紧贴：
   容器 gap(10px) + 块内下边距(2px) 全部抵消，只留行高。 */
.entry-shell + .entry-shell { margin-top: -12px; }
.entry-shell + .term-live { margin-top: -12px; }
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

/* 不用 align-items: baseline：WKWebView 里 <input> 在 flex 中的基线
   由边框盒合成，会和旁边 span 的文字错开半行（输入内容整体偏高）。
   prompt span 与 input 行高同为 12.5px×1.6，居中对齐即基线对齐。 */
.term-live { display: flex; align-items: center; margin-top: 4px; }
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
  color: var(--accent);
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
  background: var(--inset-bg);
  border-radius: 7px;
  padding: 7px 9px;
  color: var(--danger);
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
  /* 卡片整体不超过面板剩余高度，避免底部按钮被 overflow:hidden 裁掉 */
  max-height: 70%;
  display: flex;
  flex-direction: column;
}
.approval-head { display: flex; align-items: center; gap: 8px; margin-bottom: 8px; flex-shrink: 0; }
.approval-cmd {
  background: var(--inset-bg);
  border-radius: 7px;
  padding: 8px 10px;
  color: var(--text);
  /* 卡片内只做预览：限高裁剪，全文走「详细」弹窗 */
  max-height: 168px;
  overflow: hidden;
}
.approval-reason { font-size: 12px; color: var(--warn); margin-top: 6px; flex-shrink: 0; }
.approval .row { flex-shrink: 0; }

.composer-bar {
  flex-shrink: 0;
  border-top: 1px solid var(--border);
  background: var(--surface);
  padding: 8px 18px;
  display: flex;
  align-items: center;
  gap: 8px;
}
/* 「清空记录」与上下文操作之间的竖分隔线 */
.composer-sep {
  width: 1px;
  height: 18px;
  background: var(--border);
  margin: 0 4px;
  flex-shrink: 0;
}
.grow-hint { margin-left: auto; }
</style>
