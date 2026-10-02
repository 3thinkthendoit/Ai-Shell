// ConsolePanel 组件测试。
//
// store.js 的测试锁住了状态机，terminal.test.js 锁住了 xterm 的接线，
// 这里锁住「状态 -> 界面」这一段：单表面控制台的三件事——
//   1. agent 事件被画进那片终端表面（回复/工具块/审批注记/交接注记/快照注记）
//   2. 终端表面内直接输入按 classifyInput 分流（见 terminal.test.js），composer-bar 只剩工具条与运行指示
//   3.「对话归档」是只读时间线，审批条/弹窗照旧可交互
// 这一段没有测试的话，状态对了但界面没反应同样是个 bug。
import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { nextTick } from 'vue'
import ConsolePanel from '../components/ConsolePanel.vue'
import { store, push, textToBase64, bindEvents } from '../store.js'
import { instances, resetInstances } from './stubs/xterm.js'

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
    // 终端内直接输入的 shell 行走常驻终端通道（策略闸门 + 写进 PTY），
    // 默认放行；ReportActiveSession 报当前会话，挂载/切会话时都会调。
    RunShellInTerminal: async () => ({ status: 'done', decision: 'allow', reason: '', rule: 'auto_safe', risk: 'low' }),
    ReportActiveSession: async () => undefined,
    // 会话列表：默认只有一条默认会话，与后端的不变式一致。
    // 不给这个实现的话，切主机触发的 refreshSessions 会抛异常、
    // 在对话流里插一条红色报错，把别的断言全带偏。
    ListSessions: async () => [
      { id: 'default', name: '', turns: 0, archivedTurns: 0, isDefault: true }
    ],
    CreateSession: async () => ({ id: 's1', name: '新会话', turns: 0, archivedTurns: 0, isDefault: false }),
    RenameSession: async () => undefined,
    DeleteSession: async () => undefined,
    // 常驻终端四件套：挂载即附着会真的去开 PTY，
    // 不给实现的话 openTerminal 会掉进 error 分支，把断言带偏。
    OpenTerminal: async () => undefined,
    WriteTerminal: async () => undefined,
    ResizeTerminal: async () => undefined,
    CloseTerminal: async () => undefined
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

function withHost() {
  store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
  store.currentHostId = 'h1'
  // 与真实流程对齐：附着时已有默认会话。表面按 (host, session) 键，
  // 快照注记才能路由到同一块已附着的表面上。
  store.currentSessionId = 'default'
}

// 挂载即附着常驻终端：flushPromises 让 ensureTerm → OpenTerminal 这条链跑完，
// 之后 instances[0] 就是那台主机的表面，sink 也登记好了。
async function setupAttached(impl) {
  withHost()
  setup(impl)
  await flushPromises()
  await nextTick()
}

