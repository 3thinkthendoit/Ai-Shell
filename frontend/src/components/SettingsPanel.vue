<script setup>
import { reactive, ref, computed } from 'vue'
import { store, push } from '../store'
import UiSelect from './UiSelect.vue'

// ---- 方案（多套 LLM 配置，可切换） ----
//
// 数据事实来源是 store.llmProfiles（Bootstrap 拉回，每项带 active 标记）。
// editingId 是「正在编辑」的方案；空串表示「新建中」，而不是某个真实方案。
// 与「正在使用」（active）是两回事：编辑非激活方案时，两者不同。
const profiles = computed(() => store.llmProfiles || [])
const editingId = ref(profiles.value.find(p => p.active)?.id || '')

const profileOptions = computed(() =>
  profiles.value.map(p => ({ value: p.id, label: `${p.name}${p.active ? '（使用中）' : ''}` }))
)

const form = reactive({
  name: profiles.value.find(p => p.id === editingId.value)?.name || '默认',
  baseUrl: store.llm.baseUrl,
  model: store.llm.model,
  apiKey: '',
  clearKey: false
})

const busy = ref(false)
const testing = ref(false)
const testRes = ref(null)   // TestLLM 的返回，见 app.go 的 LLMTestResult
const corrected = ref('')   // 被自动修正的 Base URL（原值多填了端点）

const editingProfile = () => profiles.value.find(p => p.id === editingId.value)

// 把某套方案载入表单。Key 永远不回填（后端不下发），留空 = 不修改。
function loadEditing() {
  const p = editingProfile()
  form.name = p ? p.name : ''
  form.baseUrl = p ? p.baseUrl : ''
  form.model = p ? p.model : ''
  form.apiKey = ''
  form.clearKey = false
  testRes.value = null
  corrected.value = ''
}

// 新建模式：清空表单，编辑目标指向尚不存在的方案。
function newProfile() {
  editingId.value = ''
  form.name = ''
  form.baseUrl = ''
  form.model = ''
  form.apiKey = ''
  form.clearKey = false
  testRes.value = null
  corrected.value = ''
}

// 保存/切换/删除后统一重拉状态。顺带把表单对齐到「当前编辑的方案」——
// 新增方案的 ID 是后端生成的，前端必须跟着改。
async function refreshLLM() {
  const info = await window.go.main.App.Bootstrap()
  store.llm = info.llm
  store.llmProfiles = info.llmProfiles || []
  // 「设为当前」/删除/新增都会改激活方案 —— 让「下一轮用哪套」跟着走，
  // 否则在设置里切了方案，下一轮请求仍带着旧的那套（见 store.ask）。
  store.selectedProfileId = (store.llmProfiles.find(p => p.active) || {}).id || ''
  if (!profiles.value.some(p => p.id === editingId.value)) {
    editingId.value = profiles.value.find(p => p.active)?.id || ''
  }
  loadEditing()
}

// 设为当前使用的方案。下一次对话即生效，无需重启。
async function activate() {
  const id = editingId.value
  if (!id) return
  busy.value = true
  try {
    await window.go.main.App.ActivateLLMProfile(id)
    await refreshLLM()
    push({ kind: 'system', content: `已切换到服务商「${form.name}」，下一轮对话生效。` })
  } catch (e) {
    push({ kind: 'error', content: `切换服务商失败：${e && e.message ? e.message : e}` })
  } finally {
    busy.value = false
  }
}

// 删除方案。后端保证至少留一套，这里不重复判断 —— 让后端说那句话。
async function removeProfile() {
  const id = editingId.value
  if (!id) return
  busy.value = true
  try {
    await window.go.main.App.DeleteLLMProfile(id)
    await refreshLLM()
    push({ kind: 'system', content: '已删除该服务商。' })
  } catch (e) {
    push({ kind: 'error', content: `删除服务商失败：${e && e.message ? e.message : e}` })
  } finally {
    busy.value = false
  }
}

