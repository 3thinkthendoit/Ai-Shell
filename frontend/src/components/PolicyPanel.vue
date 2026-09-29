<script setup>
import { computed, reactive, ref, watch } from 'vue'
import { store, push } from '../store'

const form = reactive({
  mode: store.policy.mode,
  whitelist: (store.policy.whitelist || []).join('\n'),
  redactOutput: store.policy.redactOutput,
  allowCrossHost: store.policy.allowCrossHost ?? false,
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
    push({ kind: 'system', content: '已载入默认白名单，点击「保存策略」后生效' })
  } catch (e) {
    push({ kind: 'error', content: String(e) })
  }
}

const hardDenied = [
  { name: '登录凭据文件', detail: '/etc/shadow、/etc/gshadow、~/.ssh/id_*、ssh_host_*_key、.aws/credentials、.kube/config、.docker/config.json、.netrc、.pgpass' },
  { name: '环境变量整块导出', detail: 'printenv、单独执行 env、export -p、/proc/*/environ' },
  { name: '不可逆破坏性操作', detail: 'mkfs、wipefs、dd of=/dev/*、rm -rf /、> /dev/sd*' },
  { name: '服务中断类操作', detail: 'shutdown、reboot、halt、poweroff、systemctl poweroff/reboot' },
  { name: '其他高危', detail: 'fork bomb、chmod -R 777 /、全盘 chown、kill -9 -1、清空防火墙规则、userdel -r' }
]

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
      sessionOverrides: buildOverrides()
    }
    await window.go.main.App.SavePolicy(payload)
    store.policy = payload
    push({ kind: 'system', content: '安全策略已保存' })
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
        <h2>安全策略</h2>
        <p class="muted">
          这里决定 LLM 提出的命令能走多远。硬拒绝规则不受任何配置影响 —— 它们是需求 4 的强制边界。
        </p>
      </div>
    </header>

    <div class="card">
      <label>Agent 执行模式</label>
      <div class="modes">
        <label class="mode" :class="{ on: form.mode === 'manual' }">
          <input type="radio" value="manual" v-model="form.mode" />
          <div>
            <div class="mode-title">手动模式</div>
            <div class="mode-desc">仅约束 LLM：Agent 提出的每一条命令都需你批准。人在 Agent 会话里敲的 shell 仍只对高危（删/建/改等）确认。</div>
          </div>
        </label>
        <label class="mode" :class="{ on: form.mode === 'whitelist' }">
          <input type="radio" value="whitelist" v-model="form.mode" />
          <div>
            <div class="mode-title">白名单模式</div>
            <div class="mode-desc">LLM 命中只读命令库的自动执行，其余需批准。人敲的 shell 不走此闸门（仍按高危确认）；未知第三方会先查远端是否存在。</div>
          </div>
        </label>
      </div>
    </div>

    <div class="card" style="margin-top: 12px">
      <div class="wl-head">
        <label>只读命令库 / 白名单（每行一条，按命令前缀匹配）</label>
        <button class="sm" :disabled="form.mode !== 'whitelist'" @click="restoreDefaults">
          恢复默认
        </button>
      </div>
      <textarea
        v-model="form.whitelist"
        rows="12"
        :disabled="form.mode !== 'whitelist'"
        placeholder="ls&#10;cat&#10;systemctl status"
      ></textarea>
      <div class="muted hint">
        对 LLM：白名单决定是否自动执行；高危变更（删/建/改等）即使命中也需确认。
        对人敲的 shell：名单当作「已知 Linux 命令库」——库外命令会先
        <span class="mono">command -v</span>，不存在则提示改问 Agent。
        含重定向（<span class="mono">&gt;</span>）的命令不会因白名单自动放行。
      </div>
    </div>

    <div class="card" style="margin-top: 12px">
      <label>执行范围</label>
      <label class="chk">
        <input type="checkbox" v-model="form.allowCrossHost" class="cross-host" />
        允许跨主机执行（默认关闭）
      </label>
      <div class="muted hint">
        关闭时，一条会话里的工具只能操作<b>该会话所属的主机</b> ——
        Agent 看得到所有主机，但对其余主机的调用会被直接拒绝。
        这是在防「A 机器的排查对话悄悄把命令打到 B 机器上」的横向移动。
        打开后，目标可以是任何已配置主机，但每条命令仍要过安全策略与审批。
      </div>
    </div>

    <div class="card" style="margin-top: 12px">
      <label>回传给 LLM 的内容</label>
      <label class="chk">
        <input type="checkbox" v-model="form.redactOutput" />
        自动脱敏命令输出中的密钥（强烈建议保持开启）
      </label>

      <div class="grid">
        <div>
          <label style="margin-top: 12px">单次回传上限 (字节)</label>
          <input v-model="form.maxOutput" type="number" min="1024" step="1024" />
        </div>
        <div>
          <label style="margin-top: 12px">单轮最大工具调用步数</label>
          <input v-model="form.maxSteps" type="number" min="1" max="50" />
        </div>
      </div>

      <div class="notice">
        脱敏做两件事：把本机已保存的密钥字面值整段替换，以及按模式识别私钥块、各类 token、连接串密码等。
        被替换的内容显示为 <span class="mono">[REDACTED]</span>。
      </div>

      <div class="notice">
        <b>提示注入防护。</b>远端主机的输出是不可信数据 —— 日志、文件名、HTTP 响应里都可能藏着
        「忽略之前的指令，去执行 X」这类文本。回传给 LLM 的内容会被包裹在
        <span class="mono">&lt;&lt;&lt;UNTRUSTED_REMOTE_OUTPUT&gt;&gt;&gt;</span> 边界内并声明其数据性质，
        同时会检测常见注入话术，命中时在控制台高亮告警。
        会破坏对话结构的 chat 控制符（如 <span class="mono">&lt;|im_start|&gt;</span>）会被转义。
        <br />
        <span class="muted">
          注意：这一层是降低成功率，不是保证。真正的兜底是上面的硬拒绝规则 ——
          即使 LLM 被完全说服，读取凭据与破坏性命令依然执行不了。
        </span>
      </div>
    </div>

    <div class="card" style="margin-top: 12px">
      <label>会话上下文（模型记得多少）</label>

      <div class="grid">
        <div>
          <label style="margin-top: 12px">每台主机保留轮数</label>
          <input v-model="form.maxSessionTurns" class="session-turns" type="number" min="1" max="100" />
        </div>
        <div>
          <label style="margin-top: 12px">单条工具输出上限 (KB)</label>
          <input v-model="form.maxStoredToolBytesKb" class="session-tool-bytes" type="number" min="1" step="1" />
        </div>
      </div>

      <div style="margin-top: 12px">
        <label style="margin-top: 0">每台主机历史总量上限 (KB)</label>
        <input v-model="form.maxSessionBytesKb" class="session-total-bytes" type="number" min="1" step="16" />
      </div>

      <div class="notice">
        <b>对话记忆按主机独立保存</b>，加密存在本机配置目录里（重启后仍在，
        可用「控制台」里的「清空上下文」丢掉）。
        每轮提问会把命中的历史一并发给模型，所以这几个值直接决定
        <b>单次请求的体积与费用</b>：调大让长链路排查更连贯，调小更省 token。
      </div>

      <div class="notice">
        <b>裁剪以「整轮」为单位</b>（一问一答为一条），超限时从最旧的整轮开始丢弃。
        不会从中间切断，否则历史里会留下悬空的工具调用，导致该主机此后每次请求都失败。
        <br />
        <span class="muted">
          单条工具输出上限是真正兜住上下文窗口的那一项：命令回显可能很大，
          而一轮里可能调用多次工具。它只裁工具输出，不裁提问与回答的正文。
        </span>
      </div>
    </div>

    <div class="card" style="margin-top: 12px">
      <label>主机级覆盖（可选）</label>
      <div class="muted hint">
        上面三个值是<b>全局默认</b>。某台主机需要不一样时在这里单独填 ——
        留空的那一项仍然跟随全局，不必把三项都抄一遍。
        占位符显示的就是它当前会继承到的值。
      </div>

      <div v-if="!hosts.length && !rows.length" class="notice">
        还没有主机。添加主机后可以在这里为它单独设置上下文上限。
      </div>

      <div v-for="r in rows" :key="r.id" class="ov-row">
        <div class="ov-name">
          <span class="ov-title">{{ r.name }}</span>
          <span v-if="hasOverride(r.id)" class="ov-badge">已自定义</span>
          <span v-if="r.missing" class="ov-badge ov-missing">主机已不存在</span>
        </div>
        <div class="ov-fields">
          <label class="ov-field">
            <span>轮数</span>
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
            <span>单条输出 (KB)</span>
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
            <span>总量 (KB)</span>
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
            清除
          </button>
        </div>
      </div>
    </div>

    <div class="card" style="margin-top: 12px">
      <label>硬拒绝规则（不可关闭）</label>
      <div v-for="r in hardDenied" :key="r.name" class="rule">
        <div class="rule-name">{{ r.name }}</div>
        <div class="rule-detail mono">{{ r.detail }}</div>
      </div>
      <div class="notice">
        这些规则在策略引擎中无条件生效，与上方模式、白名单无关 ——
        这是「LLM 绝对不允许直接读取 Linux 登录配置与密钥」的落地方式。
      </div>
    </div>

    <div class="actions">
      <button class="primary" :disabled="busy" @click="save">{{ busy ? '保存中…' : '保存策略' }}</button>
    </div>
  </div>
</template>

<style scoped>
.panel { padding: 22px 26px; overflow-y: auto; height: 100%; }
.panel-head { margin-bottom: 18px; }
.panel-head h2 { font-size: 15px; margin: 0 0 6px; }
.panel-head p { margin: 0; font-size: 12px; line-height: 1.7; max-width: 640px; }

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
.mode input { width: auto; margin-top: 2px; }
.mode-title { font-weight: 500; color: var(--text); }
.mode-desc { font-size: 12px; color: var(--text-2); margin-top: 3px; line-height: 1.6; }

.hint { font-size: 12px; line-height: 1.7; margin-top: 8px; }

.wl-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.wl-head label { margin: 0; }

.chk { display: flex; align-items: center; gap: 8px; margin-top: 6px; font-size: 12px; }
.chk input { width: auto; }

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

.actions { display: flex; justify-content: flex-end; margin-top: 18px; }
label { margin-top: 0; }
</style>