// 表面至今收到的全部文本，供「画进了什么」的断言用。
// 去掉 ANSI SGR 转义只留可见字符：termPaint 会在「$ 」与命令之间夹一个 RESET，
// 不去掉的话 toContain('$ cmd') 这种按人眼所见的断言会被转义序列割裂而假失败。
const stripAnsi = s => s.replace(/\x1b\[[0-9;]*m/g, '')
const surfaceText = () => (instances[0] ? stripAnsi(instances[0].text()) : '')

// 切到「对话归档」只读时间线 / 切回活表面。
async function toArchive() {
  await wrapper.findAll('.seg button')[1].trigger('click')
  await nextTick()
}
async function toSurface() {
  await wrapper.findAll('.seg button')[0].trigger('click')
  await nextTick()
}

beforeEach(() => {
  resetStore()
  resetInstances()
  rt = makeRuntime()
  delete window.go
})

afterEach(() => {
  if (wrapper) wrapper.unmount()
  wrapper = null
  delete window.go
  delete window.runtime
})

describe('ConsolePanel 空状态与主机选择', () => {
  it('没有主机时提示先去主机管理添加', () => {
    setup()
    expect(wrapper.text()).toContain('暂无主机')
  })

  it('已有主机时列出「名称 — user@addr」', () => {
    withHost()
    setup()
    expect(wrapper.text()).toContain('web')
    expect(wrapper.text()).toContain('root@10.0.0.1')
  })

  it('分段按钮是「终端 / 对话归档」，能力横幅标明高危确认与 LLM', () => {
    setup()
    const segs = wrapper.findAll('.seg button')
    expect(segs[0].text()).toBe('终端')
    expect(segs[1].text()).toBe('对话归档')
    expect(wrapper.find('.sess-banner').text()).toMatch(/高危/)
    expect(wrapper.find('.sess-banner').text()).toMatch(/LLM/)
  })
})

describe('ConsolePanel 审批条', () => {
  it('无待审批时不渲染审批条', () => {
    setup()
    expect(wrapper.find('.approval').exists()).toBe(false)
  })

  it('有待审批时渲染命令原文与理由', async () => {
    setup()
    store.pending = {
      id: 'a1',
      name: 'run_command',
      command: 'systemctl restart nginx',
      reason: '需要确认的重启操作',
      hostName: 'web'
    }
    await nextTick()
    const bar = wrapper.find('.approval')
    expect(bar.exists()).toBe(true)
    expect(bar.text()).toContain('systemctl restart nginx')
    expect(bar.text()).toContain('需要确认的重启操作')
    expect(bar.text()).toContain('@web')
  })

  it('write_file 这类没有 command 的调用退化为展示 args', async () => {
    setup()
    store.pending = { id: 'a2', name: 'write_file', args: '{"path":"/etc/x"}' }
    await nextTick()
    expect(wrapper.find('.approval-cmd').text()).toContain('/etc/x')
  })

  it('点「批准执行」把 (id, true) 发给后端并收起审批条', async () => {
    setup()
    store.pending = { id: 'a3', name: 'run_command', command: 'ls' }
    await nextTick()
    const buttons = wrapper.findAll('.approval .row button')
    await buttons[0].trigger('click')
    await nextTick()
    const approves = calls.filter(c => c.name === 'Approve')
    expect(approves).toHaveLength(1)
    expect(approves[0].args).toEqual(['a3', true])
    expect(wrapper.find('.approval').exists()).toBe(false)
  })

  it('点「拒绝」把 (id, false) 发给后端', async () => {
    setup()
    store.pending = { id: 'a4', name: 'run_command', command: 'rm -rf /tmp/x' }
    await nextTick()
    const buttons = wrapper.findAll('.approval .row button')
    await buttons[1].trigger('click')
    await nextTick()
    expect(calls.filter(c => c.name === 'Approve')[0].args).toEqual(['a4', false])
  })

  it('审批失效时审批条收起并显示错误，不显示「已批准执行」', async () => {
    setup({ Approve: async () => false })
    store.pending = { id: 'stale', name: 'run_command', command: 'ls' }
    await nextTick()
    await wrapper.findAll('.approval .row button')[0].trigger('click')
    await nextTick()
    expect(wrapper.find('.approval').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('已批准执行')
    expect(wrapper.text()).toContain('已失效')
  })

  it('「详细」按钮弹出全文弹窗，可关闭，批准后随之收起', async () => {
    setup()
    const cmd = Array.from({ length: 40 }, (_, i) => `echo line-${i}`).join('\n')
    store.pending = { id: 'a5', name: 'run_command', command: cmd }
    await nextTick()

    // 默认无弹窗；点「详细」后出现且包含完整命令
    expect(wrapper.find('.cmd-modal').exists()).toBe(false)
    const detailBtn = wrapper.findAll('.approval button').find(b => b.text() === '详细')
    await detailBtn.trigger('click')
    await nextTick()
    const modal = wrapper.find('.cmd-modal')
    expect(modal.exists()).toBe(true)
    expect(modal.find('.cmd-detail').text()).toContain('echo line-39')

    // Esc 关闭
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    await nextTick()
    expect(wrapper.find('.cmd-modal').exists()).toBe(false)

    // 再打开后批准，pending 清空，弹窗一并收起
    await detailBtn.trigger('click')
    await nextTick()
    await wrapper.findAll('.approval .row button')[0].trigger('click')
    await nextTick()
    expect(wrapper.find('.approval').exists()).toBe(false)
    expect(wrapper.find('.cmd-modal').exists()).toBe(false)
  })

  it('「复制」成功显示已复制，关闭重开不残留', async () => {
    setup()
    const orig = navigator.clipboard
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText: async () => {} },
      configurable: true
    })
    try {
      store.pending = { id: 'a6', name: 'run_command', command: 'ls' }
      await nextTick()
      const detailBtn = wrapper.findAll('.approval button').find(b => b.text() === '详细')
      await detailBtn.trigger('click')
      await nextTick()
      const copyBtn = wrapper.findAll('.cmd-modal button').find(b => b.text() === '复制')
      await copyBtn.trigger('click')
      await flushPromises()
      await nextTick()
      expect(copyBtn.text()).toBe('已复制')

      // 关闭后重开：不能残留上一次的「已复制」
      await wrapper.findAll('.cmd-modal button').find(b => b.text() === '关闭').trigger('click')
      await nextTick()
      await detailBtn.trigger('click')
      await nextTick()
      const copyBtn2 = wrapper.findAll('.cmd-modal button').find(b => b.text().includes('复制'))
      expect(copyBtn2.text()).toBe('复制')
    } finally {
      Object.defineProperty(navigator, 'clipboard', { value: orig, configurable: true })
    }
  })

  it('新建任务弹窗打开时 Esc 让路，只在其关闭后才收命令弹窗', async () => {
    withHost()
    setup()
    store.pending = { id: 'a7', name: 'run_command', command: 'ls' }
    await nextTick()
    await wrapper.findAll('.approval button').find(b => b.text() === '详细').trigger('click')
    await nextTick()
    // 叠加打开新建任务弹窗
    await wrapper.findAll('button').find(b => b.text().includes('新建任务')).trigger('click')
    await nextTick()
    expect(wrapper.find('.cmd-modal').exists()).toBe(true)

    // 新建弹窗在顶层：Esc 不应误关命令弹窗
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    await nextTick()
    expect(wrapper.find('.cmd-modal').exists()).toBe(true)
    expect(wrapper.find('.modal').exists()).toBe(true)

    // 新建弹窗关掉后，Esc 恢复关闭命令弹窗
    await wrapper.findAll('.modal-actions button').find(b => b.text() === '取消').trigger('click')
    await nextTick()
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    await nextTick()
    expect(wrapper.find('.cmd-modal').exists()).toBe(false)
  })
})

