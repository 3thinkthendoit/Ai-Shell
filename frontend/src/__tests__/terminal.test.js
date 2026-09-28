// 交互终端的接线测试。
//
// xterm 本身在 jsdom 里跑不起来（没有布局、没有 canvas），所以
// vitest.config.js 把 @xterm/xterm 换成了替身（见 __tests__/stubs/xterm.js）。
// 这里测的是**我们自己的那层接线**，也正是最容易出错的一层：
//   - 实例什么时候建、建几个（建晚了会丢启动提示符，建多了会丢滚动回放）
//   - 按键怎么送出去、尺寸变化怎么上报
//   - 后端推来的字节有没有原样写进去（含跨块的多字节字符）
import { describe, it, expect, beforeEach, afterEach, beforeAll, afterAll } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { nextTick } from 'vue'
import ConsolePanel from '../components/ConsolePanel.vue'
import {
  store,
  bindEvents,
  bytesToBase64,
  base64ToBytes,
  textToBase64
} from '../store.js'
import { instances, resetInstances } from './stubs/xterm.js'

// toBytes 把字节数组归一成普通数组再比较。
//
// 不能直接 toEqual 两个 Uint8Array：jsdom 的 TextEncoder 产出的是
// **另一个 realm** 的 Uint8Array，原型不是全局的 Uint8Array.prototype，
// 深度比较因此在原型这一层就判不等 —— 而失败信息里两边看起来一模一样，
// 极难看出问题在哪。这里要断言的是字节内容，不是原型身份。
const toBytes = x => Array.from(x)

// ---- jsdom 缺的那两样东西 ----

// jsdom 没有布局引擎：clientWidth / clientHeight 恒为 0，而组件刻意在
// 尺寸为 0 时跳过测量（那是为了避开隐藏的容器，见 doFit）。给原型装上
// 固定尺寸，「fit → 上报尺寸」这条链路才走得通。
let origCW
let origCH

beforeAll(() => {
  origCW = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'clientWidth')
  origCH = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'clientHeight')
  Object.defineProperty(HTMLElement.prototype, 'clientWidth', { configurable: true, get: () => 800 })
  Object.defineProperty(HTMLElement.prototype, 'clientHeight', { configurable: true, get: () => 600 })
})

afterAll(() => {
  if (origCW) Object.defineProperty(HTMLElement.prototype, 'clientWidth', origCW)
  else delete HTMLElement.prototype.clientWidth
  if (origCH) Object.defineProperty(HTMLElement.prototype, 'clientHeight', origCH)
  else delete HTMLElement.prototype.clientHeight
})

// jsdom 也没有 ResizeObserver。留一个能手动触发的替身，
// 用来验证「容器尺寸变化 → 重新测量」这条路。
const roInstances = []
class FakeResizeObserver {
  constructor(cb) {
    this.cb = cb
    roInstances.push(this)
  }
  observe() {}
  disconnect() {}
}

// ---- Wails 运行时与后端替身 ----

function makeRuntime() {
  const handlers = new Map()
  return {
    handlers,
    EventsOn(name, cb) {
      if (!handlers.has(name)) handlers.set(name, [])
      handlers.get(name).push(cb)
    },
    emit(name, payload) {
      for (const cb of handlers.get(name) || []) cb(payload)
    }
  }
}

