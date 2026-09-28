// AuditPanel 组件测试。
//
// 重点覆盖本轮新增的轮转相关界面：截断提示、段数说明、以及新增的「日志轮转」记录类型。
// 这类"后端多返回一个字段、界面忘了显示"的缺口，契约测试（只查方法名）是抓不到的。
import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import AuditPanel from '../components/AuditPanel.vue'
import { store } from '../store.js'

function entry(seq, kind, extra = {}) {
  return {
    seq,
    time: '2026-09-27T22:00:00+08:00',
    kind,
    prevHash: 'p' + seq,
    hash: 'h' + seq,
    ...extra
  }
}

function makeApp(impl = {}) {
  const calls = []
  const base = {
    ListAudit: async () => ({ entries: [], size: 0, segments: 1, error: '' }),
    VerifyAudit: async () => ({ ok: true, count: 0, message: '哈希链完整' }),
    ExportAudit: async () => ''
  }
  const merged = { ...base, ...impl }
  const app = {}
  for (const k of Object.keys(merged)) {
    app[k] = (...args) => {
      calls.push({ name: k, args })
      return merged[k](...args)
    }
  }
  return { app, calls }
}

let wrapper
let calls

async function setup(impl) {
  const made = makeApp(impl)
  calls = made.calls
  window.go = { main: { App: made.app } }
  wrapper = mount(AuditPanel)
  // onMounted 里的 load() 是异步的，等它落地
  await nextTick()
  await nextTick()
  return wrapper
}

beforeEach(() => {
  delete window.go
  store.bySession = {}
  store.sessions = {}
  store.currentSessionId = ''
  store.activeHostId = ''
  store.activeSessionId = ''
})

afterEach(() => {
  if (wrapper) wrapper.unmount()
  wrapper = null
})

describe('AuditPanel 记录列表', () => {
  it('挂载时拉取记录并渲染序号与类型', async () => {
    await setup({
      ListAudit: async () => ({
        entries: [entry(1, 'tool', { tool: 'run_command', command: 'ls' }), entry(2, 'host')],
        size: 2048,
        segments: 1,
        error: ''
      })
    })
    expect(calls.filter(c => c.name === 'ListAudit')).toHaveLength(1)
    expect(wrapper.text()).toContain('#1')
    expect(wrapper.text()).toContain('#2')
    expect(wrapper.text()).toContain('工具调用')
    expect(wrapper.text()).toContain('2.0 KB')
  })

  it('后端返回 error 时展示错误卡片而不是空列表', async () => {
    await setup({
      ListAudit: async () => ({ entries: [], size: 0, segments: 0, error: '审计日志未初始化' })
    })
    expect(wrapper.find('.error-card').text()).toContain('审计日志未初始化')
  })

  it('调用抛异常时也进入错误态', async () => {
    await setup({ ListAudit: async () => { throw new Error('bridge down') } })
    expect(wrapper.find('.error-card').text()).toContain('bridge down')
  })

  it('拒绝的审批显示「已拒绝」徽章', async () => {
    await setup({
      ListAudit: async () => ({
        entries: [entry(1, 'tool', { decision: 'confirm', approved: false })],
        size: 1, segments: 1, error: ''
      })
    })
    expect(wrapper.text()).toContain('已拒绝')
  })

  it('注入与脱敏计数以徽章呈现', async () => {
    await setup({
      ListAudit: async () => ({
        entries: [entry(1, 'tool', { injection: ['忽略先前指令'], redacted: 3 })],
        size: 1, segments: 1, error: ''
      })
    })
    expect(wrapper.text()).toContain('注入 1')
    expect(wrapper.text()).toContain('脱敏 3')
  })
})

