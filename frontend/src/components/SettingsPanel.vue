<script setup>
import { reactive, ref, computed } from 'vue'
import { store, push } from '../store'
import { t } from '../i18n'

// ---- 方案（多套 LLM 配置，可切换）----
//
// 布局与 HostsPanel 一致：列表卡片 + 弹窗编辑。
// 数据事实来源是 store.llmProfiles（Bootstrap 拉回，每项带 active 标记）。
// editingId 是「弹窗正在编辑」的方案；空串表示「新建中」。
// 与「正在使用」（active）是两回事：编辑非激活方案时，两者不同。
const profiles = computed(() => store.llmProfiles || [])

const editing = ref(false)
const editingId = ref('')
const busy = ref(false)
const testing = ref(false)
const testRes = ref(null)   // 弹窗内「测试连接」的结果，见 app.go 的 LLMTestResult
const corrected = ref('')   // 被自动修正的 Base URL（原值多填了端点）

// 卡片上的「测试连接」：对已保存的方案发一次真实请求。
// 传空字段 —— 后端 TestLLMProfile 会回落到该方案已保存的值。
// 按卡片 id 记录进行中标记：允许同时测多张卡片，互不挤掉对方的状态。
const cardTesting = reactive({})
const cardResult = reactive({})

// 「模型名」的候选列表：从网关标准 GET /models 拉取。
// 仅辅助手输（datalist 不限制输入），拉不到时用户照常手动填写。
const modelOptions = ref([])
const modelsBusy = ref(false)

// Wails 在 macOS 上跑的是 WKWebView，原生 confirm() 不会弹框且直接返回 undefined。
// 与 HostsPanel 相同，用一个 Promise 化的自绘模态替代。
const confirmState = ref(null)

function askConfirm({ message, okLabel, danger = false }) {
  return new Promise(resolve => {
    confirmState.value = { message, okLabel, danger, resolve }
  })
}

function settleConfirm(ok) {
  const s = confirmState.value
  confirmState.value = null
  if (s) s.resolve(ok)
}

const form = reactive({
  name: '',
  baseUrl: '',
  model: '',
  // 空串 = 自动按模型名推断；数字 = 用户指定的上下文窗口（token）。
  contextWindow: '',
  supportsVision: false,
  apiKey: '',
  clearKey: false
})

// 新建模式：清空表单，编辑目标指向尚不存在的方案。
function startNew() {
  editingId.value = ''
  resetForm()
  modelOptions.value = []
  editing.value = true
}

// 编辑模式：把方案载入表单。Key 永远不回填（后端不下发），留空 = 不修改。
function startEdit(p) {
  editingId.value = p.id
  form.name = p.name
  form.baseUrl = p.baseUrl
  form.model = p.model
  form.contextWindow = p.contextWindow > 0 ? String(p.contextWindow) : ''
  form.supportsVision = !!p.supportsVision
  form.apiKey = ''
  form.clearKey = false
  testRes.value = null
  corrected.value = ''
  modelOptions.value = []
  editing.value = true
}

function resetForm() {
  form.name = ''
  form.baseUrl = ''
  form.model = ''
  form.contextWindow = ''
  form.supportsVision = false
  form.apiKey = ''
  form.clearKey = false
  testRes.value = null
  corrected.value = ''
}

function closeModal() {
  // 保存进行中不允许关闭：半途关掉会让人误以为已保存。
  if (busy.value) return
  editing.value = false
}

function parseContextWindow(raw) {
  const s = String(raw || '').trim().replace(/[,_\s]/g, '')
  if (!s) return 0
  const n = Number(s)
  if (!Number.isFinite(n) || n < 0) return 0
  return Math.floor(n)
}

// 保存/切换/删除后统一重拉状态。
// 「设为当前」/删除/新增都会改激活方案 —— 让「下一轮用哪套」跟着走，
// 否则在设置里切了方案，下一轮请求仍带着旧的那套（见 store.ask）。
async function refreshLLM() {
  const info = await window.go.main.App.Bootstrap()
  store.llm = info.llm
  store.llmProfiles = info.llmProfiles || []
  store.selectedProfileId = (store.llmProfiles.find(p => p.active) || {}).id || ''
}

// 设为当前使用的方案。下一次对话即生效，无需重启。
async function activate(p) {
  busy.value = true
  try {
    await window.go.main.App.ActivateLLMProfile(p.id)
    await refreshLLM()
    push({ kind: 'system', content: t('settings.switched', { name: p.name }) })
  } catch (e) {
    push({ kind: 'error', content: t('settings.switchFail', { err: e && e.message ? e.message : e }) })
  } finally {
    busy.value = false
  }
}

