// store.js 状态机测试。
//
// 这一层刻意不 mock 协议、不 mock 组件，只 mock Wails 暴露的两个全局对象
// (window.go / window.runtime)，然后驱动真实的事件流。
// 目的：把「Go 发事件 -> JS 改状态 -> UI 据此渲染」这条链路里最容易出错的
// 状态流转（审批条的生死、running 标志、并发发送）钉死。
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import {
  store, push, clearLog, clearSession, bootstrap, refreshHosts, refreshSessions, bindEvents,
  ask, approve, stop, dismissAuditError, bucketKey,
  runShellInTerminal, reportActiveSession, selectSession,
  registerTermSink, unregisterTermSink, textToBase64
} from '../store.js'

// 桶键由 bucketKey 生成，测试里不要手写 'h1\x00default' 这种字面量 ——
// 分隔符哪天变了，手写的那些会静默指向一个空桶，断言就全成了摆设。
const at = (hostId, sessionId) => bucketKey(hostId, sessionId)

// termPaint 会在文字之间夹 ANSI SGR 转义（颜色/加粗/复位）。断言「人眼在终端上
// 看到的文本」时先把这些转义去掉 —— 否则 '$ ls' 会被中间的 RESET 割成
// '$ ' + 'ls'，toContain 就匹配不上了。
const stripAnsi = s => s.replace(/\x1b\[[0-9;]*m/g, '')

// 往某块终端表面（按 host+session 键）挂一个「捕获落点」：termPaint 写进来的字符串收进 got，
// painted() 返回去掉 ANSI 后的可读文本。用完必须 unregisterTermSink 清掉 ——
// termSinks 是模块级单例，漏清会串到下一个用例。
// sessionId 默认取当前会话：与 paintHost()（agent 事件的落点）同源，两边键才对得上。
function captureSink(hostId, sessionId = store.currentSessionId) {
  const got = []
  const fn = x => got.push(typeof x === 'string' ? x : new TextDecoder().decode(x))
  registerTermSink(hostId, sessionId, fn)
  return { hostId, sessionId, fn, painted: () => stripAnsi(got.join('')) }
}

// ---- 测试替身 ----

// 假 runtime：记录注册的回调，允许测试主动触发事件。
function makeRuntime() {
  const handlers = new Map()
  return {
    handlers,
    EventsOn(name, cb) {
      if (!handlers.has(name)) handlers.set(name, [])
      handlers.get(name).push(cb)
    },
    // 模拟 Go 侧 runtime.EventsEmit
    emit(name, payload) {
      const list = handlers.get(name) || []
      if (!list.length) throw new Error(`没有监听者的事件被触发: ${name}`)
      list.forEach(cb => cb(payload))
    },
    handlerCount(name) {
      return (handlers.get(name) || []).length
    }
  }
}

// 假 App 绑定：记录每次调用的方法名与参数，返回值可被单个用例覆盖。
function makeApp(impl = {}) {
  const calls = []
  const base = {
    Bootstrap: async () => ({
      error: '',
      posture: { keyProtection: 'keyring', configDir: 'C:/cfg', hostCount: 0, degraded: false },
      hosts: [],
      llm: { baseUrl: 'https://api.openai.com/v1', model: 'gpt-4o-mini', hasApiKey: true },
      policy: { mode: 'manual', whitelist: [], redactOutput: true, maxOutput: 32768, maxSteps: 12 }
    }),
    ListHosts: async () => [],
    // 会话列表的默认实现：只有一条默认会话 —— 与后端的不变式一致
    // （每台主机永远至少有一条默认会话）。
    ListSessions: async () => [{ id: 'default', name: '', turns: 0, archivedTurns: 0, isDefault: true }],
    Ask: async () => undefined,
    Approve: async () => true,
    Stop: async () => undefined
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

function install({ runtime, app }) {
  if (app) window.go = { main: { App: app } }
  window.runtime = runtime
}

// store 是模块级单例，必须在用例之间手动复位。
function resetStore() {
  store.ready = false
  store.error = ''
  store.posture = { keyProtection: '', configDir: '', hostCount: 0, degraded: false }
  store.hosts = []
  store.llm = { baseUrl: '', model: '', hasApiKey: false }
  store.policy = { mode: 'manual', whitelist: [], redactOutput: true, maxOutput: 32768, maxSteps: 12 }
  store.currentHostId = ''
  // bySession 是会话记录的真实存储（entries 只是当前会话的派生视图），
  // 复位必须清它 —— 给 entries 赋值会失败，因为它是只读的 getter。
  store.bySession = {}
  store.sessions = {}
  store.currentSessionId = ''
  store.activeHostId = ''
  store.activeSessionId = ''
  store.pending = null
  store.running = false
  store.busy = false
  store.auditError = ''
}

let rt
beforeEach(() => {
  resetStore()
  delete window.go
  delete window.runtime
  rt = makeRuntime()
})

// ---- 事件接线 ----

describe('bindEvents 事件接线', () => {
  it('为后端声明的每个事件都注册了监听', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    for (const name of [
      'agent:message', 'agent:tool', 'agent:toolResult', 'agent:injection',
      'agent:approval', 'agent:status', 'agent:error', 'agent:done',
      'audit:error'
    ]) {
      expect(rt.handlerCount(name), `缺少监听: ${name}`).toBe(1)
    }
  })

  it('没有 runtime 时不抛异常（浏览器里裸跑 dev server 的场景）', () => {
    delete window.runtime
    expect(() => bindEvents()).not.toThrow()
  })

  it('agent:message 按 role 区分用户与 LLM 气泡', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('agent:message', { role: 'user', content: 'nginx 挂了' })
    rt.emit('agent:message', { role: 'assistant', content: '我来看一下' })
    expect(store.entries.map(e => e.kind)).toEqual(['user', 'assistant'])
    expect(store.entries[0].content).toBe('nginx 挂了')
  })

  it('agent:tool 用 id 做 upsert，同一个工具调用只占一条记录', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('agent:tool', { id: 'c1', name: 'run_command', status: 'pending', decision: 'confirm' })
    rt.emit('agent:tool', { id: 'c1', name: 'run_command', status: 'running', decision: 'allow' })
    const tools = store.entries.filter(e => e.kind === 'tool')
    expect(tools).toHaveLength(1)
    expect(tools[0].tool.status).toBe('running')
    expect(tools[0].tool.decision).toBe('allow')
  })

  it('两个不同 id 的工具调用各自占一条记录', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('agent:tool', { id: 'c1', name: 'run_command', status: 'pending' })
    rt.emit('agent:tool', { id: 'c2', name: 'read_file', status: 'pending' })
    expect(store.entries.filter(e => e.kind === 'tool')).toHaveLength(2)
  })

  it('agent:toolResult 带上脱敏计数与注入告警', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('agent:toolResult', { id: 'c1', exitCode: 0, content: 'x', redacted: 3, injection: ['忽略先前指令'] })
    const r = store.entries.find(e => e.kind === 'result')
    expect(r.redacted).toBe(3)
    expect(r.injection).toEqual(['忽略先前指令'])
  })

  it('agent:snapshot 落一条快照记录到归属会话的时间线', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('agent:snapshot', {
      hostId: 'h1', sessionId: 's1', termId: 'h1#a1',
      text: '> load: 9.9', redacted: 2, injection: ['忽略先前指令']
    })
    const list = store.bySession[at('h1', 's1')] || []
    expect(list).toHaveLength(1)
    expect(list[0]).toMatchObject({ kind: 'snapshot', text: '> load: 9.9', redacted: 2 })
    expect(list[0].injection).toEqual(['忽略先前指令'])
    // 不能落进当前选中的会话：快照属于开块那一轮
    expect(store.bySession[at('h1', '')] || []).toHaveLength(0)
  })

  it('agent:toolResult 命中 TTY 时交接到常驻终端（写进 PTY），不再 spawn 内联块', () => {
    // WriteTerminal 必须在 impl 里：TTY 分支会调它把命令写进常驻 PTY。缺了它
    // api().WriteTerminal 抛 TypeError，被 writeTerminal 兜住后会推一条 error 记录，
    // 污染下面「只落一条 result」的断言。
    const { app, calls } = makeApp({ WriteTerminal: async () => undefined })
    install({ runtime: rt, app })
    bindEvents()
    // 模拟「跑到一半用户切走了」：交接必须仍归属发起那一轮的主机（h1），命令写进
    // h1 的常驻 PTY、记录也落在 h1/s1 的时间线上，不串到用户切过去的 h2。
    store.activeHostId = 'h1'
    store.activeSessionId = 's1'
    store.currentHostId = 'h2'
    rt.emit('agent:toolResult', {
      id: 'c1', exitCode: 1, content: 'TERM environment variable not set.',
      tty: true, command: 'top', hostId: 'h1', sessionId: 's1'
    })
    // 归档时间线上只多一条 result —— 内联块已退役，不再有 'inline-term' 条目。
    const list = store.bySession[at('h1', 's1')] || []
    expect(list.map(e => e.kind)).toEqual(['result'])
    // 命令被交接进 h1/s1 这条任务自己的常驻 PTY：与人亲手敲 'top' 回车同形（原文字节 + 换行）。
    const writes = calls.filter(c => c.name === 'WriteTerminal')
    expect(writes).toHaveLength(1)
    expect(writes[0].args).toEqual(['h1', 's1', textToBase64('top\n')])
    // 不能落进用户切过去的那条时间线
    expect(store.bySession[at('h2', '')] || []).toHaveLength(0)
  })

  it('agent:toolResult 未命中 TTY 时只落 result，不做终端交接', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    store.activeHostId = 'h1'
    rt.emit('agent:toolResult', { id: 'c2', exitCode: 0, content: 'ok' })
    const list = store.bySession[at('h1', '')] || []
    expect(list.map(e => e.kind)).toEqual(['result'])
  })

  it('agent:injection 生成一条告警记录，缺字段时降级为空值而不是 undefined', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('agent:injection', {})
    const e = store.entries.find(x => x.kind === 'injection')
    expect(e.findings).toEqual([])
    expect(e.command).toBe('')
    expect(e.hostName).toBe('')
  })

  it('agent:error 结束运行态并落一条错误记录', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    store.running = true
    rt.emit('agent:error', { message: '连接超时' })
    expect(store.running).toBe(false)
    expect(store.entries.at(-1)).toMatchObject({ kind: 'error', content: '连接超时' })
  })

  it('agent:done 同时清掉运行态与待审批条', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    store.running = true
    store.pending = { id: 'a1' }
    rt.emit('agent:done', {})
    expect(store.running).toBe(false)
    expect(store.pending).toBe(null)
  })
})

