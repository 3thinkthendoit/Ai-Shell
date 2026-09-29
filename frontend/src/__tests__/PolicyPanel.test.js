// PolicyPanel 组件测试。
//
// 重点锁「会话上下文」这块：界面上以 KB 输入，存进去必须是**字节**。
// 这是本次改动里最容易出错的一处 —— 换算错了不会报错，
// 只会表现为「模型记性不对劲」，用户根本无从察觉，也说明不了是哪里坏了。
//
// 第二个重点是「读到什么就显示什么」：Bootstrap 回来的字节值必须正确回显成 KB，
// 否则用户看到 256 却不知道那是 256KB 还是别的，改一下就把上限缩了 1024 倍。
//
// 选择器一律用类名（.session-turns 等），不用位置下标：
// 下标会随表单顺序调整而指向别的输入框 —— 那时用例可能仍然通过，
// 只是断言的已经是另一个字段了。
import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import PolicyPanel from '../components/PolicyPanel.vue'
import { store } from '../store.js'

function makeApp(impl = {}) {
  const calls = []
  const base = {
    SavePolicy: async () => undefined,
    DefaultWhitelist: async () => ['ls', 'cat']
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
  wrapper = mount(PolicyPanel, { attachTo: document.body })
  return wrapper
}

const sel = {
  turns: '.session-turns',
  toolBytes: '.session-tool-bytes',
  totalBytes: '.session-total-bytes',
  ovTurns: '.ov-turns',
  ovToolKb: '.ov-tool-bytes',
  ovTotalKb: '.ov-total-bytes'
}

async function save() {
  await wrapper.find('.actions button.primary').trigger('click')
  await flushPromises()
  return calls.find(c => c.name === 'SavePolicy')
}

// 默认策略：与后端出厂默认一致。
function defaultPolicy() {
  return {
    mode: 'manual',
    whitelist: ['ls'],
    redactOutput: true,
    maxOutput: 32768,
    maxSteps: 12,
    maxSessionTurns: 8,
    maxStoredToolBytes: 8192,
    maxSessionBytes: 262144
  }
}

beforeEach(() => {
  store.policy = defaultPolicy()
  // 主机列表是主机级覆盖那一块的数据源。默认清空，
  // 需要它的用例自己在 setup() 之前填 —— 否则用例之间会互相影响。
  store.hosts = []
})

afterEach(() => {
  if (wrapper) wrapper.unmount()
  wrapper = null
  delete window.go
})

describe('PolicyPanel 会话上下文', () => {
  it('把后端的字节值回显成 KB（262144 字节 → 256）', () => {
    setup()
    // 「每台主机历史总量上限 (KB)」应是 256，而不是 262144 ——
    // 直接显示字节数没人看得懂，而且一改就会缩掉 1024 倍。
    expect(wrapper.find(sel.totalBytes).element.value).toBe('256')
    // 单条工具输出 8192 字节 → 8
    expect(wrapper.find(sel.toolBytes).element.value).toBe('8')
    // 轮数不换算，原样显示
    expect(wrapper.find(sel.turns).element.value).toBe('8')
  })

  it('保存时把 KB 换算成字节，且不把界面单位当字节存', async () => {
    setup()
    await wrapper.find(sel.totalBytes).setValue(512)
    const saved = await save()

    expect(saved).toBeTruthy()
    // 512 KB 必须存成 524288 字节。若这里存成 512，
    // 后端会按 512 字节裁历史，几乎等于没有上下文。
    expect(saved.args[0].maxSessionBytes).toBe(512 * 1024)
  })

  it('单条工具输出上限也按 KB 换算', async () => {
    setup()
    await wrapper.find(sel.toolBytes).setValue(64)
    const saved = await save()
    expect(saved.args[0].maxStoredToolBytes).toBe(64 * 1024)
  })

  it('轮数原样提交，不做单位换算', async () => {
    setup()
    await wrapper.find(sel.turns).setValue(20)
    const saved = await save()
    // 轮数是「条数」不是字节 —— 被误乘 1024 的话用户根本看不出来，
    // 只会觉得「我明明设了 20 轮，怎么留了这么多」。
    expect(saved.args[0].maxSessionTurns).toBe(20)
  })

  it('填了 0 或空值时退回默认值，不把上下文意外关掉', async () => {
    setup()
    await wrapper.find(sel.turns).setValue('')      // 轮数清空
    await wrapper.find(sel.totalBytes).setValue('') // 总量清空
    const saved = await save()

    // 0 会被后端当「未设置」回填成默认值，但前端也不该主动送 0 过去 ——
    // 送默认值让行为在两端都明确。
    expect(saved.args[0].maxSessionTurns).toBe(8)
    expect(saved.args[0].maxSessionBytes).toBe(256 * 1024)
  })

  it('缺失会话字段时（老版本 Bootstrap）回退到默认值而不是 NaN', async () => {
    // 模拟后端还没升级 / Bootstrap 里没有这三个字段。
    store.policy = { mode: 'manual', whitelist: ['ls'], redactOutput: true, maxOutput: 32768, maxSteps: 12 }
    setup()
    // 不能出现 NaN 或空 —— 那样用户一保存就把上限写坏了。
    expect(wrapper.find(sel.turns).element.value).toBe('8')
    expect(wrapper.find(sel.toolBytes).element.value).toBe('8')
    expect(wrapper.find(sel.totalBytes).element.value).toBe('256')

    const saved = await save()
    expect(saved.args[0].maxSessionTurns).toBe(8)
    expect(saved.args[0].maxStoredToolBytes).toBe(8192)
    expect(saved.args[0].maxSessionBytes).toBe(262144)
  })

  it('保存后把新值写回 store，供其他面板读取', async () => {
    setup()
    await wrapper.find(sel.totalBytes).setValue(512)
    await save()
    expect(store.policy.maxSessionBytes).toBe(512 * 1024)
  })
})

// 策略面板里对「对话记忆存在哪」的说明同样必须与事实一致。
describe('PolicyPanel 上下文说明与落盘事实一致', () => {
  it('不再声称「只存在内存里、不会落盘」', () => {
    setup({})
    const text = wrapper.text()
    expect(text).toContain('按主机独立保存')
    expect(text).not.toContain('只存在内存')
    expect(text).not.toContain('不会落盘')
  })
})

// 主机级覆盖：让某台主机用与全局不同的上下文上限。
//
// 这一块最容易出错的地方是「空」与「0」的混淆：留空表示**继承全局**，
// 若在提交时把空串当 0 送出去，语义就从「跟随全局」变成了「我要 0」——
// 后端目前会把 0 当未设置，所以暂时看不出问题，但这条链路已经在
// 表达一个错误的意思，后端一旦改成严格校验就会立刻把上下文关掉。
describe('PolicyPanel 主机级覆盖', () => {
  const hosts = [{ id: 'h1', name: 'web-1' }, { id: 'h2', name: 'db-1' }]

  function policyWithOverrides(ov) {
    return { ...defaultPolicy(), sessionOverrides: ov }
  }

  it('把已保存的覆盖按 KB 回显，没设的项留空', () => {
    store.hosts = hosts
    store.policy = policyWithOverrides({
      h1: { maxSessionTurns: 30, maxStoredToolBytes: 65536, maxSessionBytes: 2097152 }
    })
    setup()

    const turns = wrapper.findAll(sel.ovTurns)
    expect(turns[0].element.value).toBe('30')
    expect(wrapper.findAll(sel.ovToolKb)[0].element.value).toBe('64')
    expect(wrapper.findAll(sel.ovTotalKb)[0].element.value).toBe('2048')
    // 没有覆盖的主机必须全空。若这里被填成全局值，用户下次一保存
    // 就把它变成一条显式覆盖，从此再也不跟全局走 —— 而界面上看不出区别。
    expect(turns[1].element.value).toBe('')
    expect(wrapper.findAll(sel.ovToolKb)[1].element.value).toBe('')
    expect(wrapper.findAll(sel.ovTotalKb)[1].element.value).toBe('')
  })

  it('留空的项不提交，只提交填了的那一项', async () => {
    store.hosts = hosts
    setup()
    await wrapper.findAll(sel.ovTurns)[0].setValue(30)
    const saved = await save()

    const ov = saved.args[0].sessionOverrides
    expect(ov.h1).toEqual({ maxSessionTurns: 30 })
    expect('maxStoredToolBytes' in ov.h1).toBe(false)
    expect('maxSessionBytes' in ov.h1).toBe(false)
    // 没碰过的主机不该被写进去。
    expect(ov.h2).toBeUndefined()
  })

  it('覆盖里的 KB 同样换算成字节', async () => {
    store.hosts = hosts
    setup()
    await wrapper.findAll(sel.ovTotalKb)[1].setValue(2048)
    const saved = await save()
    // 2048 KB 必须存成 2097152 字节。存成 2048 的话，
    // 后端会按 2048 字节裁这台主机的历史，几乎等于没有上下文。
    expect(saved.args[0].sessionOverrides.h2.maxSessionBytes).toBe(2048 * 1024)
  })

  it('三项都留空的主机整条不提交', async () => {
    store.hosts = hosts
    setup()
    await wrapper.findAll(sel.ovTurns)[0].setValue('')
    const saved = await save()
    expect(saved.args[0].sessionOverrides).toEqual({})
  })

  it('主机列表尚未加载时保存，不丢已有的覆盖', async () => {
    // 模拟 Bootstrap 还没回来：store.hosts 为空，但库里已经有覆盖。
    store.hosts = []
    store.policy = policyWithOverrides({ h1: { maxSessionTurns: 30 } })
    setup()
    const saved = await save()
    // 若按「界面上渲染出来的主机」构造提交内容，这里会是 {} ——
    // 用户设好的覆盖被静默抹掉，且没有任何提示。
    expect(saved.args[0].sessionOverrides.h1).toEqual({ maxSessionTurns: 30 })
  })

  it('清除按钮把该主机的三项清空，保存后条目消失', async () => {
    store.hosts = hosts
    store.policy = policyWithOverrides({ h1: { maxSessionTurns: 30, maxSessionBytes: 1 << 20 } })
    setup()
    expect(wrapper.findAll(sel.ovTurns)[0].element.value).toBe('30')

    await wrapper.findAll('.ov-clear')[0].trigger('click')
    expect(wrapper.findAll(sel.ovTurns)[0].element.value).toBe('')
    expect(wrapper.findAll(sel.ovTotalKb)[0].element.value).toBe('')

    const saved = await save()
    expect(saved.args[0].sessionOverrides.h1).toBeUndefined()
  })

  it('没有覆盖的主机，清除按钮是禁用的', () => {
    store.hosts = hosts
    setup()
    expect(wrapper.findAll('.ov-clear')[0].attributes('disabled')).toBeDefined()
  })

  it('占位符显示当前会继承到的全局值', () => {
    store.hosts = hosts
    setup()
    // 占位符就是「不填会怎样」的答案，必须跟着全局输入框一起变。
    expect(wrapper.findAll(sel.ovTurns)[0].attributes('placeholder')).toBe('8')
    expect(wrapper.findAll(sel.ovToolKb)[0].attributes('placeholder')).toBe('8')
    expect(wrapper.findAll(sel.ovTotalKb)[0].attributes('placeholder')).toBe('256')
  })

  it('没有主机时给出说明，而不是留一块空白', () => {
    store.hosts = []
    setup()
    expect(wrapper.text()).toContain('还没有主机')
  })

  // 覆盖表里可能存在「主机已不存在」的条目（老版本删主机时没清理）。
  // 这些条目会被原样保存下去，所以**必须渲染出来** ——
  // 只给一句提示而不给行，用户就永远没法把它们删掉，只能一直看着。
  it('主机已不存在的覆盖条目也要渲染出来，并且能清掉', async () => {
    store.hosts = hosts
    store.policy = policyWithOverrides({
      h1: { maxSessionTurns: 30 },
      'h-gone': { maxSessionTurns: 99 }
    })
    setup()

    // 两行：h1、h2 各一行，外加 h-gone 一行。
    expect(wrapper.findAll(sel.ovTurns)).toHaveLength(3)
    expect(wrapper.text()).toContain('主机已不存在')
    // 孤儿行的值要如实回显，不能因为主机不在列表里就显示成空 ——
    // 那样用户会以为它本来就没设过。
    expect(wrapper.findAll(sel.ovTurns)[2].element.value).toBe('99')

    await wrapper.findAll('.ov-clear')[2].trigger('click')
    const saved = await save()
    expect(saved.args[0].sessionOverrides['h-gone']).toBeUndefined()
    expect(saved.args[0].sessionOverrides.h1).toEqual({ maxSessionTurns: 30 })
  })
})

describe('PolicyPanel 跨主机执行', () => {
  it('默认关闭；勾选后随策略一起保存', async () => {
    setup()
    const box = wrapper.find('.cross-host')
    expect(box.element.checked).toBe(false)

    await box.setValue(true)
    const saved = await save()
    expect(saved.args[0].allowCrossHost).toBe(true)
  })

  it('后端已开启时回显为选中', async () => {
    store.policy = { ...defaultPolicy(), allowCrossHost: true }
    setup()
    expect(wrapper.find('.cross-host').element.checked).toBe(true)
  })
})
