// SessionsSidebar 组件测试。
//
// 会话管理（列表/新建/重命名/删除）已从 ConsolePanel 的工具条迁到侧边栏，
// 交互改为 WorkBuddy 风格：列表常驻，条目上的「…」点开才有「重命名 / 删除」。
// 迁移后的行为契约不变，只是入口变了 —— 这里把原来锁在 ConsolePanel 上的
// 那组断言原样迁过来，并适配新的交互路径。
import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { nextTick } from 'vue'
import SessionsSidebar from '../components/SessionsSidebar.vue'
import { store, push } from '../store.js'

function makeApp(impl = {}) {
  const calls = []
  const base = {
    ListSessions: async () => [
      { id: 'default', name: '', turns: 0, archivedTurns: 0, isDefault: true }
    ],
    CreateSession: async () => ({ id: 's9', name: '新会话', turns: 0, archivedTurns: 0, isDefault: false }),
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

function mountSidebar(impl = {}) {
  const made = makeApp(impl)
  calls = made.calls
  window.go = { main: { App: made.app } }
  wrapper = mount(SessionsSidebar, { attachTo: document.body })
  return wrapper
}

beforeEach(() => {
  resetStore()
  delete window.go
})

afterEach(() => {
  if (wrapper) wrapper.unmount()
  wrapper = null
  delete window.go
})

describe('SessionsSidebar 会话列表', () => {
  const SESSIONS = [
    { id: 'default', name: '', turns: 0, archivedTurns: 0, isDefault: true },
    { id: 's1', name: 'nginx 排查', turns: 4, archivedTurns: 0, isDefault: false },
    { id: 's2', name: '磁盘排查', turns: 0, archivedTurns: 0, isDefault: false }
  ]
  const copy = () => SESSIONS.map(s => ({ ...s }))

  // 会话列表由 refreshSessions 填，所以这里直接预置好 store.sessions，
  // 模拟「列表已经拉回来了」这个正常状态。
  function mountWithHost(impl = {}) {
    store.hosts = [{ id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' }]
    store.currentHostId = 'h1'
    store.sessions = { h1: copy() }
    store.currentSessionId = 'default'
    return mountSidebar(impl)
  }

  const items = () => wrapper.findAll('.ssb-item')
  const openMenu = async s => {
    const item = items()
      .filter(w => w.text().includes(s))
      .find(w => w.find('.ssb-more').exists())
    await item.find('.ssb-more').trigger('click')
    await nextTick()
    return item
  }

  it('把该主机的任务列出来，默认任务用主机名顶替并排第一', () => {
    mountWithHost()
    const list = items()
    expect(list).toHaveLength(3)
    // 默认任务没有自己的名字，显示「主机名 (user@addr)」以区分不同节点。
    expect(list[0].text()).toContain('web (root@10.0.0.1)')
    expect(list[1].text()).toContain('nginx 排查')
  })

  // 轮数必须显示出来：用户需要一眼看出哪条会话有上下文 ——
  // 这直接关系到「该清空哪条」「该压缩哪条」。
  it('条目里带上轮数，让用户看出哪条有上下文', () => {
    mountWithHost()
    const list = items()
    expect(list[1].text()).toContain('4 轮')
    expect(list[2].text(), '0 轮的会话不必显示轮数，免得噪声').not.toContain('轮')
  })

  it('点击条目切换当前会话', async () => {
    mountWithHost()
    await items()[2].trigger('click')
    expect(store.currentSessionId).toBe('s2')
  })

  // 重命名走应用内弹窗（与新建/删除同形式）：弹窗里预填当前名字。
  it('点「…→重命名」预填当前名字，Enter 后把两个 ID 都发给后端', async () => {
    mountWithHost({
      ListSessions: async () => [copy()[0], { id: 's1', name: 'nginx 与证书', turns: 4, isDefault: false }, copy()[2]]
    })
    const item = await openMenu('nginx 排查')

    await item.find('.ssb-menu button').trigger('click')
    await nextTick()
    const input = wrapper.find('.modal input')
    expect(input.element.value, '应预填当前名字，让用户在原名上改').toBe('nginx 排查')

    await input.setValue('nginx 与证书')
    await input.trigger('keydown', { key: 'Enter' })
    await flushPromises()

    const c = calls.filter(x => x.name === 'RenameSession')
    expect(c).toHaveLength(1)
    expect(c[0].args).toEqual(['h1', 's1', 'nginx 与证书'])
  })

  it('重命名空名字不打后端', async () => {
    mountWithHost()
    const item = await openMenu('nginx 排查')

    await item.find('.ssb-menu button').trigger('click')
    await nextTick()
    await wrapper.find('.modal input').setValue('  ')
    await wrapper.find('.modal input').trigger('keydown', { key: 'Enter' })
    await flushPromises()

    expect(calls.filter(x => x.name === 'RenameSession')).toHaveLength(0)
  })

  it('重命名 Esc 取消，不打后端', async () => {
    mountWithHost()
    const item = await openMenu('nginx 排查')

    await item.find('.ssb-menu button').trigger('click')
    await nextTick()
    await wrapper.find('.modal input').trigger('keydown', { key: 'Escape' })
    await nextTick()

    expect(wrapper.find('.modal').exists()).toBe(false)
    expect(calls.filter(x => x.name === 'RenameSession')).toHaveLength(0)
  })

  // 删掉一整段排查过程是不可撤销的（后端删完立刻落盘），
  // 所以必须先让用户看清自己要删的是哪一条。
  it('点「…→删除」先弹应用内确认框，确认后带上两个 ID 调后端', async () => {
    mountWithHost()
    const item = await openMenu('nginx 排查')

    const danger = item.findAll('.ssb-menu button')[1]
    await danger.trigger('click')
    await nextTick()

    // 确认框里要出现会话名，用户才知道删的是哪条。
    const modal = wrapper.find('.modal')
    expect(modal.exists()).toBe(true)
    expect(modal.text()).toContain('nginx 排查')

    await modal.findAll('button')
      .find(b => b.text() === '确认删除')
      .trigger('click')
    await flushPromises()

    const c = calls.filter(x => x.name === 'DeleteSession')
    expect(c).toHaveLength(1)
    expect(c[0].args).toEqual(['h1', 's1'])
  })

  it('确认框里点了取消就不删', async () => {
    mountWithHost()
    const item = await openMenu('nginx 排查')

    await item.findAll('.ssb-menu button')[1].trigger('click')
    await nextTick()
    await wrapper.findAll('.modal button')
      .find(b => b.text() === '取消')
      .trigger('click')
    await nextTick()

    expect(calls.filter(x => x.name === 'DeleteSession')).toHaveLength(0)
  })

  // 默认任务删不掉、也不允许重命名 —— 它的名字就是节点标识
  //（主机名 (user@addr)），所以连「…」按钮都不给，列表更干净。
  it('默认任务不显示「…」按钮，具名任务显示', () => {
    mountWithHost()
    const list = items()
    expect(list[0].find('.ssb-more').exists()).toBe(false)
    expect(list[1].find('.ssb-more').exists()).toBe(true)
  })

  // 新建入口在控制台顶栏（ConsolePanel.test.js 覆盖），
  // 侧边栏只负责列表与重命名/删除。
  it('没选主机时给出提示', () => {
    store.hosts = []
    store.currentHostId = ''
    store.currentSessionId = ''
    store.sessions = {}
    mountSidebar({})

    expect(wrapper.text()).toContain('选择主机')
  })
})
