<script setup>
import { computed, reactive, ref, watch } from 'vue'
import { store, push } from '../store'
import { t } from '../i18n'

const form = reactive({
  mode: store.policy.mode,
  whitelist: (store.policy.whitelist || []).join('\n'),
  redactOutput: store.policy.redactOutput,
  allowCrossHost: store.policy.allowCrossHost ?? false,
  allowWeb: store.policy.allowWeb ?? false,
  searchProvider: store.policy.searchProvider || '',
  maxOutput: store.policy.maxOutput,
  maxSteps: store.policy.maxSteps,
  maxSessionTurns: store.policy.maxSessionTurns ?? 8,
  maxStoredToolBytesKb: Math.round((store.policy.maxStoredToolBytes ?? 8192) / 1024),
  maxSessionBytesKb: Math.round((store.policy.maxSessionBytes ?? 262144) / 1024)
})

// 主机级覆盖的编辑状态：key = 主机 ID，值是以**字符串**保存的三个输入框内容。
//
// 用字符串而不是数字，是因为「空」与「0」在这套语义里完全不同：
// 空 = 继承全局，0 = 试图把上限设成 0（会被后端当未设置，等于继承）。
// 一旦在读取时就把空串转成数字，就再也分不清用户是清空了输入框
// 还是真的想设成 0 —— 而这正是这个功能里最容易出错的一步。
function blankOverride() {
  return { turns: '', toolKb: '', totalKb: '' }
}

// 从后端读回来的字节值要换算成 KB 再显示。
//
// 与上面全局那三个输入框同一套换算，理由也一样：直接显示 262144
// 没人看得懂，而且用户一改就缩掉 1024 倍。
function overrideFromWire(ov) {
  return {
    turns: ov.maxSessionTurns ? String(ov.maxSessionTurns) : '',
    toolKb: ov.maxStoredToolBytes ? String(Math.round(ov.maxStoredToolBytes / 1024)) : '',
    totalKb: ov.maxSessionBytes ? String(Math.round(ov.maxSessionBytes / 1024)) : ''
  }
}

const overrides = reactive({})
for (const [id, ov] of Object.entries(store.policy.sessionOverrides || {})) {
  overrides[id] = overrideFromWire(ov)
}

const hosts = computed(() => store.hosts || [])

// 给每台主机准备好一行编辑状态。
//
// 在渲染前补齐而不是在模板里 `overrides[h.id].turns`：
// 模板里直接取会在主机还没有条目时抛错，而「渲染时顺手补一个 key」
// 又会在渲染过程中改响应式状态，容易出现难查的重复渲染。
watch(
  () => hosts.value.map(h => h.id),
  ids => {
    for (const id of ids) {
      if (!overrides[id]) overrides[id] = blankOverride()
    }
  },
  { immediate: true }
)

function hasOverride(id) {
  const f = overrides[id]
  return !!f && (f.turns !== '' || f.toolKb !== '' || f.totalKb !== '')
}

function clearOverride(id) {
  overrides[id] = blankOverride()
}

// 界面上要渲染的行 = 现有主机 + 覆盖表里那些主机已不存在的条目。
//
// 为什么要把「主机已不存在」的条目也渲染出来，而不是只留一句提示：
// 它们会被原样保存下去，却因为界面上没有对应的行而**永远没法删掉** ——
// 用户看到提示也束手无策。渲染成一行并标注出来，清除按钮才有对象。
//
// 反过来说，也不能在保存时自动把它们丢掉：主机列表可能只是还没加载完，
// 那时删掉等于把用户设好的覆盖静默抹掉。
const rows = computed(() => {
  const known = new Set(hosts.value.map(h => h.id))
  const out = hosts.value.map(h => ({ id: h.id, name: h.name || h.id, missing: false }))
  for (const id of Object.keys(overrides)) {
    if (!known.has(id)) out.push({ id, name: id, missing: true })
  }
  return out
})

