// 交互式会话的输入分流：同一输入框既收自然语言（→ Agent），也收 shell 命令（→ 硬拒绝后写 PTY）。
//
// resolveInput 的判定顺序（与下面函数一致）：
//   1. 以 ? / ？ 开头 → 强制走 Agent（去掉前缀）
//   2. 以 ! 开头      → 强制走 shell（去掉前缀，中文命令也能跑）
//   3. 以 ? 结尾 / 多行 → Agent
//   4. 数字编号回复   → Agent（`2`、`2 写入`）
//   5. 首词是本地命令 → shell（含 agent 对话中的 `ls`）
//      例外：agent 对话中、整行只有一个回复同形词（`id`/`true`/`date`）→ Agent
//      例外：命令/路径后跟中文自然语言（`nginx 起不来了`、`./start.sh 执行报错`）→ Agent
//            （`echo 你好` 这类「文本就是参数」的命令仍走 shell）
//   6. 含中日韩且不是命令 → Agent（`看看磁盘`、`好的`、`执行报错 ./start.sh`）
//   7. 问不了命令库   → 非中文按 shell；中文已在上一步走 Agent
//   8. agent 对话中、首词是回复撞车词（`yes`、`open`）→ Agent，不探测、不执行
//      要真跑用 `!yes` / `!open`
//   9. 其余未知词（含单个 `bt`）→ 远端 command -v：存在就执行，不存在才 Agent
//
// 「首词是不是命令」由**后端**判定（见 ClassifyShellInput）。
// 命令库 = shellBuiltins + vault.DefaultWhitelist，再加用户白名单作扩展；
// **不是**用户当前策略白名单本身（清空后 `ls` 仍是命令）。
// 前端不内置完整命令清单；「命令 + 中文提问」靠 isCJKNaturalLanguage 与
// 少数「中文是数据」的命令白名单区分，避免再把 `nginx 起不来了` 判成命令。
//
// 因此分流分两步：
//   - 同步部分：前缀、语言特征等纯文本规则（classifyInput）；
//   - 异步部分：首词识别要问后端（resolveInput），只在真正需要时才问一次。
//
// 首词不在本地库时不再弹窗：远端 `command -v` 存在就跑，不存在才交给 Agent。

/**
 * 同步分流。只看**纯文本特征**，不做命令词识别。
 *
 * 之所以能同步：前缀是显式表态、中文/问号/多行是语言特征，
 * 这些都不依赖命令库。命令词识别单独走 resolveInput。
 *
 * @param {string} raw
 * @returns {{ kind: 'agent' | 'shell', text: string }}
 */
export function classifyInput(raw) {
  const trimmed = String(raw ?? '').trim()
  if (!trimmed) return { kind: 'agent', text: '' }

  if (trimmed.startsWith('?') || trimmed.startsWith('？')) {
    return { kind: 'agent', text: trimmed.slice(1).trim() }
  }
  if (trimmed.startsWith('!')) {
    return { kind: 'shell', text: trimmed.slice(1).trim() }
  }
  if (isNumberedReply(trimmed)) {
    return { kind: 'agent', text: trimmed }
  }
  if (hasCJK(trimmed)) {
    return { kind: 'agent', text: trimmed }
  }
  if (/\?\s*$/.test(trimmed)) {
    return { kind: 'agent', text: trimmed }
  }
  if (trimmed.includes('\n')) {
    return { kind: 'agent', text: trimmed }
  }
  return { kind: 'shell', text: trimmed }
}

// isNumberedReply 判断一行是不是「对编号选项的回复」：整行是数字（`2`），或数字后
// 跟编号标点/空白（`2.`、`2)`、`2、`、`2 写入`）。
//
// 为什么单列一条：用户在回答 LLM 的「1. 换个路径  2. 写入一些内容」这类编号选项时，
// 常直接回「2」或「2 写入 ...」。shell 里没有以数字命名的真实命令，把 `2` 当命令执行
// 必然报 `2: command not found` —— 它是选项编号，不是命令，必须交给 LLM。
// 极少数 fd 重定向写法（`2>file`）不在交互输入场景，且不匹配本规则（数字后不是空格/标点结尾）。
export function isNumberedReply(trimmed) {
  return /^\d+[.、)]?(?=\s|$)/.test(trimmed)
}

export function hasCJK(s) {
  return /[\u4e00-\u9fff\u3040-\u30ff\uac00-\ud7af]/.test(s)
}

