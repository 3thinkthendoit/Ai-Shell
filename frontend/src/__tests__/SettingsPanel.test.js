// SettingsPanel 组件测试。
//
// 这里锁的是「测试连接」这个按钮的真实价值 —— 它必须真的打后端、
// 并把结果（尤其是失败原因和实际请求地址）呈现出来。
//
// 之前那个版本只查「有没有填 API Key」然后让人自己去控制台试，
// 属于「看起来验过了、其实什么都没验」。所以第一条用例就断言它真的调了 TestLLM。
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

const clickTest = async () => {
  const btn = wrapper.findAll('.actions button').at(-2) // [测试连接, 保存]
  await btn.trigger('click')
  await flushPromises()
  return btn
}

beforeEach(() => {
  store.llm = { baseUrl: 'https://api.openai.com/v1', model: 'gpt-4o-mini', hasApiKey: true }
})

afterEach(() => {
  if (wrapper) wrapper.unmount()
  wrapper = null
  delete window.go
})

describe('SettingsPanel 测试连接', () => {
  it('真的调用后端 TestLLM，而不是只做本地形状检查', async () => {
    setup()
    await clickTest()
    expect(calls.filter(c => c.name === 'TestLLM')).toHaveLength(1)
  })

  it('传的是表单里的值，所以「先测再存」可行', async () => {
    setup()
    await wrapper.find('.base-url').setValue('https://my.endpoint/v1')
    await wrapper.find('.model-name').setValue('my-model')
    await wrapper.find('.api-key').setValue('sk-typed')
    await clickTest()

    const call = calls.find(c => c.name === 'TestLLM')
    expect(call.args).toEqual(['https://my.endpoint/v1', 'my-model', 'sk-typed'])
  })

  it('API Key 留空时传空串（后端会改用钥匙串里已保存的那把）', async () => {
    setup()
    await clickTest()
    expect(calls.find(c => c.name === 'TestLLM').args[2]).toBe('')
  })

  it('成功时显示模型、耗时与模型回复 —— 证明对方真的回了话', async () => {
    setup()
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
    await wrapper.find('.base-url').setValue('https://token.sensenova.cn/v1/chat/completions')
    await clickTest()

    // 表单被改回「到 /v1 为止」，否则会出现「测试通过但保存的是另一个值」
    expect(wrapper.find('.base-url').element.value).toBe('https://token.sensenova.cn/v1')
    expect(wrapper.find('.fix-hint').text()).toContain('/chat/completions')
  })

  it('地址本来就正确时不出现修正提示', async () => {
    setup()
    await wrapper.find('.base-url').setValue('https://api.openai.com/v1')
    await clickTest()
    expect(wrapper.find('.fix-hint').exists()).toBe(false)
  })

  it('测试中禁用按钮并改文案，防止重复点击', async () => {
    let release
    setup({ TestLLM: () => new Promise(r => { release = () => r({ ok: true, message: '连接成功', url: '', model: '', reply: '', durationMs: 1 }) }) })
    const btn = wrapper.findAll('.actions button').at(-2)
    await btn.trigger('click')
    await wrapper.vm.$nextTick()

    const during = wrapper.findAll('.actions button').at(-2)
    expect(during.text()).toContain('测试中')
    expect(during.attributes('disabled')).toBeDefined()

    release()
    await flushPromises()
    expect(wrapper.findAll('.actions button').at(-2).text()).toContain('测试连接')
  })

  it('后端抛异常时渲染成失败结果，而不是整体崩掉', async () => {
    setup({ TestLLM: async () => { throw new Error('凭证库尚未初始化') } })
    await clickTest()
    expect(wrapper.find('.test-result').classes()).toContain('bad')
    expect(wrapper.find('.test-result').text()).toContain('凭证库尚未初始化')
  })

  it('切换预设会清掉上一次的测试结果（避免拿旧结论当新配置的结论）', async () => {
    setup()
    await clickTest()
    expect(wrapper.find('.test-result').exists()).toBe(true)

    await wrapper.findAll('.presets button')[0].trigger('click')
    expect(wrapper.find('.test-result').exists()).toBe(false)
    expect(wrapper.find('.fix-hint').exists()).toBe(false)
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
    // 默认编辑激活方案 p1
    await clickTest()
    const call = calls.find(c => c.name === 'TestLLMProfile')
    expect(call).toBeTruthy()
    expect(call.args[0]).toBe('p1')
  })

  // profile-bar 里第一个 button 已是 UiSelect 的触发器（自绘下拉），
  // 所以这里的按钮一律按文案找，不按下标。
  const barBtn = label => {
    const b = wrapper.findAll('.profile-bar button').find(x => x.text().trim() === label)
    if (!b) throw new Error(`未找到按钮：${label}`)
    return b
  }

  it('保存时带方案 ID，新增时不带（由后端生成 ID）', async () => {
    setup()
    await wrapper.findAll('.actions button').at(-1).trigger('click') // 保存
    await flushPromises()
    expect(calls.find(c => c.name === 'SaveLLMProfile').args[0].id).toBe('p1')

    // 新建模式：editingId 清空
    await barBtn('新建').trigger('click')
    await wrapper.findAll('.actions button').at(-1).trigger('click')
    await flushPromises()
    expect(calls.filter(c => c.name === 'SaveLLMProfile')[1].args[0].id).toBe('')
  })

  // pickProfile 通过 UiSelect（自绘下拉）切换正在编辑的方案。
  // 原生 select 已被替换：触发器是按钮，选项是列表项，不能用 setValue。
  async function pickProfile(id) {
    const p = store.llmProfiles.find(x => x.id === id)
    await wrapper.find('.profile-select .uis-trigger').trigger('click')
    await flushPromises()
    const item = wrapper
      .findAll('.profile-select .uis-item')
      .filter(w => w.text() === `${p.name}${p.active ? '（使用中）' : ''}`)[0]
    await item.trigger('click')
    await flushPromises()
  }

  it('「设为当前」调用 ActivateLLMProfile 并切换到选中的方案', async () => {
    setup()
    await pickProfile('p2')
    const btn = wrapper.findAll('.profile-bar button').find(b => b.text().includes('设为当前'))
    await btn.trigger('click')
    await flushPromises()
    expect(calls.find(c => c.name === 'ActivateLLMProfile').args[0]).toBe('p2')
  })

  it('删除方案调用 DeleteLLMProfile', async () => {
    setup()
    await pickProfile('p2')
    const btn = wrapper.findAll('.profile-bar button').find(b => b.text().includes('删除'))
    await btn.trigger('click')
    await flushPromises()
    expect(calls.find(c => c.name === 'DeleteLLMProfile').args[0]).toBe('p2')
  })

  it('新建模式下表单全空时不发测试请求 —— 否则会拿到激活方案配置的「连接成功」', async () => {
    setup()
    await barBtn('新建').trigger('click')
    await clickTest()

    expect(calls.filter(c => c.name === 'TestLLM' || c.name === 'TestLLMProfile')).toHaveLength(0)
    expect(wrapper.find('.test-result').classes()).toContain('bad')
    expect(wrapper.find('.test-result').text()).toContain('请先填写')
  })
})