const presets = [
  { label: 'OpenAI', baseUrl: 'https://api.openai.com/v1', model: 'gpt-4o-mini' },
  { label: 'DeepSeek', baseUrl: 'https://api.deepseek.com/v1', model: 'deepseek-chat' },
  { label: '通义千问', baseUrl: 'https://dashscope.aliyuncs.com/compatible-mode/v1', model: 'qwen-plus' },
  { label: '本地 Ollama', baseUrl: 'http://localhost:11434/v1', model: 'qwen2.5:14b' },
  { label: '本地 vLLM', baseUrl: 'http://localhost:8000/v1', model: 'Qwen2.5-14B-Instruct' }
]

function applyPreset(p) {
  form.baseUrl = p.baseUrl
  form.model = p.model
  testRes.value = null
  corrected.value = ''
}

// 从后端回传的「实际请求地址」反推它真正使用的 Base URL。
//
// 刻意不在前端再实现一遍规范化规则 —— 规则只有后端一处，不会两边漂移。
function baseFromURL(u) {
  return String(u || '').replace(/\/chat\/completions$/, '')
}

async function save() {
  busy.value = true
  try {
    const saved = await window.go.main.App.SaveLLMProfile({
      id: editingId.value,
      name: form.name || '默认',
      baseUrl: form.baseUrl,
      model: form.model,
      apiKey: form.apiKey,
      clearKey: form.clearKey
    })
    editingId.value = saved.id
    // refreshLLM 内部会 loadEditing()：表单对齐到保存后的值
    // （含后端规范化过的 Base URL），并清掉 Key 输入框与测试结果。
    await refreshLLM()
    push({ kind: 'system', content: `服务商「${saved.name}」已保存` })
  } catch (e) {
    push({ kind: 'error', content: String(e) })
  } finally {
    busy.value = false
  }
}

// 真的发一次最小请求。
//
// 传的是**表单里的值**而不是已保存的值，所以「先测再存」是可行的。
// 编辑已有方案时走 TestLLMProfile：空字段回落到**该方案**已保存的值，
// 而不是当前激活方案的 —— 否则编辑 B 方案时拿 A 的 Key 去测，结论是错的。
async function test() {
  // 新建模式下表单全空时不发请求：后端 TestLLM 会回落到激活方案的配置，
  // 用户会拿到一个「连接成功」—— 但验证的根本不是他没填完的这套。
  if (!editingId.value && !form.baseUrl.trim() && !form.model.trim()) {
    testRes.value = { ok: false, message: '请先填写 Base URL 与模型名。' }
    return
  }
  testing.value = true
  testRes.value = null
  corrected.value = ''
  try {
    let res
    if (editingId.value) {
      res = await window.go.main.App.TestLLMProfile(editingId.value, form.baseUrl, form.model, form.apiKey)
    } else {
      res = await window.go.main.App.TestLLM(form.baseUrl, form.model, form.apiKey)
    }
    testRes.value = res

    // 后端会规范化地址。若与用户填的不一致就把表单改过来 ——
    // 否则会出现「测试通过、保存后存的是另一个值」这种对不上的情况。
    const used = baseFromURL(res.url)
    if (used && used !== form.baseUrl) {
      corrected.value = used
      form.baseUrl = used
    }
  } catch (e) {
    testRes.value = { ok: false, message: String(e) }
  } finally {
    testing.value = false
  }
}

const isLocal = () => /localhost|127\.0\.0\.1|0\.0\.0\.0/.test(form.baseUrl)
</script>

