// ConsolePanel 组件测试。
//
// store.js 的测试锁住了状态机，这里锁住「状态 -> 界面」这一段：
// 审批条是否出现、按钮点了到底调了什么、注入告警有没有渲染出来。
// 这一段没有测试的话，状态对了但界面没反应同样是个 bug。
import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { nextTick } from 'vue'
import ConsolePanel from '../components/ConsolePanel.vue'
import { store, push } from '../store.js'

function makeApp(impl = {}) {
  const calls = []
  const base = {
    Ask: async () => undefined,
    Approve: async () => true,
    Stop: async () => undefined,
    RunShell: async () => ({
      status: 'done', decision: 'allow', stdout: 'ok\n', stderr: '',
      exitCode: 0, durationMs: 3, reason: '', rule: 'whitelist', risk: 'low'
    }),
    // 会话列表：默认只有一条默认会话，与后端的不变式一致。
    // 不给这个实现的话，切主机触发的 refreshSessions 会抛异常、
    // 在对话流里插一条红色报错，把别的断言全带偏。
    ListSessions: async () => [
      { id: 'default', name: '', turns: 0, archivedTurns: 0, isDefault: true }
    ],
    CreateSession: async () => ({ id: 's1', name: '新会话', turns: 0, archivedTurns: 0, isDefault: false }),
    RenameSession: async () => undefined,
    DeleteSession: async () => undefined
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
  store.currentSessionId = ''
  store.activeHostId = ''
  store.activeSessionId = ''
  store.pending = null
  store.running = false
  store.busy = false
}

let calls
let wrapper

function setup(impl) {
  const made = makeApp(impl)
  calls = made.calls
  window.go = { main: { App: made.app } }
  wrapper = mount(ConsolePanel, { attachTo: document.body })
  return wrapper
}

beforeEach(() => {
  resetStore()
  delete window.go
})

afterEach(() => {
  if (wrapper) wrapper.unmount()
  wrapper = null
})

describe('ConsolePanel 空状态与主机选择', () => {
  it('没有主机时提示先去主机管理添加', () => {
    setup()
    expect(wrapper.text()).toContain('暂无主机')
  })

  it('已有主机时列出「名称 — user@addr」', () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    setup()
    expect(wrapper.text()).toContain('web')
    expect(wrapper.text()).toContain('root@10.0.0.1')
  })

  it('分段按钮显示「Agent会话」，能力横幅标明高危确认与 LLM', () => {
    setup()
    const segs = wrapper.findAll('.seg button')
    expect(segs[0].text()).toBe('Agent会话')
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
    expect(calls.filter(c => c.name === 'Approve')).toHaveLength(1)
    expect(calls[0].args).toEqual(['a3', true])
    expect(wrapper.find('.approval').exists()).toBe(false)
  })

  it('点「拒绝」把 (id, false) 发给后端', async () => {
    setup()
    store.pending = { id: 'a4', name: 'run_command', command: 'rm -rf /tmp/x' }
    await nextTick()
    const buttons = wrapper.findAll('.approval .row button')
    await buttons[1].trigger('click')
    await nextTick()
    expect(calls[0].args).toEqual(['a4', false])
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
    setup()
    // 「新建任务」按钮在无主机时是禁用的，先给一台主机
    store.hosts = [{ id: 'h1', name: 'web', user: 'r', addr: 'a' }]
    store.currentHostId = 'h1'
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

describe('ConsolePanel 对话发送', () => {
  it('无主机时输入框禁用', () => {
    setup()
    expect(wrapper.find('.term-input').attributes('disabled')).toBeDefined()
  })

  it('中文 Enter 走 Ask 并清空草稿', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'r', addr: 'a' }]
    store.currentHostId = 'h1'
    store.currentSessionId = 's3'
    setup()
    const inp = wrapper.find('.term-input')
    await inp.setValue('nginx 起不来')
    await inp.trigger('keydown', { key: 'Enter' })
    await flushPromises()
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(1)
    expect(calls[0].args).toEqual(['h1', 's3', 'nginx 起不来'])
    expect(inp.element.value).toBe('')
  })

  it('shell 命令 Enter 走 RunShell 而非 Ask', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    setup()
    const inp = wrapper.find('.term-input')
    await inp.setValue('ls -la')
    await inp.trigger('keydown', { key: 'Enter' })
    await flushPromises()
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(0)
    const runs = calls.filter(c => c.name === 'RunShell')
    expect(runs).toHaveLength(1)
    expect(runs[0].args[0]).toBe('h1')
    expect(runs[0].args[1]).toBe('ls -la')
    expect(runs[0].args[2]).toBe('~')
    expect(wrapper.find('.shell-block .p-cmd').text()).toBe('ls -la')
    expect(wrapper.find('.term-out').text()).toContain('ok')
  })

  it('空回车只在本地刷新一个提示符，不下发远端', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    setup()
    const inp = wrapper.find('.term-input')
    await inp.setValue('')
    await inp.trigger('keydown', { key: 'Enter' })
    await flushPromises()
    // 本地 no-op：既不走 Ask 也不走 RunShell
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(0)
    expect(calls.filter(c => c.name === 'RunShell')).toHaveLength(0)
    // 时间线末尾多出一个命令行为空的 shell 块（像真实终端按了次回车）
    const blocks = wrapper.findAll('.shell-block')
    expect(blocks).toHaveLength(1)
    expect(blocks[0].find('.p-cmd').text()).toBe('')
  })

  it('空白字符回车同样视为空命令，并清空输入框', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    setup()
    const inp = wrapper.find('.term-input')
    await inp.setValue('   ')
    await inp.trigger('keydown', { key: 'Enter' })
    await flushPromises()
    expect(calls.filter(c => c.name === 'RunShell')).toHaveLength(0)
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(0)
    // 像真实终端一样：回车即清空当前行，空格不残留在输入框里
    expect(inp.element.value).toBe('')
    expect(wrapper.findAll('.shell-block')).toHaveLength(1)
  })

  it('输入法组词期间的 Enter 不发送、不插空提示符', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    setup()
    const inp = wrapper.find('.term-input')
    await inp.setValue('ls')
    // 确认候选词的 Enter：isComposing 为真，必须被忽略
    await inp.trigger('keydown', { key: 'Enter', isComposing: true })
    await flushPromises()
    expect(calls.filter(c => c.name === 'RunShell')).toHaveLength(0)
    expect(wrapper.findAll('.shell-block')).toHaveLength(0)
    // 老内核兜底：keyCode 229 同样忽略
    await inp.trigger('keydown', { key: 'Enter', keyCode: 229 })
    await flushPromises()
    expect(calls.filter(c => c.name === 'RunShell')).toHaveLength(0)
    // 组词结束后的 Enter 才真正发送
    await inp.trigger('keydown', { key: 'Enter' })
    await flushPromises()
    expect(calls.filter(c => c.name === 'RunShell')).toHaveLength(1)
  })

  it('? 前缀强制走 Agent', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'r', addr: 'a' }]
    store.currentHostId = 'h1'
    setup()
    const inp = wrapper.find('.term-input')
    await inp.setValue('?ls -la')
    await inp.trigger('keydown', { key: 'Enter' })
    await flushPromises()
    expect(calls.filter(c => c.name === 'Ask')).toHaveLength(1)
    expect(calls[0].args[2]).toBe('ls -la')
    expect(calls.filter(c => c.name === 'RunShell')).toHaveLength(0)
  })

  it('运行中显示「中断」并禁用输入', async () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'r', addr: 'a' }]
    store.currentHostId = 'h1'
    store.running = true
    setup()
    expect(wrapper.text()).toContain('中断')
    expect(wrapper.find('.term-input').attributes('disabled')).toBeDefined()
  })

  it('运行中且无审批时显示「LLM 正在思考」', async () => {
    store.running = true
    setup()
    await nextTick()
    expect(wrapper.text()).toContain('LLM 正在思考')
  })

  it('等待审批时不显示「正在思考」（此时在等人，不是在算）', async () => {
    store.running = true
    store.pending = { id: 'a1', name: 'run_command', command: 'ls' }
    setup()
    await nextTick()
    expect(wrapper.text()).not.toContain('LLM 正在思考')
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

  it('正在逐字输出时不显示「LLM 正在思考」（此时在输出，不是在算）', async () => {
    store.running = true
    setup()
    push({ kind: 'assistant', content: '输出中', stream: 0, streaming: true })
    await nextTick()
    expect(wrapper.text()).not.toContain('LLM 正在思考')
  })

  it('思考阶段（还没有任何增量）仍显示「LLM 正在思考」', async () => {
    store.running = true
    setup()
    await nextTick()
    expect(wrapper.text()).toContain('LLM 正在思考')
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

describe('ConsolePanel 记录渲染', () => {
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
})

// ---- 清空会话上下文 ----
//
// 这个按钮和「清空记录」长得像，干的事完全不同：
// 一个清屏幕上的文字，一个清 LLM 的记忆。界面上必须都能点得到，
// 且点的是各自那一个 —— 混在一起用户就永远学不到它们不是一回事。

describe('ConsolePanel 清空会话上下文', () => {
  function setupWithHost(impl) {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
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
    expect(calls[0].args).toEqual(['h1', 's7'])
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
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
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
    expect(calls[0].args).toEqual(['h1', 's7'])
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
// 说明性文字已精简（空态引导整块移除，只剩能力横幅），这里锁住底线：
// 界面上任何位置都不得再声称「重启即清空 / 只存内存」。
describe('ConsolePanel 上下文存放说明', () => {
  it('界面不得声称「重启即清空 / 只存内存」', () => {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
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
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    setup({})

    store.currentHostId = 'h2'
    await flushPromises()

    const c = calls.filter(x => x.name === 'ListSessions')
    expect(c).toHaveLength(1)
    expect(c[0].args).toEqual(['h2'])
  })
})
