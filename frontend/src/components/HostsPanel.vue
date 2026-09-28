<script setup>
import { reactive, ref } from 'vue'
import { store, refreshHosts, push } from '../store'

const editing = ref(false)
const busy = ref(false)
const testing = ref('')
const testResult = reactive({})

const blank = () => ({
  id: '',
  name: '',
  addr: '',
  user: 'root',
  authMethod: 'password',
  note: '',
  password: '',
  privateKey: '',
  passphrase: '',
  clearSecret: false
})

const form = reactive(blank())

function startNew() {
  Object.assign(form, blank())
  editing.value = true
}

function startEdit(h) {
  Object.assign(form, blank(), {
    id: h.id,
    name: h.name,
    addr: h.addr,
    user: h.user,
    authMethod: h.authMethod,
    note: h.note || ''
  })
  editing.value = true
}

async function save() {
  if (!form.name.trim() || !form.addr.trim() || !form.user.trim()) {
    push({ kind: 'error', content: '名称、地址、用户名均为必填。' })
    return
  }
  busy.value = true
  try {
    await window.go.main.App.SaveHost({ ...form })
    await refreshHosts()
    editing.value = false
    push({ kind: 'system', content: `已保存主机「${form.name}」` })
  } catch (e) {
    push({ kind: 'error', content: String(e) })
  } finally {
    busy.value = false
  }
}

async function remove(h) {
  if (!confirm(`确认删除主机「${h.name}」及其本地保存的密钥材料？此操作不可撤销。`)) return
  try {
    await window.go.main.App.DeleteHost(h.id)
    await refreshHosts()
    push({ kind: 'system', content: `已删除主机「${h.name}」` })
  } catch (e) {
    push({ kind: 'error', content: `删除失败：${e}` })
  }
}

async function test(h) {
  testing.value = h.id
  testResult[h.id] = null
  try {
    const r = await window.go.main.App.TestHost(h.id)
    testResult[h.id] = r
  } catch (e) {
    testResult[h.id] = { ok: false, message: String(e) }
  } finally {
    testing.value = ''
  }
}

async function forgetKey(h) {
  if (!confirm(`清除「${h.name}」的主机指纹？下次连接将重新信任首次出现的密钥。`)) return
  try {
    await window.go.main.App.ForgetHostKey(h.id)
    push({ kind: 'system', content: `已清除「${h.name}」的主机指纹` })
  } catch (e) {
    push({ kind: 'error', content: `清除指纹失败：${e}` })
  }
}

const authLabel = m => ({ password: '密码', private_key: '私钥', agent: 'ssh-agent' }[m] || m)
</script>