// ---- 运行指示 ----
//
// 输入已整体搬进终端表面（分流用例见 terminal.test.js 的直接输入组），
// composer-bar 只剩工具条；这里锁住工具条上的运行指示。
describe('ConsolePanel 运行指示', () => {
  it('运行中显示「中断」', () => {
    withHost()
    store.running = true
    setup()
    expect(wrapper.text()).toContain('中断')
  })

  it('运行中且无审批时显示「Thinking」', async () => {
    store.running = true
    setup()
    await nextTick()
    expect(wrapper.text()).toContain('Thinking')
  })

  it('等待审批时不显示「Thinking」（此时在等人，不是在算）', async () => {
    store.running = true
    store.pending = { id: 'a1', name: 'run_command', command: 'ls' }
    setup()
    await nextTick()
    expect(wrapper.text()).not.toContain('Thinking')
  })
})

// ---- 模型小徽标 ----
//
// 厂商靠 model/baseUrl/name 推（无厂商字段），认不出回退方案名首字。
describe('ConsolePanel 模型徽标', () => {
  it('命中厂商显彩色缩写，认不出回退首字母', async () => {
    store.llmProfiles = [
      { id: 'p1', name: '深度', model: 'deepseek-v4', baseUrl: 'https://api.deepseek.com/v1', active: true },
      { id: 'p2', name: 'MyBox', model: 'custom-llm', baseUrl: 'https://internal.example/v1', active: false }
    ]
    setup()
    await nextTick()
    const badge = wrapper.find('.model-select .uis-badge')
    expect(badge.exists()).toBe(true)
    expect(badge.text()).toBe('DS')
    await wrapper.find('.model-select .uis-trigger').trigger('click')
    await nextTick()
    const marks = wrapper.findAll('.model-select .uis-item .uis-badge').map(b => b.text())
    expect(marks).toEqual(['DS', 'M'])
  })
})

// ---- 表面渲染：agent 事件画进那片终端 ----
//
// 单表面控制台的核心：对话/命令/审批/交接全落在同一条终端流里。
// 这里驱动真实事件流（bindEvents + rt.emit），断言 xterm 替身收到的文本。
describe('ConsolePanel 表面渲染', () => {
  it('agent 回复画进表面：角色抬头 + 正文', async () => {
    await setupAttached()
    rt.emit('agent:message', { role: 'assistant', content: '我来看一下 nginx 的状态' })
    await nextTick()
    expect(surfaceText()).toContain('●')
    expect(surfaceText()).toContain('我来看一下 nginx 的状态')
  })

  it('流式增量逐字接在抬头后面', async () => {
    await setupAttached()
    rt.emit('agent:delta', { step: 0, text: '磁盘' })
    rt.emit('agent:delta', { step: 0, text: '充足' })
    await nextTick()
    expect(surfaceText()).toContain('●')
    expect(surfaceText()).toContain('磁盘充足')
  })

  it('回复里的裸 \n 归一成 \r\n（否则终端不补 CR→阶梯状错位）', async () => {
    await setupAttached()
    rt.emit('agent:delta', { step: 0, text: '第一行\n第二行' })
    await nextTick()
    const raw = instances[0].text()
    expect(raw).toContain('第一行\r\n第二行')
    // 归档（给模型吃）仍存原文，不受显示归一影响
    const msg = store.entries.filter(e => e.kind === 'assistant').pop()
    expect(msg.content).toBe('第一行\n第二行')
  })

  it('工具块画 `$ cmd`', async () => {
    await setupAttached()
    rt.emit('agent:tool', {
      id: 'c1', name: 'run_command', command: 'systemctl status nginx',
      decision: 'allow', status: 'running', hostName: 'web'
    })
    await nextTick()
    expect(surfaceText()).toContain('$ systemctl status nginx')
  })

  it('工具结果画退出码与截断输出', async () => {
    await setupAttached()
    rt.emit('agent:toolResult', { id: 'c1', exitCode: 0, content: 'active (running)', hostId: 'h1' })
    await nextTick()
    expect(surfaceText()).toContain('退出码 0')
    expect(surfaceText()).toContain('active (running)')
  })

  it('审批注记画进表面（交互入口仍在审批条）', async () => {
    await setupAttached()
    rt.emit('agent:approval', {
      id: 'a1', name: 'run_command', command: 'rm -rf /tmp/x', reason: '高危删除', decision: 'confirm'
    })
    await nextTick()
    expect(surfaceText()).toContain('需要你的批准')
    expect(surfaceText()).toContain('rm -rf /tmp/x')
    // 交互入口是审批条本身
    expect(wrapper.find('.approval').exists()).toBe(true)
  })

  it('TTY 命中画交接注记，并把命令写进常驻 PTY（不再 spawn 块）', async () => {
    await setupAttached()
    rt.emit('agent:toolResult', {
      id: 'c1', exitCode: 1, content: 'TERM environment variable not set.',
      tty: true, command: 'top', hostId: 'h1', sessionId: 'default'
    })
    await nextTick()
    expect(surfaceText()).toContain('已交接到这片终端表面')
    const writes = calls.filter(c => c.name === 'WriteTerminal')
    // 新签名：WriteTerminal(hostId, sessionId, data)，命令写进本任务自己的 PTY。
    expect(writes.some(w => w.args[0] === 'h1' && w.args[1] === 'default' && w.args[2] === textToBase64('top\n'))).toBe(true)
  })

  it('快照只落一行注记，不重画那一帧', async () => {
    await setupAttached()
    rt.emit('agent:snapshot', {
      hostId: 'h1', sessionId: 'default', termId: 'h1#a1', text: '> load: 9.9', final: true
    })
    await nextTick()
    expect(surfaceText()).toContain('已进归档与模型上下文')
    // 定格画面本来就在 scrollback 里，表面不重画帧内容
    expect(surfaceText()).not.toContain('load: 9.9')
  })

  it('错误画进表面', async () => {
    await setupAttached()
    rt.emit('agent:error', { message: '连接超时' })
    await nextTick()
    expect(surfaceText()).toContain('连接超时')
  })

  it('思考增量画进表面，正文开始时另起一行落新抬头', async () => {
    await setupAttached()
    // 思考先于正文到达（推理型模型的真实时序），先由思考建出这条消息
    rt.emit('agent:reasoning', { step: 0, text: '先看服务状态' })
    rt.emit('agent:delta', { step: 0, text: 'Docker 正常。' })
    await nextTick()
    // 思考与正文都上屏，且正文另起一行有自己的●抬头
    expect(surfaceText()).toContain('先看服务状态')
    expect(surfaceText()).toContain('Docker 正常。')
    expect(surfaceText().split('●').length - 1).toBe(2)
    // 归档时间线里挂到同一条消息上，思考不混进正文
    const msg = store.entries.filter(e => e.kind === 'assistant').pop()
    expect(msg.reasoning).toBe('先看服务状态')
    expect(msg.content).toBe('Docker 正常。')
  })
})

