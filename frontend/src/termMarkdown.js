// termMarkdown.js —— 流式 Markdown → ANSI，供终端表面（xterm）画助手正文。
//
// 约束：xterm 只能吃字符 + SGR，不能吃 HTML。对话页的 marked→DOM 搬不过来。
// 策略（与对话页 marked/GFM 对齐）：
//   1. 完整行：按块级规则渲染（标题/列表/引用/围栏/分隔线/表格），行内再加粗等。
//   2. 未完成的尾行：纯散文立刻软输出；块级或行内标记则缓冲到换行。
//   3. 若散文已软输出后又出现行内标记：已画出的前缀不动，后缀憋到换行再 styleInline
//      （xterm 无法回写；避免把 `**x**` 当原文刷出去）。
//   4. 不做 `_italic_`：会误伤 max_length / foo_bar；斜体只用 *…*，加粗用 **/** 与 __/__。

const RESET = '\x1b[0m'
const BOLD = '\x1b[1m'
const DIM = '\x1b[2m'
const ITALIC = '\x1b[3m'
const CYAN = '\x1b[36m'
const YELLOW = '\x1b[33m'

const DEFAULT_WIDTH = 72

/** 行首像块级 MD（应整行缓冲到 \n）。与 marked：`##标题` 不是标题，故要求 # 后空白或行尽。 */
function isHoldBlock(line) {
  const s = String(line || '')
  if (!s) return false
  if (/^#{1,6}(\s|$)/.test(s)) return true
  if (/^```/.test(s)) return true
  // marked/CommonMark：`>` 后空格可选，`>notquote` 也是引用
  if (/^>/.test(s)) return true
  if (/^(\s{0,3})([-*+])(\s|$)/.test(s)) return true
  if (/^(\s{0,3})\d+\.(\s|$)/.test(s)) return true
  if (/^(\*{3,}|-{3,}|_{3,})\s*$/.test(s)) return true
  if (/^\s*\|/.test(s)) return true
  return false
}

/** 行内尚可能构成 MD 标记（含 __加粗__ 的 `_`）。 */
function hasInlineSyntax(line) {
  return /[*_`~\[]/.test(String(line || ''))
}

/** 行内样式：完整 span 才替换；不成对的标记原样保留。无 `_italic_`。 */
export function styleInline(src) {
  let s = String(src == null ? '' : src)
  s = s.replace(/\[([^\]]+)\]\(([^)]+)\)/g, (_, text, url) => {
    return styleInlineBasic(text) + DIM + ' (' + url + ')' + RESET
  })
  return styleInlineBasic(s)
}

