import { describe, it, expect } from 'vitest'
import { createMdAnsiStream, renderMdAnsi, styleInline } from '../termMarkdown.js'

const stripAnsi = s => s.replace(/\x1b\[[0-9;]*m/g, '')

describe('styleInline', () => {
  it('加粗 / 行内代码 / 链接 / *斜体*', () => {
    expect(stripAnsi(styleInline('见 **重要** 与 `cmd`'))).toBe('见 重要 与 cmd')
    expect(styleInline('**重要**')).toContain('\x1b[1m')
    expect(styleInline('`cmd`')).toContain('\x1b[36m')
    expect(stripAnsi(styleInline('*斜*'))).toBe('斜')
    expect(stripAnsi(styleInline('[腾讯云](https://cloud.tencent.com/)'))).toBe(
      '腾讯云 (https://cloud.tencent.com/)'
    )
  })

  it('不把 snake_case / __加粗__ 搞错：无 _italic_', () => {
    expect(stripAnsi(styleInline('use max_length and foo_bar_baz'))).toBe(
      'use max_length and foo_bar_baz'
    )
    expect(stripAnsi(styleInline('__粗__'))).toBe('粗')
    expect(styleInline('__粗__')).toContain('\x1b[1m')
  })
})

describe('renderMdAnsi 块级（与 marked 对齐）', () => {
  it('标题去掉 #，列表变成 •', () => {
    const out = stripAnsi(renderMdAnsi('## 二、Web\n\n- **加粗**项\n'))
    expect(out).toContain('二、Web')
    expect(out).not.toContain('##')
    expect(out).toContain('•')
    expect(out).toContain('加粗项')
    expect(out).not.toContain('**')
  })

  it('##标题（# 后无空格）不是标题，原样保留（同 marked）', () => {
    expect(stripAnsi(renderMdAnsi('##标题\n'))).toContain('##标题')
  })

  it('>notquote 仍是引用（同 marked/CommonMark）', () => {
    const out = stripAnsi(renderMdAnsi('>notquote\n'))
    expect(out).toContain('│')
    expect(out).toContain('notquote')
  })

  it('围栏代码保留内容、弱化围栏标记', () => {
    const out = stripAnsi(renderMdAnsi('```bash\necho hi\n```\n'))
    expect(out).toContain('echo hi')
    expect(out).toContain('bash')
  })

  it('表格分隔行变成横线，单元格可读', () => {
    const md = '| 服务 | 端口 |\n|---|---|\n| Nginx | 80 |\n'
    const out = stripAnsi(renderMdAnsi(md))
    expect(out).toContain('Nginx')
    expect(out).toContain('80')
    expect(out).not.toContain('|---|')
  })
})

describe('createMdAnsiStream 流式', () => {
  it('纯散文增量立刻上屏（不必等换行）', () => {
    const s = createMdAnsiStream()
    expect(s.push('磁盘')).toBe('磁盘')
    expect(s.push('充足')).toBe('充足')
    expect(s.flush()).toBe('')
  })

  it('裸 \\n 归一成 \\r\\n', () => {
    const s = createMdAnsiStream()
    const out = s.push('第一行\n第二行')
    expect(out).toContain('第一行\r\n第二行')
    expect(s.flush()).toBe('')
  })

  it('标题行缓冲到换行再渲，定稿 flush 未完成行', () => {
    const s = createMdAnsiStream()
    expect(s.push('## Hel')).toBe('')
    expect(s.push('lo')).toBe('')
    const done = s.push('\n')
    expect(stripAnsi(done)).toBe('Hello\r\n')
    expect(done).not.toContain('##')
  })

  it('flush 冲出未换行的标题', () => {
    const s = createMdAnsiStream()
    expect(s.push('## 结论')).toBe('')
    expect(stripAnsi(s.flush())).toBe('结论')
  })

  it('加粗行等完整行再剥标记', () => {
    const s = createMdAnsiStream()
    const out = s.push('见 **重要** 项\n')
    expect(stripAnsi(out)).toBe('见 重要 项\r\n')
    expect(out).not.toContain('**')
  })

  it('先软输出散文再出现 **：后缀憋到换行后上色，不留原始星号', () => {
    const s = createMdAnsiStream()
    expect(s.push('See ')).toBe('See ')
    expect(s.push('**x**')).toBe('')
    const done = s.push('\n')
    expect(stripAnsi(done)).toBe('x\r\n')
    expect(done).not.toContain('**')
    expect(done).toContain('\x1b[1m')
  })

  it('含 underscore 的标识符整行憋住，定稿后仍是 max_length', () => {
    const s = createMdAnsiStream()
    expect(s.push('use max_length')).toBe('')
    expect(stripAnsi(s.push('\n'))).toBe('use max_length\r\n')
  })
})
