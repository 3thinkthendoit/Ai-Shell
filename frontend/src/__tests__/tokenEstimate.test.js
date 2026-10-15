import { describe, it, expect } from 'vitest'
import {
  estimateTokens, formatTokenCount, assistantTokenStats,
  estimateSessionContext, estimateImageTokens, contextWindowForModel, resolveContextWindow
} from '../tokenEstimate.js'

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

describe('estimateSessionContext', () => {
  it('空时间线为 0', () => {
    expect(estimateSessionContext([])).toEqual({ tokens: 0, messages: 0 })
  })

  it('有 user/assistant 时带固定开销', () => {
    const st = estimateSessionContext([
      { kind: 'user', content: 'abcdefgh' },
      { kind: 'assistant', content: 'abcdefgh' }
    ])
    expect(st.messages).toBe(2)
    expect(st.tokens).toBeGreaterThan(1800)
  })

  it('待发附图计入估算（与空时间线也能显示）', () => {
    const img = { mime: 'image/png', data: 'A'.repeat(200) }
    const onlyImg = estimateSessionContext([], [img])
    expect(onlyImg.messages).toBe(1)
    expect(onlyImg.tokens).toBeGreaterThanOrEqual(estimateImageTokens(img))

    const withHist = estimateSessionContext(
      [{ kind: 'user', content: 'abcdefgh' }],
      [img]
    )
    const base = estimateSessionContext([{ kind: 'user', content: 'abcdefgh' }])
    expect(withHist.tokens).toBeGreaterThan(base.tokens)
    expect(withHist.messages).toBe(base.messages + 1)
  })
})

describe('contextWindowForModel', () => {
  it('识别常见模型窗口（与 Go 档位对齐）', () => {
    expect(contextWindowForModel('deepseek-v4.1-flash')).toBe(65536)
    expect(contextWindowForModel('gpt-4o-mini')).toBe(128000)
    expect(contextWindowForModel('')).toBe(0)
  })
})

describe('resolveContextWindow', () => {
  it('方案配置优先于模型名推断', () => {
    expect(resolveContextWindow({ model: 'deepseek-chat', contextWindow: 128000 })).toBe(128000)
    expect(resolveContextWindow({ model: 'deepseek-chat' })).toBe(65536)
  })
})