// ---- 审计日志写入失败 ----

// 审计写入失败必须是**可见**的，不能只落在 stderr 里。
//
// 背景：审计日志是这套工具的合规底座。若它悄悄写失败，用户会以为有完整轨迹，
// 而实际上记录已经缺失。因此后端在 Append 失败时会发 audit:error，
// 前端必须同时做两件事：留下一条错误记录（进控制台流），
// 以及把 auditError 置位（驱动常驻警告条，不会被后续消息刷掉）。
describe('审计日志写入失败', () => {
  it('audit:error 既进日志流，也置位常驻警告', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('audit:error', { message: '审计日志写入失败，审计轨迹可能已不完整：磁盘已满' })

    expect(store.auditError).toContain('磁盘已满')
    const errs = store.entries.filter(e => e.kind === 'error')
    expect(errs).toHaveLength(1)
    expect(errs[0].content).toContain('审计轨迹可能已不完整')
  })

  it('警告是常驻的：后续消息不会把它冲掉', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('audit:error', { message: '审计日志写入失败' })
    rt.emit('agent:message', { role: 'assistant', content: '我继续处理' })
    expect(store.auditError).toContain('审计日志写入失败')
  })

  it('dismissAuditError 可以清除警告', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('audit:error', { message: '审计日志写入失败' })
    dismissAuditError()
    expect(store.auditError).toBe('')
  })

  it('payload 不是对象时也能降级成字符串（不抛异常）', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('audit:error', '纯字符串')
    expect(store.auditError).toBe('纯字符串')
  })
})

