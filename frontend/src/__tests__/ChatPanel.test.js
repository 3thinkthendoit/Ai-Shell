// ChatPanel 核心路径：发送前校验、草稿保留、模型菜单、主机绑定后不可切换。
import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { nextTick } from 'vue'
import ChatPanel from '../components/ChatPanel.vue'
import { store, ask, push, resetSessionChromeSeen, requestHostPicker } from '../store.js'

function makeApp(impl = {}) {
  const calls = []
  const base = {
    Ask: async () => undefined,
    ActivateLLMProfile: async () => undefined,
    Stop: async () => undefined,
    Bootstrap: async () => ({
      llm: {},
      llmProfiles: store.llmProfiles || []
    }),
    ClearSession: async () => ({ cleared: 0, busy: false }),
    CompactSession: async () => ({ compacted: false, message: '' }),
    CreateSession: async () => ({ id: 's-new', name: '新任务', turns: 0, archivedTurns: 0, isDefault: false }),
    ListSessions: async () => [
      { id: 'default', name: '', turns: 0, archivedTurns: 0, isDefault: true },
      { id: 's-new', name: '新任务', turns: 0, archivedTurns: 0, isDefault: false }
    ],
    RenameSession: async () => undefined
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
  store.draftHostId = ''
  store.freshTaskPending = false
  store.sessionBound = false
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
  store.hostPickerIntent = 0
  store.newTaskIntent = 0
  store.activeHostId = ''
  store.activeSessionId = ''
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
    expect(store.currentHostId).toBe('')
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
  it('权限 pill 显示当前模式，切换后文案跟着变且不灌系统提示', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.policy = { mode: 'manual', whitelist: [], redactOutput: true, maxOutput: 32768, maxSteps: 12 }
    const made = makeApp({
      SavePolicy: async (p) => { store.policy = p }
    })
    calls = made.calls
    window.go = { main: { App: made.app } }
    wrapper = mount(ChatPanel, { attachTo: document.body })
    push({ kind: 'user', content: 'hi' })
    await nextTick()
    const before = store.entries.length
    expect(wrapper.find('.policy-menu-wrap .meta-label').text()).toBe('手动批准')
    await wrapper.find('.policy-menu-wrap .meta-btn').trigger('click')
    await nextTick()
    expect(wrapper.find('.policy-menu').exists()).toBe(true)
    await wrapper.findAll('.policy-menu .model-menu-item')[1].trigger('click')
    await flushPromises()
    expect(calls.some(c => c.name === 'SavePolicy')).toBe(true)
    expect(store.policy.mode).toBe('whitelist')
    expect(wrapper.find('.policy-menu-wrap .meta-label').text()).toBe('白名单')
    expect(store.entries.length).toBe(before)
    expect(wrapper.text()).not.toContain('已切换为')
    expect(wrapper.emitted('page')).toBeFalsy()
  })
})

