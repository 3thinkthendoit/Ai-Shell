// terminal 层拆分后的纯 JS 单测（不挂载 ConsolePanel）。
//
// 原来这些逻辑全埋在 ConsolePanel.vue 的 <script setup> 里，只能靠挂载整棵
// 组件去间接触发；拆成 termSession / termInput / termSubmit 三个模块后，可以
// 直接对模块说话 —— 测的是「我们自己的那层接线」，而非 Vue 渲染。
//
// xterm 仍走 vitest.config.js 挂上的替身（__tests__/stubs/xterm.js）：本地回显
// 落在 instance.written，退格/擦除是转义序列，都能逐字节断言。
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { store, bucketKey, base64ToBytes } from '../store.js'
import {
  openedKeys,
  ensureTerm,
  disposeTerm,
  getTerm,
  setSurfaceEl,
  surfaceWrite,
  displayWidth,
  cursorCol,
  eraseLocalLine,
  isFullscreen,
  isCmdRunning
} from '../terminal/termSession.js'
import { onTermData } from '../terminal/termInput.js'
import { enqueueSubmit } from '../terminal/termSubmit.js'
import { CMD_PENDING_IDLE_MS } from '../terminal/termEvents.js'
import { instances, resetInstances } from './stubs/xterm.js'

const toBytes = x => Array.from(x)

