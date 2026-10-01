// 一次性预览脚手架：把真实的组件挂在浏览器里，喂假数据，
// 用来肉眼确认观感。**不属于交付物。**
//
//   /preview.html                  → Agent会话
//   /preview.html?panel=settings   → LLM 设置（含「测试连接」）
import { createApp, nextTick } from 'vue'
import ConsolePanel from './components/ConsolePanel.vue'
import SettingsPanel from './components/SettingsPanel.vue'
import { store, push } from './store.js'
import './style.css'

const which = new URLSearchParams(location.search).get('panel') || 'console'

// 模拟后端：Base URL 被规范化后再拼端点，所以回传的 url 里只有一个 /chat/completions。
// 前端据此反推出规范化后的 Base URL 并修正表单。
async function TestLLM(baseURL, model, apiKey) {
  await new Promise(r => setTimeout(r, 260))
  const normalized = String(baseURL || '').replace(/\/+$/, '').replace(/\/chat\/completions$/, '')
  return {
    ok: true,
    message: '连接成功',
    url: normalized + '/chat/completions',
    status: 200,
    model: model || 'deepseek-flash',
    reply: 'pong',
    durationMs: 264
  }
}

window.runtime = {
  EventsOn() {}, EventsOff() {}, EventsEmit() {},
  LogPrint() {}, LogError() {}, WindowShow() {}, WindowHide() {}
}

// ClearSession 的模拟：记一个轮数，让「清空上下文」的提示有真实数字可显示。
// 目前预览只摆静态记录，没有真跑过 agent，所以第一轮点它会显示「本来就没有上下文」。
let sessionTurns = 0
async function ClearSession() {
  const cleared = sessionTurns
  sessionTurns = 0
  return { cleared, busy: false }
}

// ---- 会话列表的模拟 ----
//
// 只为实现「选择器有内容、新建/改名/删除有反馈」这条链路 ——
// 会话的**真实规则**（默认会话不能删、上限 20 条、名字清洗）都在后端，
// 这里不重复实现：预览脚手架不是交付物，把规则抄一遍只会误导读者
// 以为它们是前端的事。
const DEFAULT_SESSION = 'default'
let previewSessions = [
  { id: DEFAULT_SESSION, name: '', turns: 0, archivedTurns: 0, updatedAt: new Date().toISOString(), isDefault: true },
  { id: 's1', name: 'nginx 排查', turns: 4, archivedTurns: 0, updatedAt: new Date().toISOString(), isDefault: false },
  { id: 's2', name: '磁盘排查', turns: 0, archivedTurns: 0, updatedAt: new Date().toISOString(), isDefault: false }
]
let seq = 2
async function ListSessions() {
  return previewSessions.map(s => ({ ...s }))
}
async function CreateSession(hostId, name) {
  const info = {
    id: 's' + ++seq, name: String(name).trim(), turns: 0,
    archivedTurns: 0, updatedAt: new Date().toISOString(), isDefault: false
  }
  previewSessions.push(info)
  return { ...info }
}
async function RenameSession(hostId, sessionId, name) {
  const s = previewSessions.find(x => x.id === sessionId)
  if (s) s.name = String(name).trim()
}
async function DeleteSession(hostId, sessionId) {
  if (sessionId === DEFAULT_SESSION) throw new Error('默认会话不能删除，用「清空上下文」把它清空即可')
  previewSessions = previewSessions.filter(x => x.id !== sessionId)
}
async function CompactSession() {
  await new Promise(r => setTimeout(r, 400))
  return { compacted: false, busy: false, message: '这条会话还没有记录。' }
}

window.go = {
  main: {
    App: {
      TestLLM, ClearSession, CompactSession,
      ListSessions, CreateSession, RenameSession, DeleteSession,
      SaveLLM: async () => {},
      Bootstrap: async () => ({ llm: store.llm }),
      // 常驻终端绑定：预览里不开真 PTY，表面保持静态即可。
      OpenTerminal: async () => {},
      WriteTerminal: async () => {},
      ResizeTerminal: async () => {},
      CloseTerminal: async () => true,
      // composer 里人敲的 shell 行走这条：策略闸门 + 写进常驻 PTY（无有界捕获）。
      // 预览没有真 PTY，只回一个裁决，够验证 composer 分流与放行/拒绝注记渲染。
      RunShellInTerminal: async (hostId, cmd) => {
        if (/^\s*rm\s+-rf\b/.test(cmd)) {
          return { status: 'denied', decision: 'deny', reason: '硬拒绝：递归强删', rule: 'deny_rm_rf', risk: 'high' }
        }
        return { status: 'done', decision: 'allow', reason: '', rule: 'auto_safe', risk: 'low' }
      },
      // 报告「当前看着哪条会话」，后端用它路由 PTY 定格快照。预览里静默即可。
      ReportActiveSession: async () => {}
    }
  }
}

store.ready = true
store.hosts = [
  { id: 'h1', name: 'web-01', user: 'root', addr: '8.163.114.84:22' },
  { id: 'h2', name: 'db-01', user: 'root', addr: '10.0.0.7' }
]
store.currentHostId = 'h1'
store.sessions = { h1: previewSessions.map(s => ({ ...s })) }
store.currentSessionId = DEFAULT_SESSION
store.llm = { baseUrl: 'https://token.sensenova.cn/v1/chat/completions', model: 'deepseek-flash', hasApiKey: true }

const sleep = ms => new Promise(r => setTimeout(r, ms))

createApp(which === 'settings' ? SettingsPanel : ConsolePanel).mount('#app')

;(async () => {
  await nextTick()

  if (which === 'settings') {
    const btn = [...document.querySelectorAll('.actions button')].find(b => b.textContent.includes('测试'))
    btn?.click()
    await sleep(600)
    document.title = 'settings-ready'
    return
  }

  // 默认 / agent：摆几条对话，看会话栏与主机切换。
  push({ kind: 'user', content: 'nginx 起不来了，帮我看看为什么' })
  push({ kind: 'tool', toolId: 'c1', tool: { id: 'c1', name: 'system_info', decision: 'allow', status: 'done', hostName: 'web-01', durationMs: 812, command: 'system_info' } })
  push({ kind: 'assistant', content: '我先在 web-01 上采集一次系统概览。' })
  push({ kind: 'assistant', content: 'nginx 没起来是因为 80 端口被另一个进程占了，建议先确认那个进程。' })
  store.currentHostId = 'h2'
  push({ kind: 'user', content: '这台机器磁盘还够吗' })
  store.currentHostId = 'h1'
  document.title = 'agent-ready'
})()
