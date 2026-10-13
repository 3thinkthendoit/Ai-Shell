import { describe, it, expect } from 'vitest'
import { classifyInput } from '../inputRoute.js'

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
