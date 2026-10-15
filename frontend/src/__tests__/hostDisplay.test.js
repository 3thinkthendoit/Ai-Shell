import { describe, it, expect } from 'vitest'
import { maskHostAddr, formatHostLabel } from '../hostDisplay.js'

describe('maskHostAddr', () => {
  it('IPv4 中间两段脱敏', () => {
    expect(maskHostAddr('47.117.94.84')).toBe('47.***.***.84')
    expect(maskHostAddr('10.0.0.1')).toBe('10.***.***.1')
  })

  it('带端口时保留端口', () => {
    expect(maskHostAddr('47.117.94.84:22')).toBe('47.***.***.84:22')
  })

  it('非 IPv4（主机名）原样返回', () => {
    expect(maskHostAddr('db.internal')).toBe('db.internal')
    expect(maskHostAddr('')).toBe('')
  })
})

describe('formatHostLabel', () => {
  it('有名字时用「名 · 脱敏 IP」', () => {
    expect(formatHostLabel({
      name: '98小程序',
      user: 'root',
      addr: '47.117.94.84'
    })).toBe('98小程序 · 47.***.***.84')
  })

  it('无名字时用 user@脱敏 IP', () => {
    expect(formatHostLabel({
      name: '',
      user: 'root',
      addr: '10.0.0.1:22'
    })).toBe('root@10.***.***.1:22')
  })
})