// ---- running 标志 ----

describe('running 标志的推导', () => {
  it('status=thinking 置为运行中', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('agent:status', { status: 'thinking' })
    expect(store.running).toBe(true)
  })

  it('工具执行期间不会被误判为「空闲」（否则用户能在半途再发一轮）', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('agent:status', { status: 'thinking' })
    // 工具开始执行：后端只发 tool 事件，不发新的 status
    rt.emit('agent:tool', { id: 'c1', name: 'run_command', status: 'running' })
    rt.emit('agent:approval', { id: 'c1', name: 'run_command' })
    expect(store.running).toBe(true)
  })
})

// ---- 审批生命周期 ----

describe('审批生命周期', () => {
  it('批准：调用后端、收起审批条、留下系统提示', async () => {
    const { app, calls } = makeApp({ Approve: async () => true })
    install({ runtime: rt, app })
    store.pending = { id: 'a1', name: 'run_command', command: 'systemctl restart nginx' }
    await approve(true)
    expect(calls.filter(c => c.name === 'Approve')).toHaveLength(1)
    expect(calls[0].args).toEqual(['a1', true])
    expect(store.pending).toBe(null)
    expect(store.entries.some(e => e.kind === 'system' && e.content === '已批准执行')).toBe(true)
  })

  it('拒绝：把 false 透传给后端', async () => {
    const { app, calls } = makeApp({ Approve: async () => true })
    install({ runtime: rt, app })
    store.pending = { id: 'a2', name: 'write_file' }
    await approve(false)
    expect(calls[0].args).toEqual(['a2', false])
    expect(store.entries.some(e => e.content === '已拒绝执行')).toBe(true)
  })

  it('没有待审批时点按钮是空操作，不打后端', async () => {
    const { app, calls } = makeApp()
    install({ runtime: rt, app })
    store.pending = null
    await approve(true)
    expect(calls.filter(c => c.name === 'Approve')).toHaveLength(0)
  })

  it('连点两次只提交一次，不会向后端重复投递', async () => {
    const { app, calls } = makeApp({ Approve: async () => true })
    install({ runtime: rt, app })
    store.pending = { id: 'a3', name: 'run_command' }
    await Promise.all([approve(true), approve(true)])
    expect(calls.filter(c => c.name === 'Approve')).toHaveLength(1)
  })

  // 回归测试：后端 Resolve() 对已过期/已取消的审批 id 返回 false（不抛错）。
  // 早期实现忽略了这个返回值，于是界面会收起审批条并显示「已批准执行」，
  // 而实际上命令根本没有执行 —— UI 在骗用户。
  it('审批已失效时（后端返回 false）必须报错，绝不能谎报成功', async () => {
    const { app, calls } = makeApp({ Approve: async () => false })
    install({ runtime: rt, app })
    store.pending = { id: 'stale', name: 'run_command' }
    await approve(true)

    expect(calls.filter(c => c.name === 'Approve')).toHaveLength(1)
    expect(store.pending).toBe(null)
    expect(store.entries.some(e => e.content === '已批准执行')).toBe(false)
    const err = store.entries.find(e => e.kind === 'error')
    expect(err, '失效审批必须给用户可见的错误').toBeTruthy()
    expect(err.content).toMatch(/失效|超时|中断/)
  })

  it('后端调用抛异常时给出错误提示而不是静默吞掉', async () => {
    const { app } = makeApp({
      Approve: async () => { throw new Error('bridge down') }
    })
    install({ runtime: rt, app })
    store.pending = { id: 'a4', name: 'run_command' }
    await approve(true)
    expect(store.entries.some(e => e.content === '已批准执行')).toBe(false)
    expect(store.entries.find(e => e.kind === 'error').content).toContain('bridge down')
  })
})

// ---- 提问 ----

describe('ask 发送一轮对话', () => {
  it('没选主机时直接提示，不打后端', async () => {
    const { app, calls } = makeApp()
    install({ runtime: rt, app })
    store.currentHostId = ''
    await ask('看看磁盘')
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(0)
    expect(store.entries.at(-1)).toMatchObject({ kind: 'error' })
  })

  it('正常发送：置运行态、带上主机 id、会话 id 与原文', async () => {
    const { app, calls } = makeApp({ Ask: async () => undefined })
    install({ runtime: rt, app })
    store.currentHostId = 'h1'
    // 特意选中一条具名会话：若 ask 漏传会话 id，后端会把它落到默认会话上，
    // 用户的提问就会进到另一条对话里 —— 而界面上看不出任何异常。
    store.currentSessionId = 's9'
    await ask('看看磁盘')
    expect(calls[0].args).toEqual(['h1', 's9', '看看磁盘'])
    expect(store.running).toBe(true)
  })

  it('后端同步拒绝（例如已有会话在跑）时复位运行态', async () => {
    const { app } = makeApp({
      Ask: async () => { throw new Error('已有会话正在运行') }
    })
    install({ runtime: rt, app })
    store.currentHostId = 'h1'
    await ask('再来一轮')
    expect(store.running).toBe(false)
    expect(store.entries.at(-1).content).toContain('已有会话正在运行')
  })

  it('连续两次发送只放行一次（防连点）', async () => {
    const { app, calls } = makeApp({ Ask: async () => undefined })
    install({ runtime: rt, app })
    store.currentHostId = 'h1'
    await Promise.all([ask('一'), ask('二')])
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(1)
  })
})

// ---- 中断 ----

