// 文件管理弹窗的状态机测试。
//
// 这一层测的是「后端事件 → store 状态 → 弹窗行为」的流转，特别是
// 两条最容易写错、又最难从界面上察觉的路径：
//   1. term:cwd 事件到达时，弹窗要跟着换目录（否则用户在终端里 cd 了，
//      弹窗还停在旧目录，看起来像没刷新）；
//   2. 目录状态必须按「终端表面」而不是「主机」分开 —— 同一台主机可以
//      有多条任务，各停在不同目录，混用一个全局值会打开到别人的目录。
import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { watch } from 'vue'
import {
  store, bindEvents, bucketKey,
  openFileManager, closeFileManager, loadDir, fmEnter, fmGoParent, fmUpload,
  fmDownload, fmCancelTransfer
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
    UploadRemoteFile: async () => ({ ok: true, message: '已上传' }),
    DownloadRemoteFile: async () => ({ ok: true, message: '' }),
    CancelFileTransfer: async () => true
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
    entries: [], loading: false, error: '', busy: false, notice: '',
    progress: null
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

// 上传进度。
//
// 这一组的价值在于：进度条坏掉时**界面看起来仍然是对的** ——
// 它会显示"上传中"、最后也会成功，只是百分比是 NaN%、或者停在 0%、
// 或者永远不消失。人眼很难发现，所以需要断言把它钉住。
describe('文件管理 上传进度', () => {
  // 造一个能被分块读的假 File：jsdom 的 File 没有真实的字节流，
  // 所以直接给一个带 size/slice 的对象，slice 返回能被 FileReader 读的 Blob。
  function fakeFile(name, size) {
    const bytes = new Uint8Array(size)
    // 填点非零内容，避免全零被误当成"空文件"。
    for (let i = 0; i < size; i++) bytes[i] = i % 251
    const blob = new Blob([bytes])
    return {
      name,
      size,
      // store 里用的是 file.slice(offset, end)，返回的必须是 Blob。
      slice: (s, e) => blob.slice(s, e)
    }
  }

  it('上传过程中 progress 依次经过 reading → sending，结束后清空', async () => {
    const seen = []
    const { app } = makeApp({
      UploadRemoteFile: async () => {
        // 调后端这一刻，进度应该已经是"发送中"。
        seen.push(JSON.parse(JSON.stringify(store.fileManager.progress)))
        return { ok: true, message: '已上传' }
      }
    })
    window.go = { main: { App: app } }
    await openFileManager('h1', 's1')

    // 3 MiB：会分成多个块，确保 reading 阶段真的推进过多次。
    await fmUpload(fakeFile('big.bin', 3 * 1024 * 1024))

    // 结束时必须清空，否则下次打开弹窗会看到上次残留的进度条。
    expect(store.fileManager.progress, '结束后应清空进度').toBeNull()
    expect(seen.length).toBe(1)
    expect(seen[0].phase, '调后端时进度应处于发送阶段').toBe('sending')
    expect(seen[0].name).toBe('big.bin')
    expect(seen[0].total).toBe(3 * 1024 * 1024)
  })

  it('reading 阶段的 loaded 单调递增，且最终恰好等于文件大小', async () => {
    // 这里刻意**采样整个读取过程**，而不是只看结束时那个值。
    //
    // 只看终值是不够的：把回调改成恒报 1，终值断言照样通过，
    // 而进度条会永远停在 0% —— 一个断言看不见的真故障。
    // 所以要抓住"途中每一帧"，验证它确实在往上走、每步都在推进。
    const frames = []
    const { app } = makeApp({
      UploadRemoteFile: async () => ({ ok: true, message: '' })
    })
    window.go = { main: { App: app } }
    await openFileManager('h1', 's1')

    const size = 3 * 1024 * 1024 // 3 块（块大小 1 MiB）
    // 逐帧记录读取过程的进度。
    //
    // 不能用普通的 Proxy 去替换 store.fileManager —— store 是 reactive() 的，
    // Vue 会把赋进去的对象**再包一层自己的 proxy**，我们那个 set 陷阱永远
    // 不会被调用（写进去的是 Vue 的代理）。用 Vue 自己的 watch 才是对的：
    // 它在每次 fm.progress 被替换时同步回调。
    // flush:'sync' 很重要：默认是异步批处理的，而读取是"同步循环里一路
    // 替换 progress 对象"，异步回调只会在最后跑一次，中间那些帧全丢。
    // 要看到真实的推进过程就必须同步回调。
    const stop = watch(() => store.fileManager.progress, p => {
      if (p) frames.push({ ...p })
    }, { flush: 'sync' })

    await fmUpload(fakeFile('mid.bin', size))
    stop()

    const reading = frames.filter(f => f.phase === 'reading')
    expect(reading.length, '读取阶段应产生多次进度更新').toBeGreaterThanOrEqual(3)

    // 第 0 帧是上传开始时置的 loaded:0（进度条从 0 出发是对的），
    // 真正的推进从第 1 帧起 —— 断言对象是"推进序列"，不是"含初始值的序列"。
    const advanced = reading.slice(1)
    expect(advanced.length, '至少要有一次实际推进').toBeGreaterThanOrEqual(1)

    // 1) 首帧推进必须 > 0：第一块读完就该有可见进展，否则进度条看着像没动。
    expect(advanced[0].loaded, '第一块读完就该有进展').toBeGreaterThan(0)
    // 2) 严格递增：任何一次回退都说明 offset 记错了。
    for (let i = 1; i < advanced.length; i++) {
      expect(advanced[i].loaded, '进度不能回退').toBeGreaterThan(advanced[i - 1].loaded)
    }
    // 3) 终值精确等于文件大小：差一点就会停在 99%。
    expect(advanced[advanced.length - 1].loaded, '读完时 loaded 应恰好等于总大小').toBe(size)
    expect(reading.every(f => f.total === size), 'total 应始终是文件大小').toBe(true)
  })

  it('上传失败时也要清空进度（否则进度条会永远停在那里）', async () => {
    const { app } = makeApp({
      UploadRemoteFile: async () => ({ ok: false, error: '磁盘满了' })
    })
    window.go = { main: { App: app } }
    await openFileManager('h1', 's1')

    await fmUpload(fakeFile('x.bin', 1024))

    expect(store.fileManager.progress, '失败路径同样要清空进度').toBeNull()
    expect(store.fileManager.error).toBe('磁盘满了')
    expect(store.fileManager.busy).toBe(false)
  })

  it('超过上限的文件在开始前就被拒绝，不会出现进度', async () => {
    const { app, calls } = makeApp()
    window.go = { main: { App: app } }
    await openFileManager('h1', 's1')
    store.maxTransferBytes = 1024

    await fmUpload(fakeFile('huge.bin', 4096))

    expect(store.fileManager.error).toContain('超过上限')
    // 关键：连读都不该读 —— 进度条一闪而过也算一种失败体验，
    // 而且白读了 4KiB（真实场景里是几百 MB）。
    expect(store.fileManager.progress).toBeNull()
    expect(calls.filter(c => c.name === 'UploadRemoteFile').length).toBe(0)
  })

  it('小文件也能走完进度（不能因为只有一块就卡在 0%）', async () => {
    // 这是真会发生的：早先用 FileReader 的 progress 事件时，
    // 几百字节的文件可能一次 progress 都不触发，只有 onload，
    // 于是界面上一直显示 0%。改成按块读之后必须解决这一点。
    let atSend = null
    const { app } = makeApp({
      UploadRemoteFile: async () => {
        atSend = { ...store.fileManager.progress }
        return { ok: true, message: '' }
      }
    })
    window.go = { main: { App: app } }
    await openFileManager('h1', 's1')

    await fmUpload(fakeFile('tiny.txt', 10))

    expect(atSend.loaded, '小文件的 loaded 必须等于它的大小').toBe(10)
    expect(atSend.total).toBe(10)
  })
})

// 下载进度与取消。
//
// 下载已经不在前端做（不再 <a download>）：前端发一次请求，后端分块
// 落盘并逐块推 fm:transfer 事件。这一组守的是「事件 → 进度条」的接线，
// 以及取消按钮真的把 id 还给了后端。
describe('文件管理 下载', () => {
  it('fm:transfer 事件驱动 downloading 进度（含 id 与速度）', async () => {
    window.go = { main: { App: makeApp().app } }
    bindEvents()
    await openFileManager('h1', 's1')

    rt.emit('fm:transfer', { kind: 'download', id: 't1', name: 'big.bin', done: 524288, total: 2621440, bps: 900000 })

    const p = store.fileManager.progress
    expect(p.phase, '应进入下载相位（与上传共用进度条渲染）').toBe('downloading')
    expect(p.id).toBe('t1')
    expect(p.loaded).toBe(524288)
    expect(p.total).toBe(2621440)
    expect(p.bps).toBe(900000)
    expect(p.name).toBe('big.bin')
  })

  it('传输完成后进度清空且 notice 带保存路径', async () => {
    const { app } = makeApp({
      DownloadRemoteFile: async () => ({ ok: true, message: '/Users/me/Downloads/app.conf' })
    })
    window.go = { main: { App: app } }
    await openFileManager('h1', 's1')

    await fmDownload('/srv/app.conf', 'app.conf')

    expect(store.fileManager.error).toBe('')
    expect(store.fileManager.notice).toContain('/Users/me/Downloads/app.conf')
    expect(store.fileManager.progress, '结束后应清空进度').toBeNull()
    expect(store.fileManager.busy).toBe(false)
  })

  it('用户在保存对话框取消时静默返回：不弹错误也不弹成功', async () => {
    const { app } = makeApp({
      DownloadRemoteFile: async () => ({ cancelled: true })
    })
    window.go = { main: { App: app } }
    await openFileManager('h1', 's1')

    await fmDownload('/srv/app.conf', 'app.conf')

    expect(store.fileManager.error, '取消不是错误').toBe('')
    expect(store.fileManager.notice, '取消也不是成功').toBe('')
    expect(store.fileManager.progress).toBeNull()
  })

  it('下载失败时给出原因并清空进度', async () => {
    const { app } = makeApp({
      DownloadRemoteFile: async () => ({ ok: false, error: '连接中断' })
    })
    window.go = { main: { App: app } }
    await openFileManager('h1', 's1')

    await fmDownload('/srv/app.conf', 'app.conf')

    expect(store.fileManager.error).toBe('连接中断')
    expect(store.fileManager.progress).toBeNull()
    expect(store.fileManager.busy).toBe(false)
  })

  it('取消按钮把事件里带回的 id 还给后端', async () => {
    const { app, calls } = makeApp({ DownloadRemoteFile: () => new Promise(() => {}) })
    window.go = { main: { App: app } }
    bindEvents()
    await openFileManager('h1', 's1')

    // 挂起的请求：模拟传输进行中
    fmDownload('/srv/big.bin', 'big.bin')
    rt.emit('fm:transfer', { kind: 'download', id: 't9', name: 'big.bin', done: 1024, total: 4096, bps: 1000 })

    fmCancelTransfer()
    const cancel = calls.find(c => c.name === 'CancelFileTransfer')
    expect(cancel, '应调用后端取消').toBeTruthy()
    expect(cancel.args, '必须用事件里带回的那个 id').toEqual(['t9'])
  })

  it('id 还没来（第一块没传完）时取消不发请求', async () => {
    const { app, calls } = makeApp()
    window.go = { main: { App: app } }
    await openFileManager('h1', 's1')

    fmCancelTransfer()
    expect(calls.find(c => c.name === 'CancelFileTransfer')).toBeUndefined()
  })
})

describe('文件管理 不含删除', () => {
  // 弹窗只做「浏览 / 上传 / 下载」。删除要用户走终端 —— 那条路径看得见、
  // 留得下历史、也能被审批闸门拦住；做成列表里的按钮误点成本太高。
  //
  // 这条测试守的是"不要哪天后端有这个 API，前端就顺手又接回来"。
  // 后端 DeleteRemotePath 依然存在（终端与自动化可用），只是这里不该有入口。
  it('store 不再导出 fmDelete', async () => {
    const mod = await import('../store.js')
    expect(mod.fmDelete, '文件管理弹窗不该再暴露删除动作').toBeUndefined()
  })

  it('store 源码里没有对 DeleteRemotePath 的调用', () => {
    const src = readFileSync(resolve(process.cwd(), 'src/store.js'), 'utf8')
      // 剥掉注释再查：store.js 里那段说明**故意**写了后端方法名
      // （"App.DeleteRemotePath 仍然保留"），不剥的话这条测试会因为
      // 一句解释性注释而永远失败 —— 那种假报警会让人干脆把测试删掉。
      .replace(/\/\*[\s\S]*?\*\//g, '')
      .replace(/(^|[^:])\/\/.*$/gm, '$1')
    expect(src, 'store 里不该有删除调用').not.toMatch(/DeleteRemotePath/)
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
      })
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