// ---- 本地行编辑的退格 ----
//
// 退格必须按**显示宽度**回退擦除：中文/全角占 2 列，只擦 1 列会留下右半、
// 光标错位 —— 就是「输入自然语言时删不干净」的根因。
describe('ConsolePanel 本地行编辑退格', () => {
  it('删一个全角字符：按显示宽度回退擦两列，且不发给 PTY', async () => {
    await setupAttached()
    const term = instances[0]
    term.emitData('你')
    term.emitData('\x7f')
    expect(term.written).toContain('你')
    // displayWidth('你')===2 → '\b \b' 重复两次
    expect(term.written).toContain('\b \b\b \b')
    // 本地删除不应把退格转发给远端
    expect(calls.filter(c => c.name === 'WriteTerminal')).toHaveLength(0)
  })

  it('删一个 ASCII 字符：只擦一列', async () => {
    await setupAttached()
    const term = instances[0]
    term.emitData('l')
    term.emitData('s')
    term.emitData('\x7f')
    expect(term.written).toContain('\b \b')
  })

  it('代理对（字外汉字/emoji）当一个字符删，不拆半', async () => {
    await setupAttached()
    const term = instances[0]
    const astral = '\ud840\udc00' // 两个 UTF-16 码元组成一个码点
    term.emitData(astral)
    term.emitData('\x7f') // 整个码点一次删完，缓冲应清空
    const afterFirst = term.written.length
    term.emitData('\x7f') // 缓冲已空 → 退格应为 no-op
    // 旧 slice(0,-1) 只去掉半个代理项，缓冲残留孤立高位代理→第二次退格还会再擦
    expect(term.written.length).toBe(afterFirst)
  })

  // 按方向键/Tab 等控制键会先把本地缓冲 flush 给远端，这一行从此由 shell 接管。
  // 此时本地缓冲已空，退格必须转发给 PTY，否则会被 if (t.line) 吞掉 → 删不动。
  it('控制键把整行交回远端后，退格转发给 PTY', async () => {
    await setupAttached()
    const term = instances[0]
    for (const ch of 'rm -rf tex') term.emitData(ch) // 全本地缓冲，未发 PTY
    expect(calls.filter(c => c.name === 'WriteTerminal')).toHaveLength(0)
    term.emitData('\x1b[D') // 左方向键：flush 整行 + 交回 shell（降级透传）
    const before = calls.filter(c => c.name === 'WriteTerminal').length
    term.emitData('\x7f') // 退格：本地缓冲已空 → 应转发给远端
    const writes = calls.filter(c => c.name === 'WriteTerminal')
    expect(writes.length).toBeGreaterThan(before)
    expect(writes[writes.length - 1].args[2]).toBe(textToBase64('\x7f'))
  })

  it('Ctrl-C 中断该行后回到本地编辑（后续输入不再转发）', async () => {
    await setupAttached()
    const term = instances[0]
    for (const ch of 'ls') term.emitData(ch)
    term.emitData('\x03') // Ctrl-C：中断、远端回到空提示符 → 不降级透传
    const afterCtrlC = calls.filter(c => c.name === 'WriteTerminal').length
    term.emitData('x') // 应回到本地行编辑：本地回显，不发 PTY
    expect(calls.filter(c => c.name === 'WriteTerminal')).toHaveLength(afterCtrlC)
    expect(term.written).toContain('x')
  })
})

