<script setup>
import { reactive, ref } from 'vue'
import { store, refreshHosts, push } from '../store'
import { t } from '../i18n'
import UiSelect from './UiSelect.vue'

const editing = ref(false)
const busy = ref(false)
const testing = ref('')
const testResult = reactive({})

// Wails 在 macOS 上跑的是 WKWebView，原生 confirm() 不会弹框且直接返回 undefined，
// 会让「删除」「清除指纹」在首行 return。这里用一个 Promise 化的自绘模态替代。
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
    push({ kind: 'error', content: t('hosts.required') })
    return
  }
  busy.value = true
  try {
    await window.go.main.App.SaveHost({ ...form })
    await refreshHosts()
    editing.value = false
    push({ kind: 'system', content: t('hosts.saved', { name: form.name }) })
  } catch (e) {
    push({ kind: 'error', content: String(e) })
  } finally {
    busy.value = false
  }
}

async function remove(h) {
  const ok = await askConfirm({
    message: t('hosts.confirmDelete', { name: h.name }),
    okLabel: t('hosts.delete'),
    danger: true
  })
  if (!ok) return
  try {
    await window.go.main.App.DeleteHost(h.id)
    await refreshHosts()
    push({ kind: 'system', content: t('hosts.deleted', { name: h.name }) })
  } catch (e) {
    push({ kind: 'error', content: t('hosts.deleteFail', { err: e }) })
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
  const ok = await askConfirm({
    message: t('hosts.confirmForget', { name: h.name }),
    okLabel: t('hosts.forgetKey')
  })
  if (!ok) return
  try {
    await window.go.main.App.ForgetHostKey(h.id)
    push({ kind: 'system', content: t('hosts.forgot', { name: h.name }) })
  } catch (e) {
    push({ kind: 'error', content: t('hosts.forgetFail', { err: e }) })
  }
}

// 登录方式显示名。值（password/private_key/agent）是协议标识符，不翻；
// ssh-agent 是产品名，保留原文。
const authLabel = m => ({ password: t('hosts.auth.password'), private_key: t('hosts.auth.privateKey'), agent: 'ssh-agent' }[m] || m)
</script>

<template>
  <div class="panel">
    <header class="panel-head">
      <div>
        <h2>{{ t('hosts.title') }}</h2>
        <p class="muted">
          {{ t('hosts.headHint.a') }}
          {{ t('hosts.headHint.b1') }}<b>{{ t('hosts.headHint.no') }}</b>{{ t('hosts.headHint.b2') }}
        </p>
      </div>
      <button class="primary" @click="startNew">{{ t('hosts.add') }}</button>
    </header>

    <div class="list">
      <div v-if="!store.hosts.length" class="card muted">{{ t('hosts.empty') }}</div>

      <div v-for="h in store.hosts" :key="h.id" class="card host">
        <div class="host-main">
          <div class="host-name">
            {{ h.name }}
            <span class="tag">{{ authLabel(h.authMethod) }}</span>
            <span v-if="!h.hasSecret && h.authMethod !== 'agent'" class="tag danger">{{ t('hosts.noSecret') }}</span>
          </div>
          <div class="host-addr mono">{{ h.user }}@{{ h.addr }}</div>
          <div v-if="h.note" class="muted note">{{ h.note }}</div>
          <div v-if="testResult[h.id]" class="test" :class="testResult[h.id].ok ? 'ok' : 'bad'">
            {{ testResult[h.id].ok ? t('hosts.test.ok') : t('hosts.test.fail') }}{{ testResult[h.id].message }}
            <span v-if="testResult[h.id].durationMs" class="muted">({{ testResult[h.id].durationMs }}ms)</span>
          </div>
        </div>
        <div class="host-actions">
          <button class="sm" :disabled="testing === h.id" @click="test(h)">
            {{ testing === h.id ? t('hosts.test.testing') : t('hosts.test.run') }}
          </button>
          <button class="sm" @click="startEdit(h)">{{ t('hosts.edit') }}</button>
          <button class="sm" @click="forgetKey(h)">{{ t('hosts.forgetKey') }}</button>
          <button class="sm danger" @click="remove(h)">{{ t('hosts.delete') }}</button>
        </div>
      </div>
    </div>

    <div v-if="confirmState" class="overlay" @click.self="settleConfirm(false)">
      <div class="modal confirm-modal">
        <p class="confirm-msg">{{ confirmState.message }}</p>
        <div class="modal-actions">
          <button @click="settleConfirm(false)">{{ t('hosts.cancel') }}</button>
          <button
            :class="confirmState.danger ? 'danger' : 'primary'"
            @click="settleConfirm(true)"
          >{{ confirmState.okLabel }}</button>
        </div>
      </div>
    </div>

    <div v-if="editing" class="overlay" @click.self="editing = false">
      <div class="modal">
        <h3>{{ form.id ? t('hosts.editTitle') : t('hosts.addTitle') }}</h3>

        <div class="grid">
          <div>
            <label>{{ t('hosts.f.name') }}</label>
            <input v-model="form.name" :placeholder="t('hosts.f.namePh')" />
          </div>
          <div>
            <label>{{ t('hosts.f.auth') }}</label>
            <UiSelect
              v-model="form.authMethod"
              :options="[
                { value: 'password', label: t('hosts.auth.password') },
                { value: 'private_key', label: t('hosts.auth.privateKey') },
                { value: 'agent', label: 'ssh-agent' },
              ]"
            />
          </div>
          <div>
            <label>{{ t('hosts.f.addr') }}</label>
            <input v-model="form.addr" placeholder="10.0.0.12:22" />
          </div>
          <div>
            <label>{{ t('hosts.f.user') }}</label>
            <input v-model="form.user" placeholder="root" />
          </div>
        </div>

        <template v-if="form.authMethod === 'password'">
          <label>{{ t('hosts.f.password') }}</label>
          <input v-model="form.password" type="password" :placeholder="form.id ? t('hosts.keepEmpty') : t('hosts.f.passwordPh')" />
        </template>

        <template v-else-if="form.authMethod === 'private_key'">
          <label>{{ t('hosts.f.key') }}</label>
          <textarea
            v-model="form.privateKey"
            rows="6"
            :placeholder="form.id ? t('hosts.keepEmpty') : '-----BEGIN OPENSSH PRIVATE KEY-----'"
          ></textarea>
          <label style="margin-top: 10px">{{ t('hosts.f.passphrase') }}</label>
          <input v-model="form.passphrase" type="password" :placeholder="t('hosts.f.passphrasePh')" />
          <label style="margin-top: 10px">{{ t('hosts.f.backupPwd') }}</label>
          <input v-model="form.password" type="password" :placeholder="t('hosts.f.backupPwdPh')" />
        </template>

        <div v-else class="hint">
          {{ t('hosts.agentHint') }}
          <span class="muted">{{ t('hosts.agentHint.win') }}</span>
        </div>

        <label style="margin-top: 12px">{{ t('hosts.f.note') }}</label>
        <input v-model="form.note" :placeholder="t('hosts.f.optional')" />

        <label class="chk" v-if="form.id">
          <input type="checkbox" v-model="form.clearSecret" />
          {{ t('hosts.clearSecret') }}
        </label>

        <div class="modal-actions">
          <button @click="editing = false">{{ t('hosts.cancel') }}</button>
          <button class="primary" :disabled="busy" @click="save">
            {{ busy ? t('hosts.saving') : t('hosts.save') }}
          </button>
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
.confirm-modal { width: 420px; }
.confirm-msg { margin: 0 0 6px; font-size: 13px; line-height: 1.7; color: var(--text); }
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