<template>
  <div class="panel">
    <header class="panel-head">
      <div>
        <h2>主机管理</h2>
        <p class="muted">
          密钥材料用 AES-256-GCM 加密后落盘，主密钥托管在操作系统钥匙串。
          这些内容<b>不会</b>进入 LLM 的上下文。
        </p>
      </div>
      <button class="primary" @click="startNew">新增主机</button>
    </header>

    <div class="list">
      <div v-if="!store.hosts.length" class="card muted">还没有主机，点击右上角「新增主机」开始。</div>

      <div v-for="h in store.hosts" :key="h.id" class="card host">
        <div class="host-main">
          <div class="host-name">
            {{ h.name }}
            <span class="tag">{{ authLabel(h.authMethod) }}</span>
            <span v-if="!h.hasSecret && h.authMethod !== 'agent'" class="tag danger">缺少凭据</span>
          </div>
          <div class="host-addr mono">{{ h.user }}@{{ h.addr }}</div>
          <div v-if="h.note" class="muted note">{{ h.note }}</div>
          <div v-if="testResult[h.id]" class="test" :class="testResult[h.id].ok ? 'ok' : 'bad'">
            {{ testResult[h.id].ok ? '连接成功' : '连接失败' }}：{{ testResult[h.id].message }}
            <span v-if="testResult[h.id].durationMs" class="muted">({{ testResult[h.id].durationMs }}ms)</span>
          </div>
        </div>
        <div class="host-actions">
          <button class="sm" :disabled="testing === h.id" @click="test(h)">
            {{ testing === h.id ? '测试中…' : '测试连接' }}
          </button>
          <button class="sm" @click="startEdit(h)">编辑</button>
          <button class="sm" @click="forgetKey(h)">清除指纹</button>
          <button class="sm danger" @click="remove(h)">删除</button>
        </div>
      </div>
    </div>

    <div v-if="editing" class="overlay" @click.self="editing = false">
      <div class="modal">
        <h3>{{ form.id ? '编辑主机' : '新增主机' }}</h3>

        <div class="grid">
          <div>
            <label>名称</label>
            <input v-model="form.name" placeholder="生产 Web 服务器" />
          </div>
          <div>
            <label>登录方式</label>
            <select v-model="form.authMethod">
              <option value="password">密码</option>
              <option value="private_key">私钥</option>
              <option value="agent">ssh-agent</option>
            </select>
          </div>
          <div>
            <label>地址 (host:port)</label>
            <input v-model="form.addr" placeholder="10.0.0.12:22" />
          </div>
          <div>
            <label>用户名</label>
            <input v-model="form.user" placeholder="root" />
          </div>
        </div>

        <template v-if="form.authMethod === 'password'">
          <label>密码</label>
          <input v-model="form.password" type="password" :placeholder="form.id ? '留空表示不修改' : '登录密码'" />
        </template>

        <template v-else-if="form.authMethod === 'private_key'">
          <label>私钥内容 (PEM / OpenSSH)</label>
          <textarea
            v-model="form.privateKey"
            rows="6"
            :placeholder="form.id ? '留空表示不修改' : '-----BEGIN OPENSSH PRIVATE KEY-----'"
          ></textarea>
          <label style="margin-top: 10px">私钥口令 (可选)</label>
          <input v-model="form.passphrase" type="password" placeholder="没有就留空" />
          <label style="margin-top: 10px">备用密码 (可选)</label>
          <input v-model="form.password" type="password" placeholder="部分主机同时允许密码登录" />
        </template>

        <div v-else class="hint">
          使用本机 ssh-agent 中已加载的密钥，无需在此保存任何密钥材料。
          <span class="muted">（Windows 下需已启动 OpenSSH 认证代理并设置 SSH_AUTH_SOCK）</span>
        </div>

        <label style="margin-top: 12px">备注</label>
        <input v-model="form.note" placeholder="可选" />

        <label class="chk" v-if="form.id">
          <input type="checkbox" v-model="form.clearSecret" />
          清除该主机已保存的密钥材料
        </label>

        <div class="modal-actions">
          <button @click="editing = false">取消</button>
          <button class="primary" :disabled="busy" @click="save">
            {{ busy ? '保存中…' : '保存' }}
          </button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.panel { padding: 22px 26px; overflow-y: auto; height: 100%; }
.panel-head { display: flex; justify-content: space-between; align-items: flex-start; gap: 20px; margin-bottom: 18px; }
.panel-head h2 { font-size: 15px; margin: 0 0 6px; }
.panel-head p { margin: 0; font-size: 12px; line-height: 1.7; max-width: 620px; }

.list { display: flex; flex-direction: column; gap: 10px; }

.host { display: flex; justify-content: space-between; gap: 16px; align-items: flex-start; }
.host-main { min-width: 0; }
.host-name { font-weight: 500; display: flex; align-items: center; gap: 8px; }
.host-addr { color: var(--text-2); margin-top: 3px; font-size: 12px; }
.note { font-size: 12px; margin-top: 4px; }
.host-actions { display: flex; gap: 6px; flex-shrink: 0; }

.tag {
  font-size: 11px;
  background: var(--surface-2);
  color: var(--text-2);
  border-radius: 20px;
  padding: 1px 8px;
  font-weight: 400;
}
.tag.danger { background: var(--danger-bg); color: var(--danger); }

.test { margin-top: 8px; font-size: 12px; }
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
  background: var(--surface);
  border-radius: 12px;
  padding: 22px 24px;
  width: 620px;
  max-width: 100%;
  max-height: 100%;
  overflow-y: auto;
}
.modal h3 { margin: 0 0 16px; font-size: 14px; }
.grid { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; margin-bottom: 12px; }
.modal-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 20px; }
.chk { display: flex; align-items: center; gap: 8px; margin-top: 14px; color: var(--danger); font-size: 12px; }
.chk input { width: auto; }
.hint {
  background: var(--surface-2);
  border-radius: 8px;
  padding: 10px 12px;
  font-size: 12px;
  line-height: 1.7;
  color: var(--text-2);
}
label { margin-top: 12px; }
</style>