// ---- 对话归档：只读时间线 ----
//
// 归档视图复用 store.entries 渲染，但不接活事件、没有任何交互按钮 ——
// 它是「回看历史」的地方，活的操作全在表面与 composer。
describe('ConsolePanel 对话归档', () => {
  it('默认在终端表面，归档视图隐藏', () => {
    setup()
    expect(wrapper.find('.surface').isVisible()).toBe(true)
    expect(wrapper.find('.archive').isVisible()).toBe(false)
  })

  it('切到对话归档显示时间线，切回显示表面', async () => {
    setup()
    await toArchive()
    expect(wrapper.find('.archive').isVisible()).toBe(true)
    expect(wrapper.find('.surface').isVisible()).toBe(false)
    await toSurface()
    expect(wrapper.find('.surface').isVisible()).toBe(true)
    expect(wrapper.find('.archive').isVisible()).toBe(false)
  })

  it('空归档给出引导文案', async () => {
    setup()
    await toArchive()
    expect(wrapper.find('.archive-empty').exists()).toBe(true)
  })

  it('归档视图只读：条目里没有任何交互按钮', async () => {
    setup()
    push({ kind: 'tool', toolId: 'c1', tool: { id: 'c1', name: 'run_command', status: 'done', decision: 'allow', command: 'ls' } })
    push({ kind: 'result', toolId: 'c1', exitCode: 0, content: 'ok', redacted: 0, injection: [] })
    await toArchive()
    expect(wrapper.findAll('.archive .entry button')).toHaveLength(0)
  })

  it('用户/LLM 气泡按角色区分', async () => {
    setup()
    push({ kind: 'user', content: '你好' })
    push({ kind: 'assistant', content: '在的' })
    await nextTick()
    expect(wrapper.find('.msg.user .msg-body').text()).toBe('你好')
    expect(wrapper.find('.msg.assistant .msg-body').text()).toBe('在的')
  })

  it('工具卡片显示裁决徽章与状态', async () => {
    setup()
    push({
      kind: 'tool',
      toolId: 'c1',
      tool: { id: 'c1', name: 'run_command', status: 'pending', decision: 'confirm', command: 'ls -l' }
    })
    await nextTick()
    const t = wrapper.find('.tool')
    expect(t.text()).toContain('run_command')
    expect(t.text()).toContain('需确认')
    expect(t.text()).toContain('待批准')
    expect(t.text()).toContain('ls -l')
  })

  it('结果区显示退出码与脱敏计数', async () => {
    setup()
    push({ kind: 'result', toolId: 'c1', exitCode: 0, content: 'out', redacted: 2, injection: [] })
    await nextTick()
    const r = wrapper.find('.result')
    expect(r.text()).toContain('退出码 0')
    // 断言必须落在徽章元素上：结果区有一句常驻说明文案「…已脱敏并标记为不可信」，
    // 直接对整块文本做 toContain('已脱敏') 会被这句固定文案假阳性命中。
    expect(r.find('.badge.warn').text()).toContain('已脱敏 2 处')
  })

  it('未脱敏时不显示脱敏徽章', async () => {
    setup()
    push({ kind: 'result', toolId: 'c1', exitCode: 1, content: 'err', redacted: 0, injection: [] })
    await nextTick()
    expect(wrapper.find('.result .badge.warn').exists()).toBe(false)
  })

  it('注入告警渲染命中话术与来源主机', async () => {
    setup()
    push({
      kind: 'injection',
      findings: ['要求忽略先前指令'],
      command: 'cat /tmp/evil.txt',
      hostName: 'web'
    })
    await nextTick()
    const box = wrapper.find('.injection')
    expect(box.exists()).toBe(true)
    expect(box.text()).toContain('要求忽略先前指令')
    expect(box.text()).toContain('web')
    expect(box.text()).toContain('不会作为指令执行')
  })

  it('错误记录用错误样式渲染', async () => {
    setup()
    push({ kind: 'error', content: '连接超时' })
    await nextTick()
    expect(wrapper.find('.msg.error').text()).toContain('连接超时')
  })

  it('屏幕快照记录渲染净化文本、脱敏计数与注入告警', async () => {
    withHost()
    setup()
    push({
      kind: 'snapshot', termId: 'h1#a1', text: '> load: 9.9',
      redacted: 1, injection: ['忽略先前指令']
    }, 'h1')
    await nextTick()
    const el = wrapper.find('.result')
    expect(el.exists()).toBe(true)
    expect(el.text()).toContain('终端屏幕快照')
    expect(el.text()).toContain('load: 9.9')
    expect(el.text()).toContain('已脱敏 1 处')
    expect(el.text()).toContain('忽略先前指令')
  })

  it('结束画面记录渲染定格帧徽章与最后一屏文本', async () => {
    withHost()
    setup()
    push({
      kind: 'snapshot', termId: 'h1#a1', text: '> load: 9.9',
      redacted: 1, injection: [], final: true
    }, 'h1')
    await nextTick()
    const el = wrapper.find('.result')
    expect(el.exists()).toBe(true)
    expect(el.text()).toContain('终端结束画面')
    expect(el.text()).toContain('最后一屏')
    expect(el.text()).toContain('load: 9.9')
    expect(el.text()).not.toContain('终端屏幕快照')
  })
})