// 把一项输入框的内容转成正整数，没填返回 null（= 不提交这一项）。
//
// 返回 null 而不是 0：0 送过去虽然也会被后端当「未设置」，但那样前端
// 就在主动表达「我要 0」，一旦后端哪天改成严格校验，这里会立刻变成
// 一个把上下文关掉的 bug。
//
// 只用一个数值判据，不逐个判断字符串形态：空串和 null 经 Number()
// 都是 0，而 0 与负数一律视为「没填」，所以它们已经被同一条判据覆盖 ——
// 另写 `v === ''` 之类的分支是**冗余**的，冗余分支无法被任何用例
// 区分出来（删掉它没有任何行为变化），留着只会让人误以为它有用。
// 数值判据还顺带处理了 type=number 输入框可能给出的 ' 12 '、'1e3' 等写法。
function posOrNull(v) {
  const n = Number(v)
  if (!Number.isFinite(n) || n <= 0) return null
  return Math.round(n)
}

// 构造提交用的覆盖表。
//
// 遍历的是**全部**编辑状态而不是界面上渲染出来的那几台主机：
// 若主机列表尚未加载（hosts 为空）时用户点了保存，按渲染列表构造
// 会把已有的覆盖全部删光，而界面上什么都看不出来。
function buildOverrides() {
  const out = {}
  for (const [id, f] of Object.entries(overrides)) {
    const item = {}
    const turns = posOrNull(f.turns)
    const toolKb = posOrNull(f.toolKb)
    const totalKb = posOrNull(f.totalKb)
    if (turns !== null) item.maxSessionTurns = turns
    if (toolKb !== null) item.maxStoredToolBytes = toolKb * 1024
    if (totalKb !== null) item.maxSessionBytes = totalKb * 1024
    // 三项都空 → 整条不发。发一条全零的过去只会被后端规整掉，
    // 徒增一次无意义的读写。
    if (Object.keys(item).length) out[id] = item
  }
  return out
}

const busy = ref(false)

// 载入内置的只读诊断白名单。只填充表单，需点「保存策略」才生效。
async function restoreDefaults() {
  try {
    const list = await window.go.main.App.DefaultWhitelist()
    form.whitelist = (list || []).join('\n')
    push({ kind: 'system', content: t('policy.defaultsLoaded') })
  } catch (e) {
    push({ kind: 'error', content: String(e) })
  }
}

// 硬拒绝规则清单：分类名走字典（语言可切）；detail 是路径/命令枚举，
// 属技术原文，不翻。
const hardDenied = computed(() => [
  { name: t('policy.hard.r1'), detail: '/etc/shadow、/etc/gshadow、~/.ssh/id_*、ssh_host_*_key、.aws/credentials、.kube/config、.docker/config.json、.netrc、.pgpass' },
  { name: t('policy.hard.r2'), detail: 'printenv、单独执行 env、export -p、/proc/*/environ' },
  { name: t('policy.hard.r3'), detail: 'mkfs、wipefs、dd of=/dev/*、rm -rf /、> /dev/sd*' },
  { name: t('policy.hard.r4'), detail: 'shutdown、reboot、halt、poweroff、systemctl poweroff/reboot' },
  { name: t('policy.hard.r5'), detail: 'fork bomb、chmod -R 777 /、全盘 chown、kill -9 -1、清空防火墙规则、userdel -r' }
])

// 把界面上以 KB 输入的会话上限换算回字节。
//
// 界面用 KB 是因为 262144 这种数字没人看得懂；但存进去必须是字节，
// 后端的所有判断都是按字节算的。换算放在提交前一步，
// 避免出现「界面显示 256、后端按 256 字节裁」这种量级错位。
function kbToBytes(kb, fallbackKb) {
  const n = Number(kb)
  if (!Number.isFinite(n) || n <= 0) return fallbackKb * 1024
  return Math.round(n) * 1024
}

