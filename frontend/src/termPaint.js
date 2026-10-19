// termPaint.js —— 把 agent 事件写成带 ANSI 样式的终端行，渲染进控制台那一片
// xterm 表面（阿里云 Workbench 同款：对话/命令/审批/错误全落在同一条终端流里）。
//
// 颜色只用**基础 ANSI SGR**（30-37 那套），不写死具体色值：xterm 会用当前
// 主题（termTheme.js 注入的 options.theme）的调色板去映射这些颜色，于是明暗
// 主题自动跟随，这里不必关心配色。
//
// 每个函数都收一个 `write(str)` 落点（就是某台主机 xterm 实例的 write），
// 自己负责补 \r\n —— 终端不像 DOM 会自动换行。

import { t } from './i18n'
import { createMdAnsiStream } from './termMarkdown.js'

export { createMdAnsiStream }

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
  if (trimmed) out += '\r\n' + DIM + t('tl.clip.trimmed') + RESET
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
//
// 前缀是软件的**文字 wordmark**：`Ai-Shell >` —— 一眼认出「这是 Ai-Shell 在向你
// 提问」，而不是远端 shell 的提示符（远端是 `root@host:~$`）。用文字而不是图形字形
// 是因为图形 logo（❯▮ 之类）在等宽字体里容易与真提示符、光标挤在一起，还依赖字体
// 是否含对应字形；文字版在任何字体下都清晰、可复制、可回滚，语义也更直白。
//
// 颜色沿用 logo 的渐变两端：`Ai-Shell` 青（#22D3EE），`>` 紫（#8B5CF6），
// 正文用默认前景色。
const LOGO_CYAN = '\x1b[38;2;34;211;238m'   // #22D3EE
const LOGO_PURPLE = '\x1b[38;2;139;92;246m' // #8B5CF6

// 返回用户行前缀的 ANSI 串：`Ai-Shell`（青、粗） + 空格 + `>`（紫、粗）。
function userPromptMark() {
  return LOGO_CYAN + BOLD + 'Ai-Shell' + RESET + ' ' + LOGO_PURPLE + BOLD + '>' + RESET
}

export function paintUser(write, text) {
  if (!write) return
  nl(write, '')
  // 前缀后空一格再写正文；正文用默认前景色，不被前缀的颜色带跑。
  nl(write, userPromptMark() + ' ' + String(text == null ? '' : text))
}

// agent 回复的开头：先落「模型图标 + 模型名 + `>`」抬头，随后 delta 逐字接在后面。
//
// 为什么用 `>` 而不是 `:`：与提问行 `Ai-Shell >` 是**同一套语法** ——
// `X >` 就是「X 在说话」，看一眼就懂，用户不必学第二套归属标记。
// 用 `:` 则界面里同时存在 `Ai-Shell >` 和 `DS 国外:` 两种写法，反而模糊。
//
// info（可选）来自 providerTermBadge：{ mark, color, name } —— mark 是厂商图标字形，
// color 是品牌色（#RRGGBB，转 truecolor），name 是模型/方案名。缺省时退回原来的绿点，
// 保证没有模型信息（如测试、异常路径）时抬头依然成立。
export function paintAssistantHeader(write, info) {
  if (!write) return
  nl(write, '')
  // 模型身份不明：退化成原来的绿点抬头，不与正文挤在一行。
  if (!info || (!info.mark && !info.name)) {
    write(GREEN + BOLD + '● ' + RESET)
    return
  }
  const bullet = GREEN + BOLD + '●' + RESET
  const icon = info.color
    ? trueColor(info.color) + BOLD + String(info.mark || '') + RESET
    : BOLD + String(info.mark || '') + RESET
  const name = info.name ? ' ' + info.name : ''
  // 紫色 `>` 与提问行 `Ai-Shell >` 的那个 `>` 同一个色（LOGO_PURPLE）——
  // 「这是模型在回答」这件事由符号本身承担，不靠颜色区分。
  const chevron = LOGO_PURPLE + BOLD + ' >' + RESET
  write(bullet + ' ' + icon + name + chevron + '  ')
}

// trueColor 把 #RRGGBB 转成 ANSI 真彩前景序列。xterm 支持 24 位色，
// 于是终端抬头的图标颜色能精确对上界面里的品牌色。非法值回退默认色（空串）。
function trueColor(hex) {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex || '')
  if (!m) return ''
  const n = parseInt(m[1], 16)
  return `\x1b[38;2;${(n >> 16) & 255};${(n >> 8) & 255};${n & 255}m`
}