describe('ConsolePanel 流式输出', () => {
  it('生成中的消息带光标，定稿后光标消失', async () => {
    setup()
    push({ kind: 'assistant', content: '正在生成', stream: 0, streaming: true })
    await nextTick()
    expect(wrapper.find('.msg.assistant .caret').exists()).toBe(true)

    store.entries[0].streaming = false
    await nextTick()
    expect(wrapper.find('.msg.assistant .caret').exists()).toBe(false)
  })

  it('非流式消息不带光标', async () => {
    setup()
    push({ kind: 'assistant', content: '一次性返回' })
    await nextTick()
    expect(wrapper.find('.msg.assistant .caret').exists()).toBe(false)
  })

  it('正在逐字输出时不显示「Thinking」（此时在输出，不是在算）', async () => {
    store.running = true
    setup()
    push({ kind: 'assistant', content: '输出中', stream: 0, streaming: true })
    await nextTick()
    expect(wrapper.text()).not.toContain('Thinking')
  })

  it('思考阶段（还没有任何增量）仍显示「Thinking」', async () => {
    store.running = true
    setup()
    await nextTick()
    expect(wrapper.text()).toContain('Thinking')
  })

  it('生成过程中内容增长会触发自动滚动', async () => {
    setup()
    push({ kind: 'assistant', content: 'a', stream: 0, streaming: true })
    await nextTick()

    // jsdom 不做真实布局，这里只验证滚动逻辑被触发（scrollTop 被写入）
    const log = wrapper.find('.log').element
    Object.defineProperty(log, 'scrollHeight', { value: 1234, configurable: true })
    store.entries[0].content = 'ab'
    await nextTick()
    await nextTick()
    expect(log.scrollTop).toBe(1234)
  })
})

// ---- 思考过程（推理型模型） ----

describe('ConsolePanel 思考过程', () => {
  // 思考区渲染在「对话归档」时间线里（活表面是 xterm），可见性断言先切过去
  async function toArchive() {
    await wrapper.findAll('.seg button').find(b => b.text() === '对话归档').trigger('click')
    await nextTick()
  }

  it('思考中显示「思考中…」且默认展开，点击可折叠', async () => {
    setup()
    await toArchive()
    push({ kind: 'assistant', content: '', stream: 0, streaming: true, reasoning: '先看服务状态', reasoningOpen: true })
    await nextTick()

    const head = wrapper.find('.reasoning-head')
    expect(head.exists()).toBe(true)
    expect(head.text()).toContain('思考中')
    expect(wrapper.find('.reasoning-body').isVisible()).toBe(true)
    expect(wrapper.find('.reasoning-body').text()).toContain('先看服务状态')

    await head.trigger('click')
    await nextTick()
    expect(wrapper.find('.reasoning-body').isVisible()).toBe(false)
  })

  it('正文到达后标题变「思考过程」，默认收起但可再展开', async () => {
    setup()
    await toArchive()
    push({ kind: 'assistant', content: '', stream: 0, streaming: true, reasoning: '先看服务状态', reasoningOpen: true })
    await nextTick()

    // 定稿：store.js 的 agent:message 处理器会把 reasoningOpen 收起
    store.entries[0].content = 'Docker 正常。'
    store.entries[0].streaming = false
    store.entries[0].reasoningOpen = false
    await nextTick()

    expect(wrapper.find('.reasoning-head').text()).toContain('思考过程')
    expect(wrapper.find('.reasoning-head').text()).not.toContain('思考中')
    expect(wrapper.find('.reasoning-body').isVisible()).toBe(false)

    await wrapper.find('.reasoning-head').trigger('click')
    await nextTick()
    expect(wrapper.find('.reasoning-body').isVisible()).toBe(true)
    expect(wrapper.text()).toContain('Docker 正常。')
  })

  it('有思考内容的消息不显示「空回复」占位符', async () => {
    setup()
    await toArchive()
    push({ kind: 'assistant', content: '', stream: 0, streaming: false, reasoning: '想了很多', reasoningOpen: false })
    await nextTick()
    expect(wrapper.text()).not.toContain('模型返回了空回复')
  })
})

