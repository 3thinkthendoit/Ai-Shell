// i18n 与终端字体偏好的回归测试。
//
// 两块职责：
// 1. i18n 运行时：zh/en 切换、{param} 占位、缺键回退链（当前语言 -> zh -> key）。
//    特别钉死「两张表的键集必须完全一致」——任何域字典加了 zh 忘了 en，
//    会在英文界面静默露中文，靠人眼巡检不可靠，这里直接 fail。
// 2. store 的字体偏好：initTermFont 的坏数据回退、setTermFont 的夹紧与持久化。
import { describe, it, expect, beforeEach, vi } from 'vitest'
import { i18n, t, setLocale, initLocale } from '../i18n'
import { app } from '../locales/app'
import { settingsMenu } from '../locales/settingsMenu'
import { console as consoleDomain } from '../locales/console'
import { timeline } from '../locales/timeline'
import { storeMsg } from '../locales/storeMsg'
import { panels } from '../locales/panels'
import { files } from '../locales/files'
import { sessions } from '../locales/sessions'
import {
  store, DEFAULT_TERM_FONT, initTermFont, setTermFont, setTheme, initTheme
} from '../store'

beforeEach(() => {
  localStorage.clear()
  // store 是模块级单例：用例间把字体偏好复位回默认，避免上例的改动泄漏。
  store.termFont = { ...DEFAULT_TERM_FONT }
  setLocale('zh')
})

// ---- i18n 运行时 ----

describe('i18n', () => {
  it('默认中文，切到英文后 t() 跟着换', () => {
    setLocale('zh')
    expect(t('audit.title')).toBe('审计日志')
    setLocale('en')
    expect(i18n.locale).toBe('en')
    expect(t('audit.title')).toBe('Audit Log')
  })

  it('{param} 占位符填充；缺失的占位符原样保留', () => {
    expect(t('hosts.saved', { name: 'web1' })).toBe('已保存主机「web1」')
    expect(t('hosts.saved')).toBe('已保存主机「{name}」')
  })

  it('缺键回退：中文表兜底，两边都没有则返回 key 本身', () => {
    // 模拟一个只在 zh 表存在的键：直接验证回退链的中间环节不可行
    // （生产字典不该有这种键，下一个用例钉死了），这里验证最终环节。
    expect(t('no.such.key')).toBe('no.such.key')
  })

  it('setLocale 忽略非法值', () => {
    setLocale('en')
    setLocale('fr')
    expect(i18n.locale).toBe('en')
  })

  it('initLocale：无记录时跟随系统语言（英文系统 -> en），并把结果持久化', () => {
    vi.stubGlobal('navigator', { language: 'en-US' })
    localStorage.clear()
    initLocale()
    expect(i18n.locale).toBe('en')
    expect(localStorage.getItem('aishell.locale')).toBe('en')
    // 中文系统 -> zh。注意先清掉上一步持久化的 'en'：initLocale 优先读记录。
    vi.stubGlobal('navigator', { language: 'zh-CN' })
    localStorage.clear()
    initLocale()
    expect(i18n.locale).toBe('zh')
    vi.unstubAllGlobals()
  })

  it('initLocale：有持久化记录时优先恢复，不跟随系统', () => {
    localStorage.setItem('aishell.locale', 'en')
    vi.stubGlobal('navigator', { language: 'zh-CN' })
    initLocale()
    expect(i18n.locale).toBe('en')
    vi.unstubAllGlobals()
  })

  it('document.documentElement.lang 跟随语言', () => {
    setLocale('en')
    expect(document.documentElement.lang).toBe('en')
    setLocale('zh')
    expect(document.documentElement.lang).toBe('zh-CN')
  })
})