describe('stop 中断', () => {
  it('调用后端并清空运行态与审批条', async () => {
    const { app, calls } = makeApp()
    install({ runtime: rt, app })
    store.running = true
    store.pending = { id: 'a9' }
    await stop()
    expect(calls.filter(c => c.name === 'Stop')).toHaveLength(1)
    expect(store.running).toBe(false)
    expect(store.pending).toBe(null)
  })

  // 中断失败也必须回到可交互状态，否则用户会被永久锁在「运行中」，
  // 既发不出新指令也点不了中断。
  it('后端中断失败时仍复位本地状态并提示', async () => {
    const { app } = makeApp({ Stop: async () => { throw new Error('bridge down') } })
    install({ runtime: rt, app })
    store.running = true
    store.pending = { id: 'a9' }
    await stop()
    expect(store.running).toBe(false)
    expect(store.pending).toBe(null)
    expect(store.entries.at(-1).content).toContain('bridge down')
  })
})

// ---- 流式输出 ----

describe('流式输出（agent:delta）', () => {
  it('增量逐段累加到同一条消息上', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('agent:delta', { step: 0, text: 'nginx ' })
    rt.emit('agent:delta', { step: 0, text: '起不来' })
    rt.emit('agent:delta', { step: 0, text: '，我看一下' })

    const msgs = store.entries.filter(e => e.kind === 'assistant')
    expect(msgs, '增量必须落在同一条消息上，否则界面会出现一堆碎片').toHaveLength(1)
    expect(msgs[0].content).toBe('nginx 起不来，我看一下')
    expect(msgs[0].streaming).toBe(true)
  })

  it('定稿消息原地替换内容并结束流式标记', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('agent:delta', { step: 0, text: '部分' })
    rt.emit('agent:message', { role: 'assistant', content: '完整回答', step: 0 })

    const msgs = store.entries.filter(e => e.kind === 'assistant')
    expect(msgs).toHaveLength(1)
    expect(msgs[0].content).toBe('完整回答')
    expect(msgs[0].streaming).toBe(false)
  })

  it('不同 step 的流各自成条，不会互相污染', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('agent:delta', { step: 0, text: '第一轮' })
    rt.emit('agent:message', { role: 'assistant', content: '第一轮', step: 0 })
    rt.emit('agent:delta', { step: 1, text: '第二轮' })

    const msgs = store.entries.filter(e => e.kind === 'assistant')
    expect(msgs).toHaveLength(2)
    expect(msgs[0].content).toBe('第一轮')
    expect(msgs[1].content).toBe('第二轮')
  })

  it('没有 step 的定稿消息不会误改已有消息', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    // 先来一条普通（非流式）assistant 消息
    rt.emit('agent:message', { role: 'assistant', content: '普通消息' })
    // 再来一条不带 step 的定稿 —— 不能把上面那条覆盖掉
    rt.emit('agent:message', { role: 'assistant', content: '另一条' })

    const msgs = store.entries.filter(e => e.kind === 'assistant')
    expect(msgs).toHaveLength(2)
    expect(msgs[0].content).toBe('普通消息')
    expect(msgs[1].content).toBe('另一条')
  })

  it('中断时冻结流式标记，不留永远闪烁的光标', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('agent:delta', { step: 0, text: '说到一半' })
    rt.emit('agent:done', {})

    expect(store.entries.find(e => e.kind === 'assistant').streaming).toBe(false)
  })

  it('出错时同样冻结流式标记', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('agent:delta', { step: 0, text: '说到一半' })
    rt.emit('agent:error', { message: '连接断了' })

    expect(store.entries.find(e => e.kind === 'assistant').streaming).toBe(false)
  })

  it('stop() 也会冻结流式标记', async () => {
    const { app } = makeApp()
    install({ runtime: rt, app })
    bindEvents()
    rt.emit('agent:delta', { step: 0, text: '说到一半' })
    await stop()
    expect(store.entries.find(e => e.kind === 'assistant').streaming).toBe(false)
  })
})

// ---- 日志缓冲 ----

describe('日志缓冲', () => {
  it('每条记录都拿到唯一 id 与时间戳', () => {
    push({ kind: 'system', content: 'a' })
    push({ kind: 'system', content: 'b' })
    expect(store.entries[0].id).not.toBe(store.entries[1].id)
    expect(store.entries[0].ts).toBeGreaterThan(0)
  })

  it('超过 500 条时丢弃最旧的，内存不无限增长', () => {
    for (let i = 0; i < 620; i++) push({ kind: 'system', content: `n${i}` })
    expect(store.entries).toHaveLength(500)
    expect(store.entries.at(-1).content).toBe('n619')
    expect(store.entries[0].content).toBe('n120')
  })

  it('clearLog 同时清空记录与待审批条', () => {
    push({ kind: 'system', content: 'x' })
    store.pending = { id: 'a1' }
    clearLog()
    expect(store.entries).toEqual([])
    expect(store.pending).toBe(null)
  })
})

// ---- 初始化与主机刷新 ----

describe('bootstrap 初始化', () => {
  it('成功后填充姿态/主机/LLM/策略并选中第一台主机', async () => {
    const { app } = makeApp({
      Bootstrap: async () => ({
        error: '',
        posture: { keyProtection: 'keyring', configDir: 'C:/cfg', hostCount: 1, degraded: false },
        hosts: [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }],
        llm: { baseUrl: 'https://x/v1', model: 'm', hasApiKey: true },
        policy: { mode: 'whitelist', whitelist: ['ls'], redactOutput: false, maxOutput: 1024, maxSteps: 3 }
      })
    })
    install({ runtime: rt, app })
    await bootstrap()
    expect(store.ready).toBe(true)
    expect(store.currentHostId).toBe('h1')
    expect(store.policy.mode).toBe('whitelist')
  })

  it('后端带 error 时置错误并保持未就绪（UI 显示初始化失败）', async () => {
    const { app } = makeApp({
      Bootstrap: async () => ({ error: '钥匙串不可用', posture: {}, hosts: [], llm: {}, policy: {} })
    })
    install({ runtime: rt, app })
    await bootstrap()
    expect(store.error).toBe('钥匙串不可用')
    expect(store.ready).toBe(false)
  })

  it('调用抛异常时也进入错误态而不是白屏', async () => {
    const { app } = makeApp({ Bootstrap: async () => { throw new Error('no bridge') } })
    install({ runtime: rt, app })
    await bootstrap()
    expect(store.error).toContain('no bridge')
    expect(store.ready).toBe(false)
  })

  it('已选中主机不会被覆盖', async () => {
    const { app } = makeApp({
      Bootstrap: async () => ({
        error: '',
        posture: {},
        hosts: [{ id: 'h1' }, { id: 'h2' }],
        llm: {}, policy: {}
      })
    })
    install({ runtime: rt, app })
    store.currentHostId = 'h2'
    await bootstrap()
    expect(store.currentHostId).toBe('h2')
  })
})

