// 文件管理弹窗的状态机测试。
//
// 这一层测的是「后端事件 → store 状态 → 弹窗行为」的流转，特别是
// 两条最容易写错、又最难从界面上察觉的路径：
//   1. term:cwd 事件到达时，弹窗要跟着换目录（否则用户在终端里 cd 了，
//      弹窗还停在旧目录，看起来像没刷新）；
//   2. 目录状态必须按「终端表面」而不是「主机」分开 —— 同一台主机可以
//      有多条任务，各停在不同目录，混用一个全局值会打开到别人的目录。
import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import {
  store, bindEvents, bucketKey,
  openFileManager, closeFileManager, loadDir, fmEnter, fmGoParent, fmDelete
} from '../store.js'
const at = (hostId, sessionId) => bucketKey(hostId, sessionId)

function makeRuntime() {
  const handlers = new Map()
  return {
    EventsOn(name, cb) {
      if (!handlers.has(name)) handlers.set(name, [])
      handlers.get(name).push(cb)
    },
    EventsOff(name) { handlers.delete(name) },
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
    ListRemoteDir: async () => ({ ok: true, path: '/home/test', parent: '/home', entries: [] }),
    DeleteRemotePath: async () => ({ ok: true, message: '已删除' }),
    UploadRemoteFile: async () => ({ ok: true, message: '已上传' }),
    DownloadRemoteFile: async () => ({ ok: true, message: '' })
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
  store.termCwd = {}
  store.maxTransferBytes = 32 * 1024 * 1024
  store.fileManager = {
    open: false, key: '', hostId: '', sessionId: '', cwd: '', parent: '',
    entries: [], loading: false, error: '', busy: false, notice: ''
  }
  store.hosts = []
}

let rt
beforeEach(() => {
  resetStore()
  rt = makeRuntime()
  window.runtime = rt
})

afterEach(() => {
  window.go = undefined
  window.runtime = undefined
})

describe('文件管理 目录状态', () => {
  it('term:cwd 事件按终端表面记录目录', () => {
    window.go = { main: { App: makeApp().app } }
    bindEvents()

    rt.emit('term:cwd', { hostId: 'h1', sessionId: 's1', cwd: '/var/log' })
    rt.emit('term:cwd', { hostId: 'h1', sessionId: 's2', cwd: '/etc' })

    // 同一台主机的两条任务各记各的 —— 混用会让「切到另一条任务再打开
    // 文件管理」定位到别人的目录。
    expect(store.termCwd[at('h1', 's1')]).toBe('/var/log')
    expect(store.termCwd[at('h1', 's2')]).toBe('/etc')
  })

  it('空 cwd 的事件被忽略（不能让空串覆盖已知目录）', () => {
    window.go = { main: { App: makeApp().app } }
    bindEvents()

    rt.emit('term:cwd', { hostId: 'h1', sessionId: 's1', cwd: '/var/log' })
    rt.emit('term:cwd', { hostId: 'h1', sessionId: 's1', cwd: '' })

    expect(store.termCwd[at('h1', 's1')]).toBe('/var/log')
  })

  it('弹窗开着且看着同一块表面时，term:cwd 会跟着更新浏览目录', () => {
    window.go = { main: { App: makeApp().app } }
    bindEvents()

    store.fileManager.open = true
    store.fileManager.key = at('h1', 's1')
    store.fileManager.cwd = '/var/log'

    rt.emit('term:cwd', { hostId: 'h1', sessionId: 's1', cwd: '/srv' })
    expect(store.fileManager.cwd).toBe('/srv')
  })

  it('弹窗看着别的表面时，term:cwd 不打扰它', () => {
    window.go = { main: { App: makeApp().app } }
    bindEvents()

    store.fileManager.open = true
    store.fileManager.key = at('h1', 's2')
    store.fileManager.cwd = '/etc'

    // s1 的目录变了，但弹窗看的是 s2，不该被改。
    rt.emit('term:cwd', { hostId: 'h1', sessionId: 's1', cwd: '/srv' })
    expect(store.fileManager.cwd).toBe('/etc')
  })
})

describe('文件管理 打开与导航', () => {
  it('打开时用该表面的当前目录（来自 OSC 7）', async () => {
    const { app, calls } = makeApp()
    window.go = { main: { App: app } }
    store.termCwd[at('h1', 's1')] = '/var/log'

    await openFileManager('h1', 's1')

    expect(store.fileManager.open).toBe(true)
    expect(calls[0].name).toBe('ListRemoteDir')
    expect(calls[0].args).toEqual(['h1', '/var/log'])
  })

  it('没收到过目录时传空串，由后端回落到家目录', async () => {
    const { app, calls } = makeApp()
    window.go = { main: { App: app } }

    await openFileManager('h1', 's1')

    // 传空而不是在前端猜一个 ~：落家目录这件事只该有一个处理点。
    expect(calls[0].args).toEqual(['h1', ''])
  })

  it('列目录成功后回写实际落到的目录', async () => {
    const { app } = makeApp({
      ListRemoteDir: async () => ({ ok: true, path: '/root', parent: '/', entries: [] })
    })
    window.go = { main: { App: app } }

    await openFileManager('h1', 's1')

    // 用户在拿不到 OSC 7 时打开弹窗、被回落到家目录，
    // 该记住的是**实际列出的**那个目录。
    expect(store.termCwd[at('h1', 's1')]).toBe('/root')
    expect(store.fileManager.cwd).toBe('/root')
  })

  it('列目录失败时给出错误且不留下半截列表', async () => {
    const { app } = makeApp({
      ListRemoteDir: async () => ({ ok: false, error: '权限不足' })
    })
    window.go = { main: { App: app } }
    store.fileManager.entries = [{ name: 'stale' }]

    await openFileManager('h1', 's1')

    expect(store.fileManager.error).toBe('权限不足')
    expect(store.fileManager.entries).toEqual([])
  })

  it('进入子目录与返回上一级', async () => {
    const { app, calls } = makeApp({
      ListRemoteDir: async (_h, p) => ({ ok: true, path: p || '/home/test', parent: '/', entries: [] })
    })
    window.go = { main: { App: app } }

    await openFileManager('h1', 's1')
    await fmEnter('/var/log')

    expect(calls[calls.length - 1].args).toEqual(['h1', '/var/log'])

    store.fileManager.parent = '/var'
    await fmGoParent()
    expect(calls[calls.length - 1].args).toEqual(['h1', '/var'])
  })

  it('已在根目录时「上一级」不发请求（按钮本就该置灰）', async () => {
    const { app, calls } = makeApp()
    window.go = { main: { App: app } }

    await openFileManager('h1', 's1')
    const n = calls.length
    store.fileManager.parent = '/'
    store.fileManager.cwd = '/'
    await fmGoParent()

    expect(calls.length).toBe(n)
  })
})

describe('文件管理 删除', () => {
  it('删除成功后刷新当前目录', async () => {
    const { app, calls } = makeApp()
    window.go = { main: { App: app } }

    await openFileManager('h1', 's1')
    store.fileManager.cwd = '/srv'
    const n = calls.length

    await fmDelete('/srv/old.txt')

    expect(calls[n].name).toBe('DeleteRemotePath')
    expect(calls[n].args).toEqual(['h1', '/srv/old.txt'])
    // 删完必须重新列：不刷新的话文件还在列表上，用户以为没删掉，会再点一次。
    expect(calls[n + 1].name).toBe('ListRemoteDir')
    expect(store.fileManager.notice).toBe('已删除')
  })

  it('删除失败时展示后端给的原因', async () => {
    const { app } = makeApp({
      DeleteRemotePath: async () => ({ ok: false, error: '设备忙' })
    })
    window.go = { main: { App: app } }

    await openFileManager('h1', 's1')
    await fmDelete('/srv/busy')

    expect(store.fileManager.error).toBe('设备忙')
    expect(store.fileManager.ok).toBeFalsy()
  })

  it('删除期间 busy 为真，结束后复位', async () => {
    let seen = false
    const { app } = makeApp({
      DeleteRemotePath: async () => {
        seen = store.fileManager.busy
        return { ok: true, message: '' }
      }
    })
    window.go = { main: { App: app } }

    await openFileManager('h1', 's1')
    store.fileManager.busy = false
    await fmDelete('/srv/x')

    expect(seen).toBe(true)
    expect(store.fileManager.busy).toBe(false)
  })
})

describe('文件管理 关闭', () => {
  it('关闭会清掉列表与错误，避免下次打开闪出旧内容', async () => {
    const { app } = makeApp()
    window.go = { main: { App: app } }

    await openFileManager('h1', 's1')
    store.fileManager.error = '旧错误'
    store.fileManager.entries = [{ name: 'x' }]

    closeFileManager()

    expect(store.fileManager.open).toBe(false)
    expect(store.fileManager.entries).toEqual([])
    expect(store.fileManager.error).toBe('')
    expect(store.fileManager.key).toBe('')
  })
})

// 这一组守的是两个「界面看起来正常、实际定位错了」的竞态。
// 两者的共同点是：都发生在 await 期间状态被外部改掉之后。
describe('文件管理 竞态', () => {
  // 让 ListRemoteDir 按目录名可控地决定何时返回（先进先出的门闩）。
  function gatedApp() {
    const pending = []
    const { app, calls } = makeApp({
      // 每个请求挂一个门闩，由用例决定何时、以什么路径返回。
      // 返回的 path 就是请求里的 p；传空串时后端会回落到家目录，
      // 这里显式回一个家目录，避免用例断言到空串而误以为是竞态导致的。
      ListRemoteDir: (hostId, p) => new Promise(resolve => {
        const path = p || '/home/test'
        pending.push({ p, path, resolve: () => resolve({ ok: true, path, parent: '/', entries: [{ name: path }] }) })
      }),
      DeleteRemotePath: async () => ({ ok: true, message: '' })
    })
    return { app, calls, pending }
  }

  it('慢请求后返回时不会覆盖后点目录的结果', async () => {
    // 用户点了 /slow（慢），又点了 /fast（快）。/fast 先回，/slow 后回。
    const { app, pending } = gatedApp()
    window.go = { main: { App: app } }

    const first = openFileManager('h1', 's1')   // 发出 /slow 之前的首屏请求
    // 首屏请求先放行，弹窗才有 cwd 可点
    pending[0].resolve()
    await first

    // 连点两次：先进 /slow，立刻改点 /fast
    const slow = fmEnter('/slow')
    const fast = fmEnter('/fast')

    // /fast 先返回
    expect(pending[1].p).toBe('/slow')
    expect(pending[2].p).toBe('/fast')
    pending[2].resolve()
    await fast
    expect(store.fileManager.cwd).toBe('/fast')

    // /slow 迟到。它必须被丢弃 —— 否则列表会「自己跳回」/slow，
    // 而用户明明点的是 /fast。
    pending[1].resolve()
    await slow
    expect(store.fileManager.cwd).toBe('/fast')
    expect(store.fileManager.entries.map(e => e.name)).toEqual(['/fast'])
  })

  it('关闭弹窗后，在途请求不会写回 termCwd', async () => {
    const { app, pending } = gatedApp()
    window.go = { main: { App: app } }

    const opening = openFileManager('h1', 's1')
    pending[0].resolve()
    await opening

    // 发起一次导航，然后立刻关掉弹窗
    const nav = fmEnter('/var/log')
    closeFileManager()

    // 请求迟到返回：不能把 /var/log 写进任何地方的 termCwd
    pending[1].resolve()
    await nav

    expect(store.termCwd[at('h1', 's1')]).not.toBe('/var/log')
    expect(store.fileManager.cwd).not.toBe('/var/log')
  })

  it('切换任务时，旧任务的迟到结果不会污染新任务的目录', async () => {
    // 这是最难在界面上察觉的一种：s1 的请求返回时，弹窗已经在看 s2。
    const { app, pending } = gatedApp()
    window.go = { main: { App: app } }

    const opening1 = openFileManager('h1', 's1')
    pending[0].resolve()
    await opening1
    expect(pending.length).toBe(1)

    // s1 上发起一次导航（还没回来）
    const nav = fmEnter('/s1-only')
    expect(pending[1].p).toBe('/s1-only')

    // 用户切到 s2 打开弹窗（s2 没有 termCwd，于是传空路径）
    const opening2 = openFileManager('h1', 's2')
    expect(pending[2].p).toBe('')

    // 让 s2 的请求**先**返回，弹窗正确定位；此时 s1 的还在路上。
    // （空路径在后端会回落到家目录，见 gatedApp 的注释。）
    pending[2].resolve()
    await opening2
    expect(store.fileManager.cwd).toBe('/home/test')

    // s1 的请求此刻返回 —— 它属于另一块表面，必须被丢弃，
    // 既不能改弹窗当前目录，也不能写进 s1 的 termCwd。
    pending[1].resolve()
    await nav

    // s1 的结果既没进弹窗、也没进 s1 的 termCwd。
    expect(store.termCwd[at('h1', 's1')]).not.toBe('/s1-only')
    // 弹窗仍停在 s2 首屏列出的目录上（空路径回落家目录），没被 s1 顶掉。
    expect(store.fileManager.cwd).toBe('/home/test')
  })
})

