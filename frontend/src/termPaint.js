// termPaint.js —— 把 agent 事件写成带 ANSI 样式的终端行，渲染进控制台那一片
// xterm 表面（阿里云 Workbench 同款：对话/命令/审批/错误全落在同一条终端流里）。
//
// 颜色只用**基础 ANSI SGR**（30-37 那套），不写死具体色值：xterm 会用当前
// 主题（termTheme.js 注入的 options.theme）的调色板去映射这些颜色，于是明暗
// 主题自动跟随，这里不必关心配色。
//
// 每个函数都收一个 `write(str)` 落点（就是某台主机 xterm 实例的 write），
// 自己负责补 \r\n —— 终端不像 DOM 会自动换行。

const RESET = '\x1b[0m'
const DIM = '\x1b[2m'
const BOLD = '\x1b[1m'
const RED = '\x1b[31m'
const GREEN = '\x1b[32m'
const YELLOW = '\x1b[33m'
const BLUE = '\x1b[34m'
const MAGENTA = '\x1b[35m'
const CYAN = '\x1b[36m'

// 工具输出在终端流里只做「瞥一眼」用途，完整原文在归档视图与模型上下文里。
// 封顶避免一条 bigout 把整片表面刷爆。
const OUT_LINES = 20
const OUT_COLS = 2000

function nl(write, s) {
  write((s || '') + '\r\n')
}

// 把多行文本裁到上限，超出补一行省略提示。
function clip(text) {
  const raw = String(text == null ? '' : text)
  if (!raw) return ''
  let lines = raw.replace(/\r\n/g, '\n').split('\n')
  let trimmed = false
  if (lines.length > OUT_LINES) {
    lines = lines.slice(0, OUT_LINES)
    trimmed = true
  }
  let out = lines.join('\r\n')
  if (out.length > OUT_COLS) {
    out = out.slice(0, OUT_COLS)
    trimmed = true
  }
  if (trimmed) out += '\r\n' + DIM + '…（输出过长已截断，完整内容见归档）' + RESET
  return out
}

// 会话分隔线：切会话/开新任务时在表面上画一条，让人看清「下面是另一段对话」。
export function paintSessionDivider(write, label) {
  if (!write) return
  const text = label ? ` ${label} ` : ''
  nl(write, '')
  nl(write, CYAN + DIM + '──────' + text + '──────' + RESET)
}

// 人在 composer 里交给 agent 的那句话（shell 行不画：它由 PTY 自己回显）。
// 用纯符号提示符❯（蓝）代替文字称呼，更像 shell、更省地方。
export function paintUser(write, text) {
  if (!write) return
  nl(write, '')
  nl(write, BLUE + BOLD + '❯ ' + RESET + String(text == null ? '' : text))
}

// agent 回复的开头：先落一个符号抬头●（绿），随后 delta 逐字接在后面。
export function paintAssistantHeader(write) {
  if (!write) return
  nl(write, '')
  write(GREEN + BOLD + '● ' + RESET)
}

// 流式增量：原样追加，不加行尾换行（模型自己会发 \n）。
// 但必须把裸 \n 归一成 \r\n：xterm 默认 convertEol=false，裸 LF 只「下移一行、
// 列不变」，于是每行都从上一行的结束列开始 → 阶梯状右偏、左右错乱。终端不像
// DOM 会自动回行，这里显式补 CR。
export function paintAssistantDelta(write, text) {
  if (!write) return
  write(String(text == null ? '' : text).replace(/\r?\n/g, '\r\n'))
}

// 思考增量：弱化灰字，样式自包含（每段自带 DIM…RESET），
// 后续正文另起一行落●抬头时天然回到正常样式。终端表面无法折叠，
// 思考全程平铺，与 Claude Code 等终端工具的观感一致。
export function paintAssistantReasoning(write, text) {
  if (!write) return
  write(DIM + String(text == null ? '' : text).replace(/\r?\n/g, '\r\n') + RESET)
}

// 回复定稿：补一个换行，把光标停在新一行行首。
export function paintAssistantEnd(write) {
  if (!write) return
  nl(write, '')
}

const DECISION = { allow: '自动放行', confirm: '需确认', deny: '硬拒绝' }

