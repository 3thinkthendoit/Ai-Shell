// terminal 层拆分后的纯 JS 单测（不挂载 ConsolePanel）。
//
// 原来这些逻辑全埋在 ConsolePanel.vue 的 <script setup> 里，只能靠挂载整棵
// 组件去间接触发；拆成 termSession / termInput / termSubmit 三个模块后，可以
// 直接对模块说话 —— 测的是「我们自己的那层接线」，而非 Vue 渲染。
//
// xterm 仍走 vitest.config.js 挂上的替身（__tests__/stubs/xterm.js）：本地回显
// 落在 instance.written，退格/擦除是转义序列，都能逐字节断言。
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { store, bucketKey, base64ToBytes, resetShellMarks } from '../store.js'
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
import { bindTerminalEvents, CMD_PENDING_IDLE_MS } from '../terminal/termEvents.js'
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
    // 乐观窗口回车的硬拒绝复核替身：默认放行；命中 rm 模式才 Deny，
    // 供「窗口内敲危险命令被拦」用例复用同一个 calls 记录通道。
    CheckShellCommand: async (hostID, cmd) => {
      const c = String(cmd || '')
      if (/\brm\b/.test(c)) {
        return { status: 'denied', decision: 'deny', reason: '不可逆删除', rule: 'rm_irreversible', risk: 'critical' }
      }
      return { status: 'ok', decision: 'allow', reason: '', rule: 'auto_safe', risk: 'low' }
    },
    ClassifyShellInput: async text => {
      const first = String(text || '').trim().split(/\s+/)[0] || ''
      if (first.includes('/')) return true
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
  resetShellMarks()
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
  it('容器 0x0（对话页隐藏）时 ensureTerm 不 open，有尺寸后再建', async () => {
    withHost()
    const key = bucketKey('h1', 'default')
    setSurfaceEl(key, { clientWidth: 0, clientHeight: 0 })
    await ensureTerm('h1', 'default', 'light')
    expect(instances).toHaveLength(0)
    expect(callsOf('OpenTerminal')).toHaveLength(0)
    // 占位键已登记，切回终端时 v-for 能挂上容器。
    expect(openedKeys.value).toContain(key)

    setSurfaceEl(key, { clientWidth: 800, clientHeight: 600 })
    await ensureTerm('h1', 'default', 'light')
    expect(instances).toHaveLength(1)
    expect(callsOf('OpenTerminal')).toHaveLength(1)
  })

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

  it('agent 等待回复：已知命令仍执行（回归：重连后敲 ls 不许被 Agent 劫持）', async () => {
    withHost()
    // 时间线最后一条是 assistant（AI 刚说完话在等回复）。用户看到的却是
    // 终端提示符，敲 `ls` 就是要列目录 —— 之前这里被当成对话交给 Agent，
    // 实测重连后第一条命令全被吞。命令识别优先于 agentMode。
    store.bySession[bucketKey('h1', 'default')] = [{ kind: 'assistant', content: '先确认来源 IP。' }]
    const key = await createSurface()
    await enqueueSubmit(key, 'ls -la')
    expect(callsOf('RunShellInTerminal').length).toBe(1)
    expect(callsOf('Ask')).toEqual([])
  })

  it('提示符空闲压过时间线：AI 刚说完话、终端空闲，中文短回复仍走 Agent', async () => {
    withHost()
    store.bySession[bucketKey('h1', 'default')] = [{ kind: 'assistant', content: '要继续吗？' }]
    const key = await createSurface()
    await enqueueSubmit(key, '好的')
    expect(callsOf('Ask').length).toBe(1)
    expect(callsOf('RunShellInTerminal')).toEqual([])
  })

  it('跑过命令后：回到命令模式，ls 恢复执行', async () => {
    withHost()
    store.bySession[bucketKey('h1', 'default')] = [{ kind: 'assistant', content: '先确认来源 IP。' }]
    const key = await createSurface()
    // ! 前缀强制跑一条命令（触发 markShellRun，退出 agent 等待态）。
    await enqueueSubmit(key, '!who')
    calls.length = 0
    await enqueueSubmit(key, 'ls -la')
    expect(callsOf('RunShellInTerminal').length).toBe(1)
    expect(callsOf('Ask')).toEqual([])
  })

  it('agent 对话中回车：非命令回复交给 Agent，不碰 PTY', async () => {
    withHost()
    store.bySession[bucketKey('h1', 'default')] = [{ kind: 'assistant', content: '先确认来源 IP。' }]
    const key = await createSurface()
    await enqueueSubmit(key, '好的')
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
    onTermData(key, '\r') // E325 里 "Press ENTER" —— 回车先经硬拒绝复核，放行后才透传
    await flushPromises()
    const wt = callsOf('WriteTerminal')
    expect(wt.length).toBe(2)
    expect(toBytes(base64ToBytes(wt[0].args[2]))).toEqual(toBytes(new TextEncoder().encode('R')))
    expect(toBytes(base64ToBytes(wt[1].args[2]))).toEqual(toBytes(new TextEncoder().encode('\r')))
    // 回车走的是透传分支，没再触发一次提交。
    expect(callsOf('RunShellInTerminal').length).toBe(0)
  })

  it('窗口内敲危险命令：回车被硬拒绝复核吞掉，窗口保持、无 Ctrl-U 副作用', async () => {
    withHost()
    const key = await createSurface()
    await enqueueSubmit(key, 'ls')
    expect(isCmdRunning(key)).toBe(true)
    calls.length = 0
    onTermData(key, 'rm')
    onTermData(key, ' -rf /data')
    onTermData(key, '\r')
    await flushPromises()
    // 可见字符本就透传（'rm'、' -rf /data'），吞掉的只是回车 —— 回车不发，
    // 命令就跑不了。不发 Ctrl-U、不关窗：窗口开着不代表这是提示符（vi 这类
    // 无信号程序里回车可能是正文换行），关窗会让 vi 冻死。
    const wt = callsOf('WriteTerminal')
    expect(wt.length).toBe(2)
    // 复核拿到的是攒出来的整行
    const chk = callsOf('CheckShellCommand')
    expect(chk.length).toBe(1)
    expect(chk[0].args[1]).toBe('rm -rf /data')
    // 窗口保持：后续按键继续透传（见下个用例的知情放行）
    expect(isCmdRunning(key)).toBe(true)
  })

  it('同一行被拒后再次回车：知情放行（裁决已画上屏，用户看过仍要继续）', async () => {
    withHost()
    const key = await createSurface()
    await enqueueSubmit(key, 'ls')
    calls.length = 0
    onTermData(key, 'rm -rf /data')
    onTermData(key, '\r') // 拒绝：吞一次
    await flushPromises()
    expect(callsOf('WriteTerminal').length).toBe(1) // 只有字符透传，没有回车
    calls.length = 0
    onTermData(key, '\r') // 同一行再次回车：知情放行
    await flushPromises()
    const wt = callsOf('WriteTerminal')
    expect(wt.length).toBe(1)
    expect(toBytes(base64ToBytes(wt[0].args[2]))).toEqual(toBytes(new TextEncoder().encode('\r')))
    // 放行即消费：攒行清空，拒绝记忆清除
    onTermData(key, 'rm -rf /data')
    onTermData(key, '\r') // 内容相同但已是新的一行：重新复核
    await flushPromises()
    // 重新走了一遍复核（记录窗口自知情放行起算，恰好 1 次），并且照旧拦下
    expect(callsOf('CheckShellCommand')).toHaveLength(1)
    // 写进 PTY 的只有知情放行的回车与新行的字符 —— 第二次回车照旧被吞
    const enc = s => toBytes(new TextEncoder().encode(s))
    const tail = callsOf('WriteTerminal').map(c => toBytes(base64ToBytes(c.args[2])))
    expect(tail).toEqual([enc('\r'), enc('rm -rf /data')])
  })

  it('窗口内菜单输入 + 回车：复核放行，回车照常透传且窗口保持', async () => {
    withHost()
    const key = await createSurface()
    await enqueueSubmit(key, 'ls')
    calls.length = 0
    onTermData(key, '1')
    onTermData(key, '\r')
    await flushPromises()
    const wt = callsOf('WriteTerminal')
    expect(wt.length).toBe(2)
    expect(toBytes(base64ToBytes(wt[1].args[2]))).toEqual(toBytes(new TextEncoder().encode('\r')))
    expect(callsOf('CheckShellCommand').length).toBe(1)
    // 交互程序还在前台（read/菜单在等输入），窗口不该关。
    expect(isCmdRunning(key)).toBe(true)
  })

  it('窗口内按↑历史键后回车：攒行已不可靠，不再复核直接放行', async () => {
    withHost()
    const key = await createSurface()
    await enqueueSubmit(key, 'ls')
    calls.length = 0
    onTermData(key, 'ls')
    onTermData(key, '\x1b[A') // ↑ 历史键：远端整行替换成历史命令，攒的行作废
    onTermData(key, '\r')
    // 全程同步放行：复核通道压根没被叫醒
    expect(callsOf('CheckShellCommand')).toEqual([])
    const wt = callsOf('WriteTerminal')
    expect(wt.length).toBe(3)
    expect(toBytes(base64ToBytes(wt[2].args[2]))).toEqual(toBytes(new TextEncoder().encode('\r')))
  })

  it('窗口内敲错字退格改回：攒行弹尾，复核拿到的是改正后的行', async () => {
    withHost()
    const key = await createSurface()
    await enqueueSubmit(key, 'ls')
    calls.length = 0
    onTermData(key, 'rmx')  // 敲错的 x
    onTermData(key, '\x7f') // 退格删掉 x：弹尾而不是作废整行
    onTermData(key, '\r')
    await flushPromises()
    // 复核拿到 'rm' —— 退格不该让整行失去硬拒绝保护
    const chk = callsOf('CheckShellCommand')
    expect(chk.length).toBe(1)
    expect(chk[0].args[1]).toBe('rm')
    // 危险命令照旧拦下：字符透传（'rmx'、'\x7f'），回车被吞
    const wt = callsOf('WriteTerminal')
    expect(wt.length).toBe(2)
    expect(toBytes(base64ToBytes(wt[1].args[2]))).toEqual(toBytes(new TextEncoder().encode('\x7f')))
  })

  it('窗口内按→/Home 等光标键不动行内容：攒行仍有效，危险命令照样被拦', async () => {
    withHost()
    const key = await createSurface()
    await enqueueSubmit(key, 'ls')
    calls.length = 0
    onTermData(key, 'rm -rf /data')
    onTermData(key, '\x1b[C') // →：只移光标不改内容，攒的行继续有效
    onTermData(key, '\r')
    await flushPromises()
    // 历史上方向键一律作废攒行，「敲完命令按一下→再回车」就成了绕过
    // 硬拒绝的旁路 —— 现在光标键不动行，复核照常拿到整行、回车照旧被吞
    expect(callsOf('CheckShellCommand')[0].args[1]).toBe('rm -rf /data')
    const wt = callsOf('WriteTerminal')
    expect(wt.length).toBe(2)
    expect(toBytes(base64ToBytes(wt[1].args[2]))).toEqual(toBytes(new TextEncoder().encode('\x1b[C')))
  })

  it('复核期间继续敲的字先排队，结论出来后按序补发——顺序不颠倒', async () => {
    withHost()
    const key = await createSurface()
    await enqueueSubmit(key, 'ls')
    calls.length = 0
    onTermData(key, 'vim') // 直接透传
    onTermData(key, '\r')  // hold 住等复核
    onTermData(key, 'x')   // 复核没回来：排队，而不是抢在回车前面发
    onTermData(key, 'y')
    await flushPromises() // 复核放行：先补发回车，再按序补发排队的字符
    const enc = s => toBytes(new TextEncoder().encode(s))
    const seq = callsOf('WriteTerminal').map(c => toBytes(base64ToBytes(c.args[2])))
    expect(seq).toEqual([enc('vim'), enc('\r'), enc('x'), enc('y')])
    // 队列里的字符照常攒新行：下一次回车复核拿到的是 'xy' 而不是 'vimxy'
    onTermData(key, '\r')
    await flushPromises()
    const chk = callsOf('CheckShellCommand')
    expect(chk[chk.length - 1].args[1]).toBe('xy')
  })

  it('被拒绝后窗口保持：队列里的输入重放后继续透传给程序（vi 不冻死）', async () => {
    withHost()
    const key = await createSurface()
    await enqueueSubmit(key, 'ls')
    calls.length = 0
    onTermData(key, 'rm')
    onTermData(key, '\r') // 复核中
    onTermData(key, 'h')  // 排队
    await flushPromises() // 拒绝：吞回车、不关窗
    const enc = s => toBytes(new TextEncoder().encode(s))
    const seq = callsOf('WriteTerminal').map(c => toBytes(base64ToBytes(c.args[2])))
    // 'h' 重放后经窗口透传直达 PTY —— 程序继续收得到键
    expect(seq).toEqual([enc('rm'), enc('h')])
    // 回车被吞后远端行还挂着：'h' 追加其后（远程行此刻就是 rmh）
    onTermData(key, '\r')
    await flushPromises()
    const chk = callsOf('CheckShellCommand')
    expect(chk[chk.length - 1].args[1]).toBe('rmh')
  })

  // ---- vi 卡死回归（后端 vtscreen 的 screenTUI / alt 信号 + 前端接线） ----
  // 历史缺陷：无 OSC 133、无备用屏检测的 vi 上，窗口 6 秒静默关闭后按键
  // 全进本地行编辑，vi 从此收不到输入。修复靠后端把主屏 TUI 重绘特征
  // （busybox vi 的 CUP/ED）上报成 term:tui —— 这里模拟两条信号路径。

  function emitEv(name, payload) {
    window.runtime.emit(name, payload)
  }

  it('busybox vi：主屏 TUI 特征上报（term:tui）后按键含回车全部直达 PTY', async () => {
    withHost()
    bindTerminalEvents(window.runtime)
    const key = await createSurface()
    await enqueueSubmit(key, 'vi')
    expect(isCmdRunning(key)).toBe(true)
    // vi 画第一屏：后端 screenTUI 命中 → term:tui active
    emitEv('term:tui', { hostId: 'h1', sessionId: 'default', active: true })
    expect(isCmdRunning(key)).toBe(false) // 权威信号收窗
    calls.length = 0
    const enc = s => toBytes(new TextEncoder().encode(s))
    // 静默任意久（用户阅读）也不退化回本地行编辑
    vi.advanceTimersByTime(CMD_PENDING_IDLE_MS * 10)
    onTermData(key, 'i')
    onTermData(key, 'x')
    onTermData(key, '\r')
    await flushPromises()
    const seq = callsOf('WriteTerminal').map(c => toBytes(base64ToBytes(c.args[2])))
    expect(seq).toEqual([enc('i'), enc('x'), enc('\r')])
    // 全屏程序 owns 输入：回车不走复核
    expect(callsOf('CheckShellCommand')).toHaveLength(0)
  })

  it('竞态：tui 信号先于 RunShellInTerminal resolve、窗口后开 —— 回车是程序输入，不复核', async () => {
    withHost()
    bindTerminalEvents(window.runtime)
    const key = await createSurface()
    // 后端已把 vi 的备用屏切上报，promise 才 resolve、armCmdPending 才开窗
    emitEv('term:tui', { hostId: 'h1', sessionId: 'default', active: true })
    await enqueueSubmit(key, 'vi')
    expect(store.terms[key].cmdPending).toBe(true) // 竞态：窗口与 tui 并存
    calls.length = 0
    onTermData(key, 'i')
    onTermData(key, 'x')
    onTermData(key, '\r') // vim 插入模式的换行，不是 shell 命令
    await flushPromises()
    const enc = s => toBytes(new TextEncoder().encode(s))
    const seq = callsOf('WriteTerminal').map(c => toBytes(base64ToBytes(c.args[2])))
    expect(seq).toEqual([enc('i'), enc('x'), enc('\r')])
    expect(callsOf('CheckShellCommand')).toHaveLength(0)
  })

  it('竞态窗口与 tuiActive 并存时静默超时不关窗：续窗直到 term:tui active=false 才收', async () => {
    withHost()
    bindTerminalEvents(window.runtime)
    const key = await createSurface()
    // 竞态序：tui 信号先到、armCmdPending 后开窗（上一用例同款时序）
    emitEv('term:tui', { hostId: 'h1', sessionId: 'default', active: true })
    await enqueueSubmit(key, 'vi')
    expect(store.terms[key].cmdPending).toBe(true)
    // vi 打完第一屏静默等用户阅读：静默兜底不该把窗口关掉 —— 静默是 TUI
    // 的常态，不是「命令结束回到提示符」。
    vi.advanceTimersByTime(CMD_PENDING_IDLE_MS * 3)
    expect(store.terms[key].cmdPending).toBe(true)
    // 窗口仍在 + tuiActive：按键继续直达 PTY
    calls.length = 0
    onTermData(key, 'x')
    const enc = s => toBytes(new TextEncoder().encode(s))
    expect(callsOf('WriteTerminal').map(c => toBytes(base64ToBytes(c.args[2])))).toEqual([enc('x')])
    // vi 退出：term:tui active=false 是权威收窗信号
    emitEv('term:tui', { hostId: 'h1', sessionId: 'default', active: false })
    expect(store.terms[key].cmdPending).toBe(false)
    expect(store.terms[key].tuiActive).toBe(false)
    // 回到提示符：可打印字符重新进本地行编辑
    calls.length = 0
    onTermData(key, 'R')
    expect(callsOf('WriteTerminal')).toEqual([])
    expect(getTerm(key).line).toBe('R')
  })

  it('乐观窗口里敲中文：关窗改走本地行编辑，回车交给 Agent', async () => {
    withHost()
    const key = await createSurface()
    await enqueueSubmit(key, 'ls')
    expect(isCmdRunning(key)).toBe(true)
    calls.length = 0
    onTermData(key, '执')
    onTermData(key, '行')
    onTermData(key, '报')
    onTermData(key, '错')
    // 中文不应直灌 PTY
    expect(callsOf('WriteTerminal')).toEqual([])
    expect(getTerm(key).line).toBe('执行报错')
    expect(isCmdRunning(key)).toBe(false)
    onTermData(key, '\r')
    await flushPromises()
    expect(callsOf('Ask').length).toBe(1)
    expect(callsOf('Ask')[0].args[2]).toBe('执行报错')
    expect(callsOf('RunShellInTerminal')).toEqual([])
  })
})

// 小工具：控制键测试里断言 t.line，避免和 termSession 的内部字段名硬耦合到别处。
function t_line(t) {
  return t ? t.line : ''
}