// 删除方案。后端保证至少留一套，这里不重复判断 —— 让后端说那句话。
async function removeProfile(p) {
  const ok = await askConfirm({
    message: t('settings.confirmDelete', { name: p.name }),
    okLabel: t('settings.delete'),
    danger: true
  })
  if (!ok) return
  busy.value = true
  try {
    await window.go.main.App.DeleteLLMProfile(p.id)
    await refreshLLM()
    push({ kind: 'system', content: t('settings.removed') })
  } catch (e) {
    push({ kind: 'error', content: t('settings.removeFail', { err: e && e.message ? e.message : e }) })
  } finally {
    busy.value = false
  }
}

// 卡片上的「测试连接」：验证的是**该方案已保存的配置**（空字段回落），
// 不经过表单，所以测试结论与编辑中的草稿互不干扰。
async function testSaved(p) {
  cardTesting[p.id] = true
  cardResult[p.id] = null
  try {
    cardResult[p.id] = await window.go.main.App.TestLLMProfile(p.id, '', '', '')
  } catch (e) {
    cardResult[p.id] = { ok: false, message: String(e) }
  } finally {
    delete cardTesting[p.id]
  }
}

// 拉取当前网关的模型列表做候选。
// 与 testSaved 同款回落：编辑已有方案时表单字段留空由后端补已保存值，
// 所以不重填 API Key 也能拉；新建模式用表单里现填的值。
async function fetchModels() {
  modelsBusy.value = true
  try {
    const res = await window.go.main.App.ListLLMModels(editingId.value, form.baseUrl, form.apiKey)
    if (res.ok) {
      modelOptions.value = res.models || []
    } else {
      // 拉取失败清掉旧候选：用户可能已改 Base URL，旧候选来自别家网关，留着会误导。
      modelOptions.value = []
      push({ kind: 'error', content: res.message })
    }
  } catch (e) {
    modelOptions.value = []
    push({ kind: 'error', content: String(e) })
  } finally {
    modelsBusy.value = false
  }
}

// 预设按钮的显示名走字典（品牌名不翻，但「本地」这类描述词要跟语言走）。
const presets = computed(() => [
  { label: 'OpenAI', baseUrl: 'https://api.openai.com/v1', model: 'gpt-4o-mini' },
  { label: 'DeepSeek', baseUrl: 'https://api.deepseek.com/v1', model: 'deepseek-chat' },
  { label: t('settings.presets.qwen'), baseUrl: 'https://dashscope.aliyuncs.com/compatible-mode/v1', model: 'qwen-plus' },
  { label: t('settings.presets.ollama'), baseUrl: 'http://localhost:11434/v1', model: 'qwen2.5:14b' },
  { label: t('settings.presets.vllm'), baseUrl: 'http://localhost:8000/v1', model: 'Qwen2.5-14B-Instruct' }
])

function applyPreset(p) {
  form.baseUrl = p.baseUrl
  form.model = p.model
  testRes.value = null
  corrected.value = ''
  // Base URL 换了，上一个网关的模型候选不再是这家服务的
  modelOptions.value = []
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
      name: form.name || t('settings.defaultName'),
      baseUrl: form.baseUrl,
      model: form.model,
      contextWindow: parseContextWindow(form.contextWindow),
      supportsVision: !!form.supportsVision,
      apiKey: form.apiKey,
      clearKey: form.clearKey
    })
    await refreshLLM()
    // 配置已变，卡片上旧的测试结论不再可信 —— 清掉，避免「结论对不上已改配置」的误导。
    if (editingId.value) delete cardResult[editingId.value]
    editing.value = false
    // 用后端规范化后的名称提示，而不是表单原值（后端可能补了默认名/去掉了空格）。
    push({ kind: 'system', content: t('settings.saved', { name: saved.name }) })
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
    testRes.value = { ok: false, message: t('settings.fillFirst') }
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

// 编辑中的方案是否已保存 Key（弹窗里「留空表示不修改」的提示只对它有意义）。
const editingProfileHasApiKey = computed(() =>
  !!(profiles.value.find(p => p.id === editingId.value) || {}).hasApiKey
)
</script>