function makeApp(impl = {}) {
  const calls = []
  const base = {
    Ask: async () => undefined,
    Approve: async () => true,
    Stop: async () => undefined,
    RunShell: async () => ({ status: 'done', decision: 'allow', stdout: '', stderr: '', exitCode: 0, durationMs: 1 }),
    ListSessions: async () => [{ id: 'default', name: '', turns: 0, archivedTurns: 0, isDefault: true }],
    CreateSession: async () => ({ id: 's1', name: '新会话', turns: 0, archivedTurns: 0, isDefault: false }),
    RenameSession: async () => undefined,
    DeleteSession: async () => undefined,
    OpenTerminal: async () => undefined,
    WriteTerminal: async () => undefined,
    ResizeTerminal: async () => undefined,
    CloseTerminal: async () => true
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
  store.bySession = {}
  store.sessions = {}
  store.terms = {}
  store.currentSessionId = ''
  store.activeHostId = ''
  store.activeSessionId = ''
  store.pending = null
  store.running = false
}

let calls
let rt
let wrapper

function setup(impl) {
  const made = makeApp(impl)
  calls = made.calls
  window.go = { main: { App: made.app } }
  window.runtime = rt
  bindEvents()
  wrapper = mount(ConsolePanel, { attachTo: document.body })
  return wrapper
}

function withHost(id = 'h1', user = 'root', addr = '10.0.0.1') {
  store.hosts = [{ id, name: 'web', user, addr }]
  store.currentHostId = id
}

// toTerminal 切到「交互终端」模式（第二个分段按钮；第一个是 Agent会话）。
async function toTerminal() {
  await wrapper.findAll('.seg button')[1].trigger('click')
  await flushPromises()
  await nextTick()
}

function callsOf(name) {
  return calls.filter(c => c.name === name)
}

beforeEach(() => {
  resetStore()
  resetInstances()
  roInstances.length = 0
  globalThis.ResizeObserver = FakeResizeObserver
  rt = makeRuntime()
  delete window.go
})

afterEach(() => {
  if (wrapper) wrapper.unmount()
  wrapper = null
  delete window.go
  delete window.runtime
})

// ---- 建立会话的顺序与次数 ----

describe('交互终端 建立会话', () => {
  it('先建好本地实例，再开远端会话', async () => {
    withHost()
    // 记录 OpenTerminal 被调用的那一刻，本地实例已经建了几个。
    // 顺序很关键：远端 shell 一启动就打印提示符，落点必须在那之前就绪，
    // 否则用户看到的是一个空荡荡的终端，要敲一下回车才出现提示符。
    let instancesAtOpen = -1
    setup({
      OpenTerminal: async () => {
        instancesAtOpen = instances.length
      }
    })
    await toTerminal()

    expect(instancesAtOpen).toBe(1)
    expect(instances).toHaveLength(1)
  })

  it('开远端会话时带上 fit 之后的行列，而不是默认的 80x24', async () => {
    withHost()
    setup()
    await toTerminal()

    const open = callsOf('OpenTerminal')
    expect(open).toHaveLength(1)
    // 替身的 FitAddon 固定算出 100x30。带上真实尺寸的意义是：
    // 远端程序第一次排版就用对了宽度，而不是先按 80 列画一遍再重画。
    expect(open[0].args).toEqual(['h1', 100, 30])
  })

  it('切走再切回：复用同一个实例，也不重复开远端会话', async () => {
    withHost()
    setup()
    await toTerminal()
    const first = instances[0]

    await wrapper.findAll('.seg button')[0].trigger('click') // 回 Agent
    await flushPromises()
    await toTerminal() // 再切回交互终端

    expect(instances).toHaveLength(1)
    expect(instances[0]).toBe(first)
    // 远端会话是常驻的：切一次标签就重开一次的话，
    // 用户 cd 过去的目录、跑着的进程会被反复丢掉。
    expect(callsOf('OpenTerminal')).toHaveLength(1)
  })

  it('切换主机各开各的，切回来不重开', async () => {
    store.hosts = [
      { id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' },
      { id: 'h2', name: 'db', user: 'root', addr: '10.0.0.2' }
    ]
    store.currentHostId = 'h1'
    setup()
    await toTerminal()

    await wrapper.find('.host-select').setValue('h2')
    await flushPromises()
    expect(instances).toHaveLength(2)
    expect(callsOf('OpenTerminal').map(c => c.args[0])).toEqual(['h1', 'h2'])

    // 切回 h1：实例还在（滚动回放没丢），也不重开会话
    await wrapper.find('.host-select').setValue('h1')
    await flushPromises()
    expect(instances).toHaveLength(2)
    expect(callsOf('OpenTerminal')).toHaveLength(2)
  })

  it('没选主机时不去开终端，给出提示', async () => {
    setup()
    await toTerminal()

    expect(callsOf('OpenTerminal')).toHaveLength(0)
    expect(instances).toHaveLength(0)
    expect(wrapper.text()).toContain('请先选择一台主机')
  })

  it('没进交互终端模式就不会去开终端（不能白占一条 SSH 连接）', async () => {
    withHost()
    setup()
    await nextTick()
    await flushPromises()

    expect(callsOf('OpenTerminal')).toHaveLength(0)
    expect(instances).toHaveLength(0)
  })
})

// ---- 按键与尺寸 ----

describe('交互终端 按键与尺寸', () => {
  it('用户按键按原始字节 base64 送出去', async () => {
    withHost()
    setup()
    await toTerminal()

    instances[0].emitData('ls -l\r')
    await flushPromises()

    const w = callsOf('WriteTerminal')
    expect(w).toHaveLength(1)
    expect(w[0].args[0]).toBe('h1')
    expect(toBytes(base64ToBytes(w[0].args[1]))).toEqual(toBytes(new TextEncoder().encode('ls -l\r')))
  })

  it('中文按键按 UTF-8 字节送，不会被改写成问号', async () => {
    withHost()
    setup()
    await toTerminal()

    instances[0].emitData('中文')
    await flushPromises()

    const w = callsOf('WriteTerminal')
    expect(toBytes(base64ToBytes(w[0].args[1]))).toEqual(toBytes(new TextEncoder().encode('中文')))
  })

  it('尺寸变化上报给后端', async () => {
    withHost()
    setup()
    await toTerminal()
    const before = callsOf('ResizeTerminal').length

    instances[0].emitResize(132, 43)
    await flushPromises()

    const rs = callsOf('ResizeTerminal')
    expect(rs.length).toBeGreaterThan(before)
    expect(rs[rs.length - 1].args).toEqual(['h1', 132, 43])
  })

  it('容器尺寸变化会重新测量', async () => {
    withHost()
    setup()
    await toTerminal()
    expect(roInstances).toHaveLength(1)
    const before = callsOf('ResizeTerminal').length

    roInstances[0].cb()
    await flushPromises()

    expect(callsOf('ResizeTerminal').length).toBeGreaterThan(before)
  })
})

// ---- 后端推来的输出 ----

describe('交互终端 输出回显', () => {
  it('后端推来的字节原样写进终端', async () => {
    withHost()
    setup()
    await toTerminal()

    const bytes = new TextEncoder().encode('hello 中文\n')
    rt.emit('term:data', { hostId: 'h1', data: bytesToBase64(bytes) })
    await nextTick()

    expect(instances[0].text()).toBe('hello 中文\n')
  })

  it('被切成两块的汉字仍然完整 —— 这正是输出要按 base64 传的理由', async () => {
    withHost()
    setup()
    await toTerminal()

    // 把「中」的三个字节拆成两块分别推：远端一次 write 的边界
    // 和字符边界毫无关系，真实场景里经常发生。
    const all = new TextEncoder().encode('中')
    rt.emit('term:data', { hostId: 'h1', data: bytesToBase64(all.slice(0, 1)) })
    rt.emit('term:data', { hostId: 'h1', data: bytesToBase64(all.slice(1)) })
    await nextTick()

    expect(instances[0].text()).toBe('中')
  })

  it('输出只进对应主机的终端，不串台', async () => {
    store.hosts = [
      { id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' },
      { id: 'h2', name: 'db', user: 'root', addr: '10.0.0.2' }
    ]
    store.currentHostId = 'h1'
    setup()
    await toTerminal()
    await wrapper.find('.host-select').setValue('h2')
    await flushPromises()

    const h1 = instances[0]
    const h2 = instances[1]
    rt.emit('term:data', { hostId: 'h2', data: bytesToBase64(new TextEncoder().encode('来自 h2')) })
    await nextTick()

    expect(h2.text()).toBe('来自 h2')
    expect(h1.text()).not.toContain('来自 h2')
  })

  it('界面还没建好实例时到达的输出直接丢掉，不报错', async () => {
    withHost()
    setup()
    // 还没进交互终端模式，没有任何落点
    rt.emit('term:data', { hostId: 'h1', data: bytesToBase64(new TextEncoder().encode('x')) })
    await nextTick()

    expect(wrapper.text()).not.toContain('解码失败')
  })
})

// ---- 退出与关闭 ----

describe('交互终端 退出与关闭', () => {
  it('远端退出时显示原因并给出重新打开的入口', async () => {
    withHost()
    setup()
    await toTerminal()

    rt.emit('term:exit', { hostId: 'h1', reason: '远端 shell 已退出（退出码 0）' })
    await nextTick()

    expect(wrapper.text()).toContain('远端 shell 已退出')
    expect(wrapper.text()).toContain('重新打开')
  })

  it('点「重新打开」会开一条新的，并换掉旧实例', async () => {
    withHost()
    setup()
    await toTerminal()
    const old = instances[0]

    rt.emit('term:exit', { hostId: 'h1', reason: '远端已断开连接' })
    await nextTick()
    await wrapper.find('.iterm-status button').trigger('click')
    await flushPromises()

    expect(instances).toHaveLength(2)
    expect(instances[0]).toBe(old)
    expect(instances[0].disposed).toBe(true)
    expect(callsOf('OpenTerminal')).toHaveLength(2)
  })

  it('自己点关闭会打后端并销毁本地实例', async () => {
    withHost()
    setup()
    await toTerminal()

    await wrapper.find('.iterm-banner button').trigger('click')
    await flushPromises()

    expect(callsOf('CloseTerminal').map(c => c.args[0])).toEqual(['h1'])
    expect(instances[0].disposed).toBe(true)
  })

  it('自己关掉的终端不再弹一条「已断开」', async () => {
    withHost()
    setup()
    await toTerminal()

    await wrapper.find('.iterm-banner button').trigger('click')
    await flushPromises()

    // 后端关闭会话后必然会推一次 term:exit，但那是「用户自己关的」，
    // 再显示一条退出消息会让人以为关操作出了问题。
    rt.emit('term:exit', { hostId: 'h1', reason: '远端 shell 已退出' })
    await nextTick()

    expect(wrapper.text()).not.toContain('远端 shell 已退出')
  })

  it('打开失败时显示原因并给出重试', async () => {
    withHost()
    setup({ OpenTerminal: async () => { throw new Error('申请伪终端失败: 远端拒绝') } })
    await toTerminal()

    expect(wrapper.text()).toContain('申请伪终端失败')
    expect(wrapper.text()).toContain('重新打开')
  })
})

// ---- 界面必须说清楚风险 ----

describe('交互终端 风险提示', () => {
  it('显眼处写明不经过策略引擎', async () => {
    withHost()
    setup()
    await toTerminal()

    const banner = wrapper.find('.iterm-banner')
    expect(banner.exists()).toBe(true)
    expect(banner.text()).toContain('不经过 LLM 与策略引擎')
    // 连到哪台机器也要写在旁边：终端一旦开起来就再没有别的提示符可看
    expect(banner.text()).toContain('root@10.0.0.1')
  })
})

// ---- 字节编解码（纯函数）----

describe('终端字节编解码', () => {
  it('中文往返后逐字节一致', () => {
    const bytes = new TextEncoder().encode('中文输出')
    expect(toBytes(base64ToBytes(bytesToBase64(bytes)))).toEqual(toBytes(bytes))
  })

  it('控制字节与二进制不被改写', () => {
    const bytes = new Uint8Array([0x00, 0x1b, 0x5b, 0x32, 0x4a, 0xff, 0x0d, 0x0a])
    expect(toBytes(base64ToBytes(bytesToBase64(bytes)))).toEqual(toBytes(bytes))
  })

  it('空输入不炸', () => {
    expect(bytesToBase64(new Uint8Array(0))).toBe('')
    expect(toBytes(base64ToBytes(''))).toEqual([])
  })

  it('大块数据分块拼接后仍然正确（不能爆栈）', () => {
    const big = new Uint8Array(200000)
    for (let i = 0; i < big.length; i++) big[i] = i % 256
    expect(base64ToBytes(bytesToBase64(big))).toEqual(big)
  })

  it('textToBase64 与 TextEncoder 的结果一致', () => {
    expect(toBytes(base64ToBytes(textToBase64('echo 中文\r')))).toEqual(
      toBytes(new TextEncoder().encode('echo 中文\r'))
    )
  })
})