describe('refreshHosts 刷新主机', () => {
  it('列表变化后修正失效的选中项，并同步姿态里的主机数', async () => {
    const { app } = makeApp({ ListHosts: async () => [{ id: 'h2' }] })
    install({ runtime: rt, app })
    store.currentHostId = 'h1'
    store.hosts = [{ id: 'h1' }]
    await refreshHosts()
    expect(store.currentHostId).toBe('h2')
    expect(store.posture.hostCount).toBe(1)
  })

  it('删光主机后选中项归零（不能留一个指向空气的 id）', async () => {
    const { app } = makeApp({ ListHosts: async () => [] })
    install({ runtime: rt, app })
    store.currentHostId = 'h1'
    await refreshHosts()
    expect(store.currentHostId).toBe('')
    expect(store.posture.hostCount).toBe(0)
  })
})

// ---- 会话记录按主机分组 ----
//
// 后端是按主机存会话上下文的，界面上的对话记录必须同粒度。
// 否则用户在 A 机聊完切到 B 机，屏幕上还是 A 机的对话，
// 他会以为模型也还记着 A 机那些话 —— 而 B 机的会话其实是空的。

describe('对话记录按主机分组', () => {
  it('切换主机后看到的是那台主机自己的记录', () => {
    store.currentHostId = 'h1'
    push({ kind: 'user', content: 'A机提问' })
    store.currentHostId = 'h2'

    expect(store.entries, 'B 机不该显示 A 机的对话').toEqual([])

    push({ kind: 'user', content: 'B机提问' })
    store.currentHostId = 'h1'
    expect(store.entries.map(e => e.content)).toEqual(['A机提问'])
  })

  it('切回来时原来那台的记录还在', () => {
    store.currentHostId = 'h1'
    push({ kind: 'user', content: '第一问' })
    push({ kind: 'assistant', content: '第一答' })
    store.currentHostId = 'h2'
    push({ kind: 'user', content: '别的机器' })
    store.currentHostId = 'h1'

    expect(store.entries.map(e => e.content)).toEqual(['第一问', '第一答'])
  })

  // 后端事件里不带主机信息，所以「这一轮属于哪台主机」必须在 ask() 时记下来。
  // 用 currentHostId 归档的话，跑到一半切换主机会把 A 机的输出写进 B 机的时间线。
  it('一轮跑到一半切换主机，后续事件仍归档到原来那台', async () => {
    const { app } = makeApp()
    install({ runtime: rt, app })
    bindEvents()

    store.currentHostId = 'h1'
    await ask('看看磁盘')          // 记下 activeHostId = h1
    store.currentHostId = 'h2'     // 用户中途切走

    rt.emit('agent:message', { role: 'user', content: '看看磁盘' })
    rt.emit('agent:delta', { step: 0, text: '磁盘' })
    rt.emit('agent:message', { role: 'assistant', content: '磁盘充足', step: 0 })

    expect(store.bySession[at('h1')].length, '应落在 h1 的时间线上').toBe(2)
    expect(store.bySession[at('h2')], 'h2 不该凭空多出记录').toBeUndefined()
  })

  it('流式定稿能找到切换主机前的那条「活」消息', async () => {
    const { app } = makeApp()
    install({ runtime: rt, app })
    bindEvents()

    store.currentHostId = 'h1'
    await ask('看看磁盘')
    store.currentHostId = 'h2'

    rt.emit('agent:delta', { step: 0, text: '说到一半' })
    rt.emit('agent:message', { role: 'assistant', content: '完整回答', step: 0 })

    const msgs = store.bySession[at('h1')].filter(e => e.kind === 'assistant')
    expect(msgs, '定稿必须落在原来那条消息上，而不是新开一条').toHaveLength(1)
    expect(msgs[0].content).toBe('完整回答')
    expect(msgs[0].streaming).toBe(false)
  })

  it('clearLog 只清当前主机，别的主机不受影响', () => {
    store.currentHostId = 'h1'
    push({ kind: 'user', content: 'A' })
    store.currentHostId = 'h2'
    push({ kind: 'user', content: 'B' })

    clearLog()
    expect(store.bySession[at('h2')]).toEqual([])
    expect(store.bySession[at('h1')].map(e => e.content)).toEqual(['A'])
  })

  it('没有选中主机时的提示落在「无主机」这个桶里，仍然可见', () => {
    store.currentHostId = ''
    push({ kind: 'error', content: '请先添加一台主机' })
    expect(store.entries.map(e => e.content)).toEqual(['请先添加一台主机'])
  })

  it('每条记录的上限按时间线各自计算（A 机刷屏不会挤掉 B 机的记录）', () => {
    store.currentHostId = 'h1'
    for (let i = 0; i < 620; i++) push({ kind: 'system', content: `n${i}` })
    expect(store.entries).toHaveLength(500)

    store.currentHostId = 'h2'
    push({ kind: 'system', content: 'B机第一条' })
    expect(store.entries).toHaveLength(1)
  })

  // 删除主机后必须清掉它的会话记录。否则反复增删同一台机器，
  // bySession 里会留下一堆指向「已不存在主机」的孤儿条目。
  it('refreshHosts 会清掉被删除主机的会话记录', async () => {
    const { app } = makeApp({ ListHosts: async () => [{ id: 'h2', name: 'b' }] })
    install({ runtime: rt, app })

    store.currentHostId = 'h1'
    push({ kind: 'user', content: 'A机记录' })
    push({ kind: 'user', content: 'A机记录二' })
    // 顺便摆一条「没有选中主机」时的兜底记录，确认那条不被删。
    store.currentHostId = ''
    push({ kind: 'error', content: '请先添加主机' })
    store.currentHostId = 'h1'
    expect(store.bySession[at('h1')]).toHaveLength(2)

    await refreshHosts()

    expect(store.bySession[at('h1')], 'h1 已不在主机列表，应被清掉').toBeUndefined()
    expect(store.currentHostId).toBe('h2')
    // 「没有选中主机」的兜底桶（key 为空串）必须保留 —— 不是真主机。
    expect(store.bySession[at('')], '空串桶不是某台主机的桶，不该被剪').toBeDefined()
  })

  it('refreshHosts 把 activeHostId 也归位到当前主机', async () => {
    const { app } = makeApp({ ListHosts: async () => [{ id: 'h2' }] })
    install({ runtime: rt, app })
    store.activeHostId = 'h-gone'
    await refreshHosts()
    expect(store.activeHostId).toBe(store.currentHostId)
  })
})

