import { marked } from 'marked'
import DOMPurify from 'dompurify'

// 助手气泡：GFM（表格/列表/代码块）→ HTML，再消毒后交给 v-html。
marked.setOptions({
  gfm: true,
  breaks: true
})

const PURIFY = {
  USE_PROFILES: { html: true },
  ADD_ATTR: ['target', 'rel']
}

// 外链标成 _blank；真正拦导航靠 openExternal.installExternalLinkGuard。
if (!DOMPurify.__aiShellLinkHook) {
  DOMPurify.__aiShellLinkHook = true
  DOMPurify.addHook('afterSanitizeAttributes', node => {
    if (node.tagName !== 'A' || !node.hasAttribute('href')) return
    const href = node.getAttribute('href') || ''
    if (!href || href.startsWith('#') || href.startsWith('mailto:')) return
    node.setAttribute('target', '_blank')
    node.setAttribute('rel', 'noopener noreferrer')
  })
}

export function renderMarkdown(src) {
  if (!src) return ''
  const raw = marked.parse(String(src), { async: false })
  return DOMPurify.sanitize(raw, PURIFY)
}