<template>
  <div class="panel">
    <div class="panel-inner">
      <header class="panel-head">
        <h2>LLM 设置</h2>
        <p class="muted">
          任何兼容 OpenAI <span class="mono">/v1/chat/completions</span> 与 function calling 的服务都能接入。
          指向本地模型时，数据完全不出你的机器。可以保存多个服务商，随时切换。
        </p>
      </header>

      <div class="card">
        <div class="card-title">配置服务商</div>
        <div class="card-sub">「编辑哪一套」与「用哪一套」分开管：改完点保存，再点「设为当前」切换。</div>
        <div class="profile-bar">
          <UiSelect
            class="profile-select"
            v-model="editingId"
            :options="profileOptions"
            :disabled="!profiles.length"
            placeholder="暂无服务商"
            @change="loadEditing"
          />
          <button class="sm" @click="newProfile">新建</button>
          <button class="sm" v-if="editingId && !editingProfile()?.active" :disabled="busy" @click="activate">
            设为当前
          </button>
          <button class="sm danger" v-if="profiles.length > 1 && editingId" :disabled="busy" @click="removeProfile">
            删除
          </button>
        </div>
        <div class="profile-hint muted" v-if="editingId">
          {{ editingProfile()?.active
            ? '这个服务商正在被所有对话使用。'
            : '编辑后点「保存」，再用「设为当前」切换过去。' }}
        </div>
      </div>

      <div class="card">
        <div class="card-title">服务商信息</div>
        <div class="field">
          <label>服务商名称</label>
          <input v-model="form.name" class="profile-name" placeholder="例如：DeepSeek / 本地 Qwen" />
        </div>
        <div class="field">
          <label>快速预设</label>
          <div class="presets">
            <button v-for="p in presets" :key="p.label" class="sm" @click="applyPreset(p)">
              {{ p.label }}
            </button>
          </div>
        </div>
      </div>

      <div class="card">
        <div class="card-title">连接参数</div>
        <div class="field">
          <label>Base URL</label>
          <input v-model="form.baseUrl" class="base-url" placeholder="https://api.openai.com/v1" />
        </div>
        <div class="field">
          <label>模型名</label>
          <input v-model="form.model" class="model-name" placeholder="gpt-4o-mini" />
        </div>
        <div class="field">
          <label>
            API Key
            <span v-if="editingId && editingProfile()?.hasApiKey" class="muted">（已保存，留空表示不修改）</span>
            <span v-else-if="!editingId && store.llm.hasApiKey" class="muted">（当前服务商已保存，新服务商需另行填写）</span>
          </label>
          <input v-model="form.apiKey" class="api-key" type="password" placeholder="sk-..." />
        </div>

        <label class="chk" v-if="editingId && editingProfile()?.hasApiKey">
          <input type="checkbox" v-model="form.clearKey" />
          清除已保存的 API Key
        </label>

        <div class="notice">
          API Key 与主机密码同样走 AES-256-GCM 加密，主密钥在操作系统钥匙串里。
          它只会出现在发往 LLM 服务的 HTTP 头里，绝不会被写进对话内容。
        </div>

        <!-- 自动修正提示。用户把完整端点粘进 Base URL 是很常见的手滑，
             不说一声的话他只会看到「测试通过」，却不知道自己填错了。 -->
        <div v-if="corrected" class="fix-hint">
          Base URL 已自动修正为 <span class="mono">{{ corrected }}</span> ——
          原来的值把端点（<span class="mono">/chat/completions</span>）也填了进去，
          那会拼成重复路径并返回 404。程序会自己追加这一段，填到
          <span class="mono">/v1</span> 为止即可。
        </div>

        <div v-if="testRes" class="test-result" :class="testRes.ok ? 'ok' : 'bad'">
          <div class="tr-head">
            <span class="tr-mark">{{ testRes.ok ? '✓' : '✗' }}</span>
            <span class="tr-msg">{{ testRes.message }}</span>
            <span v-if="testRes.ok" class="tr-meta">
              {{ testRes.model }} · {{ testRes.durationMs }}ms
            </span>
          </div>
          <div v-if="testRes.ok && testRes.reply" class="tr-line">
            模型回复：<span class="mono">{{ testRes.reply }}</span>
          </div>
          <!-- 失败时把实际请求的地址摆出来：404 这类问题光看错误码无从下手 -->
          <div v-if="!testRes.ok && testRes.url" class="tr-line">
            实际请求：<span class="mono">{{ testRes.url }}</span>
          </div>
        </div>

        <div class="actions">
          <span class="test-note">测试会真的发一次最小请求（max_tokens=8），不产生实质费用。</span>
          <button :disabled="testing" @click="test">{{ testing ? '测试中…' : '测试连接' }}</button>
          <button class="primary" :disabled="busy" @click="save">{{ busy ? '保存中…' : '保存' }}</button>
        </div>

        <div v-if="isLocal()" class="local-hint">
          当前指向本地地址 —— 对话内容不会离开这台机器。
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.panel { padding: 24px 28px 36px; overflow-y: auto; height: 100%; }
/* 内容列：窗口再宽也不把表单拉成一条 —— 保持稳定的阅读宽度。 */
.panel-inner { max-width: 820px; }

