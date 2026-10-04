// 交互式会话的输入分流：同一输入框既收自然语言（→ Agent），也收 shell 命令（→ 策略后 Exec）。
//
// 判定顺序：
//   1. 以 ? / ？ 开头 → 强制走 Agent（去掉前缀）
//   2. 以 ! 开头      → 强制走 shell（去掉前缀，中文命令也能跑）
//   3. 首词是命令     → shell（**不管有没有中文**：`echo 你好` 意图明确）
//   4. 含中日韩字符   → Agent（中文自然语言：`你是什么模型`、`看看磁盘`）
//   5. 以 ? 结尾      → Agent（英文问句）
//   6. 多行           → Agent（长描述）
//   7. 其余           → shell
//
// 「首词是不是命令」由**后端**判定（见 ClassifyShellInput）。
// 这里刻意不内置命令清单：
//   - 命令识别需要「Linux 命令库」这份知识，它已经在 internal/policy 里
//     （PrimaryBinary / IsKnownBinary / shellBuiltins + vault 只读白名单）；
//   - 前端再抄一份必然与后端漂移，而且抄不准 —— 手抄版把 `nginx 起不来了`
//     判成了命令（nginx 确实在清单里），可它是句中文提问。
//
// 因此分流分两步：
//   - 同步部分：前缀、语言特征等纯文本规则（classifyInput）；
//   - 异步部分：首词识别要问后端（resolveInput），只在真正需要时才问一次。
//
// 拿不准时（首词是陌生单词）走 needsRouteChoice，由界面问一次。

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
  if (/[\u4e00-\u9fff\u3040-\u30ff\uac00-\ud7af]/.test(trimmed)) {
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

// isCommand 问后端「这行首词是不是命令」。
// 后端不可用时（浏览器裸跑 dev server、测试未装桩）返回 false ——
// 此时分流退化成「非中文即 shell」，与旧行为一致，不会更糟。
async function isCommand(text, api) {
  if (!api || typeof api.ClassifyShellInput !== 'function') return false
  try {
    return !!(await api.ClassifyShellInput(text))
  } catch {
    return false
  }
}

/**
 * resolveInput 是**界面实际该用的入口**：在同步规则之上补一次命令识别。
 *
 * 为什么需要它：`echo 你好` 含中文，同步规则会判成 Agent；
 * 但首词 echo 是明确的命令，应该走 shell。这一步只能在问过后端后才知道。
 *
 * @param {string} raw
 * @param {object} api 后端绑定（window.go.main.App），可空
 * @returns {Promise<{ kind: 'agent'|'shell', text: string, uncertain: boolean }>}
 *   uncertain=true 表示首词不认识、判断不了，界面该问用户一次。
 */
export async function resolveInput(raw, api) {
  const trimmed = String(raw ?? '').trim()
  const base = classifyInput(trimmed)
  if (!trimmed) return { ...base, uncertain: false }

  // 显式表态（? / !）与纯语言特征（问号结尾、多行）已经定了，不必再问后端：
  // 少一次跨进程往返，用户敲回车到发出提问之间没有额外延迟。
  if (trimmed.startsWith('?') || trimmed.startsWith('？') || trimmed.startsWith('!')) {
    return { ...base, uncertain: false }
  }
  if (/\?\s*$/.test(trimmed) || trimmed.includes('\n')) {
    return { ...base, uncertain: false }
  }

  // 到这里只剩两类要问后端：含中文的（可能是 `echo 你好`）与纯英文的
  // （可能是 `df -h`，也可能是 `foobar --help`）。
  if (await isCommand(trimmed, api)) {
    return { kind: 'shell', text: trimmed, uncertain: false }
  }
  // 首词不是命令。含中文 → 中文提问，直接 Agent；纯英文 → 真的拿不准。
  if (/[\u4e00-\u9fff\u3040-\u30ff\uac00-\ud7af]/.test(trimmed)) {
    return { kind: 'agent', text: trimmed, uncertain: false }
  }
  return { kind: 'shell', text: trimmed, uncertain: true }
}
