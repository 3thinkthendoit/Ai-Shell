// 「一台主机多条会话」的前端状态机测试。
//
// 这一层的核心风险是**时间线串了**：用户在 A 会话里看到的却是 B 会话的内容，
// 或者输出落进了 A 会话而他正在看 B。这类 bug 不会报错，
// 只会让人怀疑「模型怎么答非所问」—— 所以必须逐条钉死。
import { describe, it, expect, beforeEach } from 'vitest'
import {
  store, push, clearSession, compactSession, refreshSessions, refreshHosts, bindEvents, ask,
  createSession, renameSession, deleteSession, selectSession, bucketKey
} from '../store.js'

// ---- 测试替身 ----

function makeRuntime() {
  const handlers = new Map()
  return {
    EventsOn(name, cb) {
      if (!handlers.has(name)) handlers.set(name, [])
      handlers.get(name).push(cb)
    },
    emit(name, payload) {
      const list = handlers.get(name) || []
      if (!list.length) throw new Error(`没有监听者的事件被触发: ${name}`)
      list.forEach(cb => cb(payload))
    }
  }
}

function makeApp(impl = {}) {
  const calls = []
  const base = {
    ListHosts: async () => [],
    ListSessions: async () => [defaultSession()],
    Ask: async () => undefined,
    ClearSession: async () => ({ cleared: 1, busy: false }),
    CompactSession: async () => ({ compacted: true, message: '已压缩。' }),
    CreateSession: async () => namedSession('s1', '新会话'),
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

const defaultSession = (turns = 0) => ({
  id: 'default', name: '', turns, archivedTurns: 0, isDefault: true
})
const namedSession = (id, name, turns = 0) => ({
  id, name, turns, archivedTurns: 0, isDefault: false
})

// 桶键一律用 bucketKey 生成。手写 'h1\x00s2' 这种字面量的话，
// 分隔符一改，断言就会静默地指向一个空桶 —— 测试照样绿，但什么也没测。
const at = (hostId, sessionId) => bucketKey(hostId, sessionId)

function resetStore() {
  store.hosts = []
  store.currentHostId = ''
  store.currentSessionId = ''
  store.sessions = {}
  store.bySession = {}
  store.activeHostId = ''
  store.activeSessionId = ''
  store.pending = null
  store.running = false
  store.compacting = false
}

let rt
beforeEach(() => {
  resetStore()
  delete window.go
  delete window.runtime
  rt = makeRuntime()
})

function install(app) {
  window.go = { main: { App: app } }
  window.runtime = rt
}

// ---- 桶键 ----

describe('bucketKey 拼接时间线的键', () => {
  // 这是整块功能的地基。用 ':' 之类的可读分隔符拼键就会撞 ——
  // 主机 'a' + 会话 'b:c' 与主机 'a:b' + 会话 'c' 拼出同一个字符串，
  // 表现是「两台主机的对话混在一条时间线上」，且只在特定 ID 组合下出现。
  it('不会因为 ID 里含分隔符而撞键', () => {
    expect(bucketKey('a', 'b:c')).not.toBe(bucketKey('a:b', 'c'))
    expect(bucketKey('a:b', 'c')).not.toBe(bucketKey('a', 'b') + ':c')
    expect(bucketKey('a', 's')).not.toBe(bucketKey('a', 'S'))
  })

  // 空会话 ID 与「默认会话」必须落到同一个桶。
  //
  // 不归一化的话，「会话列表还没拉回来时产生的提示」会落进一个
  // 与默认会话不同的桶里 —— 等列表回来、currentSessionId 变成真实 ID 之后，
  // 那条提示就再也读不到了，用户看不到任何报错。
  it('空会话 ID 与默认会话是同一个桶', () => {
    expect(bucketKey('h1', '')).toBe(bucketKey('h1', 'default'))
    expect(bucketKey('h1')).toBe(bucketKey('h1', 'default'))
  })

  it('没有主机时也有一个稳定的兜底桶', () => {
    expect(bucketKey('', '')).toBe(bucketKey('', 'default'))
    expect(bucketKey('', '')).not.toBe(bucketKey('h1', ''))
  })
})

// ---- 会话列表 ----

describe('refreshSessions 拉取会话列表', () => {
  it('把列表存进 store，并把选中的会话补成默认会话的真实 ID', async () => {
    const { app } = makeApp({ ListSessions: async () => [defaultSession(2), namedSession('s1', 'nginx 排查')] })
    install(app)
    store.currentHostId = 'h1'

    await refreshSessions('h1')

    expect(store.currentSessions.map(s => s.id)).toEqual(['default', 's1'])
    // 关键：初始的空串必须被换成后端报回来的那个 ID。
    // 停在空串上的话，选择器里没有任何 option 与之匹配 —— 界面表现为「选择器是空的」，
    // 而用户明明有会话可用。
    expect(store.currentSessionId).toBe('default')
  })

  it('选中的会话已被删掉时回落到默认会话', async () => {
    const { app } = makeApp({ ListSessions: async () => [defaultSession(), namedSession('s2', '还在的')] })
    install(app)
    store.currentSessionId = 's-gone'

    await refreshSessions('h1')

    expect(store.currentSessionId).toBe('default')
  })

  it('选中的会话仍在列表里时不动它', async () => {
    const { app } = makeApp({ ListSessions: async () => [defaultSession(), namedSession('s2', '还在的')] })
    install(app)
    store.currentSessionId = 's2'

    await refreshSessions('h1')

    expect(store.currentSessionId).toBe('s2')
  })

  // 拉列表失败时**不能**把界面清空：用户会以为自己所有会话都没了。
  // 保留上一次的结果，同时把失败说出来。
  it('拉取失败时保留上一次的列表，并给出可见的错误', async () => {
    install(makeApp({ ListSessions: async () => [defaultSession(), namedSession('s1', '保住的')] }).app)
    store.currentHostId = 'h1'
    await refreshSessions('h1')
    expect(store.currentSessions).toHaveLength(2)

    install(makeApp({ ListSessions: async () => { throw new Error('bridge down') } }).app)
    await refreshSessions('h1')

    expect(store.currentSessions, '失败不该把列表清空').toHaveLength(2)
    expect(store.entries.at(-1).kind).toBe('error')
    expect(store.entries.at(-1).content).toContain('bridge down')
  })

  it('没有主机时不做任何请求', async () => {
    const { app, calls } = makeApp()
    install(app)
    await refreshSessions('')
    expect(calls.filter(c => c.name === 'ListSessions')).toHaveLength(0)
    expect(store.currentSessionId).toBe('')
  })

  // 会话被删掉之后，它那条时间线必须一起清掉。
  // 不清的话，用户再新建一条恰好拿到同一个 ID 时，屏幕上会冒出上一次的对话 ——
  // 后端是干净的，只有界面在骗人。
  it('列表里已经没有的会话，其时间线被剪掉', async () => {
    install(makeApp({ ListSessions: async () => [defaultSession(), namedSession('s1', '甲'), namedSession('s2', '乙')] }).app)
    store.currentHostId = 'h1'
    store.currentSessionId = 's1'
    push({ kind: 'user', content: '甲的问' })
    store.currentSessionId = 's2'
    push({ kind: 'user', content: '乙的问' })

    // s2 被删了。
    install(makeApp({ ListSessions: async () => [defaultSession(), namedSession('s1', '甲')] }).app)
    await refreshSessions('h1')

    expect(store.bySession[at('h1', 's1')], '还在的会话不能被误剪').toHaveLength(1)
    expect(store.bySession[at('h1', 's2')], '已删会话的时间线应被剪掉').toBeUndefined()
  })
})

// ---- 时间线隔离 ----

describe('会话时间线互相隔离', () => {
  it('切换会话后看到的是那条会话自己的记录', () => {
    store.currentHostId = 'h1'
    store.currentSessionId = 's1'
    push({ kind: 'user', content: '甲的问' })

    store.currentSessionId = 's2'
    expect(store.entries, '切过去时不该看到另一条会话的内容').toEqual([])
    push({ kind: 'user', content: '乙的问' })

    store.currentSessionId = 's1'
    expect(store.entries.map(e => e.content)).toEqual(['甲的问'])
  })

  it('同一台主机的默认会话与具名会话互不影响', () => {
    store.currentHostId = 'h1'
    store.currentSessionId = ''
    push({ kind: 'user', content: '默认的问' })

    store.currentSessionId = 's1'
    expect(store.entries).toEqual([])
    push({ kind: 'user', content: '具名的问' })

    store.currentSessionId = ''
    expect(store.entries.map(e => e.content)).toEqual(['默认的问'])
  })

  // 后端事件里不带会话信息，所以「这一轮属于哪条会话」必须在 ask() 时记下。
  // 用 currentSessionId 归档的话，跑到一半切换会话就会把输出写进另一条时间线。
  it('一轮跑到一半切换会话，后续事件仍归档到原来那条', async () => {
    const { app } = makeApp()
    install(app)
    bindEvents()

    store.currentHostId = 'h1'
    store.currentSessionId = 's1'
    await ask('看看磁盘')
    store.currentSessionId = 's2'   // 用户中途切走

    rt.emit('agent:message', { role: 'user', content: '看看磁盘' })
    rt.emit('agent:delta', { step: 0, text: '磁盘' })
    rt.emit('agent:message', { role: 'assistant', content: '磁盘充足', step: 0 })

    expect(store.bySession[at('h1', 's1')], '应落在发起那一轮的时间线上').toHaveLength(2)
    expect(store.bySession[at('h1', 's2')], '不该凭空多出记录').toBeUndefined()
    expect(store.entries, '当前看到的那条仍是空的').toEqual([])
  })

  it('每条时间线的 500 条上限各自计算', () => {
    store.currentHostId = 'h1'
    store.currentSessionId = 's1'
    for (let i = 0; i < 620; i++) push({ kind: 'system', content: `n${i}` })
    expect(store.entries).toHaveLength(500)

    store.currentSessionId = 's2'
    push({ kind: 'system', content: '另一条的第一条' })
    expect(store.entries).toHaveLength(1)
    expect(store.bySession[at('h1', 's1')]).toHaveLength(500)
  })
})

// ---- 清空 / 压缩 ----

describe('清空与压缩都作用于指定的那条会话', () => {
  // 只断言主机 id 的话，「永远传空会话」这种 bug 会照样绿 ——
  // 而那正是「清空清错了会话」的成因。
  it('清空的提示落在被清空的那条时间线上', async () => {
    const { app, calls } = makeApp({ ClearSession: async () => ({ cleared: 3, busy: false }) })
    install(app)
    store.currentHostId = 'h1'
    store.currentSessionId = 's2'

    await clearSession('h1', 's1')

    expect(calls[0].args).toEqual(['h1', 's1'])
    expect(store.bySession[at('h1', 's1')].at(-1).content).toContain('3 轮')
    expect(store.bySession[at('h1', 's2')], '当前在看的会话不该被写进提示').toBeUndefined()
  })

  it('压缩的提示也落在指定的那条时间线上', async () => {
    const { app, calls } = makeApp({ CompactSession: async () => ({ compacted: true, message: '已压缩 5 轮。' }) })
    install(app)
    store.currentHostId = 'h1'
    store.currentSessionId = 's2'

    await compactSession('h1', 's1')

    expect(calls[0].args).toEqual(['h1', 's1'])
    expect(store.bySession[at('h1', 's1')].at(-1).content).toContain('已压缩 5 轮')
  })
})

// ---- 增删改 ----

describe('会话的增删改', () => {
  it('selectSession 切换当前会话，空值落到默认会话', () => {
    selectSession('s3')
    expect(store.currentSessionId).toBe('s3')
    selectSession('')
    expect(store.currentSessionId).toBe('')
  })

  // 新建之后必须**切过去**：用户的意图就是「开一段新对话」，
  // 建完还停在旧会话上，他会以为新建失败了。
  it('新建会话后自动切过去，并在新会话里留下提示', async () => {
    const { app, calls } = makeApp({
      ListSessions: async () => [defaultSession(), namedSession('s9', '磁盘排查')],
      CreateSession: async () => namedSession('s9', '磁盘排查')
    })
    install(app)
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'

    await createSession('磁盘排查')

    expect(calls[0].args).toEqual(['h1', '磁盘排查'])
    expect(store.currentSessionId).toBe('s9')
    expect(store.bySession[at('h1', 's9')].at(-1).content).toContain('磁盘排查')
  })

  it('新建失败时把原因显示出来，且不切换会话', async () => {
    const { app } = makeApp({
      CreateSession: async () => { throw new Error('这台主机最多只能有 20 条会话') }
    })
    install(app)
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'

    await createSession('再来一条')

    expect(store.currentSessionId, '失败时不该切走').toBe('default')
    expect(store.entries.at(-1).kind).toBe('error')
    expect(store.entries.at(-1).content).toContain('20 条会话')
  })

  it('改名把新名字与两个 ID 一起发给后端', async () => {
    const { app, calls } = makeApp({
      ListSessions: async () => [defaultSession(), namedSession('s1', '新名字')]
    })
    install(app)
    store.currentHostId = 'h1'

    await renameSession('s1', '新名字')

    expect(calls[0].args).toEqual(['h1', 's1', '新名字'])
    expect(store.currentSessions.find(s => s.id === 's1').name).toBe('新名字')
  })

  it('删掉当前选中的会话后回落到默认会话', async () => {
    const { app } = makeApp({
      ListSessions: async () => [defaultSession()]
    })
    install(app)
    store.currentHostId = 'h1'
    store.currentSessionId = 's1'

    await deleteSession('s1')

    expect(store.currentSessionId).toBe('default')
  })

  // 上一条用例里，回落其实是**刷新的兜底**顺手做的（列表里没有 s1 了，
  // refreshSessions 就把它修正掉）。所以删会话时那句显式回落看起来可有可无 ——
  // 变异测试实测：把它删掉，上一条用例照样绿。
  //
  // 但它并非多余：refreshSessions 拉列表**失败**时会提前返回、不做修正。
  // 那种情况下没有这句显式回落，界面就会停在一个已经不存在的会话上 ——
  // 选择器里没有任何 option 与之匹配，用户看到一片空白却不知为什么。
  it('删掉当前会话后，即便紧接着拉列表失败也已离开那条会话', async () => {
    let deleted = false
    install(makeApp({
      DeleteSession: async () => { deleted = true },
      ListSessions: async () => {
        if (deleted) throw new Error('bridge down')
        return [defaultSession(), namedSession('s1', '待删的')]
      }
    }).app)
    store.currentHostId = 'h1'
    await refreshSessions('h1')
    store.currentSessionId = 's1'

    await deleteSession('s1')

    // 空串就是「默认会话」的表示（bucketKey 会把它归一化到默认会话的桶），
    // 所以这里断言的是「已经离开了那条被删掉的会话」。
    expect(store.currentSessionId, '不能停在一个已删除的会话上').not.toBe('s1')
    expect(store.currentSessionId).toBe('')
  })

  // 删掉**别的**会话时，当前选中项不该被动 —— 用户只是在清理列表，
  // 突然被弹回默认会话会让他以为点错了。
  it('删掉别的会话不影响当前选中的那条', async () => {
    const { app } = makeApp({
      ListSessions: async () => [defaultSession(), namedSession('s1', '留着的')]
    })
    install(app)
    store.currentHostId = 'h1'
    store.currentSessionId = 's1'

    await deleteSession('s2')

    expect(store.currentSessionId).toBe('s1')
  })

  // 后端会拒绝删除默认会话。前端不预判，但必须把原因显示出来 ——
  // 否则用户点了「删除」什么都没发生。
  it('后端拒绝删除默认会话时，原因要显示出来', async () => {
    const { app } = makeApp({
      DeleteSession: async () => { throw new Error('默认会话不能删除，用「清空上下文」把它清空即可') }
    })
    install(app)
    store.currentHostId = 'h1'
    store.currentSessionId = 'default'

    await deleteSession('default')

    expect(store.entries.at(-1).kind).toBe('error')
    expect(store.entries.at(-1).content).toContain('默认会话不能删除')
  })

  it('没有选中主机时增删改都不打后端', async () => {
    const { app, calls } = makeApp()
    install(app)
    store.currentHostId = ''

    await createSession('x')
    await renameSession('s1', 'x')
    await deleteSession('s1')

    for (const name of ['CreateSession', 'RenameSession', 'DeleteSession']) {
      expect(calls.filter(c => c.name === name), `${name} 不该被调用`).toHaveLength(0)
    }
    expect(store.entries.at(-1).kind).toBe('error')
  })
})

// ---- 主机刷新时的连带清理 ----

describe('refreshHosts 的连带清理', () => {
  it('主机被删掉时，它的会话列表与所有时间线一起清掉', async () => {
    const { app } = makeApp({
      ListHosts: async () => [{ id: 'h2' }],
      ListSessions: async () => [defaultSession(), namedSession('s1', '甲')]
    })
    install(app)

    store.currentHostId = 'h1'
    store.currentSessionId = 's1'
    push({ kind: 'user', content: 'h1 的会话' })
    store.sessions = { h1: [defaultSession(), namedSession('s1', '甲')] }

    await refreshHosts()

    expect(store.currentHostId).toBe('h2')
    expect(store.bySession[at('h1', 's1')], 'h1 已不存在，它的时间线该被清掉').toBeUndefined()
    expect(store.sessions.h1, 'h1 的会话列表也该被清掉').toBeUndefined()
  })
})
