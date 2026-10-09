// 常驻终端表面的接线测试。
//
// xterm 本身在 jsdom 里跑不起来（没有布局、没有 canvas），所以
// vitest.config.js 把 @xterm/xterm 换成了替身（见 __tests__/stubs/xterm.js）。
// 这里测的是**我们自己的那层接线**，也正是最容易出错的一层：
//   - 实例什么时候建、建几个（建晚了会丢启动提示符，建多了会丢滚动回放）
//   - 按键怎么送出去、尺寸变化怎么上报
//   - 后端推来的字节有没有原样写进去（含跨块的多字节字符）
//
// 重构后控制台就是终端本身：选中主机、组件挂载即附着常驻 PTY，
// 不再需要「切到交互终端模式」这一步。
import { describe, it, expect, beforeEach, afterEach, beforeAll, afterAll, vi } from 'vitest'
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
    // composer 里的 shell 行走常驻终端通道；ReportActiveSession 报当前会话。
    RunShellInTerminal: async () => ({ status: 'done', decision: 'allow', reason: '', rule: 'auto_safe', risk: 'low' }),
    ReportActiveSession: async () => undefined,
    // 命令识别：真实实现复用 policy 的命令库（见 app_shell.go ClassifyShellInput）。
    // 替身认几个常见命令即可 —— 这里验的是分流逻辑，命令库本身由 Go 侧测试钉住。
    // 不给这个实现的话，所有输入都会落进「首词不认识」分支，shell 命令根本发不出去。
    ClassifyShellInput: async text => {
      const first = String(text || '').trim().split(/\s+/)[0] || ''
      const base = first.replace(/^.*\//, '')
      return ['ls', 'echo', 'grep', 'cat', 'df', 'docker', 'systemctl', 'nginx', 'cd', 'pwd', 'a', 'b', 'uptime', 'tail', 'sed'].includes(base)
    },
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
  // 与真实流程对齐：refreshSessions 会把选中会话落到默认会话的真实 ID（mock 中为 'default'）。
  // 预先置好，挂载时的 OpenTerminal/WriteTerminal 调用才能拿到确定的 sessionId。
  store.currentSessionId = 'default'
}

// 挂载即附着：onMounted 里 attachSurface 会异步把实例建起来、把 PTY 开出来。
// flushPromises 让 nextTick + OpenTerminal 这条链跑完。
async function attached() {
  await flushPromises()
  await nextTick()
}

// 做「原样回显」这类严格断言前先把替身已写入的内容清空，才能逐字节比对。
// （表面按任务独立后不再预写会话分隔线，这里只是保险地清一次。）
function clearSurface(i = 0) {
  if (instances[i]) instances[i].written.length = 0
}

function callsOf(name) {
  return calls.filter(c => c.name === name)
}

// keyEvent 造一个按键事件对象。组件里会调 preventDefault（拦下浏览器默认行为），
// 所以替身必须提供这个方法，否则组件会抛 TypeError。
function keyEvent(key, mods = {}) {
  return { type: 'keydown', key, preventDefault() {}, ...mods }
}

// withClipboard 给 navigator.clipboard 装上 readText 替身。
// jsdom 里没有这个 API（也就没有权限模型），组件又必须能读到文本才能粘贴，
// 所以这里自己造一个。第二个参数传入 Error 即模拟「权限被拒」。
let origClipboard
function withClipboard(text, err) {
  origClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard')
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: { readText: () => (err ? Promise.reject(err) : Promise.resolve(text)) }
  })
}

afterEach(() => {
  if (origClipboard) Object.defineProperty(navigator, 'clipboard', origClipboard)
  else delete navigator.clipboard
  origClipboard = undefined
})

// withWriteClipboard 给 navigator.clipboard 装上 writeText 替身，并把写进去的
// 文本记在返回对象上。复制/粘贴用的是两个不同的 API，得分开造 —— 只装 readText
// 时 writeText 不存在，组件会一路退到 execCommand 兜底（jsdom 里也多半失败）。
function withWriteClipboard() {
  const box = { text: '' }
  origClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard')
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: {
      readText: () => Promise.resolve(''),
      writeText: t => { box.text = t; return Promise.resolve() }
    }
  })
  return box
}

// selectHost 通过 UiSelect（自绘下拉）切换目标主机。
// 原生 select 已被替换：触发器是按钮，选项是列表项，不能用 setValue。
async function selectHost(id) {
  const h = store.hosts.find(x => x.id === id)
  await wrapper.find('.host-select .uis-trigger').trigger('click')
  await flushPromises()
  const item = wrapper
    .findAll('.host-select .uis-item')
    .filter(w => w.text() === `${h.name} — ${h.user}@${h.addr}`)[0]
  await item.trigger('click')
  await flushPromises()
  await nextTick()
}