describe('ChatPanel 主机列表', () => {
  it('新建首页选主机只预选，不改 currentHostId / 不建任务', async () => {
    store.hosts = [
      { id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' },
      { id: 'h2', name: 'db', user: 'root', addr: '10.0.0.2' }
    ]
    mountChat()
    expect(wrapper.find('.home-title').exists()).toBe(true)
    await wrapper.find('.host-menu-wrap .meta-btn').trigger('click')
    await nextTick()
    expect(wrapper.find('.host-menu').exists()).toBe(true)
    await wrapper.findAll('.host-menu .model-menu-item')[1].trigger('click')
    await nextTick()
    expect(store.draftHostId).toBe('h2')
    expect(store.currentHostId).toBe('')
    expect(wrapper.find('.home-title').exists()).toBe(true)
    expect(wrapper.find('.host-menu-wrap .meta-label').text()).toContain('db')
  })

  it('无主机点发送弹出列表，选主机后不自动 Ask，需再点发送', async () => {
    store.hosts = [
      { id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' },
      { id: 'h2', name: 'db', user: 'root', addr: '10.0.0.2' }
    ]
    store.freshTaskPending = true
    mountChat()
    const input = wrapper.find('.composer-input')
    await input.setValue('看看磁盘')
    await wrapper.find('.send-btn').trigger('click')
    await flushPromises()
    expect(wrapper.find('.host-menu').exists()).toBe(true)
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(0)
    await wrapper.findAll('.host-menu .model-menu-item')[0].trigger('click')
    await flushPromises()
    expect(store.draftHostId).toBe('h1')
    expect(store.currentHostId).toBe('')
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(0)
    expect(calls.filter(c => c.name === 'CreateSession')).toHaveLength(0)
    expect(input.element.value).toBe('看看磁盘')
    expect(wrapper.find('.home-title').exists()).toBe(true)
    await wrapper.find('.send-btn').trigger('click')
    await flushPromises()
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(1)
  })

  it('新建首页预选后仍可切换，侧栏主机仍为空', async () => {
    store.hosts = [
      { id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' },
      { id: 'h2', name: 'db', user: 'root', addr: '10.0.0.2' }
    ]
    store.draftHostId = 'h1'
    mountChat()
    expect(wrapper.find('.host-menu-wrap .meta-label').text()).toContain('web')
    await wrapper.find('.host-menu-wrap .meta-btn').trigger('click')
    await nextTick()
    await wrapper.findAll('.host-menu .model-menu-item')[1].trigger('click')
    await nextTick()
    expect(store.draftHostId).toBe('h2')
    expect(store.currentHostId).toBe('')
    expect(wrapper.find('.host-menu-wrap .meta-label').text()).toContain('db')
  })

  it('发送时才把预选主机落到 currentHostId', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.draftHostId = 'h1'
    store.currentSessionId = 'default'
    store.sessions = {
      h1: [{ id: 'default', name: '', turns: 0, archivedTurns: 0, isDefault: true }]
    }
    mountChat()
    const input = wrapper.find('.composer-input')
    await input.setValue('看看磁盘')
    await wrapper.find('.send-btn').trigger('click')
    await flushPromises()
    expect(store.currentHostId).toBe('h1')
    expect(store.draftHostId).toBe('')
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(1)
  })

  it('新建任务态发送 → 静默 CreateSession，不落到已有默认会话，且立即锁定主机', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.draftHostId = 'h1'
    store.freshTaskPending = true
    store.sessions = {
      h1: [
        { id: 'default', name: '旧默认排查', turns: 5, archivedTurns: 0, isDefault: true },
        { id: 'old', name: '旧任务', turns: 2, archivedTurns: 0, isDefault: false }
      ]
    }
    mountChat({
      CreateSession: async () => ({ id: 's-fresh', name: '新任务', turns: 0, archivedTurns: 0, isDefault: false }),
      ListSessions: async () => [
        { id: 'default', name: '旧默认排查', turns: 5, archivedTurns: 0, isDefault: true },
        { id: 'old', name: '旧任务', turns: 2, archivedTurns: 0, isDefault: false },
        { id: 's-fresh', name: '新任务', turns: 0, archivedTurns: 0, isDefault: false }
      ],
      RenameSession: async () => undefined
    })
    const input = wrapper.find('.composer-input')
    await input.setValue('看看磁盘')
    await wrapper.find('.send-btn').trigger('click')
    await flushPromises()
    expect(calls.filter(c => c.name === 'CreateSession')).toHaveLength(1)
    expect(store.currentSessionId).toBe('s-fresh')
    expect(store.freshTaskPending).toBe(false)
    expect(store.sessionBound).toBe(true)
    expect(wrapper.find('.host-bound').exists()).toBe(true)
    expect(calls.filter(c => c.name === 'Ask')[0].args[1]).toBe('s-fresh')
    // 标题 = 首条用户消息
    const renames = calls.filter(c => c.name === 'RenameSession')
    expect(renames.some(c => c.args[1] === 's-fresh' && c.args[2] === '看看磁盘')).toBe(true)
  })

  it('CreateSession 失败 → 退回预选态，可重试', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.draftHostId = 'h1'
    store.freshTaskPending = true
    mountChat({
      CreateSession: async () => { throw new Error('boom') },
      ListSessions: async () => [
        { id: 'default', name: '', turns: 0, archivedTurns: 0, isDefault: true }
      ]
    })
    const input = wrapper.find('.composer-input')
    await input.setValue('看看磁盘')
    await wrapper.find('.send-btn').trigger('click')
    await flushPromises()
    expect(store.currentHostId).toBe('')
    expect(store.draftHostId).toBe('h1')
    expect(store.freshTaskPending).toBe(true)
    expect(store.sessionBound).toBe(false)
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(0)
    expect(wrapper.find('.host-bound').exists()).toBe(false)
    expect(input.element.value).toBe('看看磁盘')
  })

  it('发出消息进入对话框后主机锁定，不可再切换', async () => {
    store.hosts = [
      { id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' },
      { id: 'h2', name: 'db', user: 'root', addr: '10.0.0.2' }
    ]
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    mountChat()
    push({ kind: 'user', content: 'hi' })
    await nextTick()
    expect(wrapper.find('.host-bound').exists()).toBe(true)
    expect(wrapper.find('.host-bound').text()).toContain('web')
    expect(wrapper.find('.host-menu-wrap button.meta-btn').exists()).toBe(false)
    expect(wrapper.find('.host-menu').exists()).toBe(false)
    expect(store.currentHostId).toBe('h1')
  })

  it('无主机点「打开终端」→ 直接进终端（空态选主机），不盖欢迎页菜单', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    mountChat()
    await wrapper.find('.home-term-link').trigger('click')
    await flushPromises()
    expect(wrapper.find('.host-menu').exists()).toBe(false)
    expect(wrapper.emitted('term')).toBeTruthy()
    expect(store.currentHostId).toBe('')
  })

  it('有预选主机点「打开终端」→ 先提交再进终端', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.draftHostId = 'h1'
    store.freshTaskPending = true
    mountChat({
      CreateSession: async () => ({ id: 's-term', name: '新任务', turns: 0, archivedTurns: 0, isDefault: false }),
      ListSessions: async () => [
        { id: 'default', name: '', turns: 0, archivedTurns: 0, isDefault: true },
        { id: 's-term', name: '新任务', turns: 0, archivedTurns: 0, isDefault: false }
      ]
    })
    await wrapper.find('.home-term-link').trigger('click')
    await flushPromises()
    expect(store.currentHostId).toBe('h1')
    expect(store.draftHostId).toBe('')
    expect(store.currentSessionId).toBe('s-term')
    expect(calls.filter(c => c.name === 'CreateSession')).toHaveLength(1)
    expect(wrapper.emitted('term')).toBeTruthy()
  })

  it('requestHostPicker 弹出列表；选完只预选，不改 currentHostId', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    mountChat()
    requestHostPicker()
    await nextTick()
    expect(wrapper.find('.host-menu').exists()).toBe(true)
    await wrapper.find('.host-menu .model-menu-item').trigger('click')
    await flushPromises()
    expect(store.draftHostId).toBe('h1')
    expect(store.currentHostId).toBe('')
    expect(wrapper.find('.home-title').exists()).toBe(true)
  })
})

