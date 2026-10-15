// Wails WebView 点 <a href> 会整页跳走且无法返回。外链一律 preventDefault，
// 交给系统浏览器；应用内同源路径仍放行（资产/路由）。

function isExternalHttp(url) {
  return url.protocol === 'http:' || url.protocol === 'https:'
}

function isAppOrigin(url) {
  try {
    return url.origin === window.location.origin
  } catch {
    return false
  }
}

export function openExternalURL(href) {
  const raw = String(href || '').trim()
  if (!raw) return false
  let url
  try {
    url = new URL(raw, window.location.href)
  } catch {
    return false
  }
  if (!isExternalHttp(url) && url.protocol !== 'mailto:') return false
  if (isAppOrigin(url) && isExternalHttp(url)) {
    // 同源 http(s) 视为应用内（wails.localhost 等），不外开。
    return false
  }
  const rtOpen = window.runtime && window.runtime.BrowserOpenURL
  if (typeof rtOpen === 'function') {
    rtOpen(url.href)
    return true
  }
  // 浏览器预览兜底
  window.open(url.href, '_blank', 'noopener,noreferrer')
  return true
}

export function installExternalLinkGuard() {
  if (typeof document === 'undefined') return
  if (document.documentElement.dataset.extLinkGuard === '1') return
  document.documentElement.dataset.extLinkGuard = '1'

  document.addEventListener(
    'click',
    e => {
      if (e.defaultPrevented) return
      if (e.button !== 0) return
      if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
      const a = e.target && e.target.closest ? e.target.closest('a[href]') : null
      if (!a) return
      const href = a.getAttribute('href') || ''
      if (!href || href.startsWith('#') || href.startsWith('javascript:')) return
      let url
      try {
        url = new URL(a.href)
      } catch {
        return
      }
      if (!isExternalHttp(url) && url.protocol !== 'mailto:') return
      if (isAppOrigin(url)) return
      e.preventDefault()
      e.stopPropagation()
      openExternalURL(url.href)
    },
    true
  )
}
