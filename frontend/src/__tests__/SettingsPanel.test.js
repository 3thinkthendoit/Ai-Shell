// SettingsPanel 组件测试。
//
// 布局已改为与 HostsPanel 一致的「列表卡片 + 弹窗编辑」。
//
// 这里锁的核心价值不变 —— 「测试连接」必须真的打后端、并把结果
// （尤其是失败原因和实际请求地址）呈现出来；多方案下「编辑哪一套」
// 与「用哪一套」分开，编辑/测试时必须带对方案 ID。
import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import SettingsPanel from '../components/SettingsPanel.vue'
import { store } from '../store.js'

function makeApp(impl = {}) {
  const calls = []
  const base = {
    Bootstrap: async () => ({ llm: { baseUrl: 'https://api.openai.com/v1', model: 'gpt-4o-mini', hasApiKey: true }, llmProfiles: store.llmProfiles }),
    TestLLM: async () => ({
      ok: true, message: '连接成功', url: 'https://api.openai.com/v1/chat/completions',
      status: 200, model: 'gpt-4o-mini', reply: 'pong', durationMs: 320
    }),
    TestLLMProfile: async () => ({
      ok: true, message: '连接成功', url: 'https://a/v1/chat/completions',
      status: 200, model: 'm1', reply: 'pong', durationMs: 320
    }),
    SaveLLMProfile: async (req) => ({
      id: req.id || 'new-id', name: req.name || '默认',
      baseUrl: req.baseUrl, model: req.model, hasApiKey: !!req.apiKey, active: false
    }),
    ListLLMModels: async (id, base, key) => ({ ok: true, models: ['m1', 'm2'], message: '获取到 2 个模型' }),
    ActivateLLMProfile: async () => undefined,
    DeleteLLMProfile: async () => undefined
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

let calls
let wrapper

function setup(impl) {
  const made = makeApp(impl)
  calls = made.calls
  window.go = { main: { App: made.app } }
  wrapper = mount(SettingsPanel, { attachTo: document.body })
  return wrapper
}

// 弹窗动作区按钮顺序：[取消, 测试连接, 保存]
const modalBtn = label => {
  const b = wrapper.findAll('.actions button').find(x => x.text().trim() === label)
  if (!b) throw new Error(`未找到弹窗按钮：${label}`)
  return b
}

const clickTest = async () => {
  const btn = modalBtn('测试连接')
  await btn.trigger('click')
  await flushPromises()
  return btn
}

// 打开「新增」弹窗
const openNew = async () => {
  await wrapper.find('.panel-head button.primary').trigger('click')
}

// 打开某张卡片的「编辑」弹窗
const openEdit = async (id) => {
  const card = wrapper.findAll('.profile')
    .find(c => c.findAll('button').some(b => b.text() === '编辑') && c.text().includes(
      (store.llmProfiles.find(p => p.id === id) || {}).name
    ))
  const btn = card.findAll('button').find(b => b.text() === '编辑')
  await btn.trigger('click')
  await flushPromises()
}

// 点卡片上的某个按钮（按所在方案 + 按钮文案找）
const cardBtn = async (id, label) => {
  const p = store.llmProfiles.find(x => x.id === id)
  const card = wrapper.findAll('.profile').find(c => c.text().includes(p.name))
  const btn = card.findAll('button').find(b => b.text().includes(label))
  await btn.trigger('click')
  await flushPromises()
  return btn
}

// 自绘确认框里点确认
const confirmOk = async () => {
  const btn = wrapper.findAll('.confirm-modal button').find(b => b.text().includes('删除'))
  await btn.trigger('click')
  await flushPromises()
}

beforeEach(() => {
  store.llm = { baseUrl: 'https://api.openai.com/v1', model: 'gpt-4o-mini', hasApiKey: true }
})

afterEach(() => {
  if (wrapper) wrapper.unmount()
  wrapper = null
  delete window.go
})

// ---- 新增弹窗：测试连接 ----
describe('SettingsPanel 新增弹窗 · 测试连接', () => {
  it('真的调用后端 TestLLM，而不是只做本地形状检查', async () => {
    setup()
    await openNew()
    await wrapper.find('.base-url').setValue('https://my.endpoint/v1')
    await wrapper.find('.model-name').setValue('my-model')
    await clickTest()
    expect(calls.filter(c => c.name === 'TestLLM')).toHaveLength(1)
  })

  it('传的是表单里的值，所以「先测再存」可行', async () => {
    setup()
    await openNew()
    await wrapper.find('.base-url').setValue('https://my.endpoint/v1')
    await wrapper.find('.model-name').setValue('my-model')
    await wrapper.find('.api-key').setValue('sk-typed')
    await clickTest()

    const call = calls.find(c => c.name === 'TestLLM')
    expect(call.args).toEqual(['https://my.endpoint/v1', 'my-model', 'sk-typed'])
  })

  it('API Key 留空时传空串（后端会改用钥匙串里已保存的那把）', async () => {
    setup()
    await openNew()
    await wrapper.find('.base-url').setValue('https://api.openai.com/v1')
    await wrapper.find('.model-name').setValue('gpt-4o-mini')
    await clickTest()
    expect(calls.find(c => c.name === 'TestLLM').args[2]).toBe('')
  })

  it('新建模式下表单全空时不发测试请求 —— 否则会拿到激活方案配置的「连接成功」', async () => {
    setup()
    await openNew()
    await clickTest()

    expect(calls.filter(c => c.name === 'TestLLM' || c.name === 'TestLLMProfile')).toHaveLength(0)
    expect(wrapper.find('.test-result').classes()).toContain('bad')
    expect(wrapper.find('.test-result').text()).toContain('请先填写')
  })

  it('成功时显示模型、耗时与模型回复 —— 证明对方真的回了话', async () => {
    setup()
    await openNew()
    await wrapper.find('.base-url').setValue('https://api.openai.com/v1')
    await wrapper.find('.model-name').setValue('gpt-4o-mini')
    await clickTest()
    const box = wrapper.find('.test-result')
    expect(box.classes()).toContain('ok')
    expect(box.text()).toContain('连接成功')
    expect(box.text()).toContain('gpt-4o-mini')
    expect(box.text()).toContain('320ms')
    expect(box.text()).toContain('pong')
  })

  it('失败时显示原因与实际请求地址（404 光看错误码无从下手）', async () => {
    setup({
      TestLLM: async () => ({
        ok: false,
        message: '地址不对（404）。Base URL 应填到 /v1 为止，不要带 /chat/completions。',
        url: 'https://token.sensenova.cn/v1/chat/completions/chat/completions',
        status: 404, model: 'deepseek-flash', reply: '', durationMs: 210
      })
    })
    await openNew()
    await wrapper.find('.base-url').setValue('https://x/v1')
    await wrapper.find('.model-name').setValue('m')
    await clickTest()
    const box = wrapper.find('.test-result')
    expect(box.classes()).toContain('bad')
    expect(box.text()).toContain('404')
    expect(box.text()).toContain('https://token.sensenova.cn/v1/chat/completions/chat/completions')
  })

  it('Base URL 多填了端点时自动修正表单并说明原因', async () => {
    // 这是真实发生过的手滑：把文档里的完整端点粘进 Base URL
    setup({
      TestLLM: async () => ({
        ok: true, message: '连接成功',
        url: 'https://token.sensenova.cn/v1/chat/completions', // 后端规范化后实际请求的地址
        status: 200, model: 'deepseek-flash', reply: 'pong', durationMs: 300
      })
    })
    await openNew()
    await wrapper.find('.base-url').setValue('https://token.sensenova.cn/v1/chat/completions')
    await wrapper.find('.model-name').setValue('deepseek-flash')
    await clickTest()

    // 表单被改回「到 /v1 为止」，否则会出现「测试通过但保存的是另一个值」
    expect(wrapper.find('.base-url').element.value).toBe('https://token.sensenova.cn/v1')
    expect(wrapper.find('.fix-hint').text()).toContain('/chat/completions')
  })

  it('地址本来就正确时不出现修正提示', async () => {
    setup()
    await openNew()
    await wrapper.find('.base-url').setValue('https://api.openai.com/v1')
    await wrapper.find('.model-name').setValue('gpt-4o-mini')
    await clickTest()
    expect(wrapper.find('.fix-hint').exists()).toBe(false)
  })

  it('测试中禁用按钮并改文案，防止重复点击', async () => {
    let release
    setup({ TestLLM: () => new Promise(r => { release = () => r({ ok: true, message: '连接成功', url: '', model: '', reply: '', durationMs: 1 }) }) })
    await openNew()
    await wrapper.find('.base-url').setValue('https://x/v1')
    await wrapper.find('.model-name').setValue('m')
    const btn = modalBtn('测试连接')
    await btn.trigger('click')
    await wrapper.vm.$nextTick()

    const during = modalBtn('测试中…')
    expect(during.attributes('disabled')).toBeDefined()

    release()
    await flushPromises()
    expect(modalBtn('测试连接').text()).toContain('测试连接')
  })

  it('后端抛异常时渲染成失败结果，而不是整体崩掉', async () => {
    setup({ TestLLM: async () => { throw new Error('凭证库尚未初始化') } })
    await openNew()
    await wrapper.find('.base-url').setValue('https://x/v1')
    await wrapper.find('.model-name').setValue('m')
    await clickTest()
    expect(wrapper.find('.test-result').classes()).toContain('bad')
    expect(wrapper.find('.test-result').text()).toContain('凭证库尚未初始化')
  })

  it('切换预设会清掉上一次的测试结果（避免拿旧结论当新配置的结论）', async () => {
    setup()
    await openNew()
    await wrapper.find('.base-url').setValue('https://api.openai.com/v1')
    await wrapper.find('.model-name').setValue('gpt-4o-mini')
    await clickTest()
    expect(wrapper.find('.test-result').exists()).toBe(true)

    await wrapper.findAll('.presets button')[0].trigger('click')
    expect(wrapper.find('.test-result').exists()).toBe(false)
    expect(wrapper.find('.fix-hint').exists()).toBe(false)
  })

  it('新增保存时不带方案 ID（由后端生成），保存成功后关闭弹窗', async () => {
    setup()
    await openNew()
    await wrapper.find('.base-url').setValue('https://n/v1')
    await wrapper.find('.model-name').setValue('m')
    await modalBtn('保存').trigger('click')
    await flushPromises()

    expect(calls.find(c => c.name === 'SaveLLMProfile').args[0].id).toBe('')
    expect(wrapper.find('.overlay').exists()).toBe(false)
  })

  it('获取模型列表：新建模式带表单值调用，结果填进 datalist 候选', async () => {
    setup()
    await openNew()
    await wrapper.find('.base-url').setValue('https://n/v1')
    await wrapper.find('.api-key').setValue('sk-typed')
    await wrapper.find('.model-row button').trigger('click')
    await flushPromises()

    expect(calls.find(c => c.name === 'ListLLMModels').args).toEqual(['', 'https://n/v1', 'sk-typed'])
    expect(wrapper.findAll('#model-options option').map(o => o.attributes('value'))).toEqual(['m1', 'm2'])
  })

  it('模型名手动输入不受候选影响（datalist 只辅助，不强制）', async () => {
    setup()
    await openNew()
    await wrapper.find('.model-row button').trigger('click')
    await flushPromises()
    // 手输一个列表里没有的名字，照常生效
    await wrapper.find('.model-name').setValue('my-private-model')
    expect(wrapper.find('.model-name').element.value).toBe('my-private-model')
  })

  it('拉取失败时清空候选并提示原因（不阻塞手动填写）', async () => {
    setup({ ListLLMModels: async () => ({ ok: false, models: [], message: '这个网关没有实现标准 /models 接口（404）' }) })
    await openNew()
    await wrapper.find('.model-row button').trigger('click')
    await flushPromises()

    expect(wrapper.findAll('#model-options option')).toHaveLength(0)
    // 失败原因进了消息流，用户知道为什么没候选
    const msgs = Object.values(store.bySession).flat()
    expect(msgs.some(m => String(m.content).includes('/models'))).toBe(true)
  })

  it('换了 Base URL 后拉取失败要清掉上一家的旧候选（留着会误导）', async () => {
    setup({
      ListLLMModels: async (id, base) => base === 'https://ok/v1'
        ? { ok: true, models: ['m1'], message: '获取到 1 个模型' }
        : { ok: false, models: [], message: '连接失败' }
    })
    await openNew()
    // 先在 A 网关成功拉到候选
    await wrapper.find('.base-url').setValue('https://ok/v1')
    await wrapper.find('.model-row button').trigger('click')
    await flushPromises()
    expect(wrapper.findAll('#model-options option')).toHaveLength(1)

    // 手动改到 B 网关再拉，失败 → 旧候选（A 网关的）必须清掉
    await wrapper.find('.base-url').setValue('https://bad/v1')
    await wrapper.find('.model-row button').trigger('click')
    await flushPromises()
    expect(wrapper.findAll('#model-options option')).toHaveLength(0)
  })

  it('切换预设后清空上一家的模型候选', async () => {
    setup()
    await openNew()
    await wrapper.find('.base-url').setValue('https://n/v1')
    await wrapper.find('.model-row button').trigger('click')
    await flushPromises()
    expect(wrapper.findAll('#model-options option')).toHaveLength(2)

    await wrapper.findAll('.presets button')[0].trigger('click')
    expect(wrapper.findAll('#model-options option')).toHaveLength(0)
  })
})

// ---- 多方案（profile）----
//
// 前提不变式：编辑已有方案时必须带上方案 ID —— 否则后端会把「编辑」
// 当成「新建」，或者用错回落配置（拿激活方案的 Key 去测非激活方案）。
describe('SettingsPanel 多方案', () => {
  const PROFILES = [
    { id: 'p1', name: 'A', baseUrl: 'https://a/v1', model: 'm1', hasApiKey: true, active: true },
    { id: 'p2', name: 'B', baseUrl: 'https://b/v1', model: 'm2', hasApiKey: false, active: false }
  ]

  beforeEach(() => {
    store.llmProfiles = PROFILES.map(p => ({ ...p }))
  })

  afterEach(() => {
    store.llmProfiles = []
  })

  it('编辑已有方案时，测试连接走 TestLLMProfile 并带方案 ID（回落到该方案的 Key）', async () => {
    setup()
    await openEdit('p1')
    await clickTest()
    const call = calls.find(c => c.name === 'TestLLMProfile')
    expect(call).toBeTruthy()
    expect(call.args[0]).toBe('p1')
    expect(call.args[1]).toBe('https://a/v1') // 表单回填了方案的值
    expect(call.args[2]).toBe('m1')
  })

  it('编辑保存时带方案 ID', async () => {
    setup()
    await openEdit('p1')
    await modalBtn('保存').trigger('click')
    await flushPromises()
    expect(calls.find(c => c.name === 'SaveLLMProfile').args[0].id).toBe('p1')
  })

  it('编辑模式拉模型列表带方案 ID（Key 留空由后端回落已保存值）', async () => {
    setup()
    await openEdit('p1')
    await wrapper.find('.model-row button').trigger('click')
    await flushPromises()

    const call = calls.find(c => c.name === 'ListLLMModels')
    expect(call.args[0]).toBe('p1')
    expect(call.args[2]).toBe('') // 表单 Key 必为空，回落到该方案已保存的 Key
    expect(wrapper.findAll('#model-options option')).toHaveLength(2)
  })

  it('卡片上的「设为当前」调用 ActivateLLMProfile 并切换到该方案', async () => {
    setup()
    await cardBtn('p2', '设为当前')
    expect(calls.find(c => c.name === 'ActivateLLMProfile').args[0]).toBe('p2')
  })

  it('卡片上的「删除」先弹确认框，确认后调用 DeleteLLMProfile', async () => {
    setup()
    await cardBtn('p2', '删除')
    expect(calls.find(c => c.name === 'DeleteLLMProfile')).toBeFalsy() // 先弹框，未删除
    expect(wrapper.find('.confirm-modal').text()).toContain('B')

    await confirmOk()
    expect(calls.find(c => c.name === 'DeleteLLMProfile').args[0]).toBe('p2')
  })

  it('卡片上的「测试连接」用空字段调 TestLLMProfile —— 后端回落到该方案已保存的值', async () => {
    setup()
    await cardBtn('p2', '测试连接')
    const call = calls.find(c => c.name === 'TestLLMProfile')
    expect(call).toBeTruthy()
    expect(call.args).toEqual(['p2', '', '', ''])

    // 结论展示在卡片上而不是弹窗里
    const card = wrapper.findAll('.profile').find(c => c.text().includes('B'))
    expect(card.find('.test').classes()).toContain('ok')
    expect(card.find('.test').text()).toContain('连接成功')
  })

  it('只有一个方案时不显示删除按钮（后端保证至少留一套）', async () => {
    store.llmProfiles = [{ id: 'p1', name: 'A', baseUrl: 'https://a/v1', model: 'm1', hasApiKey: true, active: true }]
    setup()
    const card = wrapper.findAll('.profile')[0]
    expect(card.findAll('button').some(b => b.text().includes('删除'))).toBe(false)
  })

  it('空列表显示引导文案', async () => {
    store.llmProfiles = []
    setup()
    expect(wrapper.find('.list').text()).toContain('暂无服务商')
  })

  it('编辑保存后清掉卡片上旧的测试结论（旧结论对不上新配置，留着会误导）', async () => {
    setup()
    await cardBtn('p2', '测试连接')
    let card = wrapper.findAll('.profile').find(c => c.text().includes('B'))
    expect(card.find('.test').exists()).toBe(true)

    await openEdit('p2')
    await modalBtn('保存').trigger('click')
    await flushPromises()
    card = wrapper.findAll('.profile').find(c => c.text().includes('B'))
    expect(card.find('.test').exists()).toBe(false)
  })

  it('两张卡片同时测试互不干扰（测 B 不会挤掉 A 的测试中状态）', async () => {
    let releaseB
    setup({
      TestLLMProfile: id => id === 'p2'
        ? new Promise(r => { releaseB = () => r({ ok: true, message: 'ok', url: '', status: 200, model: 'm2', reply: '', durationMs: 1 }) })
        : Promise.resolve({ ok: true, message: 'ok', url: '', status: 200, model: 'm1', reply: '', durationMs: 1 })
    })
    // 先点 B（挂起不返回），再点 A（立即完成）
    await cardBtn('p2', '测试连接')
    await cardBtn('p1', '测试连接')

    // B 仍处于「测试中」：单值标记的旧实现在这里会被 A 覆盖掉
    const cardB = wrapper.findAll('.profile').find(c => c.text().includes('B'))
    const btnB = cardB.findAll('button').find(b => b.text().includes('测试中'))
    expect(btnB).toBeTruthy()
    expect(btnB.attributes('disabled')).toBeDefined()

    releaseB()
    await flushPromises()
  })

  it('保存进行中不能关闭弹窗（半途关掉会误以为已保存）', async () => {
    let releaseSave
    setup({ SaveLLMProfile: () => new Promise(r => { releaseSave = () => r({ id: 'p1', name: 'A' }) }) })
    await openEdit('p1')
    await modalBtn('保存').trigger('click')
    await wrapper.vm.$nextTick()

    expect(modalBtn('取消').attributes('disabled')).toBeDefined()
    expect(wrapper.find('.modal-close').attributes('disabled')).toBeDefined()
    expect(wrapper.find('.overlay').exists()).toBe(true)

    releaseSave()
    await flushPromises()
    expect(wrapper.find('.overlay').exists()).toBe(false)
  })
})
