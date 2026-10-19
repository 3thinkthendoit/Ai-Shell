import { describe, it, expect } from 'vitest'
import { classifyInput, resolveInput } from '../inputRoute.js'

describe('classifyInput', () => {
  it('空输入走 agent（由上层拦「请输入」）', () => {
    expect(classifyInput('')).toEqual({ kind: 'agent', text: '' })
    expect(classifyInput('   ')).toEqual({ kind: 'agent', text: '' })
  })

  it('? / ？前缀强制走 agent 并去掉前缀', () => {
    expect(classifyInput('?ls -la')).toEqual({ kind: 'agent', text: 'ls -la' })
    expect(classifyInput('？nginx 挂了')).toEqual({ kind: 'agent', text: 'nginx 挂了' })
  })

  it('含中文走 agent', () => {
    expect(classifyInput('nginx 起不来了').kind).toBe('agent')
    expect(classifyInput('看看磁盘').kind).toBe('agent')
  })

  it('英文问句走 agent', () => {
    expect(classifyInput('why is nginx down?').kind).toBe('agent')
  })

  it('多行走 agent', () => {
    expect(classifyInput('line1\nline2').kind).toBe('agent')
  })

  it('普通 shell 命令走 shell', () => {
    expect(classifyInput('ls -la')).toEqual({ kind: 'shell', text: 'ls -la' })
    expect(classifyInput('systemctl status nginx')).toEqual({ kind: 'shell', text: 'systemctl status nginx' })
    expect(classifyInput('cd /etc')).toEqual({ kind: 'shell', text: 'cd /etc' })
  })

  // 回归：用户在回答 LLM 的「1. … 2. …」编号选项时，直接回「2」或「2 写入 ...」。
  // 曾经这类输入会被当命令执行，报 `2: command not found`（用户截图里的 bug）。
  // shell 里没有以数字命名的真实命令，数字编号必须交给 LLM。
  it('数字编号回复走 agent（不被当命令执行）', () => {
    expect(classifyInput('2')).toEqual({ kind: 'agent', text: '2' })
    expect(classifyInput('2 写入 111111111111 内容')).toEqual({ kind: 'agent', text: '2 写入 111111111111 内容' })
    expect(classifyInput('2. 写入一些内容').kind).toBe('agent')
    expect(classifyInput('2) 换个路径').kind).toBe('agent')
    expect(classifyInput('3、列个清单').kind).toBe('agent')
  })

  // 反向保证：真实命令不受数字编号规则影响。
  it('真实命令仍走 shell', () => {
    expect(classifyInput('df -h').kind).toBe('shell')
    expect(classifyInput('ls /var/log').kind).toBe('shell')
  })
})