<template>
  <div class="panel">
    <header class="panel-head">
      <div>
        <h2>{{ t('settings.title') }}</h2>
        <p class="muted">
          {{ t('settings.headHint.a') }}<span class="mono">/v1/chat/completions</span>{{ t('settings.headHint.b') }}
          {{ t('settings.headHint.c') }}
        </p>
      </div>
      <button class="primary" @click="startNew">{{ t('settings.add') }}</button>
    </header>

    <div class="list">
      <div v-if="!profiles.length" class="card muted">{{ t('settings.empty') }}</div>

      <div v-for="p in profiles" :key="p.id" class="card profile">
        <div class="profile-main">
          <div class="profile-name">
            {{ p.name }}
            <span v-if="p.active" class="tag ok">{{ t('settings.using') }}</span>
            <span v-if="!p.hasApiKey" class="tag danger">{{ t('settings.noKey') }}</span>
          </div>
          <div class="profile-addr mono">{{ p.model }} · {{ p.baseUrl }}</div>
          <div v-if="cardResult[p.id]" class="test" :class="cardResult[p.id].ok ? 'ok' : 'bad'">
            {{ cardResult[p.id].message }}
            <span v-if="cardResult[p.id].durationMs" class="muted">({{ cardResult[p.id].durationMs }}ms)</span>
          </div>
        </div>
        <div class="profile-actions">
          <button class="sm" :disabled="!!cardTesting[p.id]" @click="testSaved(p)">
            {{ cardTesting[p.id] ? t('settings.test.testing') : t('settings.test.run') }}
          </button>
          <button v-if="!p.active" class="sm" :disabled="busy" @click="activate(p)">{{ t('settings.activate') }}</button>
          <button class="sm" @click="startEdit(p)">{{ t('settings.edit') }}</button>
          <button v-if="profiles.length > 1" class="sm danger" :disabled="busy" @click="removeProfile(p)">
            {{ t('settings.delete') }}
          </button>
        </div>
      </div>
    </div>

    <!-- 删除确认：与 HostsPanel 相同的自绘模态（WKWebView 下原生 confirm 不可用） -->
    <div v-if="confirmState" class="overlay" @click.self="settleConfirm(false)">
      <div class="modal confirm-modal">
        <p class="confirm-msg">{{ confirmState.message }}</p>
        <div class="modal-actions">
          <button @click="settleConfirm(false)">{{ t('settings.cancel') }}</button>
          <button
            :class="confirmState.danger ? 'danger' : 'primary'"
            @click="settleConfirm(true)"
          >{{ confirmState.okLabel }}</button>
        </div>
      </div>
    </div>

    <!-- 只允许通过「取消」按钮或右上角关闭图标关闭，避免误点遮罩丢失已填内容 -->
    <div v-if="editing" class="overlay">
      <div class="modal">
        <button class="modal-close" :aria-label="t('settings.cancel')" :disabled="busy" @click="closeModal">×</button>
        <h3>{{ editingId ? t('settings.editTitle') : t('settings.addTitle') }}</h3>

        <div class="field">
          <label>{{ t('settings.info.name') }}</label>
          <input v-model="form.name" class="profile-name" :placeholder="t('settings.info.namePh')" />
        </div>

        <div class="field">
          <label>{{ t('settings.info.presets') }}</label>
          <div class="presets">
            <button v-for="p in presets" :key="p.label" class="sm" @click="applyPreset(p)">
              {{ p.label }}
            </button>
          </div>
        </div>

        <div class="field">
          <label>Base URL</label>
          <input v-model="form.baseUrl" class="base-url" placeholder="https://api.openai.com/v1" />
        </div>
        <div class="field">
          <label>{{ t('settings.conn.model') }}</label>
          <div class="model-row">
            <input v-model="form.model" class="model-name" list="model-options" placeholder="gpt-4o-mini" />
            <button class="sm" :disabled="modelsBusy" @click="fetchModels">
              {{ modelsBusy ? t('settings.models.fetching') : t('settings.models.fetch') }}
            </button>
          </div>
          <datalist id="model-options">
            <option v-for="m in modelOptions" :key="m" :value="m" />
          </datalist>
        </div>
        <div class="field">
          <label>{{ t('settings.conn.ctxWindow') }}</label>
          <input
            v-model="form.contextWindow"
            class="ctx-window"
            inputmode="numeric"
            :placeholder="t('settings.conn.ctxWindowPh')"
          />
          <div class="field-hint muted">{{ t('settings.conn.ctxWindowHint') }}</div>
          <label class="check-row">
            <input type="checkbox" v-model="form.supportsVision" />
            {{ t('settings.conn.vision') }}
          </label>
          <div class="field-hint muted">{{ t('settings.conn.visionHint') }}</div>
        </div>
        <div class="field">
          <label>
            API Key
            <span v-if="editingId && editingProfileHasApiKey" class="muted">{{ t('settings.key.saved') }}</span>
            <span v-else-if="!editingId && store.llm.hasApiKey" class="muted">{{ t('settings.key.activeSaved') }}</span>
          </label>
          <input v-model="form.apiKey" class="api-key" type="password" placeholder="sk-..." />
        </div>

        <label class="chk" v-if="editingId && editingProfileHasApiKey">
          <input type="checkbox" v-model="form.clearKey" />
          {{ t('settings.key.clear') }}
        </label>

        <div class="notice">
          {{ t('settings.notice') }}
        </div>

        <!-- 自动修正提示。用户把完整端点粘进 Base URL 是很常见的手滑，
             不说一声的话他只会看到「测试通过」，却不知道自己填错了。 -->
        <div v-if="corrected" class="fix-hint">
          {{ t('settings.fix.k1') }}<span class="mono">{{ corrected }}</span>{{ t('settings.fix.k2') }}<span class="mono">/chat/completions</span>{{ t('settings.fix.k3') }}<span class="mono">/v1</span>{{ t('settings.fix.k4') }}
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
            {{ t('settings.test.reply') }}<span class="mono">{{ testRes.reply }}</span>
          </div>
          <!-- 失败时把实际请求的地址摆出来：404 这类问题光看错误码无从下手 -->
          <div v-if="!testRes.ok && testRes.url" class="tr-line">
            {{ t('settings.test.actual') }}<span class="mono">{{ testRes.url }}</span>
          </div>
        </div>

        <div class="actions">
          <span class="test-note">{{ t('settings.test.note') }}</span>
          <button :disabled="busy" @click="closeModal">{{ t('settings.cancel') }}</button>
          <button :disabled="testing" @click="test">{{ testing ? t('settings.test.testing') : t('settings.test.run') }}</button>
          <button class="primary" :disabled="busy" @click="save">{{ busy ? t('settings.saving') : t('settings.save') }}</button>
        </div>

        <div v-if="isLocal()" class="local-hint">
          {{ t('settings.localHint') }}
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.panel { padding: 24px 28px 36px; overflow-y: auto; height: 100%; }
.panel-head { display: flex; justify-content: space-between; align-items: flex-start; gap: 20px; margin-bottom: 18px; }
.panel-head h2 { font-size: 16px; margin: 0 0 6px; }
.panel-head p { margin: 0; font-size: 12px; line-height: 1.7; max-width: 620px; }