// ---- 清空 LLM 会话上下文 ----

describe('clearSession 清空会话上下文', () => {
  it('把主机 id 与会话 id 都传给后端', async () => {
    const { app, calls } = makeApp({ ClearSession: async () => ({ cleared: 2, busy: false }) })
    install({ runtime: rt, app })
    store.currentHostId = 'h1'
    await clearSession('h1', '')
    expect(calls.filter(c => c.name === 'ClearSession')).toHaveLength(1)
    expect(calls[0].args).toEqual(['h1', ''])
  })

  // 「清掉了 3 轮」而不是一句「已清空」：轮数能让用户确认点的就是当前这台主机。
  it('清掉若干轮时报告轮数', async () => {
    const { app } = makeApp({ ClearSession: async () => ({ cleared: 3, busy: false }) })
    install({ runtime: rt, app })
    store.currentHostId = 'h1'
    await clearSession('h1', '')
    expect(store.entries.at(-1).content).toContain('3 轮')
  })

  // 必须说明「清的是模型的记忆，不是屏幕上的文字」——
  // 否则用户看到对话还在，会以为按钮没生效。
  it('提示里说清屏幕上的记录还在（两者不是一回事）', async () => {
    const { app } = makeApp({ ClearSession: async () => ({ cleared: 1, busy: false }) })
    install({ runtime: rt, app })
    store.currentHostId = 'h1'
    await clearSession('h1', '')
    const text = store.entries.at(-1).content
    expect(text).toContain('LLM 不再记得')
    expect(text).toContain('保留')
  })

  it('本来就没有上下文时明确说出「没有」，而不是沉默', async () => {
    const { app } = makeApp({ ClearSession: async () => ({ cleared: 0, busy: false }) })
    install({ runtime: rt, app })
    store.currentHostId = 'h1'
    await clearSession('h1', '')
    expect(store.entries.at(-1).content).toContain('本来就没有')
  })

  it('有会话在跑时给出可见的原因，不静默失败', async () => {
    const { app } = makeApp({ ClearSession: async () => ({ cleared: 0, busy: true }) })
    install({ runtime: rt, app })
    store.currentHostId = 'h1'
    await clearSession('h1', '')
    const last = store.entries.at(-1)
    expect(last.kind).toBe('error')
    expect(last.content).toContain('正在运行')
  })

  it('后端抛异常时给出错误提示', async () => {
    const { app } = makeApp({ ClearSession: async () => { throw new Error('bridge down') } })
    install({ runtime: rt, app })
    store.currentHostId = 'h1'
    await clearSession('h1', '')
    expect(store.entries.at(-1).kind).toBe('error')
    expect(store.entries.at(-1).content).toContain('bridge down')
  })

  // 提示要落在被清空的那条时间线上。走 activeHostId 的话，
  // 上一次跑的主机可能不是当前这台，提示就会出现在看不见的地方。
  it('提示落在被清空的那条时间线上', async () => {
    const { app } = makeApp({ ClearSession: async () => ({ cleared: 1, busy: false }) })
    install({ runtime: rt, app })
    store.activeHostId = 'h-old'
    store.currentHostId = 'h2'
    await clearSession('h2', '')
    expect(store.bySession[at('h2')].at(-1).content).toContain('已清空')
    expect(store.bySession[at('h-old')]).toBeUndefined()
  })
})

// ---- 终端屏幕快照 ----

// top/vim 这类 TUI 退出时，后端把定格的最后一帧净化成文本发上来（final=true），
// 它既进归档时间线给人看，也进模型上下文 —— 人和模型看到同一份，审计才对得上。
// 过程快照（final=false）是中途定格的画面，两者是同一种条目、不同标记。
describe('终端屏幕快照', () => {
  it('agent:snapshot 透传 final：定格帧与过程快照是两种时间线条目', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    rt.emit('agent:snapshot', {
      hostId: 'h1', sessionId: 's1', termId: 'h1#a1',
      text: '> load: 9.9', redacted: 0, injection: [], final: true
    })
    rt.emit('agent:snapshot', {
      hostId: 'h1', sessionId: 's1', termId: 'h1#a1',
      text: '> load: 0.1', redacted: 0, injection: [], final: false
    })
    const list = store.bySession[at('h1', 's1')] || []
    expect(list.map(e => e.final)).toEqual([true, false])
  })

  it('快照落到后端指定的 (主机, 会话)，不串到当前选中的会话', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    store.currentHostId = 'h2'
    rt.emit('agent:snapshot', { hostId: 'h1', sessionId: 's1', termId: 'h1#a1', text: '> load: 9.9' })
    expect(store.bySession[at('h1', 's1')] || []).toHaveLength(1)
    expect(store.bySession[at('h2', '')] || []).toHaveLength(0)
  })

  it('表面有落点时补一行「已进归档与模型上下文」的注记，但不重画那一帧', () => {
    install({ runtime: rt, app: makeApp().app })
    bindEvents()
    const sink = captureSink('h1', 's1')
    try {
      rt.emit('agent:snapshot', { hostId: 'h1', sessionId: 's1', termId: 'h1#a1', text: '> load: 9.9', final: true })
      // 注记说明这一屏的去向；帧内容本身不重画（它已经在 scrollback 里）。
      expect(sink.painted()).toContain('终端结束画面已进归档与模型上下文')
      expect(sink.painted()).not.toContain('> load: 9.9')
    } finally {
      unregisterTermSink('h1', 's1', sink.fn)
    }
  })
})