// 流式增量：经 MD→ANSI 渲染器再写入。md 由调用方挂在本轮助手气泡上（createMdAnsiStream），
// 缺省时退回原文 + \r\n 归一（测试/异常路径）。
//
// 裸 \n 必须变成 \r\n：xterm 默认 convertEol=false，裸 LF 只「下移一行、列不变」，
// 会阶梯状右偏。渲染器内部已按 \r\n 吐出行尾。
export function paintAssistantDelta(write, text, md) {
  if (!write) return
  if (md && typeof md.push === 'function') {
    const out = md.push(text)
    if (out) write(out)
    return
  }
  write(String(text == null ? '' : text).replace(/\r?\n/g, '\r\n'))
}

// 思考增量：弱化灰字，样式自包含（每段自带 DIM…RESET），
// 后续正文另起一行落●抬头时天然回到正常样式。终端表面无法折叠，
// 思考全程平铺，与 Claude Code 等终端工具的观感一致。
export function paintAssistantReasoning(write, text) {
  if (!write) return
  write(DIM + String(text == null ? '' : text).replace(/\r?\n/g, '\r\n') + RESET)
}

// 思考占位：用户提问后、模型第一段输出到达前，就地显示一行 `Working....` 动画，
// 表示「正在工作时」。与 paintWelcomeHint 同一套「占位符」思路 —— 写在光标当前
// 行、不换行，用完由调用方整行擦掉，绝不把装饰留进命令输出流里。
//
// 用 \r 回到行首重画：调用方每帧把**点数**递增（0 → 1 → 2 → 3 → 4 → 0 …），
// 肉眼就是 `Working` 后面跟着滚动的点。`width` 是上一帧画过的显示宽度，先按它
// 擦干净再画新的一帧，避免帧变短时残留尾巴。
//
// frame 是**点数**（0..4）：0 个点时就是 `Working` 本身。
// 返回本帧的显示宽度，调用方登记下来供下一帧/擦除使用。
const WORKING_LABEL = 'Working'
// DECTCEM：隐藏 / 显示光标。思考占位期间隐藏光标 —— 否则一个实心光标块会杵在
// `Working....` 后面一闪一闪（用户明确要求：Working 后面不要显示光标）。擦除时恢复。
const HIDE_CURSOR = '\x1b[?25l'
const SHOW_CURSOR = '\x1b[?25h'
export function paintThinking(write, frame, width = 0) {
  if (!write) return width
  const dots = '.'.repeat(Math.max(0, frame))
  const text = DIM + WORKING_LABEL + dots + RESET
  // 光标已停在提问行的**下一行**行首（paintUser 结尾落了换行），直接在这一行
  // 左对齐写 `Working`：与上方提问行之间只隔「一行」——不多留空行，间距最小。
  // 后续帧用 \r 原地重画（先按上一帧宽度擦净），不会一行行往下跑。
  // 首帧（width=0）顺带隐藏光标，并把它留在文字之后（隐藏了看不见，无需回退）。
  const erase = width > 0 ? '\r' + ' '.repeat(width) + '\r' : '\r'
  const hide = width > 0 ? '' : HIDE_CURSOR
  write(hide + erase + text)
  return WORKING_LABEL.length + dots.length
}

// 擦掉思考占位，恢复光标显示，把光标停回原行行首（提问行的下一行）。
//
// 只擦这一行、不上移：占位本就只占一行，擦完这一行恢复空白，光标留在行首，
// 后续输出（回答抬头）从这一行落下，与提问行只隔一行，衔接自然、不留多余空行。
// 恢复光标必须与 paintThinking 的隐藏配对（都在确实画过 / 擦除时各做一次）。
export function clearThinking(write, width) {
  if (!write || !width) return
  write(SHOW_CURSOR + '\r' + ' '.repeat(width) + '\r')
}

// 回复定稿：冲掉 MD 渲染器里未完成的尾行，再补一个换行把光标停到下一行行首。
// md 可空（思考→正文切换时只换行、尚无正文流）。
export function paintAssistantEnd(write, md) {
  if (!write) return
  if (md && typeof md.flush === 'function') {
    const rest = md.flush()
    if (rest) write(rest)
  }
  nl(write, '')
}

// 裁决显示文案（键 → i18n key）。decision 值本身（allow/confirm/deny）是
// 与后端协议匹配的标识符，不翻；只有展示层走字典。
const DECISION_LABELS = { allow: 'tl.decision.allow', confirm: 'tl.decision.confirm', deny: 'tl.decision.deny' }