describe('ChatPanel 切换模型不塞系统气泡', () => {
  it('空首页切换模型 → 不 push，欢迎区仍在', async () => {
    store.llmProfiles = [
      { id: 'p1', name: 'deepseek', model: 'v4', active: true },
      { id: 'p2', name: 'local', model: 'qwen', active: false }
    ]
    store.selectedProfileId = 'p1'
    mountChat()
    expect(wrapper.find('.home-title').exists()).toBe(true)
    await wrapper.find('.model-pill').trigger('click')
    await nextTick()
    await wrapper.findAll('.model-menu-item')[1].trigger('click')
    await flushPromises()
    expect(store.selectedProfileId).toBe('p2')
    expect(store.entries).toEqual([])
    expect(wrapper.find('.home-title').exists()).toBe(true)
  })

  it('有消息时切换模型 → 也不 push 系统提示', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    store.llmProfiles = [
      { id: 'p1', name: 'deepseek', model: 'v4', active: true },
      { id: 'p2', name: '国外', model: 'gpt', active: false }
    ]
    store.selectedProfileId = 'p1'
    mountChat()
    push({ kind: 'user', content: 'hi' })
    push({ kind: 'assistant', content: 'hello' })
    await nextTick()
    const before = store.entries.length
    await wrapper.find('.model-pill').trigger('click')
    await nextTick()
    await wrapper.findAll('.model-menu-item')[1].trigger('click')
    await flushPromises()
    expect(store.selectedProfileId).toBe('p2')
    expect(store.entries).toHaveLength(before)
    expect(store.entries.some(e => e.kind === 'system')).toBe(false)
  })

  it('空首页切换权限 → 不 push，欢迎区仍在', async () => {
    store.policy = { mode: 'manual', whitelist: [], redactOutput: true, maxOutput: 32768, maxSteps: 12 }
    const made = makeApp({
      SavePolicy: async (p) => { store.policy = p }
    })
    calls = made.calls
    window.go = { main: { App: made.app } }
    wrapper = mount(ChatPanel, { attachTo: document.body })
    expect(wrapper.find('.home-title').exists()).toBe(true)
    await wrapper.find('.policy-menu-wrap .meta-btn').trigger('click')
    await nextTick()
    await wrapper.findAll('.policy-menu .model-menu-item')[1].trigger('click')
    await flushPromises()
    expect(store.policy.mode).toBe('whitelist')
    expect(store.entries).toEqual([])
    expect(wrapper.find('.home-title').exists()).toBe(true)
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

  it('任务中途换模型：有草稿也不自动 Ask，下一轮手动发送带新 profileId', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    store.sessionBound = true
    store.llmProfiles = [
      { id: 'p1', name: 'deepseek', model: 'v4', active: true },
      { id: 'p2', name: '国外', model: 'deepseek-v4.1-flash', active: false }
    ]
    store.selectedProfileId = 'p1'
    mountChat()
    push({ kind: 'user', content: 'hi' })
    await nextTick()
    const input = wrapper.find('.composer-input')
    await input.setValue('继续任务')
    await wrapper.find('.model-pill').trigger('click')
    await nextTick()
    await wrapper.findAll('.model-menu-item')[1].trigger('click')
    await flushPromises()
    expect(store.selectedProfileId).toBe('p2')
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(0)
    expect(input.element.value).toBe('继续任务')
    await wrapper.find('.send-btn').trigger('click')
    await flushPromises()
    const asks = calls.filter(c => c.name === 'Ask')
    expect(asks).toHaveLength(1)
    expect(asks[0].args[3]).toBe('p2')
    expect(input.element.value).toBe('')
  })

  it('运行中换模型 → 先 Stop 再切换，之后可发送；不灌「已中断」', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    store.sessionBound = true
    store.running = true
    store.llmProfiles = [
      { id: 'p1', name: 'deepseek', model: 'v4', active: true },
      { id: 'p2', name: '国外', model: 'flash', active: false }
    ]
    store.selectedProfileId = 'p1'
    mountChat()
    push({ kind: 'user', content: 'hi' })
    await nextTick()
    expect(wrapper.find('.model-pill').attributes('disabled')).toBeUndefined()
    await wrapper.find('.model-pill').trigger('click')
    await nextTick()
    await wrapper.findAll('.model-menu-item')[1].trigger('click')
    await flushPromises()
    expect(calls.some(c => c.name === 'Stop')).toBe(true)
    expect(store.running).toBe(false)
    expect(store.selectedProfileId).toBe('p2')
    expect(store.entries.some(e => e.kind === 'system' && /中断/.test(e.content || ''))).toBe(false)
    const input = wrapper.find('.composer-input')
    await input.setValue('换模型后续跑')
    await wrapper.find('.send-btn').trigger('click')
    await flushPromises()
    const asks = calls.filter(c => c.name === 'Ask')
    expect(asks).toHaveLength(1)
    expect(asks[0].args[3]).toBe('p2')
  })

  it('缺模型打开菜单后关掉 → 之后中途换模型不自动发送', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    store.llmProfiles = []
    store.selectedProfileId = ''
    mountChat()
    const input = wrapper.find('.composer-input')
    await input.setValue('继续任务')
    await wrapper.find('.send-btn').trigger('click')
    await flushPromises()
    expect(wrapper.find('.model-menu').exists()).toBe(true)
    document.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }))
    await nextTick()
    expect(wrapper.find('.model-menu').exists()).toBe(false)
    store.llmProfiles = [
      { id: 'p1', name: 'deepseek', model: 'v4', active: true },
      { id: 'p2', name: '国外', model: 'flash', active: false }
    ]
    store.selectedProfileId = 'p1'
    await nextTick()
    await wrapper.find('.model-pill').trigger('click')
    await nextTick()
    await wrapper.findAll('.model-menu-item')[1].trigger('click')
    await flushPromises()
    expect(store.selectedProfileId).toBe('p2')
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(0)
    expect(input.element.value).toBe('继续任务')
  })

  it('无模型被拦住后选模型 → 自动续发', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    store.llmProfiles = [
      { id: 'p2', name: '国外', model: 'flash', active: false }
    ]
    store.selectedProfileId = ''
    mountChat()
    const input = wrapper.find('.composer-input')
    await input.setValue('看看磁盘')
    await wrapper.find('.send-btn').trigger('click')
    await flushPromises()
    expect(wrapper.find('.model-menu').exists()).toBe(true)
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(0)
    await wrapper.findAll('.model-menu-item')[0].trigger('click')
    await flushPromises()
    expect(store.selectedProfileId).toBe('p2')
    const asks = calls.filter(c => c.name === 'Ask')
    expect(asks).toHaveLength(1)
    expect(asks[0].args[3]).toBe('p2')
    expect(input.element.value).toBe('')
  })
})