// toArchive / toSurface 切换主区：只读时间线 vs 活表面。
// 表面用 v-show 常驻，切换不会销毁实例（滚动回放不丢）。
async function toArchive() {
  await wrapper.findAll('.seg button')[1].trigger('click')
  await flushPromises()
  await nextTick()
}
async function toSurface() {
  await wrapper.findAll('.seg button')[0].trigger('click')
  await flushPromises()
  await nextTick()
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

describe('常驻终端表面 建立会话', () => {
  it('挂载即附着：选中主机时无需任何点击就建实例、开远端会话', async () => {
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
    await attached()

    expect(instancesAtOpen).toBe(1)
    expect(instances).toHaveLength(1)
    expect(callsOf('OpenTerminal')).toHaveLength(1)
  })

  it('开远端会话时带上 fit 之后的行列，而不是默认的 80x24', async () => {
    withHost()
    setup()
    await attached()

    const open = callsOf('OpenTerminal')
    expect(open).toHaveLength(1)
    // 新签名：OpenTerminal(hostId, sessionId, cols, rows)。测试里 currentSessionId 为空，
    // bucketKey 会回退到 'default'。替身的 FitAddon 固定算出 100x30。带上真实尺寸的
    // 意义是：远端程序第一次排版就用对宽度，而不是先按 80 列画一遍再重画。
    expect(open[0].args).toEqual(['h1', 'default', 100, 30])
  })

  it('切到对话归档再切回：复用同一实例，也不重复开远端会话', async () => {
    withHost()
    setup()
    await attached()
    const first = instances[0]

    await toArchive()
    await toSurface()

    expect(instances).toHaveLength(1)
    expect(instances[0]).toBe(first)
    // 远端会话是常驻的：切一次视图就重开一次的话，
    // 用户 cd 过去的目录、跑着的进程会被反复丢掉。
    expect(callsOf('OpenTerminal')).toHaveLength(1)
  })

  it('切换主机关掉上一个 shell，为新主机重开一条全新 shell（只留当前任务）', async () => {
    store.hosts = [
      { id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' },
      { id: 'h2', name: 'db', user: 'root', addr: '10.0.0.2' }
    ]
    store.currentHostId = 'h1'
    setup()
    await attached()

    await selectHost('h2')
    // 切走 h1：它的实例被销毁（旧现场丢弃），h2 新建一条全新实例。
    expect(instances).toHaveLength(2)
    expect(instances[0].disposed).toBe(true)
    expect(callsOf('OpenTerminal').map(c => c.args[0])).toEqual(['h1', 'h2'])
    expect(callsOf('CloseTerminal').map(c => c.args[0])).toContain('h1')

    // 切回 h1：关掉 h2，为 h1 重开一条全新 shell（不复用旧实例）。
    await selectHost('h1')
    expect(instances).toHaveLength(3)
    expect(instances[1].disposed).toBe(true)
    expect(instances[2]).not.toBe(instances[0])
    expect(callsOf('OpenTerminal').map(c => c.args[0])).toEqual(['h1', 'h2', 'h1'])
  })

  it('没选主机时不去开终端，给出提示', async () => {
    setup()
    await attached()

    expect(callsOf('OpenTerminal')).toHaveLength(0)
    expect(instances).toHaveLength(0)
    expect(wrapper.text()).toContain('请先选择一台主机')
  })
})

// ---- 按键与尺寸 ----

describe('常驻终端表面 按键与尺寸', () => {
  it('控制/转义按键按原始字节 base64 透传（方向键不拆散）', async () => {
    withHost()
    setup()
    await attached()

    instances[0].emitData('\x1b[A') // 方向键上：转义序列整体透传
    await flushPromises()

    const w = callsOf('WriteTerminal')
    expect(w).toHaveLength(1)
    expect(w[0].args[0]).toBe('h1')
    // 新签名：WriteTerminal(hostId, sessionId, dataB64)。
    expect(w[0].args[1]).toBe('default')
    expect(toBytes(base64ToBytes(w[0].args[2]))).toEqual(toBytes(new TextEncoder().encode('\x1b[A')))
  })

  it('中文本地回显、回车前不写 PTY（不会被当命令发出去）', async () => {
    withHost()
    setup()
    await attached()
    const inst = instances[0]
    clearSurface()

    inst.emitData('中文')
    await flushPromises()

    expect(inst.text()).toContain('中文')
    expect(callsOf('WriteTerminal')).toHaveLength(0)
  })

  // 开表面时的首测（含 rAF 补测）有自己的防抖节奏：先等它尘埃落定，
  // 再触发测试自己的 resize，否则 stub 的固定 100x30 会覆盖刚排上的新尺寸。
  async function settleResize() {
    await new Promise(r => setTimeout(r, 300))
  }

  it('尺寸变化上报给后端（防抖 150ms 后取最新尺寸）', async () => {
    withHost()
    setup()
    await attached()
    await settleResize()
    const before = callsOf('ResizeTerminal').length

    instances[0].emitResize(132, 43)
    await new Promise(r => setTimeout(r, 250))

    const rs = callsOf('ResizeTerminal')
    expect(rs.length).toBeGreaterThan(before)
    expect(rs[rs.length - 1].args).toEqual(['h1', 'default', 132, 43])
  })

  it('连续 resize 合并为一次上报，取最新尺寸', async () => {
    withHost()
    setup()
    await attached()
    await settleResize()
    const before = callsOf('ResizeTerminal').length

    instances[0].emitResize(120, 40)
    instances[0].emitResize(132, 43)
    await new Promise(r => setTimeout(r, 250))

    const rs = callsOf('ResizeTerminal').slice(before)
    expect(rs).toHaveLength(1)
    expect(rs[0].args).toEqual(['h1', 'default', 132, 43])
  })

  it('同尺寸重测不再上报（避免重复 window-change 引发 SIGWINCH 重绘提示符）', async () => {
    withHost()
    setup()
    await attached()
    await settleResize()
    instances[0].emitResize(132, 43)
    await new Promise(r => setTimeout(r, 250))
    const after = callsOf('ResizeTerminal').length
    expect(after).toBeGreaterThan(0)

    // 归档切回会触发重测：尺寸没变就不该再发 window-change
    instances[0].emitResize(132, 43)
    await new Promise(r => setTimeout(r, 250))
    expect(callsOf('ResizeTerminal').length).toBe(after)
  })

  it('容器尺寸变化会重新测量', async () => {
    withHost()
    setup()
    await attached()
    await settleResize()
    expect(roInstances).toHaveLength(1)
    const before = callsOf('ResizeTerminal').length

    // 容器真的变了：先改 fit 算出的尺寸再触发 RO，onResize 才会带着新尺寸来。
    // 真 FitAddon 尺寸没变时根本不触发 onResize；替身每次 fit 都无条件发，
    // 所以必须显式改尺寸来模拟「尺寸确实变了」。
    const inst = instances[0]
    inst.addon.cols = 132
    inst.addon.rows = 43
    roInstances[0].cb()
    await new Promise(r => setTimeout(r, 250))

    const rs = callsOf('ResizeTerminal').slice(before)
    expect(rs.length).toBeGreaterThan(0)
    expect(rs[rs.length - 1].args).toEqual(['h1', 'default', 132, 43])
  })

  it('打开后与创建尺寸相同的待发 resize 被作废（否则 SIGWINCH 重绘提示符，叠出多个提示符）', async () => {
    withHost()
    setup()
    await attached()
    // 旧实现：布局稳定期（RAF 补测、状态行增减）的 onResize 与 PTY 创建尺寸相同，
    // 仍会在 150ms 后补发一次 window-change —— SIGWINCH 让 bash 在光标处原地重绘
    // 空提示符，与启动的那一遍叠成「[root@host ~]# [root@host ~]# [root@host ~]#」。
    // 新约定：PTY 创建尺寸在打开完成时即登记为「已发送」，待发定时器作废；
    // 之后真实变了尺寸（下面那条测试）才照常上报。
    await settleResize()
    expect(callsOf('ResizeTerminal')).toHaveLength(0)
  })

  it('慢连接下防抖在打开完成前到期：opening 期间到点的 resize 不发', async () => {
    // OpenTerminal 故意耗时 250ms > 150ms 防抖：定时器会在打开完成前到期。
    // 只靠「打开完成后作废待发」拦不住这一路径 —— 回调到期时必须再查一次状态，
    // opening 期间直接丢弃（打开完成后 statusLine watcher 的 doFit 会以最终
    // 尺寸重走去重，真实变了才补发）。
    withHost()
    setup({
      OpenTerminal: async () => { await new Promise(r => setTimeout(r, 250)) }
    })
    await attached()
    await settleResize()
    expect(callsOf('ResizeTerminal')).toHaveLength(0)
  })
})

// ---- 后端推来的输出 ----

describe('常驻终端表面 输出回显', () => {
  it('后端推来的字节原样写进终端', async () => {
    withHost()
    setup()
    await attached()
    clearSurface()

    const bytes = new TextEncoder().encode('hello 中文\n')
    rt.emit('term:data', { hostId: 'h1', data: bytesToBase64(bytes) })
    await nextTick()

    expect(instances[0].text()).toBe('hello 中文\n')
  })

  it('被切成两块的汉字仍然完整 —— 这正是输出要按 base64 传的理由', async () => {
    withHost()
    setup()
    await attached()
    clearSurface()

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
    await attached()
    await selectHost('h2')

    const h1 = instances[0]
    const h2 = instances[1]
    clearSurface(1)
    rt.emit('term:data', { hostId: 'h2', data: bytesToBase64(new TextEncoder().encode('来自 h2')) })
    await nextTick()

    expect(h2.text()).toBe('来自 h2')
    expect(h1.text()).not.toContain('来自 h2')
  })

  it('没有附着任何主机表面时到达的输出直接丢掉，不报错', async () => {
    setup() // 没选主机 → 没有实例、没有落点
    rt.emit('term:data', { hostId: 'h1', data: bytesToBase64(new TextEncoder().encode('x')) })
    await nextTick()

    expect(wrapper.text()).not.toContain('解码失败')
  })
})

// ---- 退出与关闭 ----

describe('常驻终端表面 退出与关闭', () => {
  it('远端退出时显示原因并给出重新打开的入口', async () => {
    withHost()
    setup()
    await attached()

    rt.emit('term:exit', { hostId: 'h1', reason: '远端 shell 已退出（退出码 0）' })
    await nextTick()

    expect(wrapper.text()).toContain('远端 shell 已退出')
    expect(wrapper.text()).toContain('重新打开')
  })

  it('点「重新打开」会开一条新的，并换掉旧实例', async () => {
    withHost()
    setup()
    await attached()
    const old = instances[0]

    rt.emit('term:exit', { hostId: 'h1', reason: '远端已断开连接' })
    await nextTick()
    await wrapper.find('.surface-status button').trigger('click')
    await flushPromises()

    expect(instances).toHaveLength(2)
    expect(instances[0]).toBe(old)
    expect(instances[0].disposed).toBe(true)
    expect(callsOf('OpenTerminal')).toHaveLength(2)
  })

  it('自己点关闭会打后端并销毁本地实例', async () => {
    withHost()
    setup()
    await attached()

    await wrapper.find('.sess-banner button').trigger('click')
    await flushPromises()

    expect(callsOf('CloseTerminal').map(c => c.args[0])).toEqual(['h1'])
    expect(instances[0].disposed).toBe(true)
  })

  it('自己关掉的终端不再弹一条「已断开」', async () => {
    withHost()
    setup()
    await attached()

    await wrapper.find('.sess-banner button').trigger('click')
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
    await attached()

    expect(wrapper.text()).toContain('申请伪终端失败')
    expect(wrapper.text()).toContain('重新打开')
  })
})

// ---- 界面必须说清楚风险 ----

describe('常驻终端表面 风险提示', () => {
  it('显眼处写明终端内输入的分流与策略闸门', async () => {
    withHost()
    setup()
    await attached()

    const banner = wrapper.find('.sess-banner')
    expect(banner.exists()).toBe(true)
    // 输入已整体搬进终端：中文/? 交 LLM，shell 先过策略（高危需确认）。
    expect(banner.text()).toContain('交给')
    expect(banner.text()).toContain('先过策略')
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

// ---- 欢迎引导：每次打开/回到控制台都提示「这里能说人话」----
//
// 顶部 banner 是常驻的，但常驻的东西会被眼睛过滤掉。引导必须落在「正要用它」
// 的那一刻：终端刚打开、以及从归档切回表面时，在提示符旁边就地出现一次。
describe('欢迎引导', () => {
  // 引导比真提示符晚出现（远端 shell 打提示符要几百毫秒）。测试里用假时钟
  // 快进，不真的等 700ms —— 否则每个用例都白跑一秒。
  async function runHintTimer() {
    await flushPromises()
    vi.advanceTimersByTime(1000)
    await nextTick()
  }

  it('打开终端后出现一次，等真提示符出来再画', async () => {
    vi.useFakeTimers()
    try {
      withHost()
      setup()
      await vi.advanceTimersByTimeAsync(0)
      await attached()

      // 计时器还没到点：此刻不该有引导（否则会被远端提示符压过去）。
      expect(instances[0].text()).not.toContain('自然语言')

      await runHintTimer()
      const text = instances[0].text()
      expect(text).toContain('自然语言')
      expect(text).toContain('LLM')
      // 占位符必须**就地写在光标行上**：不能另起一行（nl 会先落一个 \r\n），
      // 否则这行说明会混进命令输出流里，看起来像一条命令的结果。
      expect(text).not.toContain('\r\n')
    } finally {
      vi.useRealTimers()
    }
  })

  it('占位符从提示符末尾那一列开始写，光标随后归位到该列', async () => {
    vi.useFakeTimers()
    try {
      withHost()
      setup()
      await vi.advanceTimersByTimeAsync(0)
      await attached()

      const inst = instances[0]
      // 模拟真实提示符 root@VM_0_16_centos:~$ —— 23 列宽，光标停在列 23。
      inst.buffer.active.cursorX = 23
      await runHintTimer()

      const out = inst.text()
      // 提示之前不能有换行：必须以提示文字直接开头（紧跟在提示符之后）。
      expect(out.startsWith('\x1b[2m可以直接用自然语言')).toBe(true)
      // 写完提示，光标用**绝对列**移回提示符末尾（列 23 → ANSI 24）。
      // 按提示宽度倒推会在提示符较宽时偏左，把提示符盖住。
      expect(out.endsWith('\x1b[24G')).toBe(true)
    } finally {
      vi.useRealTimers()
    }
  })

  it('开始输入时占位符被擦掉，不污染真实输入行', async () => {
    vi.useFakeTimers()
    try {
      withHost()
      setup()
      await vi.advanceTimersByTimeAsync(0)
      await attached()

      const inst = instances[0]
      // 提示符末尾列号在**写占位符时**读取（见 paintWelcomeHintIfNeeded），
      // 所以必须在计时器到点前把 cursorX 摆好，模拟「提示符占了 16 列」。
      inst.buffer.active.cursorX = 16
      await runHintTimer()
      clearSurface()
      inst.emitData('l')
      const out = inst.text()
      // 擦除：把光标移到占位符起点（列 16 → ANSI 17），用等宽空格盖掉提示，
      // 再回到提示符末尾 —— 这样后续回显落在正确位置。
      expect(out).toMatch(/\x1b\[17G +\x1b\[17G/)
      expect(out).not.toContain('\r\n')
      // 擦完之后才是用户敲的那个字符。
      expect(out.endsWith('l')).toBe(true)
    } finally {
      vi.useRealTimers()
    }
  })

  it('占位符不在「已有本地输入」时插入（避免盖掉用户打了一半的行）', async () => {
    vi.useFakeTimers()
    try {
      withHost()
      setup()
      await vi.advanceTimersByTimeAsync(0)
      await attached()

      const inst = instances[0]
      // 用户在提示符下已经敲了字，此时切回表面不该把占位符塞进来。
      inst.buffer.active.cursorX = 16
      inst.emitData('ls')
      clearSurface()
      await runHintTimer()

      expect(inst.text()).not.toContain('自然语言')
    } finally {
      vi.useRealTimers()
    }
  })

  it('同一块表面只提示一次，不随布局重测反复叠加', async () => {
    vi.useFakeTimers()
    try {
      withHost()
      setup()
      await vi.advanceTimersByTimeAsync(0)
      await attached()
      await runHintTimer()

      // 触发一次重测（容器尺寸变化会引起 doFit → 可能再走 ensureTerm）。
      roInstances[0].cb()
      await vi.advanceTimersByTimeAsync(1000)
      await nextTick()

      const n = instances[0].text().split('自然语言').length - 1
      expect(n).toBe(1)
    } finally {
      vi.useRealTimers()
    }
  })

  it('从对话归档切回表面时再提示一次', async () => {
    vi.useFakeTimers()
    try {
      withHost()
      setup()
      await vi.advanceTimersByTimeAsync(0)
      await attached()
      await runHintTimer()

      await toArchive()
      await toSurface()
      await vi.advanceTimersByTimeAsync(1000)
      await nextTick()

      const n = instances[0].text().split('自然语言').length - 1
      expect(n).toBe(2)
    } finally {
      vi.useRealTimers()
    }
  })
})

// ---- 终端内直接输入（本地行编辑 + 回车分类）----

describe('终端内直接输入自然语言', () => {
  // 逐字符敲进终端，模拟用户在提示符下打字。
  function type(inst, s) {
    for (const ch of s) inst.emitData(ch)
  }

  // 含中文的**自然语言**（首词不是命令）直接进 Agent，不再弹窗 ——
  // 曾经「含中文就问」，连 `你是什么模型` 都要点一下就纯属打扰。
  // 命令识别交由后端（见 app_shell.go ClassifyShellInput）。
  it('中文自然语言回车 → 直接交给 Agent，不弹窗', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    clearSurface()
    type(inst, '磁盘满了怎么办')
    inst.emitData('\r')
    await flushPromises()
    await nextTick()
    expect(wrapper.find('.route-modal').exists()).toBe(false)
    const asks = callsOf('Ask')
    expect(asks.length).toBe(1)
    expect(JSON.stringify(asks[0].args)).toContain('磁盘满了怎么办')
    // 自然语言绝不该被当命令发给 shell
    expect(callsOf('RunShellInTerminal').length).toBe(0)
  })

  // 含中文但首词是命令（`echo 你好`）：命令意图明确，直接走 shell，不弹窗。
  it('中文参数的命令回车 → 直接走 shell，不弹窗', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    clearSurface()
    type(inst, 'echo 你好')
    inst.emitData('\r')
    await flushPromises()
    await nextTick()
    expect(wrapper.find('.route-modal').exists()).toBe(false)
    const run = callsOf('RunShellInTerminal')
    expect(run.length).toBe(1)
    expect(run[0].args[2]).toBe('echo 你好')
  })

  it('shell 命令回车 → 走常驻终端通道（策略闸门）', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    clearSurface()
    type(inst, 'ls -la')
    inst.emitData('\r')
    await flushPromises()
    const run = callsOf('RunShellInTerminal')
    expect(run.length).toBe(1)
    expect(run[0].args[0]).toBe('h1')
    expect(run[0].args[2]).toBe('ls -la')
    // 前端不自己往 PTY 写字节（放行后由后端写入）。
    expect(callsOf('WriteTerminal').length).toBe(0)
    expect(callsOf('Ask').length).toBe(0)
  })

  it('可打印字符本地回显，回车前擦除输入行（不双份、保留提示符）', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    clearSurface()
    inst.buffer.active.cursorX = 14 // 提示符占了 14 列
    type(inst, 'ls')
    expect(inst.text()).toBe('ls')
    inst.emitData('\r')
    // 回车用等宽空格覆盖本地回显（'ls'=2 列）再回起点，跨行折返也擦得净；
    // 空格前后带 SGR 复位，避免继承远端残留的黑底画出一条黑带。
    expect(inst.text()).toContain('\r\x1b[15G\x1b[0m  \x1b[0m\r\x1b[15G')
    await flushPromises()
  })

  it('退格本地删一个字符', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    clearSurface()
    type(inst, 'lsx')
    inst.emitData('\x7f') // 退格删掉 x
    inst.emitData('\r')
    await flushPromises()
    const run = callsOf('RunShellInTerminal')
    expect(run.length).toBe(1)
    expect(run[0].args[2]).toBe('ls')
    expect(inst.text()).toContain('\b \b')
  })

  it('全屏程序接管（备用屏幕）时原样透传，不抢键', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    clearSurface()
    inst.buffer.active.type = 'alternate' // 模拟进了 vim
    inst.emitData('i')
    await flushPromises()
    const wt = callsOf('WriteTerminal')
    expect(wt.length).toBe(1)
    expect(wt[0].args[2]).toBe(textToBase64('i'))
    expect(callsOf('Ask').length).toBe(0)
    expect(callsOf('RunShellInTerminal').length).toBe(0)
  })

  it('procps top（不发备用屏幕）靠 term:tui 让路：q 原样透传', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    clearSurface()
    // top 走原地重绘，buffer 仍是 normal —— 只靠后端推来的 term:tui 识别。
    rt.emit('term:tui', { hostId: 'h1', active: true })
    await flushPromises()
    inst.emitData('q')
    await flushPromises()
    const wt = callsOf('WriteTerminal')
    expect(wt.length).toBe(1)
    expect(wt[0].args[2]).toBe(textToBase64('q'))
    expect(callsOf('Ask').length).toBe(0)
    expect(callsOf('RunShellInTerminal').length).toBe(0)

    // 退出 top 后恢复本地行编辑：可打印字符不再写 PTY。
    rt.emit('term:tui', { hostId: 'h1', active: false })
    await flushPromises()
    inst.emitData('l')
    await flushPromises()
    expect(callsOf('WriteTerminal').length).toBe(1)
  })

  it('控制键（Tab）打断本地行编辑：缓冲连同该键交给 PTY', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    clearSurface()
    type(inst, 'ls')
    inst.emitData('\t') // Tab：放弃本地编辑，透传给 shell 做补全
    await flushPromises()
    const wt = callsOf('WriteTerminal')
    expect(wt.length).toBe(2)
    expect(wt[0].args[2]).toBe(textToBase64('ls'))
    expect(wt[1].args[2]).toBe(textToBase64('\t'))
    // 本地缓冲已清空，后续回车不该再重复提交。
    inst.emitData('\r')
    await flushPromises()
    expect(callsOf('RunShellInTerminal').length).toBe(0)
  })

  it('空回车只给 PTY 一个换行', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    clearSurface()
    inst.emitData('\r')
    await flushPromises()
    const wt = callsOf('WriteTerminal')
    expect(wt.length).toBe(1)
    expect(wt[0].args[2]).toBe(textToBase64('\n'))
  })

  it('多行粘贴逐行提交（onData 多字符逐字符回放）', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    clearSurface()
    inst.emitData('ls\rpwd\r') // 一次粘贴两行
    await flushPromises()
    const run = callsOf('RunShellInTerminal')
    expect(run.map(c => c.args[2])).toEqual(['ls', 'pwd'])
  })

  // ---- Ctrl+V 粘贴接线 ----
  //
  // xterm 不碰剪贴板：Ctrl+V 会被浏览器先截走，一个字节都到不了 onData。
  // 所以组件用 attachCustomKeyEventHandler 自己接管，读剪贴板后喂给 onTermData。

  it('Ctrl+V 拦下按键并读剪贴板，内容进本地行缓冲', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    clearSurface()
    withClipboard('ls -la')
    const handled = inst.emitKey(keyEvent('v', { ctrlKey: true }))
    await flushPromises()
    // 返回 false = 组件接管了这个键，不让 xterm 再处理。
    expect(handled).toBe(false)
    // 粘贴的整段命令留在本地缓冲，回车前不发给远端。
    expect(callsOf('RunShellInTerminal').length).toBe(0)
    inst.emitData('\r')
    await flushPromises()
    expect(callsOf('RunShellInTerminal')[0].args[2]).toBe('ls -la')
  })

  it('Shift+Insert 同样触发粘贴', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    clearSurface()
    withClipboard('pwd')
    const handled = inst.emitKey(keyEvent('Insert', { shiftKey: true }))
    await flushPromises()
    expect(handled).toBe(false)
    inst.emitData('\r')
    await flushPromises()
    expect(callsOf('RunShellInTerminal')[0].args[2]).toBe('pwd')
  })

  it('普通按键放行给 xterm（不被粘贴逻辑吃掉）', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    expect(inst.emitKey(keyEvent('v'))).toBe(true)
    expect(inst.emitKey(keyEvent('a', { ctrlKey: true }))).toBe(true)
    // keyup 不处理，避免一次按键读两遍剪贴板。
    expect(inst.emitKey({ type: 'keyup', key: 'v', ctrlKey: true })).toBe(true)
  })

  // ---- 框选复制：copy-on-select / Ctrl+Shift+C / Ctrl+Insert ----
  //
  // 背景：xterm 默认 Canvas 渲染，文本是画布像素，浏览器原生框选选不到东西；
  // 且 xterm.css 在 .xterm 上设了 user-select:none。所以复制只能走 xterm 自己的
  // 选区模型（getSelection / onSelectionChange）。下面钉的就是这条接线。

  it('拖选后自动复制（copy-on-select）', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    const copied = withWriteClipboard()
    inst.emitSelection('ls -la\ntotal 0')
    // 防抖后才写剪贴板。
    await vi.waitFor(() => expect(copied.text).toBe('ls -la\ntotal 0'))
  })

  it('取消选中不写剪贴板（防抖被撤销）', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    const copied = withWriteClipboard()
    inst.emitSelection('some text')
    // 还没到防抖窗口就取消选中：应把待执行的复制撤掉。
    inst.emitSelection('')
    await new Promise(r => setTimeout(r, 120))
    expect(copied.text).toBe('')
  })

  it('Ctrl+Shift+C 复制选区并返回 false（不让 xterm 处理）', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    const copied = withWriteClipboard()
    inst.selection = 'docker ps'
    const handled = inst.emitKey(keyEvent('C', { ctrlKey: true, shiftKey: true }))
    expect(handled).toBe(false)
    await vi.waitFor(() => expect(copied.text).toBe('docker ps'))
  })

  it('Ctrl+Insert 同样复制选区', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    const copied = withWriteClipboard()
    inst.selection = 'uptime'
    expect(inst.emitKey(keyEvent('Insert', { ctrlKey: true }))).toBe(false)
    await vi.waitFor(() => expect(copied.text).toBe('uptime'))
  })

  it('无选区时裸 Ctrl+C 放行（仍是 SIGINT，不被复制逻辑吃掉）', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    const copied = withWriteClipboard()
    // 没有选区：Ctrl+C 必须继续走原来的 \x03 透传链路。
    expect(inst.emitKey(keyEvent('c', { ctrlKey: true }))).toBe(true)
    expect(copied.text).toBe('')
  })

  it('有选区时裸 Ctrl+C 改为复制（不再发 SIGINT）', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    const copied = withWriteClipboard()
    inst.selection = 'rm -rf /tmp/x'
    clearSurface()
    const handled = inst.emitKey(keyEvent('c', { ctrlKey: true }))
    expect(handled).toBe(false)
    await vi.waitFor(() => expect(copied.text).toBe('rm -rf /tmp/x'))
    // 复制分支不得把 \x03 打进 PTY。
    expect(callsOf('WriteTerminal').length).toBe(0)
  })

  it('复制不往终端流写任何字节（否则会冲掉正在编辑的输入行）', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    const copied = withWriteClipboard()
    // 回归点：早先的实现会在复制后用 paintSystem 在终端里闪一行「已复制 N 个字符」，
    // 而 paintSystem 先写 \r\n 把光标推到新行，本地行编辑的 t.line / t.startCol 却
    // 不知道光标被挪走了 —— 用户打了一半命令时复制，接着输入字符落在新行、退格
    // 却按旧列号定位，表现为擦错位置。复制必须静默。
    //
    // 只比较复制前后的增量，不断言整个流为空：终端本身可能因别的异步链路
    // （上一条用例跑过 shell 命令，静默 400ms 后画「问 LLM」提示）在后台写入，
    // 那不归复制负责。
    const before = inst.written.length
    inst.selection = 'docker ps'
    inst.emitKey(keyEvent('C', { ctrlKey: true, shiftKey: true }))
    // 等到剪贴板真的被写入，证明显式复制这条路确实跑完了。
    await vi.waitFor(() => expect(copied.text).toBe('docker ps'))
    // 剪贴板写入是 async 的，任何随之而来的终端写入都排在它之后 —— 多等一轮
    // 宏任务，确保「若实现真的画了提示」已经落进 written，断言才抓得到。
    await new Promise(r => setTimeout(r, 50))
    const extra = inst.written.slice(before).join('')
    // 复制本身不得产生任何终端输出（尤其不得是「已复制」这类提示）。
    expect(extra).not.toContain('已复制')
    expect(extra).not.toContain('想分析这段输出')
  })

  it('剪贴板被拒时不抛异常、不写出任何字节', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    clearSurface()
    // WebView2 下 readText 可能因权限被拒。
    withClipboard(null, new Error('denied'))
    expect(inst.emitKey(keyEvent('v', { ctrlKey: true }))).toBe(false)
    await flushPromises()
    expect(callsOf('WriteTerminal').length).toBe(0)
    expect(callsOf('RunShellInTerminal').length).toBe(0)
  })

  it('连敲回车串行提交（不并发 runShellInTerminal）', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    clearSurface()
    inst.emitData('a\rb\r')
    await flushPromises()
    const run = callsOf('RunShellInTerminal')
    expect(run.map(c => c.args[2])).toEqual(['a', 'b'])
  })

  it('本地编辑期间来外部输出 → flush 本地缓冲并降级透传', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    clearSurface()
    inst.emitData('l') // 本地缓冲 + 回显
    // 后端推来一段输出（后台打印）：sink 发现正在本地编辑 → flush 'l' 给 PTY 并切透传。
    rt.emit('term:data', { hostId: 'h1', data: textToBase64('X') })
    await flushPromises()
    inst.emitData('s') // 已降级：透传，不再本地缓冲
    await flushPromises()
    const wt = callsOf('WriteTerminal')
    expect(wt.map(c => c.args[2])).toEqual([textToBase64('l'), textToBase64('s')])
  })

  it('中文按显示宽度（2 列）擦除', async () => {
    setup()
    withHost()
    await attached()
    const inst = instances[0]
    clearSurface()
    inst.emitData('中')
    inst.emitData('\r')
    // '中' 占 2 列 → 覆盖 2 个空格；startCol=0 → 定位第 1 列；同样带 SGR 复位。
    expect(inst.text()).toContain('\r\x1b[1G\x1b[0m  \x1b[0m\r\x1b[1G')
    await flushPromises()
  })
})
