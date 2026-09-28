<script setup>
import { reactive, ref } from 'vue'
import { store, push } from '../store'

const form = reactive({
  baseUrl: store.llm.baseUrl,
  model: store.llm.model,
  apiKey: '',
  clearKey: false
})

const busy = ref(false)
const testing = ref(false)
const testRes = ref(null)   // TestLLM 的返回，见 app.go 的 LLMTestResult
const corrected = ref('')   // 被自动修正的 Base URL（原值多填了端点）

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
    await window.go.main.App.SaveLLM({ ...form })
    const info = await window.go.main.App.Bootstrap()
    store.llm = info.llm
    form.apiKey = ''
    form.clearKey = false
    testRes.value = null
    corrected.value = ''
    push({ kind: 'system', content: 'LLM 配置已保存' })
  } catch (e) {
    push({ kind: 'error', content: String(e) })
  } finally {
    busy.value = false
  }
}

// 真的发一次最小请求。
//
// 之前这里只检查「有没有填 API Key」，然后让人自己去控制台试 ——
// 那种按钮会给人「已经验过了」的错觉，比没有更糟。
// 地址写错（例如把完整的 /chat/completions 填进 Base URL）恰恰是最常见的问题，
// 只有真发一次请求才测得出来。
//
// 传的是**表单里的值**而不是已保存的值，所以「先测再存」是可行的。
async function test() {
  testing.value = true
  testRes.value = null
  corrected.value = ''
  try {
    const res = await window.go.main.App.TestLLM(form.baseUrl, form.model, form.apiKey)
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
    <header class="panel-head">
      <div>
        <h2>LLM 设置</h2>
        <p class="muted">
          任何兼容 OpenAI <span class="mono">/v1/chat/completions</span> 与 function calling 的服务都能接入。
          指向本地模型时，数据完全不出你的机器。
        </p>
      </div>
    </header>

    <div class="card">
      <label>快速预设</label>
      <div class="presets">
        <button v-for="p in presets" :key="p.label" class="sm" @click="applyPreset(p)">
          {{ p.label }}
        </button>
      </div>
    </div>

    <div class="card" style="margin-top: 12px">
      <label>Base URL</label>
      <input v-model="form.baseUrl" class="base-url" placeholder="https://api.openai.com/v1" />

      <label style="margin-top: 14px">模型名</label>
      <input v-model="form.model" class="model-name" placeholder="gpt-4o-mini" />

      <label style="margin-top: 14px">
        API Key
        <span v-if="store.llm.hasApiKey" class="muted">（已保存，留空表示不修改）</span>
      </label>
      <input v-model="form.apiKey" class="api-key" type="password" placeholder="sk-..." />

      <label class="chk" v-if="store.llm.hasApiKey">
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
</template>

<style scoped>
.panel { padding: 22px 26px; overflow-y: auto; height: 100%; }
.panel-head { margin-bottom: 18px; }
.panel-head h2 { font-size: 15px; margin: 0 0 6px; }
.panel-head p { margin: 0; font-size: 12px; line-height: 1.7; max-width: 640px; }

.presets { display: flex; flex-wrap: wrap; gap: 8px; }

.notice {
  margin-top: 14px;
  background: var(--surface-2);
  border-radius: 8px;
  padding: 10px 12px;
  font-size: 12px;
  line-height: 1.7;
  color: var(--text-2);
}

.actions { display: flex; justify-content: flex-end; align-items: center; gap: 10px; margin-top: 16px; }
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
.chk input { width: auto; }

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