// 这些命令的参数本来常是任意文本，中文是数据不是提问（`echo 你好`）。
const CJK_DATA_COMMANDS = new Set([
  'echo', 'printf', 'print', 'grep', 'egrep', 'fgrep', 'rg',
  'sed', 'awk', 'export', 'alias', 'set', 'expr'
])

function firstTokenRaw(trimmed) {
  return String(trimmed || '').trim().split(/\s+/)[0] || ''
}

function commandBaseName(trimmed) {
  return firstTokenRaw(trimmed).replace(/^.*\//, '').toLowerCase()
}

// isCJKNaturalLanguage：整行含中文，且不像「命令 + 文本参数」。
// 典型提问：`nginx 起不来了`、`./start.sh 执行报错`、`执行报错 ./start.sh`。
// 典型参数：`echo 你好`、`grep 错误 app.log`（首词在 CJK_DATA_COMMANDS）。
export function isCJKNaturalLanguage(trimmed) {
  const s = String(trimmed || '').trim()
  if (!s || !hasCJK(s)) return false
  const first = firstTokenRaw(s)
  if (hasCJK(first)) return true
  return !CJK_DATA_COMMANDS.has(commandBaseName(s))
}

// 对话里常被当成一句回复、同时又是命令名的单词。
// 仅「整行只有这一个词」且正处于 agent 对话时交给 Agent；
// 带参数（`date -u`、`id root`）仍是命令，走 shell。
// `ls`/`df`/`pwd` 不在此列：单独敲也必须执行。
const AMBIGUOUS_BARE = new Set([
  'id', 'who', 'w', 'true', 'false', 'test', 'type', 'time',
  'file', 'host', 'free', 'date'
])

export function isAmbiguousBareReply(trimmed) {
  if (/\s/.test(trimmed)) return false
  return AMBIGUOUS_BARE.has(String(trimmed).toLowerCase())
}

// 对话里常被当成回复、但 command -v 又能找到的词。
// `yes` 会刷屏，`open 3306` 会被当成「打开端口」的回答。
// agent 对话中整行（含参数）都不执行；非对话或 `!` 前缀仍走探测/shell。
const REPLY_COLLISION = new Set(['yes', 'open'])

function firstToken(trimmed) {
  const first = String(trimmed || '').trim().split(/\s+/)[0] || ''
  return first.replace(/^.*\//, '').toLowerCase()
}

export function isReplyCollision(trimmed) {
  return REPLY_COLLISION.has(firstToken(trimmed))
}

// isCommand 问后端「这行首词是不是命令」。
// 返回 true / false；问不了（无绑定、抛错）返回 null，调用方按「非中文即 shell」退化，
// 避免绑定失败时把 `ls` 送进 Agent。
async function isCommand(text, api) {
  if (!api || typeof api.ClassifyShellInput !== 'function') return null
  try {
    return !!(await api.ClassifyShellInput(text))
  } catch {
    return null
  }
}

// resolveUnknown 对「首词不认识」的一行做一次远端探测，决定去向。
//
// 为什么需要它：本地命令库判断不了「用户自己装的脚本 / 第三方工具」——
// `ceshi` 可能就是用户在远端写的脚本。与其弹窗打断（旧行为），不如
// 探测一次：存在就当命令跑，不存在才交给 LLM。探测是零副作用的
// （后端只跑 `command -v`，见 app_shell.go ResolveUnknownInput）。
//
// 后端不可用 / 无 hostID 时退回「当 shell 跑」—— 与本地判断的退化一致：
// 宁可让 shell 报 command not found，也不擅自转 LLM。
//
// 返回：
//   { kind: 'shell' }                          → 当命令跑
//   { kind: 'agent', missing: true, binary }   → 交给 LLM 理解意图
async function resolveUnknown(text, api, hostId) {
  if (!api || typeof api.ResolveUnknownInput !== 'function' || !hostId) {
    return { kind: 'shell' }
  }
  try {
    const d = await api.ResolveUnknownInput(hostId, text)
    if (d && d.kind === 'agent') {
      return { kind: 'agent', missing: true, binary: d.binary || '' }
    }
    return { kind: 'shell' }
  } catch {
    return { kind: 'shell' }
  }
}

// unknownToQuestion 把「一条远端不存在的命令」包装成给 LLM 的问题。
//
// 刻意**只转这一句**，不把原文直接当提问 —— 原文可能被模型当成「用户要我执行
// 这条命令」的指令（诱导风险）。这里明确告诉模型：这是一条**未找到**的命令，
// 请你理解用户意图；原文只作为被引用的对象出现，位于反引号内。
export function unknownToQuestion(binary, text) {
  const name = binary ? `\`${binary}\`` : '这条命令'
  return `我输入了一条命令 ${name}，但目标主机上不存在该命令。` +
    `请帮我理解我可能想做什么（我的输入原文：\`${text}\`）。`
}

/**
 * resolveInput 是**界面实际该用的入口**：在同步规则之上补一次命令识别。
 *
 * 为什么需要它：`echo 你好` 含中文，同步规则会判成 Agent；
 * 但首词 echo 是明确的命令，应该走 shell。这一步只能在问过后端后才知道。
 *
 * 未知词（首词不在命令库）**不再弹窗问用户**：改为补一次远端探测
 * （见 resolveUnknown）—— 存在就当命令跑，不存在才自动交给 LLM 理解意图。
 * 这样「直接执行、失败才介入」的默认成立，且只有「证明不存在」这条
 * 零副作用的路径才会触发介入（安全闸门，见 ResolveUnknownInput 注释）。
 *
 * @param {string} raw
 * @param {object} api 后端绑定（window.go.main.App），可空
 * @param {string} hostId 当前主机 id（探测需要），可空
 * @param {boolean} [agentMode] 本会话是否已在 agent 对话中。为真时：
 *   已知 Linux 命令仍走 shell；回复同形词（`id`/`true`）和回复撞车词（`yes`/`open`）走 Agent；
 *   其余未知词（含单个 `bt`）一律远端探测，存在就执行。
 * @returns {Promise<{ kind: 'agent'|'shell', text: string, missing?: boolean, binary?: string }>}
 *   missing=true 表示该命令在远端不存在，text 已包装成给 LLM 的问题。
 */
export async function resolveInput(raw, api, hostId, agentMode = false) {
  const trimmed = String(raw ?? '').trim()
  const base = classifyInput(trimmed)
  if (!trimmed) return base

  // 显式表态（? / !）与纯语言特征（问号结尾、多行）已经定了，不必再问后端：
  // 少一次跨进程往返，用户敲回车到发出提问之间没有额外延迟。
  if (trimmed.startsWith('?') || trimmed.startsWith('？') || trimmed.startsWith('!')) {
    return base
  }
  if (/\?\s*$/.test(trimmed) || trimmed.includes('\n')) {
    return base
  }
  // 数字编号回复（`2`、`2.`、`2 写入...`）直接交给 Agent，**不**去问命令库。
  // 否则 `2` 会被当成首词命令去探测/执行，报 `2: command not found`（用户截图里的 bug）。
  if (isNumberedReply(trimmed)) {
    return { kind: 'agent', text: trimmed }
  }

  const known = await isCommand(trimmed, api)
  // 已知 Linux 命令 → shell。聊过 Agent 也不能把 `ls`/`df -h` 改成提问。
  // 例外：对话中整行只有一个回复同形词（`id`/`true`/`date`）→ Agent。
  // 例外：命令/路径后跟中文自然语言 → Agent，让模型自己去查、去跑
  // （`./start.sh 执行报错`、`nginx 起不来了`）；`echo 你好` 仍走 shell。
  if (known === true) {
    if (agentMode && isAmbiguousBareReply(trimmed)) {
      return { kind: 'agent', text: trimmed }
    }
    if (isCJKNaturalLanguage(trimmed)) {
      return { kind: 'agent', text: trimmed }
    }
    return { kind: 'shell', text: trimmed }
  }

  // 含中文且不是已知命令 → 提问（`看看磁盘`、`执行报错 ./start.sh`）。
  // 问不了命令库时同样按语言走，避免 `好的` 在绑定失败时被当 shell。
  if (hasCJK(trimmed)) {
    return { kind: 'agent', text: trimmed }
  }

  // 问不了后端：非中文按 shell。`ls` 不能因为 ClassifyShellInput 缺失而进 Agent。
  if (known === null) {
    return { kind: 'shell', text: trimmed }
  }

  // yes / open：远端几乎一定有同名命令。对话中当回复，不探测，避免刷屏或误执行。
  // `!yes` 已在前面走 shell。
  if (agentMode && isReplyCollision(trimmed)) {
    return { kind: 'agent', text: trimmed }
  }

  // 已确认不是本地命令（含 agent 对话里的单个词 `bt`）。
  // 一律做远端探测：存在就当命令执行，不存在才交给 Agent。
  const decided = await resolveUnknown(trimmed, api, hostId)
  if (decided.kind === 'agent') {
    return {
      kind: 'agent',
      text: unknownToQuestion(decided.binary, trimmed),
      missing: true,
      binary: decided.binary
    }
  }
  return { kind: 'shell', text: trimmed }
}
