import { describe, it, expect } from 'vitest'
import { estimateTokens, formatTokenCount, assistantTokenStats } from '../tokenEstimate.js'

describe('estimateTokens', () => {
  it('空串为 0', () => {
    expect(estimateTokens('')).toBe(0)
  })

  it('英文约 4 字符 1 token', () => {
    expect(estimateTokens('abcd')).toBe(1)
    expect(estimateTokens('abcdefgh')).toBe(2)
  })

  it('中文按宽字符估算', () => {
    expect(estimateTokens('磁盘')).toBeGreaterThan(0)
  })
})

describe('formatTokenCount', () => {
  it('格式化', () => {
    expect(formatTokenCount(856)).toBe('856')
    expect(formatTokenCount(1200)).toBe('1.2K')
    expect(formatTokenCount(128000)).toBe('128K')
  })
})

describe('assistantTokenStats', () => {
  it('优先用 tokensOut，否则按正文估算', () => {
    expect(assistantTokenStats({ tokensOut: 42, content: 'hi' })).toEqual({ out: 42, in: 0 })
    const st = assistantTokenStats({ content: 'abcdefgh' })
    expect(st.out).toBe(2)
  })
})