// 执行计划：每次 revise_plan 成功后画一帧清单，方便对照「强制遵循」的当前活动项。
export function paintPlan(write, plan) {
  if (!write || !plan) return
  nl(write, '')
  const reason = plan.reason ? DIM + ' · ' + String(plan.reason) + RESET : ''
  nl(write, CYAN + BOLD + '☰ ' + t('tl.plan.title') + RESET + reason)
  const items = Array.isArray(plan.items) ? plan.items : []
  for (const it of items) {
    const st = String(it.status || '')
    const mark = st === 'active' ? '→' : st === 'done' ? '✓' : st === 'dropped' ? '×' : '·'
    const color = st === 'active' ? YELLOW : st === 'done' ? GREEN : st === 'dropped' ? DIM : RESET
    nl(write, color + '  ' + mark + ' [' + String(it.class || '') + '] ' + String(it.intent || it.id || '') + RESET)
  }
}

// 工具调用：**实时**画一行 —— agent 每调一次画一次，让人当场看到它在调什么工具，
// 不必等收尾、也不必翻「对话归档」。卡片风格（▸ + 工具名 + 命令），
// 刻意不用 shell 的 `$ cmd`，避免一段对话看起来像命令行。
export function paintToolCall(write, view) {
  if (!write || !view) return
  nl(write, '')
  const decKey = DECISION_LABELS[view.decision]
  const dec = view.decision && view.decision !== 'allow' && decKey
    ? ' · ' + YELLOW + t(decKey) + RESET
    : ''
  const host = view.hostName ? DIM + ' · @' + view.hostName + RESET : ''
  nl(write, MAGENTA + '▸ ' + RESET + BOLD + String(view.name || t('tl.tool.fallback')) + RESET + dec + host)
  if (view.command) nl(write, DIM + '  ' + String(view.command) + RESET)
}

// 工具结果：一行紧凑状态（完成 / 失败（码 N））—— 不铺开输出、不写「退出码」，
// 输出与详情在「对话归档」与模型上下文里。命中 TTY 时改画「已交接」注记。
export function paintToolResultLine(write, res) {
  if (!write || !res) return
  if (res.tty) {
    nl(write, CYAN + '  ⤷ ' + t('tl.tool.tty') + RESET)
    return
  }
  const code = res.exitCode
  const ok = code === 0 || code == null || code === ''
  const status = ok ? GREEN + t('tl.tool.done') : RED + t('tl.tool.fail', { code })
  const red = res.redacted ? YELLOW + t('tl.tool.redacted', { n: res.redacted }) : ''
  nl(write, '  ⤷ ' + status + RESET + red + RESET)
}

// 审批提示：审批条/弹窗是交互入口，表面上只留一行注记说明「为什么停住了」。
export function paintApproval(write, view) {
  if (!write || !view) return
  nl(write, '')
  nl(write, YELLOW + BOLD + '⚠ ' + t('tl.approval') + RESET + YELLOW + String(view.command || view.name || '') + RESET)
  if (view.reason) nl(write, DIM + '  ' + view.reason + RESET)
}

export function paintError(write, msg) {
  if (!write) return
  nl(write, '')
  nl(write, RED + BOLD + t('tl.error') + ' › ' + RESET + RED + String(msg == null ? '' : msg) + RESET)
}

export function paintInjection(write, e) {
  if (!write || !e) return
  nl(write, '')
  nl(write, RED + '⚠ ' + t('tl.injection', { findings: (e.findings || []).join('、') }) + RESET)
}

// 快照注记：**不重画那一帧**（它本来就在 scrollback 里），只留一行说明
// 「结束画面已进归档与模型上下文」，让人和审计对得上。
export function paintSnapshotNote(write, final) {
  if (!write) return
  nl(write, DIM + '  ▸ ' + t(final ? 'tl.snapshot.final' : 'tl.snapshot.live') + RESET)
}

// 人工 shell 被硬拒绝或执行失败：在表面上说明原因（放行时无需多话，
// PTY 的命令回显与输出就是全部）。
export function paintShellVerdict(write, res) {
  if (!write || !res) return
  const st = res.status
  if (st === 'done') return
  const msg = {
    denied: t('tl.shell.denied', { reason: res.reason || res.error || '' }),
    error: t('tl.shell.error', { err: res.error || '' }),
    missing: res.reason || t('tl.shell.missing')
  }[st] || t('tl.shell.notRun', { st })
  nl(write, RED + '  ⤷ ' + msg + RESET)
}