describe('ChatPanel 运行态（发送钮变中断）', () => {
  it('空闲时是发送钮；运行中变为中断，思考占位在消息流（logo + Ai-Shell）', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    store.activeHostId = 'h1'
    store.activeSessionId = 'default'
    mountChat()
    push({ kind: 'user', content: 'hi' })
    await nextTick()
    expect(wrapper.find('.send-btn.stop-btn').exists()).toBe(false)
    expect(wrapper.find('.thinking-placeholder').exists()).toBe(false)
    expect(wrapper.find('.composer-status').exists()).toBe(false)
    store.running = true
    await nextTick()
    expect(wrapper.find('.send-btn.stop-btn').exists()).toBe(true)
    const tip = wrapper.find('.thinking-placeholder')
    expect(tip.exists()).toBe(true)
    expect(tip.find('.asst-name').text()).toBe('Ai-Shell')
    expect(tip.find('.asst-avatar').exists()).toBe(true)
    expect(tip.find('.reasoning').exists()).toBe(true)
    expect(tip.text()).toContain('思考中')
    expect(tip.text()).toContain('正在分析')
    expect(wrapper.find('.composer-status').exists()).toBe(false)
  })

  it('别的任务在跑时点新建任务 → 欢迎页不显示思考占位', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.activeHostId = 'h1'
    store.activeSessionId = 's-busy'
    store.running = true
    store.freshTaskPending = true
    store.currentHostId = ''
    store.currentSessionId = ''
    mountChat()
    expect(wrapper.find('.home-title').exists()).toBe(true)
    expect(wrapper.find('.thinking-placeholder').exists()).toBe(false)
    expect(wrapper.find('.log.archive').exists()).toBe(false)
  })

  it('新建任务态强制欢迎页，即使焦点仍停在忙碌会话也不显示其思考', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.activeHostId = 'h1'
    store.activeSessionId = 's-busy'
    store.running = true
    store.currentHostId = 'h1'
    store.currentSessionId = 's-busy'
    store.sessionBound = true
    mountChat()
    push({
      kind: 'assistant',
      content: '',
      reasoning: '让我用 openssl s_client 检查证书',
      streaming: true,
      stream: 0,
      reasoningOpen: true
    })
    await nextTick()
    expect(wrapper.text()).toContain('openssl')
    // 只打新建标记、不先清主机：欢迎页仍必须盖住忙碌会话的流式思考。
    store.freshTaskPending = true
    store.sessionBound = false
    await nextTick()
    expect(wrapper.find('.home-title').exists()).toBe(true)
    expect(wrapper.find('.log.archive').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('openssl')
  })

  it('空首页仅选主机仍留在欢迎首页，无运行态提示', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.sessions = {
      h1: [{ id: 'default', name: '', turns: 5, archivedTurns: 0, isDefault: true }]
    }
    mountChat()
    expect(wrapper.find('.thinking-placeholder').exists()).toBe(false)
    expect(wrapper.find('.home-title').exists()).toBe(true)
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    await nextTick()
    expect(wrapper.find('.home-title').exists()).toBe(true)
    expect(wrapper.find('.home.docked').exists()).toBe(false)
    expect(wrapper.find('.send-btn.stop-btn').exists()).toBe(false)
  })
})