// ---- 清空会话上下文 ----
//
// 这个按钮和「清空记录」长得像，干的事完全不同：
// 一个清屏幕上的文字，一个清 LLM 的记忆。界面上必须都能点得到，
// 且点的是各自那一个 —— 混在一起用户就永远学不到它们不是一回事。
describe('ConsolePanel 清空会话上下文', () => {
  function setupWithHost(impl) {
    withHost()
    return setup(impl)
  }

  // 按**文字**找按钮，不要按下标。
  //
  // 早先这里写的是 wrapper.find('.ctx-bar button')，靠位置取到第一个 ——
  // 后来在旁边加了个「压缩上下文」，它就成了第一个，四条用例一起红。
  // 位置选择在「同一个容器里会有几个按钮」这件事上没有表达力。
  // 模型/压缩/清空上下文整行已挪到底部 composer-bar（紧邻输入框）。
  function ctxBtn(label) {
    const b = wrapper.findAll('.composer-bar button').find(x => x.text().includes(label))
    if (!b) throw new Error(`未找到按钮：${label}`)
    return b
  }

  it('点「清空上下文」把当前主机 id 与会话 id 都发给后端', async () => {
    setupWithHost({ ClearSession: async () => ({ cleared: 2, busy: false }) })
    // 特意选中一条具名会话。只断言主机 id 的话，「永远传空会话」
    // 这种 bug 会照样绿 —— 而那正是「清空清错了会话」的成因：
    // 用户点的是「nginx 排查」，被清掉的却是默认会话。
    store.currentSessionId = 's7'
    await ctxBtn('清空上下文').trigger('click')
    await flushPromises()

    expect(calls.filter(c => c.name === 'ClearSession')).toHaveLength(1)
    expect(calls.filter(c => c.name === 'ClearSession')[0].args).toEqual(['h1', 's7'])
  })

  it('清掉若干轮时把轮数显示出来', async () => {
    setupWithHost({ ClearSession: async () => ({ cleared: 3, busy: false }) })
    await ctxBtn('清空上下文').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('3 轮')
  })

  it('有会话在跑时说明原因，而不是按钮点了没反应', async () => {
    setupWithHost({ ClearSession: async () => ({ cleared: 0, busy: true }) })
    await ctxBtn('清空上下文').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('正在运行')
  })

  it('没选主机时不打后端，给出提示', async () => {
    store.hosts = []
    store.currentHostId = ''
    setup({ ClearSession: async () => ({ cleared: 1, busy: false }) })
    await ctxBtn('清空上下文').trigger('click')
    await flushPromises()

    expect(calls.filter(c => c.name === 'ClearSession')).toHaveLength(0)
    expect(wrapper.text()).toContain('请先选择一台主机')
  })

  // 「清空记录」只清屏幕：不能顺手把上下文也清了。
  it('点「清空记录」不会打到后端的 ClearSession', async () => {
    setupWithHost({ ClearSession: async () => ({ cleared: 1, busy: false }) })
    push({ kind: 'user', content: '一条记录' })
    await nextTick()

    const btn = wrapper.findAll('.composer-bar button').find(b => b.text() === '清空记录')
    await btn.trigger('click')
    await flushPromises()

    expect(calls.filter(c => c.name === 'ClearSession')).toHaveLength(0)
    expect(store.entries, '屏幕上的记录应被清掉').toEqual([])
  })

  it('两个按钮都在界面上，名称可区分', () => {
    setupWithHost({ ClearSession: async () => ({ cleared: 0, busy: false }) })
    const clearCtx = ctxBtn('清空上下文').text()
    const clearLog = wrapper.findAll('.composer-bar button').map(b => b.text())
    expect(clearCtx).toContain('清空上下文')
    expect(clearLog).toContain('清空记录')
    expect(clearCtx).not.toBe('清空记录')
  })
})

