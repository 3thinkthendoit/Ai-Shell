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
  store.pendingSessionId = ''
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

  it('隐藏零轮默认任务；具名任务照常列出', () => {
    mountWithHost()
    const list = items()
    // copy()[0] 是 turns:0 的默认会话，应被隐藏
    expect(list).toHaveLength(2)
    expect(list[0].text()).toContain('nginx 排查')
    expect(list[1].text()).toContain('磁盘排查')
    expect(wrapper.text()).not.toContain('默认任务')
  })

  it('有轮次的默认会话仍展示为「未命名任务」', async () => {
    mountWithHost()
    store.sessions = {
      h1: [
        { id: 'default', name: '', turns: 3, archivedTurns: 0, isDefault: true },
        { id: 's1', name: 'nginx 排查', turns: 4, archivedTurns: 0, isDefault: false }
      ]
    }
    await nextTick()
    const list = items()
    expect(list).toHaveLength(2)
    expect(list[0].text()).toContain('未命名任务')
    expect(list[0].text()).toContain('3 轮')
  })

  // 轮数必须显示出来：用户需要一眼看出哪条会话有上下文 ——
  // 这直接关系到「该清空哪条」「该压缩哪条」。
  it('条目里带上轮数，让用户看出哪条有上下文', () => {
    mountWithHost()
    const list = items()
    expect(list[0].text()).toContain('4 轮')
    expect(list[1].text(), '0 轮的会话不必显示轮数，免得噪声').not.toContain('轮')
  })

  it('点击条目切换当前会话', async () => {
    mountWithHost()
    await items()[1].trigger('click')
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

  it('具名任务与有轮次的默认任务都显示「…」', async () => {
    mountWithHost()
    store.sessions = {
      h1: [
        { id: 'default', name: '', turns: 3, archivedTurns: 0, isDefault: true },
        { id: 's1', name: 'nginx 排查', turns: 4, archivedTurns: 0, isDefault: false }
      ]
    }
    await nextTick()
    const list = items()
    expect(list[0].find('.ssb-more').exists()).toBe(true)
    expect(list[1].find('.ssb-more').exists()).toBe(true)
  })

  it('默认任务菜单只有删除、没有重命名', async () => {
    mountWithHost()
    store.sessions = {
      h1: [
        { id: 'default', name: '', turns: 4, archivedTurns: 0, isDefault: true }
      ]
    }
    await nextTick()
    const item = await openMenu('未命名任务')
    const labels = item.findAll('.ssb-menu button').map(b => b.text())
    expect(labels).toEqual(['删除'])
  })

  it('删除默认任务会调 DeleteSession 并清本地时间线', async () => {
    mountWithHost({
      ListSessions: async () => [
        { id: 'default', name: '', turns: 0, archivedTurns: 0, isDefault: true }
      ]
    })
    store.sessions = {
      h1: [{ id: 'default', name: '', turns: 4, archivedTurns: 0, isDefault: true }]
    }
    store.bySession = { ['h1\u0000default']: [{ id: 1, kind: 'user', content: 'hi' }] }
    store.currentSessionId = 'default'
    await nextTick()
    const item = await openMenu('未命名任务')
    await item.find('.ssb-menu-danger').trigger('click')
    await nextTick()
    await wrapper.findAll('.modal button').find(b => b.text() === '确认删除').trigger('click')
    await flushPromises()
    const c = calls.filter(x => x.name === 'DeleteSession')
    expect(c).toHaveLength(1)
    expect(c[0].args).toEqual(['h1', 'default'])
    const timeline = store.bySession['h1\u0000default'] || []
    expect(timeline.filter(e => e.kind === 'user')).toHaveLength(0)
  })

  // 新建入口在控制台顶栏（ConsolePanel.test.js 覆盖），
  // 侧边栏只负责列表与重命名/删除。
  it('没选主机也没有主机时，提示暂无任务', () => {
    store.hosts = []
    store.currentHostId = ''
    store.currentSessionId = ''
    store.sessions = {}
    mountSidebar({})

    expect(wrapper.text()).toContain('暂无任务')
  })

  // ---- 分组视图（未选主机时的欢迎态） ----

  describe('按主机分组的任务', () => {
    const HOSTS = [
      { id: 'h1', name: 'web', user: 'root', addr: '10.0.0.1' },
      { id: 'h2', name: 'db', user: 'ubuntu', addr: '10.0.0.2' }
    ]

    function mountGroups() {
      store.hosts = HOSTS.map(h => ({ ...h }))
      store.currentHostId = ''
      store.sessions = {
        h1: copy(),
        h2: [{ id: 'g9', name: '备份迁移', turns: 2, archivedTurns: 0, isDefault: false }]
      }
      return mountSidebar({})
    }

    it('按主机分组隐藏零轮默认任务，组头数量不含它们', () => {
      mountGroups()
      const heads = wrapper.findAll('.ssb-group-head')
      expect(heads).toHaveLength(2)
      expect(heads[0].text()).toContain('web')
      expect(heads[0].text()).toContain('root@10.0.0.1')
      expect(heads[0].text()).toContain('2')
      expect(heads[1].text()).toContain('db')
      expect(heads[1].text()).toContain('1')
      const nested = wrapper.findAll('.ssb-item-nested')
      expect(nested).toHaveLength(3)
      expect(wrapper.text()).not.toContain('默认任务')
      expect(nested[0].text()).toContain('nginx 排查')
      expect(nested[2].text()).toContain('备份迁移')
    })

    it('点组头进入该主机', async () => {
      mountGroups()
      await wrapper.findAll('.ssb-group-head')[1].trigger('click')
      expect(store.currentHostId).toBe('h2')
      expect(store.pendingSessionId).toBe('')
    })

    it('点组内任务直达那条会话：先记意图再切主机', async () => {
      mountGroups()
      const target = wrapper.findAll('.ssb-item-nested')[2]
      await target.trigger('click')
      expect(store.currentHostId).toBe('h2')
      expect(store.pendingSessionId).toBe('g9')
    })
  })
})