describe('ChatPanel 上下文圆环', () => {
  it('无消息时空闲态；有时间线时按本地估算显示，不必等发送', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    store.llmProfiles = [{ id: 'p1', name: '国外', model: 'deepseek-v4.1-flash', active: true }]
    store.selectedProfileId = 'p1'
    store.contextUsage = { tokens: 0, window: 0, messages: 0, model: '' }
    mountChat()
    expect(wrapper.find('.ctx-ring-wrap').exists()).toBe(true)
    expect(wrapper.find('.ctx-ring-btn.idle').exists()).toBe(true)
    push({ kind: 'user', content: '看看磁盘占用情况并给出建议' })
    push({ kind: 'assistant', content: '磁盘检查结果：根分区已使用 72%。' })
    await nextTick()
    expect(wrapper.find('.ctx-ring-btn.idle').exists()).toBe(false)
    expect(wrapper.find('.ctx-ring-wrap').exists()).toBe(true)
    // deepseek → 128K 窗口，本地估算应画出占比环
    expect(wrapper.find('.ctx-ring-btn.ok, .ctx-ring-btn.mid, .ctx-ring-btn.unknown').exists()).toBe(true)
  })

  it('有本轮 agent:context 时优先用服务端估算', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    mountChat()
    push({ kind: 'user', content: 'hi' })
    store.contextUsage = { tokens: 8500, window: 128000, messages: 12, model: 'm' }
    await nextTick()
    expect(wrapper.find('.ctx-ring-btn.ok').exists()).toBe(true)
  })
})