// ---- 压缩会话上下文 ----
//
// 与「清空上下文」是一对：清空是丢掉记忆，压缩是换一种更省的方式留着。
// 界面上必须都能点到，且用户能从措辞上分清 —— 两者混起来，
// 用户就永远学不到「压缩之后模型还记得」这件事。
describe('ConsolePanel 压缩会话上下文', () => {
  function setupWithHost(impl) {
    withHost()
    return setup(impl)
  }

  function ctxBtn(label) {
    const b = wrapper.findAll('.composer-bar button').find(x => x.text().includes(label))
    if (!b) throw new Error(`未找到按钮：${label}`)
    return b
  }

  it('点「压缩上下文」把当前主机 id 与会话 id 都发给后端', async () => {
    setupWithHost({
      CompactSession: async () => ({ compacted: true, turns: 5, bytes: 300, message: '已把 5 轮对话压缩成 300 字节的摘要。' })
    })
    // 同「清空上下文」：会话 id 必须跟着走，否则压的是另一条会话的历史，
    // 用户会看到「压缩成功」但当前这条对话一点没变。
    store.currentSessionId = 's7'
    await ctxBtn('压缩上下文').trigger('click')
    await flushPromises()

    expect(calls.filter(c => c.name === 'CompactSession')).toHaveLength(1)
    expect(calls.filter(c => c.name === 'CompactSession')[0].args).toEqual(['h1', 's7'])
  })

  // 后端把话说全了，前端照搬即可 —— 在前端重写一遍措辞，两处迟早漂移。
  it('把后端返回的说明原样显示出来', async () => {
    setupWithHost({
      CompactSession: async () => ({ compacted: true, turns: 5, bytes: 300, message: '已把 5 轮对话压缩成 300 字节的摘要。' })
    })
    await ctxBtn('压缩上下文').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('已把 5 轮对话压缩成 300 字节的摘要。')
  })

  // 「没得压」「正忙」是预期内的状态，必须以普通提示呈现。
  // 渲染成红色报错会让用户以为功能坏了。
  it('没得压时给普通提示而不是报错', async () => {
    setupWithHost({
      CompactSession: async () => ({ compacted: false, busy: false, message: '这台主机还没有会话记录。' })
    })
    await ctxBtn('压缩上下文').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('这台主机还没有会话记录。')
    expect(wrapper.find('.entry.error').exists()).toBe(false)
  })

  it('有会话在跑时说明原因，而不是按钮点了没反应', async () => {
    setupWithHost({
      CompactSession: async () => ({ compacted: false, busy: true, message: '该主机正在执行一轮对话，等它结束或先点「中断」再压缩。' })
    })
    await ctxBtn('压缩上下文').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('正在执行一轮对话')
  })

  // 真故障走 error 通道，而且要说明「原历史没被改动」——
  // 否则用户会担心压缩失败把上下文弄丢了。
  it('后端报错时说明原历史未被改动', async () => {
    setupWithHost({
      CompactSession: async () => { throw new Error('尚未配置 LLM API Key') }
    })
    await ctxBtn('压缩上下文').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('尚未配置 LLM API Key')
    expect(wrapper.text()).toContain('原历史未被改动')
  })

  it('没选主机时不打后端，给出提示', async () => {
    store.hosts = []
    store.currentHostId = ''
    setup({ CompactSession: async () => ({ compacted: true }) })
    await ctxBtn('压缩上下文').trigger('click')
    await flushPromises()

    expect(calls.filter(c => c.name === 'CompactSession')).toHaveLength(0)
    expect(wrapper.text()).toContain('请先选择一台主机')
  })

  // 压缩要等好几秒，允许连点会发出多个并发请求，
  // 而后端只有第一个能成功（其余因「会话已变化」被放弃）——
  // 用户会看到一串自相矛盾的提示。
  it('压缩期间按钮禁用并显示进度', async () => {
    let release
    const gate = new Promise(r => { release = r })
    setupWithHost({
      CompactSession: async () => { await gate; return { compacted: true, message: 'done' } }
    })

    await ctxBtn('压缩上下文').trigger('click')
    await nextTick()

    const btn = ctxBtn('压缩')
    expect(btn.attributes('disabled')).toBeDefined()
    expect(btn.text()).toContain('压缩中')

    release()
    await flushPromises()
    expect(ctxBtn('压缩').attributes('disabled')).toBeUndefined()
  })

  // 运行中**不禁用**：后端会回一句「正在跑一轮」，那句话比点不动的按钮有用。
  it('运行中仍可点击，由后端说明原因', async () => {
    store.running = true
    setupWithHost({
      CompactSession: async () => ({ compacted: false, busy: true, message: '该主机正在执行一轮对话。' })
    })
    const btn = ctxBtn('压缩上下文')
    expect(btn.attributes('disabled')).toBeUndefined()

    await btn.trigger('click')
    await flushPromises()
    expect(calls.filter(c => c.name === 'CompactSession')).toHaveLength(1)
  })
})

// ---- 上下文存放位置的说明 ----
//
// 会话从「只存内存」改成了「加密落盘」。界面文案与事实相反比没有文案更糟：
// 用户按「重启就没了」的预期去用，结果敏感对话一直留在磁盘上，而他以为早没了。
describe('ConsolePanel 上下文存放说明', () => {
  it('界面不得声称「重启即清空 / 只存内存」', () => {
    withHost()
    setup({})

    const text = wrapper.find('.console').text()
    expect(text, '会话已经落盘了，不能再声称重启即清空').not.toContain('重启应用即清空')
    expect(text).not.toContain('只存在内存')
    expect(wrapper.find('.sess-banner').text()).toMatch(/高危|确认/)
  })
})

// ---- 会话管理已移至侧边栏（SessionsSidebar.test.js）----
// 这里只保留 ConsolePanel 自己的会话相关行为：切主机时重新拉列表。
describe('ConsolePanel 会话随主机切换', () => {
  // 切主机必须重新拉列表：不拉的话，切到一台之前没看过的机器时选择器是空的，
  // 而它的默认会话其实一直都在 —— 用户会以为这台机器没有会话可用。
  it('切主机时去拉那台主机的会话列表', async () => {
    withHost()
    setup({})
    await flushPromises()

    store.currentHostId = 'h2'
    await flushPromises()

    const c = calls.filter(x => x.name === 'ListSessions')
    expect(c).toHaveLength(1)
    expect(c[0].args).toEqual(['h2'])
  })
})