// ---- agent 事件双写到终端表面 ----

// 单表面重构后，agent 的回复/工具/审批/错误既进 bySession（归档 + 模型视图），
// 也由 termPaint 画到那台主机的常驻终端表面上 —— 人和模型看的是同一条流。
// 这组测试钉住「双写」：每条事件既要落到正确的时间线，也要写进表面落点。
describe('agent 事件双写到终端表面', () => {
  let sink = null

  // 把落点挂到 h1，并让 h1 成为当前主机：paintHost() = activeHostId || currentHostId，
  // activeHostId 空时事件就画到 h1、记录也落进 at('h1','')，两边对齐。
  function attach(hostId = 'h1') {
    sink = captureSink(hostId)
    store.currentHostId = hostId
    return sink
  }

  beforeEach(() => {
    // WriteTerminal 必须在：TTY 交接分支会调它，缺了会推一条 error 污染断言。
    const { app } = makeApp({ WriteTerminal: async () => undefined })
    install({ runtime: rt, app })
    bindEvents()
  })

  afterEach(() => {
    if (sink) unregisterTermSink(sink.hostId, sink.sessionId, sink.fn)
    sink = null
  })

  it('assistant 回复既进归档时间线，也画到终端表面', () => {
    attach('h1')
    rt.emit('agent:message', { role: 'assistant', content: '磁盘充足' })

    const msgs = store.entries.filter(e => e.kind === 'assistant')
    expect(msgs).toHaveLength(1)
    expect(msgs[0].content).toBe('磁盘充足')
    // 表面上：符号抬头● + 正文
    expect(sink.painted()).toContain('●')
    expect(sink.painted()).toContain('磁盘充足')
  })

  it('用户那句话也画到表面（带「❯」符号）', () => {
    attach('h1')
    rt.emit('agent:message', { role: 'user', content: 'nginx 挂了' })

    expect(store.entries.map(e => e.kind)).toEqual(['user'])
    expect(sink.painted()).toContain('❯')
    expect(sink.painted()).toContain('nginx 挂了')
  })

  it('工具块画 `$ 命令`，同时 upsert 一条 tool 记录', () => {
    attach('h1')
    rt.emit('agent:tool', { id: 'c1', name: 'run_command', command: 'ls -l', status: 'running', decision: 'allow' })

    expect(store.entries.filter(e => e.kind === 'tool')).toHaveLength(1)
    expect(sink.painted()).toContain('$ ls -l')
  })

  it('工具结果画退出码与截断输出', () => {
    attach('h1')
    rt.emit('agent:toolResult', { id: 'c1', exitCode: 2, content: 'ls: /nope: No such file', hostId: 'h1' })

    expect(store.entries.find(e => e.kind === 'result').exitCode).toBe(2)
    expect(sink.painted()).toContain('退出码 2')
    expect(sink.painted()).toContain('No such file')
  })

  it('审批与错误都画到表面：审批注记 + 错误行', () => {
    attach('h1')
    rt.emit('agent:approval', { id: 'a1', name: 'run_command', command: 'rm -rf /' })
    expect(store.pending).toBeTruthy()
    expect(sink.painted()).toContain('需要你的批准')
    expect(sink.painted()).toContain('rm -rf /')

    rt.emit('agent:error', { message: '连接超时' })
    expect(sink.painted()).toContain('错误 ›')
    expect(sink.painted()).toContain('连接超时')
  })

  it('TTY 结果画「已交接到这片终端表面」的注记，并把命令写进 PTY', () => {
    attach('h1')
    rt.emit('agent:toolResult', {
      id: 'c1', exitCode: 1, content: 'TERM environment variable not set.',
      tty: true, command: 'top', hostId: 'h1'
    })
    expect(sink.painted()).toContain('已交接到这片终端表面执行')
  })

  it('没有表面落点时事件仍进归档，且不抛异常', () => {
    // 不 attach：termSinks 里没有 h1，surfaceWrite 返回 undefined，termPaint 全 no-op。
    store.currentHostId = 'h1'
    expect(() => rt.emit('agent:message', { role: 'assistant', content: '看不见的回复' })).not.toThrow()
    expect(() => rt.emit('agent:tool', { id: 'c1', name: 'run_command', command: 'ls' })).not.toThrow()
    expect(store.entries.filter(e => e.kind === 'assistant')).toHaveLength(1)
  })

  it('反登记落点后，事件不再画到那片表面（但仍进归档）', () => {
    attach('h1')
    unregisterTermSink('h1', sink.sessionId, sink.fn)
    rt.emit('agent:message', { role: 'assistant', content: '反登记之后的回复' })
    expect(sink.painted()).not.toContain('反登记之后的回复')
    expect(store.entries.filter(e => e.kind === 'assistant')).toHaveLength(1)
  })
})

// ---- 报告活动会话 ----