describe('ChatPanel 消息操作', () => {
  it('用户气泡显示时间与复制/编辑/重发；编辑写入输入框', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    mountChat()
    push({ kind: 'user', content: '看看磁盘', ts: new Date(2026, 9, 12, 15, 22).getTime() })
    await nextTick()
    const actions = wrapper.find('.msg.user .msg-actions')
    expect(actions.exists()).toBe(true)
    expect(actions.find('.msg-time').text()).toContain('10月12日')
    expect(actions.findAll('.msg-act')).toHaveLength(3)
    await actions.findAll('.msg-act')[2].trigger('click')
    await nextTick()
    expect(wrapper.find('.composer-input').element.value).toBe('看看磁盘')
  })

  it('助手定稿后显示复制；流式中不显示操作条', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    mountChat()
    push({ kind: 'assistant', content: '磁盘充足', streaming: true, stream: 0 })
    await nextTick()
    expect(wrapper.find('.asst-footer').exists()).toBe(false)
    store.entries[0].streaming = false
    await nextTick()
    expect(wrapper.find('.asst-footer .msg-actions').exists()).toBe(true)
  })

  it('助手定稿后底栏显示输出 token', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    mountChat()
    push({
      kind: 'assistant',
      content: '磁盘充足，可以继续部署。',
      streaming: false,
      tokensIn: 12000,
      tokensOut: 86
    })
    await nextTick()
    const tokens = wrapper.find('.msg-tokens')
    expect(tokens.exists()).toBe(true)
    expect(tokens.text()).toContain('tokens')
    expect(tokens.text()).toMatch(/输入约.*12K/)
    expect(tokens.text()).toMatch(/输出约/)
  })
})

describe('ChatPanel Markdown', () => {
  it('助手回复渲染为 HTML（标题/表格），不显示原始 # | 语法', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'
    mountChat()
    push({
      kind: 'assistant',
      content: '## 二、Web\n\n| 服务 | 端口 |\n|---|---|\n| Nginx | 80 |\n\n路径 `/www`',
      model: 'deepseek'
    })
    await nextTick()
    const body = wrapper.find('.asst-text .md-body')
    expect(body.exists()).toBe(true)
    expect(body.find('h2').exists()).toBe(true)
    expect(body.find('table').exists()).toBe(true)
    expect(body.find('code').exists()).toBe(true)
    expect(body.text()).toContain('Nginx')
    expect(body.html()).not.toContain('|---|')
  })
})