function styleInlineBasic(s) {
  let out = String(s == null ? '' : s)
  out = out.replace(/`([^`]+)`/g, (_, code) => CYAN + code + RESET)
  out = out.replace(/\*\*([^*]+)\*\*/g, (_, t) => BOLD + t + RESET)
  out = out.replace(/__([^_]+)__/g, (_, t) => BOLD + t + RESET)
  out = out.replace(/\*([^*\n]+)\*/g, (_, t) => ITALIC + t + RESET)
  out = out.replace(/~~([^~]+)~~/g, (_, t) => DIM + t + RESET)
  return out
}

function renderCodeLine(line) {
  return CYAN + line + RESET
}

function renderFenceOpen(line) {
  const lang = line.slice(3).trim()
  return DIM + '──' + (lang ? ' ' + lang + ' ' : '') + '──' + RESET
}

function renderFenceClose() {
  return DIM + '──────' + RESET
}

function renderHeading(line) {
  const m = /^(#{1,6})\s+(.*)$/.exec(line)
  if (!m) return styleInline(line)
  const level = m[1].length
  const body = styleInline(m[2])
  if (level <= 2) return BOLD + YELLOW + body + RESET
  return BOLD + body + RESET
}

function renderQuote(line) {
  // 与 marked 一致：`>` 后可选一个空格
  const body = line.replace(/^>\s?/, '')
  return DIM + '│ ' + RESET + styleInline(body)
}

function renderUl(line) {
  const m = /^(\s*)([-*+])\s+(.*)$/.exec(line)
  if (!m) return styleInline(line)
  return m[1] + CYAN + '•' + RESET + ' ' + styleInline(m[3])
}

function renderOl(line) {
  const m = /^(\s*)(\d+)\.\s+(.*)$/.exec(line)
  if (!m) return styleInline(line)
  return m[1] + CYAN + m[2] + '.' + RESET + ' ' + styleInline(m[3])
}

function renderHr(width) {
  const n = Math.max(8, Math.min(width || DEFAULT_WIDTH, 40))
  return DIM + '─'.repeat(n) + RESET
}

function renderTableRow(line) {
  const raw = line.trim().replace(/^\|/, '').replace(/\|$/, '')
  if (/^[\s|:-]+$/.test(line.trim())) {
    return renderHr(DEFAULT_WIDTH)
  }
  const cells = raw.split('|').map(c => styleInline(c.trim()))
  return cells.join(DIM + ' │ ' + RESET)
}

function renderMdLine(line, inFence, width) {
  if (inFence) {
    if (/^```/.test(line)) return { text: renderFenceClose(), inFence: false }
    return { text: renderCodeLine(line), inFence: true }
  }
  if (/^```/.test(line)) return { text: renderFenceOpen(line), inFence: true }
  if (/^(\*{3,}|-{3,}|_{3,})\s*$/.test(line)) return { text: renderHr(width), inFence: false }
  if (/^#{1,6}\s+/.test(line)) return { text: renderHeading(line), inFence: false }
  if (/^>/.test(line)) return { text: renderQuote(line), inFence: false }
  if (/^(\s*)([-*+])\s+/.test(line)) return { text: renderUl(line), inFence: false }
  if (/^(\s*)\d+\.\s+/.test(line)) return { text: renderOl(line), inFence: false }
  if (/^\s*\|/.test(line) && line.includes('|')) return { text: renderTableRow(line), inFence: false }
  return { text: styleInline(line), inFence: false }
}

/**
 * 创建一个流式 MD→ANSI 渲染器。
 * @param {{ width?: number }} [opts]
 * @returns {{ push: (chunk: string) => string, flush: () => string }}
 */
export function createMdAnsiStream(opts = {}) {
  const width = opts.width || DEFAULT_WIDTH
  let buf = ''
  let inFence = false
  /** 当前未完成行已软输出的源字符数（仅散文软输出路径）。 */
  let painted = 0

  function finishLine(line) {
    if (painted > 0) {
      // 前缀已按原文上屏；后缀可能含 ** 等，必须 styleInline，不能再原文倾倒。
      const rest = line.length > painted ? line.slice(painted) : ''
      painted = 0
      return (rest ? styleInline(rest) : '') + '\r\n'
    }
    const r = renderMdLine(line, inFence, width)
    inFence = r.inFence
    return r.text + '\r\n'
  }

  function emitTrailing(partial) {
    if (!partial) return ''
    if (inFence) return ''
    // 块级：整行憋住（含尚未软输出时）
    if (painted === 0 && isHoldBlock(partial)) return ''
    // 行内标记：未软输出则整行憋住；已软输出则只憋后缀（等换行再渲）
    if (hasInlineSyntax(partial)) {
      if (painted === 0) return ''
      return ''
    }
    // 纯散文：增量软输出
    if (partial.length <= painted) return ''
    const d = partial.slice(painted)
    painted = partial.length
    return d
  }

  function push(chunk) {
    buf += String(chunk == null ? '' : chunk).replace(/\r\n/g, '\n').replace(/\r/g, '\n')
    let out = ''
    while (true) {
      const i = buf.indexOf('\n')
      if (i < 0) break
      const line = buf.slice(0, i)
      buf = buf.slice(i + 1)
      out += finishLine(line)
    }
    out += emitTrailing(buf)
    return out
  }

  function flush() {
    let out = ''
    if (painted > 0) {
      const rest = buf.length > painted ? buf.slice(painted) : ''
      if (rest) out += styleInline(rest)
      buf = ''
      painted = 0
    } else if (buf) {
      const r = renderMdLine(buf, inFence, width)
      inFence = r.inFence
      out += r.text
      buf = ''
    }
    if (inFence) {
      if (out && !out.endsWith('\r\n')) out += '\r\n'
      out += renderFenceClose()
      inFence = false
    }
    return out
  }

  return { push, flush }
}

/** 一次性把整段 MD 渲成 ANSI（非流式场景）。 */
export function renderMdAnsi(src, opts) {
  const s = createMdAnsiStream(opts)
  return s.push(String(src == null ? '' : src)) + s.flush()
}