// 常驻终端定格的快照不带会话 ID，后端 per-host 记住这里报的值，用它把快照路由到
// 正确的时间线。用户切会话后产生的快照，就该落进新的那条。
describe('reportActiveSession 报告活动会话', () => {
  it('把主机与会话原样报给后端', async () => {
    const { app, calls } = makeApp({ ReportActiveSession: async () => undefined })
    install({ runtime: rt, app })
    await reportActiveSession('h1', 's1')
    expect(calls.filter(c => c.name === 'ReportActiveSession')).toHaveLength(1)
    expect(calls[0].args).toEqual(['h1', 's1'])
  })

  it('会话 id 缺省时归一化成空串（空串=默认会话，与后端一致）', async () => {
    const { app, calls } = makeApp({ ReportActiveSession: async () => undefined })
    install({ runtime: rt, app })
    await reportActiveSession('h1')
    expect(calls[0].args).toEqual(['h1', ''])
  })

  it('没有主机 id 时不打后端（还没选主机就没有活动会话可报）', async () => {
    const { app, calls } = makeApp({ ReportActiveSession: async () => undefined })
    install({ runtime: rt, app })
    await reportActiveSession('', 's1')
    expect(calls.filter(c => c.name === 'ReportActiveSession')).toHaveLength(0)
  })

  it('后端抛异常时静默 resolves（报不上去不该打断界面）', async () => {
    const { app } = makeApp({ ReportActiveSession: async () => { throw new Error('bridge down') } })
    install({ runtime: rt, app })
    await expect(reportActiveSession('h1', 's1')).resolves.toBeUndefined()
  })

  it('selectSession 切会话时顺带把新会话报给后端', () => {
    const { app, calls } = makeApp({ ReportActiveSession: async () => undefined })
    install({ runtime: rt, app })
    store.currentHostId = 'h1'
    selectSession('s2')
    expect(store.currentSessionId).toBe('s2')
    // selectSession 不 await reportActiveSession，但调用是同步发出的。
    expect(calls.filter(c => c.name === 'ReportActiveSession')).toHaveLength(1)
    expect(calls[0].args).toEqual(['h1', 's2'])
  })
})

// ---- 常驻终端的 shell 通道 ----

// composer 里人敲的 shell 行走这条通道：后端策略闸门裁决（高危弹审批）后把这一行
// 写进常驻 PTY。放行时 PTY 自己回显命令与输出，前端只补一句「可问 LLM」的提示；
// 被拒/取消/出错才在表面上说明原因。
describe('runShellInTerminal 常驻终端 shell 通道', () => {
  it('放行：把命令交给后端写进 PTY，返回结果并复位 busy', async () => {
    const { app, calls } = makeApp({ RunShellInTerminal: async () => ({ status: 'done' }) })
    install({ runtime: rt, app })
    store.currentHostId = 'h1'
    store.busy = true
    const res = await runShellInTerminal('h1', 's1', 'ls -la')
    expect(calls.filter(c => c.name === 'RunShellInTerminal')).toHaveLength(1)
    expect(calls[0].args).toEqual(['h1', 's1', 'ls -la'])
    expect(res.status).toBe('done')
    expect(store.busy).toBe(false)
  })

  it('放行后画一句「可问 LLM」提示（这是 done 路径唯一的额外输出）', async () => {
    const { app } = makeApp({ RunShellInTerminal: async () => ({ status: 'done' }) })
    install({ runtime: rt, app })
    const sink = captureSink('h1', 's1')
    store.currentHostId = 'h1'
    try {
      await runShellInTerminal('h1', 's1', 'ls -la')
      expect(sink.painted()).toContain('想分析这段输出')
      expect(sink.painted()).toContain('直接用中文问 LLM')
    } finally {
      unregisterTermSink('h1', 's1', sink.fn)
    }
  })

  it('被策略拒绝：在表面上说明原因，并清掉可能挂着的过期审批条', async () => {
    const { app } = makeApp({ RunShellInTerminal: async () => ({ status: 'denied', reason: 'rm -rf 属于高危命令' }) })
    install({ runtime: rt, app })
    const sink = captureSink('h1', 's1')
    store.currentHostId = 'h1'
    store.pending = { id: 'a1' }
    try {
      const res = await runShellInTerminal('h1', 's1', 'rm -rf /')
      expect(res.status).toBe('denied')
      expect(sink.painted()).toContain('命令被策略拒绝')
      expect(sink.painted()).toContain('rm -rf 属于高危命令')
      // 没执行过的命令不该出现「分析输出」提示
      expect(sink.painted()).not.toContain('想分析这段输出')
      expect(store.pending).toBe(null)
      expect(store.busy).toBe(false)
    } finally {
      unregisterTermSink('h1', 's1', sink.fn)
    }
  })

  it('审批超时/被中断（cancelled）也在表面上说明', async () => {
    const { app } = makeApp({ RunShellInTerminal: async () => ({ status: 'cancelled' }) })
    install({ runtime: rt, app })
    const sink = captureSink('h1', 's1')
    store.currentHostId = 'h1'
    try {
      const res = await runShellInTerminal('h1', 's1', 'systemctl restart nginx')
      expect(res.status).toBe('cancelled')
      expect(sink.painted()).toContain('审批超时或被中断')
    } finally {
      unregisterTermSink('h1', 's1', sink.fn)
    }
  })

  it('后端抛异常：落一条错误记录、画到表面并复位 busy', async () => {
    const { app } = makeApp({ RunShellInTerminal: async () => { throw new Error('bridge down') } })
    install({ runtime: rt, app })
    const sink = captureSink('h1', 's1')
    store.currentHostId = 'h1'
    store.busy = true
    try {
      const res = await runShellInTerminal('h1', 's1', 'ls')
      expect(res.status).toBe('error')
      expect(res.error).toContain('bridge down')
      expect(store.busy).toBe(false)
      // 错误既进归档时间线，也画到表面
      expect(store.entries.find(e => e.kind === 'error').content).toContain('bridge down')
      expect(sink.painted()).toContain('bridge down')
    } finally {
      unregisterTermSink('h1', 's1', sink.fn)
    }
  })
})