// 人工命令跑完后的一句话提示：很多人不知道输出可以直接丢给 LLM 分析。
// 只在命令真正执行完（done）后出现 —— 被拒/出错各有各的说法，不再叠加。
//
// 位置（用户反馈的演进史，别改回去）：
//   v1 追加在命令输出流里（另起一行）→ 用户误以为「进了 agent 模式」；
//   v2 DECSC/DECRC 画在提示符下方独立一行 → 不换行不占位的要求落空，
//     用户明确要求：**不要换行显示**。
//   v3（当前）就地写在与 shell 提示符**同一行**、紧跟提示符之后，像
//     paintWelcomeHint 的占位符一样；写完用绝对列定位把光标移回提示符末尾。
//     用户一打字，ConsolePanel 会把这块占位（t.hintCols）擦掉 —— 与欢迎引导
//     同一套机制，提示永远不挡输入、不占屏幕。
//
// startCol 是提示符末尾所在的列（0 基）：提示从这一列之后开始画，写完把
// 光标移回这一列。返回占位符的显示宽度（调用方登记给擦除逻辑用）。
//
// 它**只往表面写一行字**，不调用任何 Ask / 不改 store.running（见
// store.test.js 的「执行命令绝不进 agent」护栏用例）。
export function paintLLMHint(write, startCol = 0) {
  if (!write) return 0
  const text = '  ' + t('tl.llmHint')
  write(DIM + text + RESET + '\x1b[' + (startCol + 1) + 'G')
  return displayCols(text)
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
// 没坏，但看起来像「没回到输入行」）。这里补一个**与提问行完全同款**的前缀
// `Ai-Shell >`，后续输入落在它后面。本轮若发生过 PTY 写入（top 交接、shell
// 命令），shell 会自己打印真实提示符，调用方就不该再补（见 store 的 isPtyDirty）。
//
// 必须复用 userPromptMark()：早期这里写的是 `❯`，而提问行改成了 `Ai-Shell >`
// 之后，回合结束就冒出个孤零零的 `❯`，两处风格对不上（用户截图里的问题）。
export function paintInputPrompt(write) {
  if (!write) return
  write(userPromptMark() + ' ' + RESET)
}

// 欢迎引导：每次打开终端、以及从「对话归档」切回表面时提示一次。
//
// 顶部 banner 是常驻的，但常驻的东西会被眼睛过滤掉——用户第一次面对一个
// 光秃秃的 shell 提示符时，并不知道这里除了敲命令还能说人话。
//
// 位置：**就地写在光标所在的输入行上**（紧跟真提示符之后），而不是另起一行。
// 另起一行会把这行说明混进命令输出流里 —— 它看起来像一条命令的输出了什么，
// 翻回滚时也一直在。写在光标处则像「占位符」，语义清楚：这里是要你输入的地方。
//
// 它只是**占位符**，不是真输入：光标停在提示符与提示文字之间，用户敲下第一个
// 字符时由调用方（ConsolePanel 的 clearWelcomeHint）整行擦掉重画，提示消失、
// 正常回显。所以这里把光标**留在提示文字之前** —— 否则用户会直觉地在提示后面
// 打字，而本地行编辑会把字符插进提示文字中间。
//
// 返回占位符的显示宽度，调用方要拿它把光标移回原位（擦除后重画提示符）。
//
// startCol 是提示符末尾所在的列（0 基，光标就停在那里）—— 提示必须从这一列
// 开始写，写完再把光标移**回这一列**。不能用「提示宽度的倒推」去定位：ESC[nG
// 是绝对列定位，n 应当由 startCol 决定，按宽度算只在提示符恰好从列 1 起时才对，
// 而真实提示符（如 root@host:~$ ）有十几个字符宽，算出来会偏到左边盖住提示符。
export function paintWelcomeHint(write, startCol = 0) {
  if (!write) return 0
  const text = t('tl.welcomeHint')
  // 写完提示光标停在末尾，用绝对列定位移回起点（ANSI 列号从 1 起，故 +1）。
  write(DIM + text + RESET + '\x1b[' + (startCol + 1) + 'G')
  return displayCols(text)
}

// displayCols 按终端显示宽度数一列列地算：CJK/全角占 2 列，其余 1 列。
// 与 ConsolePanel 的 displayWidth 同源，但这里只关心总列数。
function displayCols(s) {
  let w = 0
  for (const ch of String(s)) {
    const c = ch.codePointAt(0)
    const wide = (c >= 0x1100 && c <= 0x115f) ||
      (c >= 0x2e80 && c <= 0xa4cf) ||
      (c >= 0xac00 && c <= 0xd7a3) ||
      (c >= 0xf900 && c <= 0xfaff) ||
      (c >= 0xfe30 && c <= 0xfe6f) ||
      (c >= 0xff00 && c <= 0xff60) ||
      (c >= 0xffe0 && c <= 0xffe6)
    w += wide ? 2 : 1
  }
  return w
}