// 工具调用块：`$ cmd` + 裁决 + 目标主机。
export function paintTool(write, view) {
  if (!write || !view) return
  nl(write, '')
  const dec = view.decision && view.decision !== 'allow'
    ? YELLOW + '[' + (DECISION[view.decision] || view.decision) + ']' + RESET + ' '
    : ''
  const host = view.hostName ? DIM + ' @' + view.hostName + RESET : ''
  nl(write, MAGENTA + BOLD + '$ ' + RESET + String(view.command || view.name || '') + ' ' + dec + host)
  if (view.reason) nl(write, DIM + '  ' + view.reason + RESET)
}

// 工具结果：退出码 + 截断输出。命中 TTY 时改画一条「交接到终端」的注记。
export function paintToolResult(write, res) {
  if (!write || !res) return
  if (res.tty) {
    nl(write, CYAN + '  ⤷ 该命令需要交互式终端，已交接到这片终端表面执行（画面不进模型上下文）。' + RESET)
    return
  }
  const code = res.exitCode == null ? '' : res.exitCode
  const head = code === 0 || code === ''
    ? DIM + '  退出码 ' + code + RESET
    : RED + '  退出码 ' + code + RESET
  nl(write, head + (res.redacted ? YELLOW + ' · 已脱敏 ' + res.redacted + ' 处' + RESET : ''))
  const body = clip(res.content)
  if (body) nl(write, DIM + body + RESET)
}

// 审批提示：审批条/弹窗是交互入口，表面上只留一行注记说明「为什么停住了」。
export function paintApproval(write, view) {
  if (!write || !view) return
  nl(write, '')
  nl(write, YELLOW + BOLD + '⚠ 需要你的批准：' + RESET + YELLOW + String(view.command || view.name || '') + RESET)
  if (view.reason) nl(write, DIM + '  ' + view.reason + RESET)
}

export function paintError(write, msg) {
  if (!write) return
  nl(write, '')
  nl(write, RED + BOLD + '错误 › ' + RESET + RED + String(msg == null ? '' : msg) + RESET)
}

export function paintInjection(write, e) {
  if (!write || !e) return
  nl(write, '')
  nl(write, RED + '⚠ 远端输出中检测到疑似提示注入：' + ((e.findings || []).join('、')) + '（已标记为不可信）' + RESET)
}

// 快照注记：**不重画那一帧**（它本来就在 scrollback 里），只留一行说明
// 「结束画面已进归档与模型上下文」，让人和审计对得上。
export function paintSnapshotNote(write, final) {
  if (!write) return
  nl(write, DIM + '  ▸ ' + (final ? '终端结束画面' : '终端屏幕快照') + '已进归档与模型上下文。' + RESET)
}

// 人工 shell 经策略闸门后被拒绝/取消：在表面上说明原因（放行时无需多话，
// PTY 的命令回显与输出就是全部）。
export function paintShellVerdict(write, res) {
  if (!write || !res) return
  const st = res.status
  if (st === 'done') return
  const msg = {
    denied: '命令被策略拒绝：' + (res.reason || res.error || ''),
    cancelled: '命令未执行（审批超时或被中断）。',
    error: '命令执行失败：' + (res.error || ''),
    missing: (res.reason || '远端未找到该命令。')
  }[st] || ('命令未执行（' + st + '）。')
  nl(write, RED + '  ⤷ ' + msg + RESET)
}

// 人工命令跑完后的一句话提示：很多人不知道输出可以直接丢给 LLM 分析。
// 只在命令真正执行完（done）后出现 —— 被拒/取消/出错各有各的说法，不再叠加。
export function paintLLMHint(write) {
  if (!write) return
  nl(write, DIM + '· 想分析这段输出？直接用中文问 LLM，如「这个报错是什么意思」。' + RESET)
}

// 系统提示（切换模型、清空上下文等界面动作的回执）。
export function paintSystem(write, text) {
  if (!write) return
  nl(write, DIM + '· ' + String(text == null ? '' : text) + RESET)
}

// agent 轮次结束后的本地输入提示符。
//
// 为什么需要它：提问不走 PTY，远端 shell 不会打印新提示符——纯问答轮结束后
// 光标悬在空行上，用户不知道该在哪输入（本地行编辑会在光标处回显，功能上
// 没坏，但看起来像「没回到输入行」）。这里补一个与 paintUser 同款的 ❯，
// 后续输入落在它后面。本轮若发生过 PTY 写入（top 交接、shell 命令），
// shell 会自己打印真实提示符，调用方就不该再补（见 store 的 isPtyDirty）。
export function paintInputPrompt(write) {
  if (!write) return
  write(BLUE + BOLD + '❯ ' + RESET)
}