.list { display: flex; flex-direction: column; gap: 10px; }

.profile { display: flex; justify-content: space-between; gap: 16px; align-items: flex-start; }
.profile-main { min-width: 0; }
.profile-name { font-weight: 500; display: flex; align-items: center; gap: 8px; }
.profile-addr { color: var(--text-2); margin-top: 3px; font-size: 12px; }
.profile-actions { display: flex; gap: 6px; flex-shrink: 0; }

.tag {
  font-size: 11px;
  background: var(--surface-2);
  color: var(--text-2);
  border-radius: 20px;
  padding: 1px 8px;
  font-weight: 400;
}
.tag.ok { background: var(--ok-bg); color: var(--ok); }
.tag.danger { background: var(--danger-bg); color: var(--danger); }

.test { margin-top: 8px; font-size: 12px; word-break: break-all; }
.test.ok { color: var(--ok); }
.test.bad { color: var(--danger); }

.overlay {
  position: absolute;
  inset: 0;
  background: rgba(0, 0, 0, 0.28);
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 30px;
  z-index: 20;
}
.modal {
  position: relative;
  background: var(--surface);
  border-radius: 12px;
  padding: 22px 24px;
  width: 620px;
  max-width: 100%;
  max-height: 100%;
  overflow-y: auto;
}
.modal h3 { margin: 0 0 16px; font-size: 14px; }
.modal-close {
  position: absolute;
  top: 12px;
  right: 14px;
  width: 28px;
  height: 28px;
  padding: 0;
  line-height: 1;
  font-size: 18px;
  border: none;
  background: transparent;
  color: var(--text-2);
  cursor: pointer;
  border-radius: 6px;
}
.modal-close:hover { background: var(--surface-2); color: var(--text); }

.confirm-modal { width: 420px; }
.confirm-msg { margin: 0 0 6px; font-size: 13px; line-height: 1.7; color: var(--text); }
.modal-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 20px; }

/* 字段：统一「标签在上、控件在下」的节奏 */
.field { margin-top: 14px; }
.field label { margin-bottom: 6px; }

.presets { display: flex; flex-wrap: wrap; gap: 8px; }

/* 长文本输入给个上限：不随窗口无限拉长 */
.base-url, .model-name, .api-key, .profile-name, .ctx-window { max-width: 100%; }
.model-row { display: flex; gap: 8px; align-items: center; }
.model-row .model-name { flex: 1; }
.field-hint { margin-top: 6px; font-size: 12px; line-height: 1.45; }

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
.check-row { display: flex; align-items: center; gap: 8px; margin-top: 10px; font-size: 13px; color: var(--text); }

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
