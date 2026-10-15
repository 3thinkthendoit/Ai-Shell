// ChatPanel 核心路径：发送前校验、草稿保留、模型菜单、清空记录后工具条仍在。
import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { nextTick } from 'vue'
import ChatPanel from '../components/ChatPanel.vue'
import { store, ask, clearLog, push, resetSessionChromeSeen } from '../store.js'

function makeApp(impl = {}) {
  const calls = []
  const base = {
    Ask: async () => undefined,
    SwitchLLMProfile: async () => undefined,
    ClearSession: async () => ({ cleared: 0, busy: false }),
    CompactSession: async () => ({ compacted: false, message: '' })
  }
  const merged = { ...base, ...impl }
  const app = {}
  for (const key of Object.keys(merged)) {
    app[key] = (...args) => {
      calls.push({ name: key, args })
      return merged[key](...args)
    }
  }
  return { app, calls }
}

function resetStore() {
  store.hosts = []
  store.currentHostId = ''
  store.currentSessionId = ''
  store.sessions = {}
  store.bySession = {}
  store.llmProfiles = [{ id: 'p1', name: 'deepseek', model: 'deepseek-v4', active: true }]
  store.selectedProfileId = 'p1'
  store.running = false
  store.busy = false
  store.pending = null
  store.policy = { mode: 'manual' }
  store.contextUsage = { tokens: 0, window: 0, messages: 0 }
  resetSessionChromeSeen()
}

let wrapper
let calls

function mountChat(impl) {
  const made = makeApp(impl)
  calls = made.calls
  window.go = { main: { App: made.app } }
  wrapper = mount(ChatPanel, { attachTo: document.body })
  return wrapper
}

beforeEach(() => {
  resetStore()
  delete window.go
})

afterEach(() => {
  if (wrapper) wrapper.unmount()
  wrapper = null
  delete window.go
})

describe('ChatPanel 发送前校验', () => {
  it('无主机点发送 → 弹出主机选择，不打 Ask，草稿保留', async () => {
    mountChat()
    const input = wrapper.find('.composer-input')
    await input.setValue('看看磁盘')
    await wrapper.find('.send-btn').trigger('click')
    await flushPromises()
    expect(wrapper.find('.host-menu').exists()).toBe(true)
    expect(wrapper.text()).toContain('选择主机')
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(0)
    expect(input.element.value).toBe('看看磁盘')
  })

  it('无模型点发送 → 打开模型菜单，不打 Ask，草稿保留', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.llmProfiles = []
    store.selectedProfileId = ''
    mountChat()
    const input = wrapper.find('.composer-input')
    await input.setValue('看看磁盘')
    await wrapper.find('.send-btn').trigger('click')
    await flushPromises()
    expect(wrapper.find('.model-menu').exists()).toBe(true)
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(0)
    expect(input.element.value).toBe('看看磁盘')
  })

  it('主机+模型齐全时发送 → Ask 成功后清草稿', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    mountChat()
    const input = wrapper.find('.composer-input')
    await input.setValue('看看磁盘')
    await wrapper.find('.send-btn').trigger('click')
    await flushPromises()
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(1)
    expect(input.element.value).toBe('')
  })

  it('busy 时发送 → 不打第二轮 Ask，草稿保留', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.running = true
    mountChat()
    const input = wrapper.find('.composer-input')
    await input.setValue('第二问')
    // send 在 running 时仍可点（disabled 只挡 running||busy||!draft — running 时 disabled）
    // 直接走 ask 守卫：
    const ok = await ask('第二问')
    expect(ok).toBe(false)
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(0)
  })
})

describe('ChatPanel 权限菜单', () => {
  it('点默认权限弹出菜单，可选模式且不跳页', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.policy = { mode: 'manual', whitelist: [], redactOutput: true, maxOutput: 32768, maxSteps: 12 }
    const made = makeApp({
      SavePolicy: async (p) => { store.policy = p }
    })
    calls = made.calls
    window.go = { main: { App: made.app } }
    wrapper = mount(ChatPanel, { attachTo: document.body })
    await wrapper.find('.policy-menu-wrap .meta-btn').trigger('click')
    await nextTick()
    expect(wrapper.find('.policy-menu').exists()).toBe(true)
    await wrapper.findAll('.policy-menu .model-menu-item')[1].trigger('click')
    await flushPromises()
    expect(calls.some(c => c.name === 'SavePolicy')).toBe(true)
    expect(store.policy.mode).toBe('whitelist')
    expect(wrapper.emitted('page')).toBeFalsy()
  })
})

describe('ChatPanel 模型菜单', () => {
  it('点模型 pill 弹出锚定菜单，可选方案', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.llmProfiles = [
      { id: 'p1', name: 'deepseek', model: 'v4', active: true },
      { id: 'p2', name: 'local', model: 'qwen', active: false }
    ]
    store.selectedProfileId = 'p1'
    mountChat()
    expect(wrapper.find('.model-pill-label').text()).toBe('deepseek · v4')
    expect(wrapper.find('.model-menu').exists()).toBe(false)
    await wrapper.find('.model-pill').trigger('click')
    await nextTick()
    expect(wrapper.find('.model-menu').exists()).toBe(true)
    expect(wrapper.findAll('.model-menu-item')).toHaveLength(2)
    await wrapper.findAll('.model-menu-item')[1].trigger('click')
    await flushPromises()
    expect(store.selectedProfileId).toBe('p2')
    expect(wrapper.find('.model-menu').exists()).toBe(false)
  })
})

describe('ChatPanel 清空记录后工具条', () => {
  it('会话 turns>0 时清空记录，工具条仍在', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    store.sessions = {
      h1: [{ id: 'default', name: '', turns: 2, archivedTurns: 0, isDefault: true }]
    }
    mountChat()
    push({ kind: 'user', content: 'hi' })
    await nextTick()
    expect(wrapper.find('.chat-tools').exists()).toBe(true)
    clearLog()
    await nextTick()
    expect(store.entries).toEqual([])
    expect(wrapper.find('.chat-tools').exists()).toBe(true)
  })

  it('空首页仅选主机（旧会话 turns>0）不冒出工具条，也不回营销首页', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.sessions = {
      h1: [{ id: 'default', name: '', turns: 5, archivedTurns: 0, isDefault: true }]
    }
    mountChat()
    expect(wrapper.find('.chat-tools').exists()).toBe(false)
    expect(wrapper.find('.home-title').exists()).toBe(true)
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    await nextTick()
    expect(wrapper.find('.home-title').exists()).toBe(false)
    expect(wrapper.find('.home.docked').exists()).toBe(true)
    expect(wrapper.find('.chat-tools').exists()).toBe(false)
  })
})