async function save() {
  busy.value = true
  try {
    const payload = {
      mode: form.mode,
      whitelist: form.whitelist.split('\n').map(s => s.trim()).filter(Boolean),
      redactOutput: form.redactOutput,
      allowCrossHost: form.allowCrossHost,
      maxOutput: Number(form.maxOutput) || 32768,
      maxSteps: Number(form.maxSteps) || 12,
      maxSessionTurns: Number(form.maxSessionTurns) || 8,
      maxStoredToolBytes: kbToBytes(form.maxStoredToolBytesKb, 8),
      maxSessionBytes: kbToBytes(form.maxSessionBytesKb, 256),
      sessionOverrides: buildOverrides(),
      allowWeb: form.allowWeb,
      searchProvider: form.searchProvider || ''
    }
    await window.go.main.App.SavePolicy(payload)
    store.policy = payload
    push({ kind: 'system', content: t('policy.saved') })
  } catch (e) {
    push({ kind: 'error', content: String(e) })
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="panel">
    <header class="panel-head">
      <div>
        <h2>{{ t('policy.title') }}</h2>
        <p class="muted">
          {{ t('policy.headHint') }}
        </p>
      </div>
    </header>

    <div class="card">
      <div class="card-title">{{ t('policy.mode.title') }}</div>
      <div class="modes">
        <label class="mode" :class="{ on: form.mode === 'manual' }">
          <input type="radio" value="manual" v-model="form.mode" />
          <div>
            <div class="mode-title">{{ t('policy.mode.manual') }}</div>
            <div class="mode-desc">{{ t('policy.mode.manualDesc') }}</div>
          </div>
        </label>
        <label class="mode" :class="{ on: form.mode === 'whitelist' }">
          <input type="radio" value="whitelist" v-model="form.mode" />
          <div>
            <div class="mode-title">{{ t('policy.mode.whitelist') }}</div>
            <div class="mode-desc">{{ t('policy.mode.whitelistDesc') }}</div>
          </div>
        </label>
      </div>
    </div>

    <div class="card">
      <div class="wl-head">
        <div class="card-title">{{ t('policy.wl.title') }}</div>
        <button class="sm" :disabled="form.mode !== 'whitelist'" @click="restoreDefaults">
          {{ t('policy.wl.restore') }}
        </button>
      </div>
      <div class="card-sub">{{ t('policy.wl.sub') }}</div>
      <textarea
        v-model="form.whitelist"
        rows="10"
        :disabled="form.mode !== 'whitelist'"
        placeholder="ls&#10;cat&#10;systemctl status"
      ></textarea>
      <div class="muted hint">
        {{ t('policy.wl.hint.a') }}
        {{ t('policy.wl.hint.b1') }}
        <span class="mono">command -v</span>{{ t('policy.wl.hint.b2') }}
        {{ t('policy.wl.hint.c1') }}<span class="mono">&gt;</span>{{ t('policy.wl.hint.c2') }}
      </div>
    </div>

    <div class="card">
      <div class="card-title">{{ t('policy.scope.title') }}</div>
      <label class="chk">
        <input type="checkbox" v-model="form.allowCrossHost" class="cross-host" />
        {{ t('policy.scope.crossHost') }}
      </label>
      <div class="muted hint">
        {{ t('policy.scope.hint.a1') }}<b>{{ t('policy.scope.hint.bold') }}</b>{{ t('policy.scope.hint.a2') }}
        {{ t('policy.scope.hint.a3') }}
        {{ t('policy.scope.hint.a4') }}
        {{ t('policy.scope.hint.a5') }}
      </div>
    </div>

    <div class="card">
      <div class="card-title">{{ t('policy.web.title') }}</div>
      <label class="chk">
        <input type="checkbox" v-model="form.allowWeb" class="allow-web" />
        {{ t('policy.web.allow') }}
      </label>
      <div class="muted hint">
        {{ t('policy.web.hint.a') }}
        <br />
        <b>{{ t('policy.web.hint.bold') }}</b>{{ t('policy.web.hint.b1') }}
        {{ t('policy.web.hint.b2') }}
        {{ t('policy.web.hint.b3') }}
      </div>

      <div v-if="form.allowWeb" class="grid">
        <div>
          <label style="margin-top: 12px">{{ t('policy.web.search') }}</label>
          <select v-model="form.searchProvider">
            <option value="">{{ t('policy.web.noProvider') }}</option>
            <option value="duckduckgo">{{ t('policy.web.ddg') }}</option>
          </select>
        </div>
      </div>
      <div v-if="form.allowWeb" class="muted hint">
        {{ t('policy.web.noSearchHint') }}
      </div>
    </div>

    <div class="card">
      <div class="card-title">{{ t('policy.content.title') }}</div>
      <label class="chk">
        <input type="checkbox" v-model="form.redactOutput" />
        {{ t('policy.content.redact') }}
      </label>

      <div class="grid">
        <div>
          <label style="margin-top: 12px">{{ t('policy.content.maxOutput') }}</label>
          <input v-model="form.maxOutput" type="number" min="1024" step="1024" />
        </div>
        <div>
          <label style="margin-top: 12px">{{ t('policy.content.maxSteps') }}</label>
          <input v-model="form.maxSteps" type="number" min="1" max="50" />
        </div>
      </div>

      <div class="notice">
        {{ t('policy.content.redactNotice.a') }}
        {{ t('policy.content.redactNotice.b1') }}<span class="mono">[REDACTED]</span>{{ t('policy.content.redactNotice.b2') }}
      </div>

      <div class="notice">
        <b>{{ t('policy.content.inj.bold') }}</b>{{ t('policy.content.inj.p1') }}
        {{ t('policy.content.inj.p2') }}
        <span class="mono">&lt;&lt;&lt;UNTRUSTED_REMOTE_OUTPUT&gt;&gt;&gt;</span>{{ t('policy.content.inj.p3') }}
        {{ t('policy.content.inj.p4') }}
        {{ t('policy.content.inj.p5a') }}<span class="mono">&lt;|im_start|&gt;</span>{{ t('policy.content.inj.p5b') }}
        <br />
        <span class="muted">
          {{ t('policy.content.inj.n1') }}
          {{ t('policy.content.inj.n2') }}
        </span>
      </div>
    </div>

    <div class="card">
      <div class="card-title">{{ t('policy.ctx.title') }}</div>

      <div class="grid">
        <div>
          <label style="margin-top: 12px">{{ t('policy.ctx.turns') }}</label>
          <input v-model="form.maxSessionTurns" class="session-turns" type="number" min="1" max="100" />
        </div>
        <div>
          <label style="margin-top: 12px">{{ t('policy.ctx.toolKb') }}</label>
          <input v-model="form.maxStoredToolBytesKb" class="session-tool-bytes" type="number" min="1" step="1" />
        </div>
      </div>

      <div style="margin-top: 12px">
        <label style="margin-top: 0">{{ t('policy.ctx.totalKb') }}</label>
        <input v-model="form.maxSessionBytesKb" class="session-total-bytes" type="number" min="1" step="16" />
      </div>

      <div class="notice">
        <b>{{ t('policy.ctx.mem.bold1') }}</b>{{ t('policy.ctx.mem.p1') }}
        {{ t('policy.ctx.mem.p2') }}
        {{ t('policy.ctx.mem.p3') }}
        <b>{{ t('policy.ctx.mem.bold2') }}</b>{{ t('policy.ctx.mem.p4') }}
      </div>

      <div class="notice">
        <b>{{ t('policy.ctx.trim.bold') }}</b>{{ t('policy.ctx.trim.p1') }}
        {{ t('policy.ctx.trim.p2') }}
        <br />
        <span class="muted">
          {{ t('policy.ctx.trim.note') }}
        </span>
      </div>
    </div>

    <div class="card">
      <div class="card-title">{{ t('policy.ov.title') }}</div>
      <div class="muted hint">
        {{ t('policy.ov.hint.a1') }}<b>{{ t('policy.ov.hint.bold') }}</b>{{ t('policy.ov.hint.a2') }}
        {{ t('policy.ov.hint.a3') }}
        {{ t('policy.ov.hint.a4') }}
      </div>

      <div v-if="!hosts.length && !rows.length" class="notice">
        {{ t('policy.ov.noHosts') }}
      </div>

      <div v-for="r in rows" :key="r.id" class="ov-row">
        <div class="ov-name">
          <span class="ov-title">{{ r.name }}</span>
          <span v-if="hasOverride(r.id)" class="ov-badge">{{ t('policy.ov.custom') }}</span>
          <span v-if="r.missing" class="ov-badge ov-missing">{{ t('policy.ov.missing') }}</span>
        </div>
        <div class="ov-fields">
          <label class="ov-field">
            <span>{{ t('policy.ov.turns') }}</span>
            <input
              v-model="overrides[r.id].turns"
              class="ov-turns"
              type="number"
              min="1"
              max="100"
              :placeholder="String(form.maxSessionTurns)"
            />
          </label>
          <label class="ov-field">
            <span>{{ t('policy.ov.toolKb') }}</span>
            <input
              v-model="overrides[r.id].toolKb"
              class="ov-tool-bytes"
              type="number"
              min="1"
              step="1"
              :placeholder="String(form.maxStoredToolBytesKb)"
            />
          </label>
          <label class="ov-field">
            <span>{{ t('policy.ov.totalKb') }}</span>
            <input
              v-model="overrides[r.id].totalKb"
              class="ov-total-bytes"
              type="number"
              min="1"
              step="16"
              :placeholder="String(form.maxSessionBytesKb)"
            />
          </label>
          <button class="sm ov-clear" :disabled="!hasOverride(r.id)" @click="clearOverride(r.id)">
            {{ t('policy.ov.clear') }}
          </button>
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-title">{{ t('policy.hard.title') }}</div>
      <div v-for="r in hardDenied" :key="r.name" class="rule">
        <div class="rule-name">{{ r.name }}</div>
        <div class="rule-detail mono">{{ r.detail }}</div>
      </div>
      <div class="notice">
        {{ t('policy.hard.notice.a') }}
        {{ t('policy.hard.notice.b') }}
      </div>
    </div>

    <div class="actions">
      <button class="primary" :disabled="busy" @click="save">{{ busy ? t('policy.saving') : t('policy.save') }}</button>
    </div>
  </div>
</template>

<style scoped>
.panel { padding: 24px 28px 36px; overflow-y: auto; height: 100%; }
/* 内容列：窗口再宽也不把表单 / 文本域拉成一条 —— 保持稳定的阅读宽度。 */
.panel > * { max-width: 820px; }

.panel-head { margin-bottom: 18px; }
.panel-head h2 { font-size: 16px; margin: 0 0 6px; }
.panel-head p { margin: 0; font-size: 12px; line-height: 1.7; max-width: 640px; }

/* 卡片节奏与标题层级（与「LLM 设置」面板同一套规则） */
.card + .card { margin-top: 12px; }
.card-title { font-size: 13px; font-weight: 600; margin: 0 0 12px; }
.card-sub { font-size: 12px; color: var(--text-3); line-height: 1.6; margin: -6px 0 12px; }
/* 白名单可能很长：给个高度上限，别把整页顶掉 */
textarea { max-height: 340px; }

.modes { display: flex; flex-direction: column; gap: 10px; }
.mode {
  display: flex;
  gap: 10px;
  align-items: flex-start;
  border: 1px solid var(--border);
  border-radius: 10px;
  padding: 12px 14px;
  margin: 0;
  cursor: pointer;
}
.mode.on { border-color: var(--accent); background: var(--accent-bg); }
.mode input { margin-top: 2px; }
.mode-title { font-weight: 500; color: var(--text); }
.mode-desc { font-size: 12px; color: var(--text-2); margin-top: 3px; line-height: 1.6; }

.hint { font-size: 12px; line-height: 1.7; margin-top: 8px; }

.wl-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.wl-head .card-title { margin: 0; }

.chk { display: flex; align-items: center; gap: 8px; margin-top: 6px; font-size: 12px; }

.grid { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }

.notice {
  margin-top: 14px;
  background: var(--surface-2);
  border-radius: 8px;
  padding: 10px 12px;
  font-size: 12px;
  line-height: 1.7;
  color: var(--text-2);
}

.rule { padding: 8px 0; border-bottom: 1px solid var(--border); }
.rule:last-of-type { border-bottom: none; }
.rule-name { font-size: 12px; font-weight: 500; color: var(--danger); }
.rule-detail { font-size: 11px; color: var(--text-3); margin-top: 3px; line-height: 1.6; word-break: break-word; }

.ov-row { border-top: 1px solid var(--border); padding: 10px 0 4px; }
.ov-row:first-of-type { border-top: none; padding-top: 4px; }
.ov-name { display: flex; align-items: center; gap: 8px; }
.ov-title { font-size: 12px; font-weight: 500; color: var(--text); }
.ov-badge {
  font-size: 10px;
  line-height: 1.6;
  padding: 0 6px;
  border-radius: 999px;
  background: var(--accent-bg);
  color: var(--accent);
}
.ov-missing { background: var(--surface-2); color: var(--text-3); }
.ov-fields { display: flex; align-items: flex-end; gap: 10px; margin-top: 6px; flex-wrap: wrap; }
.ov-field { display: flex; flex-direction: column; gap: 3px; margin: 0; }
.ov-field span { font-size: 11px; color: var(--text-3); }
.ov-field input { width: 110px; }
.ov-clear { margin-bottom: 1px; }

/* 动作区：一条分隔线收尾（与「LLM 设置」一致） */
.actions {
  display: flex;
  justify-content: flex-end;
  margin-top: 18px;
  padding-top: 14px;
  border-top: 1px solid var(--border);
}
label { margin-top: 0; }
</style>
