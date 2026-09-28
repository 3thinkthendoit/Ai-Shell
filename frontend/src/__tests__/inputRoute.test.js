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
})
