import { describe, it, expect } from 'vitest'
import { renderMarkdown } from '../markdown.js'

describe('renderMarkdown', () => {
  it('空内容返回空串', () => {
    expect(renderMarkdown('')).toBe('')
    expect(renderMarkdown(null)).toBe('')
  })

  it('渲染标题、加粗与列表', () => {
    const html = renderMarkdown('## 标题\n\n- **加粗**项')
    expect(html).toContain('<h2')
    expect(html).toContain('标题')
    expect(html).toContain('<strong>')
    expect(html).toContain('<ul>')
  })

  it('渲染 GFM 表格', () => {
    const md = '| 服务 | 端口 |\n|---|---|\n| Nginx | 80 |'
    const html = renderMarkdown(md)
    expect(html).toContain('<table')
    expect(html).toContain('<th')
    expect(html).toContain('Nginx')
  })

  it('消毒 script，不执行危险标签', () => {
    const html = renderMarkdown('hi<script>alert(1)</script>')
    expect(html.toLowerCase()).not.toContain('<script')
  })

  it('外链带 target=_blank 与 rel', () => {
    const html = renderMarkdown('[腾讯云](https://cloud.tencent.com/)')
    expect(html).toContain('target="_blank"')
    expect(html).toContain('noopener')
  })
})
