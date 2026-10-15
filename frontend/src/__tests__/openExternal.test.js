import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { openExternalURL, installExternalLinkGuard } from '../openExternal.js'

describe('openExternalURL', () => {
  beforeEach(() => {
    window.runtime = { BrowserOpenURL: vi.fn() }
  })
  afterEach(() => {
    delete window.runtime
  })

  it('http(s) 外链走 BrowserOpenURL', () => {
    expect(openExternalURL('https://example.com/a')).toBe(true)
    expect(window.runtime.BrowserOpenURL).toHaveBeenCalledWith('https://example.com/a')
  })

  it('非法 / 空串不打开', () => {
    expect(openExternalURL('')).toBe(false)
    expect(openExternalURL('not a url')).toBe(false)
    expect(window.runtime.BrowserOpenURL).not.toHaveBeenCalled()
  })
})

describe('installExternalLinkGuard', () => {
  beforeEach(() => {
    delete document.documentElement.dataset.extLinkGuard
    window.runtime = { BrowserOpenURL: vi.fn() }
    installExternalLinkGuard()
  })
  afterEach(() => {
    delete window.runtime
    delete document.documentElement.dataset.extLinkGuard
    document.body.innerHTML = ''
  })

  it('点击外链 preventDefault 并外开', () => {
    document.body.innerHTML = '<a id="a" href="https://cloud.tencent.com/x">备案</a>'
    const a = document.getElementById('a')
    const ev = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 })
    a.dispatchEvent(ev)
    expect(ev.defaultPrevented).toBe(true)
    expect(window.runtime.BrowserOpenURL).toHaveBeenCalled()
    expect(String(window.runtime.BrowserOpenURL.mock.calls[0][0])).toContain('cloud.tencent.com')
  })

  it('锚点链接不拦截', () => {
    document.body.innerHTML = '<a id="a" href="#section">跳</a>'
    const a = document.getElementById('a')
    const ev = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 })
    a.dispatchEvent(ev)
    expect(window.runtime.BrowserOpenURL).not.toHaveBeenCalled()
  })

  it('Ctrl/Meta 点击外链同样拦下并外开（避免 WebView 整页跳走）', () => {
    document.body.innerHTML = '<a id="a" href="https://example.com/x">外链</a>'
    const a = document.getElementById('a')
    const ev = new MouseEvent('click', {
      bubbles: true, cancelable: true, button: 0, ctrlKey: true
    })
    a.dispatchEvent(ev)
    expect(ev.defaultPrevented).toBe(true)
    expect(window.runtime.BrowserOpenURL).toHaveBeenCalled()
  })
})