// agent 模式：对话中「非命令」当回复；已知 Linux 命令仍走 shell。
describe('resolveInput · agent 模式', () => {
  const known = ['ls', 'df', 'echo', 'pwd', 'id', 'true', 'date', 'file', 'nginx', 'grep']
  function makeApi(resolveImpl) {
    const calls = []
    const api = {
      ClassifyShellInput: async text => {
        const first = String(text || '').trim().split(/\s+/)[0] || ''
        // 与后端一致：路径形（./x）算命令意图。
        if (first.includes('/')) return true
        return known.includes(first.replace(/^.*\//, ''))
      },
      ResolveUnknownInput: async (...args) => {
        calls.push(args)
        return resolveImpl ? resolveImpl(...args) : { kind: 'shell' }
      }
    }
    return { api, calls }
  }

  it('agent 对话中：已知命令仍走 shell', async () => {
    const { api } = makeApi()
    expect(await resolveInput('ls -la', api, 'h1', true)).toEqual({ kind: 'shell', text: 'ls -la' })
    expect(await resolveInput('df -h', api, 'h1', true)).toEqual({ kind: 'shell', text: 'df -h' })
    expect(await resolveInput('ls', api, 'h1', true)).toEqual({ kind: 'shell', text: 'ls' })
  })
  it('agent 对话中：含中文参数的文本类命令走 shell', async () => {
    const { api } = makeApi()
    expect(await resolveInput('echo 你好', api, 'h1', true)).toEqual({ kind: 'shell', text: 'echo 你好' })
    expect(await resolveInput('grep 错误 app.log', api, 'h1', true)).toEqual({ kind: 'shell', text: 'grep 错误 app.log' })
  })
  it('命令/路径 + 中文自然语言 → Agent（让模型自己分析）', async () => {
    const { api, calls } = makeApi()
    expect(await resolveInput('nginx 起不来了', api, 'h1', false)).toEqual({ kind: 'agent', text: 'nginx 起不来了' })
    expect(await resolveInput('./start.sh 执行报错', api, 'h1', false)).toEqual({ kind: 'agent', text: './start.sh 执行报错' })
    expect(await resolveInput('执行报错 ./start.sh', api, 'h1', false)).toEqual({ kind: 'agent', text: '执行报错 ./start.sh' })
    expect(calls).toEqual([])
  })
  it('! 前缀仍可强制把「命令+中文」当 shell 跑', async () => {
    const { api } = makeApi()
    expect(await resolveInput('!./start.sh 执行报错', api, 'h1', false)).toEqual({ kind: 'shell', text: './start.sh 执行报错' })
  })
  it('agent 对话中：回复同形的单词交给 Agent，带参数仍执行', async () => {
    const { api, calls } = makeApi()
    expect(await resolveInput('id', api, 'h1', true)).toEqual({ kind: 'agent', text: 'id' })
    expect(await resolveInput('true', api, 'h1', true)).toEqual({ kind: 'agent', text: 'true' })
    expect(await resolveInput('date', api, 'h1', true)).toEqual({ kind: 'agent', text: 'date' })
    expect(await resolveInput('date -u', api, 'h1', true)).toEqual({ kind: 'shell', text: 'date -u' })
    expect(await resolveInput('id root', api, 'h1', true)).toEqual({ kind: 'shell', text: 'id root' })
    expect(calls).toEqual([])
  })
  it('非 agent 对话：同形单词仍当命令执行', async () => {
    const { api } = makeApi()
    expect(await resolveInput('id', api, 'h1', false)).toEqual({ kind: 'shell', text: 'id' })
    expect(await resolveInput('date', api, 'h1', false)).toEqual({ kind: 'shell', text: 'date' })
  })
  it('agent 对话中：中文回复不探测', async () => {
    const { api, calls } = makeApi()
    expect(await resolveInput('好的', api, 'h1', true)).toEqual({ kind: 'agent', text: '好的' })
    expect(calls).toEqual([])
  })
  it('agent 对话中：yes / open 不探测、不执行', async () => {
    const { api, calls } = makeApi(() => ({ kind: 'shell', binary: 'yes' }))
    expect(await resolveInput('yes', api, 'h1', true)).toEqual({ kind: 'agent', text: 'yes' })
    expect(await resolveInput('open 3306', api, 'h1', true)).toEqual({ kind: 'agent', text: 'open 3306' })
    expect(calls).toEqual([])
  })
  it('非 agent 对话：yes 仍探测，存在就执行', async () => {
    const { api, calls } = makeApi(() => ({ kind: 'shell', binary: 'yes' }))
    expect(await resolveInput('yes', api, 'h1', false)).toEqual({ kind: 'shell', text: 'yes' })
    expect(calls).toHaveLength(1)
  })
  it('agent 对话中：!yes 仍强制 shell', async () => {
    const { api, calls } = makeApi()
    expect(await resolveInput('!yes', api, 'h1', true)).toEqual({ kind: 'shell', text: 'yes' })
    expect(calls).toEqual([])
  })
  it('agent 对话中：单个未知词也探测，远端存在就执行', async () => {
    const { api, calls } = makeApi(() => ({ kind: 'shell', binary: 'bt' }))
    expect(await resolveInput('bt', api, 'h1', true)).toEqual({ kind: 'shell', text: 'bt' })
    expect(await resolveInput('ceshi --flag', api, 'h1', true)).toEqual({ kind: 'shell', text: 'ceshi --flag' })
    expect(calls).toHaveLength(2)
  })
  it('agent 对话中：探测证明不存在才交给 Agent', async () => {
    const { api, calls } = makeApi(() => ({ kind: 'agent', binary: 'nosuch', missing: true }))
    const got = await resolveInput('nosuch', api, 'h1', true)
    expect(got.kind).toBe('agent')
    expect(got.missing).toBe(true)
    expect(calls).toHaveLength(1)
  })
  it('agent 对话中：显式 ! 前缀仍强制 shell', async () => {
    const { api } = makeApi()
    expect(await resolveInput('!ls -la', api, 'h1', true)).toEqual({ kind: 'shell', text: 'ls -la' })
    expect(await resolveInput('!ceshi', api, 'h1', true)).toEqual({ kind: 'shell', text: 'ceshi' })
  })
  it('agent 对话中：? 前缀仍走 Agent 并剥前缀', async () => {
    const { api } = makeApi()
    expect(await resolveInput('?ls -la', api, 'h1', true)).toEqual({ kind: 'agent', text: 'ls -la' })
  })
  it('非 agent 对话：命令仍走 shell', async () => {
    const { api } = makeApi()
    expect(await resolveInput('ls -la', api, 'h1', false)).toEqual({ kind: 'shell', text: 'ls -la' })
  })
  it('命令库问不了：agent 对话中 ls 仍走 shell，中文仍走 Agent', async () => {
    expect(await resolveInput('ls -la', undefined, 'h1', true)).toEqual({ kind: 'shell', text: 'ls -la' })
    expect(await resolveInput('ls', undefined, 'h1', true)).toEqual({ kind: 'shell', text: 'ls' })
    expect(await resolveInput('好的', undefined, 'h1', true)).toEqual({ kind: 'agent', text: '好的' })
  })
  it('命令库抛错：与无绑定相同，非中文按 shell', async () => {
    const api = {
      ClassifyShellInput: async () => { throw new Error('bind down') }
    }
    expect(await resolveInput('pwd', api, 'h1', true)).toEqual({ kind: 'shell', text: 'pwd' })
  })
})