.panel-head { margin-bottom: 18px; }
.panel-head h2 { font-size: 16px; margin: 0 0 6px; }
.panel-head p { margin: 0; font-size: 12px; line-height: 1.7; max-width: 640px; }

/* 卡片节奏：统一间距（不再用行内 style 拼），标题与说明做层级 */
.card { padding: 16px 18px; }
.card + .card { margin-top: 12px; }
.card-title { font-size: 13px; font-weight: 600; margin: 0 0 12px; }
.card-sub { font-size: 12px; color: var(--text-3); line-height: 1.6; margin: -6px 0 12px; }

/* 字段：统一「标签在上、控件在下」的节奏，间距只在这里定义 */
.field { margin-top: 14px; }
.field label { margin-bottom: 6px; }

.presets { display: flex; flex-wrap: wrap; gap: 8px; }

.profile-bar { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
/* 方案选择器不必占满整行：给个舒适宽度，按钮紧跟其后 */
.profile-select { flex: 0 1 340px; min-width: 200px; }
.profile-hint { margin-top: 10px; font-size: 12px; }

/* 长文本输入给个上限：不随窗口无限拉长 */
.base-url, .model-name, .api-key, .profile-name { max-width: 620px; }

.notice {
  margin-top: 14px;
  background: var(--surface-2);
  border-radius: 8px;
  padding: 10px 12px;
  font-size: 12px;
  line-height: 1.7;
  color: var(--text-2);
}

/* 动作区：一条分隔线收尾，和上面的字段区分开 */
.actions {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-top: 18px;
  padding-top: 14px;
  border-top: 1px solid var(--border);
}
.test-note { margin-right: auto; font-size: 11.5px; color: var(--text-3); }

.fix-hint {
  margin-top: 12px;
  font-size: 12px;
  line-height: 1.7;
  color: var(--warn);
  background: var(--warn-bg);
  border-radius: 8px;
  padding: 9px 12px;
}

.test-result {
  margin-top: 12px;
  border-radius: 8px;
  padding: 10px 12px;
  font-size: 12px;
  line-height: 1.7;
}
.test-result.ok { background: var(--ok-bg); color: var(--ok); }
.test-result.bad { background: var(--danger-bg); color: var(--danger); }

.tr-head { display: flex; align-items: baseline; gap: 8px; flex-wrap: wrap; }
.tr-mark { font-weight: 600; }
.tr-msg { font-weight: 500; }
.tr-meta { color: var(--text-2); }
.tr-line { margin-top: 4px; word-break: break-all; }
.tr-line .mono { font-size: 11.5px; }

.chk { display: flex; align-items: center; gap: 8px; margin-top: 12px; color: var(--danger); font-size: 12px; }

.local-hint {
  margin-top: 12px;
  font-size: 12px;
  color: var(--ok);
  background: var(--ok-bg);
  border-radius: 8px;
  padding: 8px 12px;
}
label { margin-top: 0; }
</style>