// ---- Wails 后端替身 ----
// 只需覆盖被这些模块摸到的那几只方法；其余留空即可。
function makeApp(impl = {}) {
  const calls = []
  const base = {
    Ask: async () => undefined,
    Stop: async () => undefined,
    RunShellInTerminal: async () => ({ status: 'done', decision: 'allow', reason: '', rule: 'auto_safe', risk: 'low' }),
    ClassifyShellInput: async text => {
      const first = String(text || '').trim().split(/\s+/)[0] || ''
      const binary = first.replace(/^.*\//, '')
      return ['ls', 'echo', 'df', 'pwd'].includes(binary)
    },
    ResolveUnknownInput: async () => ({ kind: 'shell' }),
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
  store.busy = false
  store.llmProfiles = [{ id: 'p1', name: 'default', model: 'm', active: true }]
  store.selectedProfileId = 'p1'
}

let calls
let rtHandlers

// 建一块真实存在的表面：塞一个有尺寸的容器元素（替身没有布局，doFit 只看
// clientWidth/clientHeight 是否非零），再走 ensureTerm 把 xterm 实例建起来。
// 返回 bucketKey，供 getTerm/onTermData/enqueueSubmit 直接引用。
async function createSurface(hostId = 'h1', sessionId = 'default') {
  setSurfaceEl(bucketKey(hostId, sessionId), { clientWidth: 800, clientHeight: 600 })
  await ensureTerm(hostId, sessionId, 'light')
  return bucketKey(hostId, sessionId)
}

function withHost(id = 'h1') {
  store.hosts = [{ id, name: 'web', user: 'root', addr: '10.0.0.1' }]
  store.currentHostId = id
  store.currentSessionId = 'default'
}

function callsOf(name) {
  return calls.filter(c => c.name === name)
}

beforeEach(() => {
  // 假定时器：ensureTerm 会挂防抖 resize（150ms）与欢迎引导（700ms）两只延时，
  // 不手动推进就永远不落 —— 既避免污染 calls，也防止跨用例泄漏。
  vi.useFakeTimers()
  resetInstances()
  resetStore()
  rtHandlers = new Map()
  const made = makeApp()
  calls = made.calls
  window.go = { main: { App: made.app } }
  window.runtime = {
    EventsOn(name, cb) {
      if (!rtHandlers.has(name)) rtHandlers.set(name, [])
      rtHandlers.get(name).push(cb)
    },
    emit(name, payload) {
      for (const cb of rtHandlers.get(name) || []) cb(payload)
    }
  }
})

afterEach(() => {
  for (const key of [...openedKeys.value]) disposeTerm(key)
  resetInstances()
  vi.useRealTimers()
})

describe('termSession · 纯计算工具', () => {
  it('displayWidth 按显示宽度计数：东亚宽字符算 2，ASCII 算 1', () => {
    expect(displayWidth('abc')).toBe(3)
    expect(displayWidth('你好')).toBe(4)
    expect(displayWidth('ａ')).toBe(2) // 全角 a（U+FF41）
    expect(displayWidth('')).toBe(0)
    expect(displayWidth('a你b')).toBe(4)
  })

  it('cursorCol 读缓冲区光标列，缺缓冲区时回退 0', () => {
    expect(cursorCol({ term: { buffer: { active: { cursorX: 9 } } } })).toBe(9)
    expect(cursorCol({ term: { buffer: { active: { cursorX: 0 } } } })).toBe(0)
    expect(cursorCol({ term: {} })).toBe(0)
  })

  it('eraseLocalLine 从起始列擦掉整行显示宽度并复位光标', () => {
    const t = { startCol: 2, term: { written: [], write(d) { this.written.push(d) } } }
    eraseLocalLine(t, 'ab')
    const go = '\r\x1b[3G'
    expect(t.term.written).toEqual([go + '\x1b[0m' + '  ' + '\x1b[0m' + go])
  })

  it('eraseLocalLine 对空行是 no-op（不该盲目擦掉提示符）', () => {
    const t = { startCol: 0, term: { written: [], write(d) { this.written.push(d) } } }
    eraseLocalLine(t, '')
    expect(t.term.written).toEqual([])
  })
})

describe('termSession · 实例生命周期', () => {
  it('ensureTerm 建一个 xterm 实例、登记落点，surfaceWrite 返回可写落点', async () => {
    withHost()
    const key = await createSurface()
    expect(instances.length).toBe(1)
    const t = getTerm(key)
    expect(t).toBeTruthy()
    expect(t.hostId).toBe('h1')
    expect(t.sessionId).toBe('default')
    expect(openedKeys.value).toContain(key)
    // 落点存在 → surfaceWrite 返回函数；写进去的字节落在替身 written。
    const write = surfaceWrite(key)
    expect(typeof write).toBe('function')
    const before = instances[0].written.length
    write('ping')
    expect(instances[0].written.length).toBe(before + 1)
    expect(instances[0].text()).toContain('ping')
  })

  it('disposeTerm 拆掉实例并从 openedKeys 摘除', async () => {
    withHost()
    const key = await createSurface()
    disposeTerm(key)
    expect(getTerm(key)).toBeUndefined()
    expect(openedKeys.value).not.toContain(key)
    expect(instances[0].disposed).toBe(true)
  })

  it('isFullscreen / isCmdRunning 跟随 termState 的让路信号', async () => {
    withHost()
    const key = await createSurface()
    expect(isFullscreen(key)).toBe(false)
    expect(isCmdRunning(key)).toBe(false)
    store.terms[key].cmdActive = true
    expect(isCmdRunning(key)).toBe(true)
    store.terms[key].cmdActive = false
    store.terms[key].tuiActive = true
    expect(isFullscreen(key)).toBe(true)
  })
})

describe('termInput · onTermData 本地行编辑', () => {
  it('可打印字符本地回显并缓冲，不写 PTY', async () => {
    withHost()
    const key = await createSurface()
    const t = getTerm(key)
    const w = instances[0]
    w.written.length = 0
    onTermData(key, 'l')
    onTermData(key, 's')
    expect(t.line).toBe('ls')
    expect(w.text()).toBe('ls')
    expect(callsOf('WriteTerminal')).toEqual([])
  })

  it('退格按码点删除并擦一列，空行退格不越界', async () => {
    withHost()
    const key = await createSurface()
    const t = getTerm(key)
    const w = instances[0]
    onTermData(key, 'a')
    onTermData(key, 'b')
    w.written.length = 0
    onTermData(key, '\x7f')
    expect(t.line).toBe('a')
    expect(w.text()).toBe('\b \b')
    // 把最后一个也删掉，再退一次：空行时不应再写任何擦除
    onTermData(key, '\x7f')
    expect(t.line).toBe('')
    w.written.length = 0
    onTermData(key, '\x7f')
    expect(w.written).toEqual([])
  })

  it('控制键（非回车/退格）flush 缓冲后透传 PTY 并降级为 passthrough', async () => {
    withHost()
    const key = await createSurface()
    const t = getTerm(key)
    onTermData(key, '\x0c') // Ctrl+L：清屏控制键，归 shell/readline
    expect(callsOf('WriteTerminal').length).toBe(1)
    expect(toBytes(base64ToBytes(callsOf('WriteTerminal')[0].args[2]))).toEqual(toBytes(new TextEncoder().encode('\x0c')))
    expect(t.passthrough).toBe(true)
  })

  it('agent 运行中的按键被丢弃（不回显、不透传），Ctrl-C 例外放行 stop', async () => {
    withHost()
    const key = await createSurface()
    const w = instances[0]
    store.running = true
    w.written.length = 0
    onTermData(key, 'x')
    expect(t_line(getTerm(key))).toBe('')
    expect(w.written).toEqual([])
    expect(callsOf('WriteTerminal')).toEqual([])
    onTermData(key, '\x03')
    expect(callsOf('Stop').length).toBe(1)
    store.running = false
  })

  it('回车清掉本地缓冲并把这一行交给提交队列（不直接写 PTY）', async () => {
    withHost()
    const key = await createSurface()
    const t = getTerm(key)
    onTermData(key, 'l')
    onTermData(key, 's')
    // 回车：同步阶段只擦本地行 + 入队，真正的 shell 写是异步的，此刻还没落 WriteTerminal
    onTermData(key, '\r')
    expect(t.line).toBe('')
    expect(callsOf('WriteTerminal')).toEqual([])
  })
})

describe('termSubmit · enqueueSubmit 分流', () => {
  it('识别为命令的 shell 行经硬拒绝过滤写进常驻 PTY', async () => {
    withHost()
    const key = await createSurface()
    await enqueueSubmit(key, 'ls -la')
    const run = callsOf('RunShellInTerminal')
    expect(run.length).toBe(1)
    const [hostId, sessionId, command] = run[0].args
    expect(hostId).toBe('h1')
    expect(sessionId).toBe('default')
    expect(command).toBe('ls -la')
    expect(callsOf('Ask')).toEqual([])
  })

  it('含中文的自然语言交给 Agent，不碰 PTY', async () => {
    withHost()
    const key = await createSurface()
    await enqueueSubmit(key, '你是什么模型')
    const ask = callsOf('Ask')
    expect(ask.length).toBe(1)
    // store.ask 把 (hostId, sessionId, prompt, profileId) 一起递交后端，提问在第 3 位。
    expect(ask[0].args[2]).toBe('你是什么模型')
    expect(callsOf('RunShellInTerminal')).toEqual([])
  })

  it('空回车只给 PTY 一个换行，让 shell 回显新提示符', async () => {
    withHost()
    const key = await createSurface()
    calls.length = 0
    await enqueueSubmit(key, '   ')
    const wt = callsOf('WriteTerminal')
    expect(wt.length).toBe(1)
    expect(toBytes(base64ToBytes(wt[0].args[2]))).toEqual(toBytes(new TextEncoder().encode('\n')))
  })

  it('agent 对话中回车：像命令的回复也交给 Agent，不碰 PTY', async () => {
    withHost()
    // 本会话已有 Agent 回复 → 进入 agent 模式：即便 `ls -la` 是已知命令也当回复交给 Agent。
    store.bySession[bucketKey('h1', 'default')] = [{ kind: 'assistant', content: '先确认来源 IP。' }]
    const key = await createSurface()
    await enqueueSubmit(key, 'ls -la')
    expect(callsOf('Ask').length).toBe(1)
    expect(callsOf('RunShellInTerminal')).toEqual([])
  })
})

describe('cmdPending · 提交命令后的乐观透传窗口', () => {
  // 兜住「shell integration 缺席、程序又在切备用屏前就等输入」的卡死：
  // vim 的 E325、read、菜单都落在这一类。见 termEvents cmdPending 注释。
  it('提交 shell 命令后开窗口：按键直达 PTY，不再被本地行编辑吞掉', async () => {
    withHost()
    const key = await createSurface()
    await enqueueSubmit(key, 'ls')
    // 命令写进 PTY 后（status done）乐观窗口应把 isCmdRunning 顶起来，
    // 让接下来输入的「R/O/Q」这类选项字母走透传分支（termInput line 29）。
    expect(isCmdRunning(key)).toBe(true)
    calls.length = 0
    onTermData(key, 'R')
    const wt = callsOf('WriteTerminal')
    expect(wt.length).toBe(1)
    expect(toBytes(base64ToBytes(wt[0].args[2]))).toEqual(toBytes(new TextEncoder().encode('R')))
    // 透传的字符不该再进本地缓冲
    expect(getTerm(key).line).toBe('')
  })

  it('输出静默 CMD_PENDING_IDLE_MS 后窗口自动关闭，可打印字符回到本地行编辑', async () => {
    withHost()
    const key = await createSurface()
    await enqueueSubmit(key, 'ls')
    expect(isCmdRunning(key)).toBe(true)
    // 没有权威接管信号（本文件不 bindTerminalEvents），只靠静默计时兜底关窗。
    vi.advanceTimersByTime(CMD_PENDING_IDLE_MS)
    expect(isCmdRunning(key)).toBe(false)
    calls.length = 0
    onTermData(key, 'R')
    // 回到提示符：R 是普通可打印字符，应重新进本地缓冲、不写 PTY。
    expect(callsOf('WriteTerminal')).toEqual([])
    expect(getTerm(key).line).toBe('R')
  })

  it('窗口开着时退回的每个可打印字符都直达 PTY（覆盖 vim E325 连续选项）', async () => {
    withHost()
    const key = await createSurface()
    await enqueueSubmit(key, 'ls')
    expect(callsOf('RunShellInTerminal').length).toBe(1)
    calls.length = 0
    onTermData(key, 'R')
    onTermData(key, '\r') // E325 里 "Press ENTER" —— 回车也应透传给程序而非本地提交
    const wt = callsOf('WriteTerminal')
    expect(wt.length).toBe(2)
    expect(toBytes(base64ToBytes(wt[0].args[2]))).toEqual(toBytes(new TextEncoder().encode('R')))
    expect(toBytes(base64ToBytes(wt[1].args[2]))).toEqual(toBytes(new TextEncoder().encode('\r')))
    // 回车走的是透传分支，没再触发一次提交。
    expect(callsOf('RunShellInTerminal').length).toBe(0)
  })
})

// 小工具：控制键测试里断言 t.line，避免和 termSession 的内部字段名硬耦合到别处。
function t_line(t) {
  return t ? t.line : ''
}