// 每个域字典都是 { zh, en } 两张表。键集不一致 = 漏译，直接列出差集 fail。
// 占位符也必须一致：zh 用 {n} 而 en 写成 {count} 的话，英文界面会静默露出
// 裸的 {count} 字样（t() 只做字符串替换，不知道两种语言占位符该对齐）。
describe('zh/en 字典键集一致', () => {
  const domains = {
    app, settingsMenu, console: consoleDomain, timeline, storeMsg, panels, files, sessions
  }
  // 提取一段文案里的占位符名，排序后拼串，方便整值比较。
  const ph = s => [...String(s ?? '').matchAll(/\{(\w+)\}/g)].map(m => m[1]).sort().join(',')

  for (const [name, d] of Object.entries(domains)) {
    it(`${name}: zh 与 en 键集完全一致`, () => {
      const zhKeys = Object.keys(d.zh).sort()
      const enKeys = Object.keys(d.en).sort()
      const missingInEn = zhKeys.filter(k => !(k in d.en))
      const missingInZh = enKeys.filter(k => !(k in d.zh))
      expect(missingInEn, `以下键缺英文翻译: ${missingInEn.join(', ')}`).toEqual([])
      expect(missingInZh, `以下键缺中文翻译: ${missingInZh.join(', ')}`).toEqual([])
    })

    it(`${name}: zh 与 en 占位符一致`, () => {
      const bad = Object.keys(d.zh)
        .filter(k => k in d.en && ph(d.zh[k]) !== ph(d.en[k]))
        .map(k => `${k}: zh{${ph(d.zh[k])}} vs en{${ph(d.en[k])}}`)
      expect(bad, `以下键中英占位符不一致:\n${bad.join('\n')}`).toEqual([])
    })
  }
})

// ---- 终端字体偏好 ----

describe('终端字体偏好', () => {
  it('默认值与历史默认一致（升级用户观感不变）', () => {
    expect(DEFAULT_TERM_FONT.size).toBe(12.5)
    expect(DEFAULT_TERM_FONT.family).toContain('monospace')
    expect(store.termFont).toEqual(DEFAULT_TERM_FONT)
  })

  it('initTermFont：恢复持久化的偏好', () => {
    localStorage.setItem('aishell.termfont', JSON.stringify({ family: 'Consolas', size: 15 }))
    initTermFont()
    expect(store.termFont).toEqual({ family: 'Consolas', size: 15 })
  })

  it('initTermFont：坏数据当没有 —— store 保持原值，不被坏数据污染', () => {
    localStorage.setItem('aishell.termfont', '{bad json')
    initTermFont()
    expect(store.termFont).toEqual(DEFAULT_TERM_FONT)

    localStorage.setItem('aishell.termfont', JSON.stringify({ family: '', size: 999 }))
    initTermFont()
    expect(store.termFont.family).toBe(DEFAULT_TERM_FONT.family)
    expect(store.termFont.size).toBe(DEFAULT_TERM_FONT.size)

    localStorage.setItem('aishell.termfont', JSON.stringify({ size: 'NaN-ish' }))
    initTermFont()
    expect(store.termFont.size).toBe(DEFAULT_TERM_FONT.size)
  })

  it('setTermFont：字号夹紧到 8~28 且步进 0.5，并持久化', () => {
    setTermFont({ size: 3 })
    expect(store.termFont.size).toBe(8)
    setTermFont({ size: 100 })
    expect(store.termFont.size).toBe(28)
    setTermFont({ size: 12.37 })
    expect(store.termFont.size).toBe(12.5)
    expect(JSON.parse(localStorage.getItem('aishell.termfont'))).toEqual(store.termFont)
  })

  it('setTermFont：只传 family 时字号保持不变，空白 family 被忽略', () => {
    setTermFont({ family: 'Consolas, monospace' })
    expect(store.termFont.family).toBe('Consolas, monospace')
    expect(store.termFont.size).toBe(DEFAULT_TERM_FONT.size)
    setTermFont({ family: '   ' })
    expect(store.termFont.family).toBe('Consolas, monospace')
  })

  // 与主题持久化同款行为（顺带钉死，防止两套偏好将来漂移）
  it('主题持久化与字体持久化互不干扰', () => {
    initTheme()
    setTheme('dark')
    setTermFont({ size: 20 })
    initTermFont()
    initTheme()
    expect(store.theme).toBe('dark')
    expect(store.termFont.size).toBe(20)
  })
})