describe('AuditPanel 轮转相关界面', () => {
  it('单段时说明会自动轮转', async () => {
    await setup({
      ListAudit: async () => ({ entries: [entry(1, 'system')], size: 10, segments: 1, error: '' })
    })
    expect(wrapper.text()).toContain('单段')
    expect(wrapper.text()).not.toContain('已轮转为')
  })

  it('多段时显示段数，且不再说「不做轮转」', async () => {
    await setup({
      ListAudit: async () => ({ entries: [entry(1, 'system')], size: 10, segments: 4, error: '' })
    })
    expect(wrapper.text()).toContain('已轮转为 4 个段')
    expect(wrapper.text()).not.toContain('不做轮转')
  })

  it('后端没返回 segments 字段时兜底为 1，不显示 undefined', async () => {
    await setup({
      ListAudit: async () => ({ entries: [entry(1, 'system')], size: 10, error: '' })
    })
    expect(wrapper.text()).not.toContain('undefined')
    expect(wrapper.text()).toContain('单段')
  })

  it('新增的「日志轮转」类型有独立筛选项与中文标签', async () => {
    await setup({
      ListAudit: async () => ({
        entries: [entry(1, 'retention', { note: '保留策略生效：已丢弃 2 个历史段' }), entry(2, 'tool')],
        size: 10, segments: 3, error: ''
      })
    })
    const btn = wrapper.findAll('.filters button').find(b => b.text() === '日志轮转')
    expect(btn, '应存在「日志轮转」筛选项').toBeTruthy()

    await btn.trigger('click')
    await nextTick()
    // 过滤后只剩 retention 那条
    expect(wrapper.findAll('.row')).toHaveLength(1)
    expect(wrapper.text()).toContain('保留策略生效')
  })

  it('校验结果里的截断提示会渲染，并指引对照轮转记录', async () => {
    await setup({
      VerifyAudit: async () => ({
        ok: true, count: 50, startSeq: 201, truncated: true, segments: 3,
        message: '自第 201 条起哈希链完整'
      })
    })
    await wrapper.findAll('.head-actions button')[1].trigger('click')
    await nextTick()

    const note = wrapper.find('.verify-note')
    expect(note.exists(), '截断时必须显示提示').toBe(true)
    expect(note.text()).toContain('201')
    expect(note.text()).toContain('日志轮转')
  })

  it('未截断时不渲染截断提示（避免制造虚假警报）', async () => {
    await setup({
      VerifyAudit: async () => ({
        ok: true, count: 50, startSeq: 1, truncated: false, segments: 1,
        message: '哈希链完整，共 50 条记录、1 个段'
      })
    })
    await wrapper.findAll('.head-actions button')[1].trigger('click')
    await nextTick()

    expect(wrapper.find('.verify').exists()).toBe(true)
    expect(wrapper.find('.verify-note').exists()).toBe(false)
  })

  it('校验失败时用失败样式呈现后端给出的断点信息', async () => {
    await setup({
      VerifyAudit: async () => ({
        ok: false, count: 50, brokenAt: 7,
        message: '第 7 条（序号 9）应为序号 8，链在此处断裂'
      })
    })
    await wrapper.findAll('.head-actions button')[1].trigger('click')
    await nextTick()

    const v = wrapper.find('.verify')
    expect(v.classes()).toContain('bad')
    expect(v.text()).toContain('校验未通过')
    expect(v.text()).toContain('第 7 条')
  })
})

describe('AuditPanel 导出', () => {
  it('导出成功后提示路径并刷新列表', async () => {
    await setup({ ExportAudit: async () => 'C:/tmp/audit.jsonl' })
    await wrapper.findAll('.head-actions button')[2].trigger('click')
    await nextTick()

    expect(store.entries.some(e => e.content.includes('C:/tmp/audit.jsonl'))).toBe(true)
    // 导出后应重新拉取（初次挂载 1 次 + 导出后 1 次）
    expect(calls.filter(c => c.name === 'ListAudit').length).toBeGreaterThanOrEqual(2)
  })

  it('用户取消保存对话框（返回空路径）时不提示、不刷新', async () => {
    await setup({ ExportAudit: async () => '' })
    await wrapper.findAll('.head-actions button')[2].trigger('click')
    await nextTick()

    expect(store.entries.filter(e => e.content.includes('已导出'))).toHaveLength(0)
    expect(calls.filter(c => c.name === 'ListAudit')).toHaveLength(1)
  })

  it('导出失败时给出错误提示', async () => {
    await setup({ ExportAudit: async () => { throw new Error('磁盘已满') } })
    await wrapper.findAll('.head-actions button')[2].trigger('click')
    await nextTick()

    expect(store.entries.some(e => e.kind === 'error' && e.content.includes('磁盘已满'))).toBe(true)
  })
})

// 交互终端是绕过策略引擎的，它在审计里的可读性比别的种类更重要 ——
// 出问题时第一件事就是来这里查「谁在什么时候开了终端」。
describe('AuditPanel 交互终端记录', () => {
  it('筛选项里有「交互终端」这一项', async () => {
    await setup()
    const labels = wrapper.findAll('.filters button').map(b => b.text())
    expect(labels).toContain('交互终端')
  })

  it('交互终端记录显示成「交互终端」，而不是一个空标签', async () => {
    await setup({
      ListAudit: async () => ({
        entries: [
          entry(1, 'terminal', { decision: 'human', note: '打开了交互终端（不经过策略引擎）' })
        ],
        size: 10,
        segments: 1,
        error: ''
      })
    })

    expect(wrapper.find('.badge.neutral').text()).toBe('交互终端')
    expect(wrapper.text()).toContain('打开了交互终端')
  })

  it('按「交互终端」筛选只留下这一类', async () => {
    await setup({
      ListAudit: async () => ({
        entries: [
          entry(1, 'direct', { note: '人工执行了一条命令' }),
          entry(2, 'terminal', { note: '打开了交互终端' })
        ],
        size: 20,
        segments: 1,
        error: ''
      })
    })

    const btn = wrapper.findAll('.filters button').find(b => b.text() === '交互终端')
    await btn.trigger('click')
    await nextTick()

    expect(wrapper.text()).toContain('打开了交互终端')
    expect(wrapper.text()).not.toContain('人工执行了一条命令')
  })
})
