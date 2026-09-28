// 交互式会话的输入分流：同一输入框既收自然语言（→ Agent），也收 shell 命令（→ 策略后 Exec）。
//
// 规则刻意保守、可预测，避免「猜错用户意图」：
//   1. 以 ? / ？ 开头 → 强制走 Agent（去掉前缀）
//   2. 含中日韩字符 → Agent（排查话术几乎都是中文）
//   3. 以 ? 结尾 → Agent（英文问句）
//   4. 多行 → Agent（长描述）
//   5. 其余 → shell
//
// 拿不准时让用户加 ? 前缀，比静默跑错命令安全。

/**
 * @param {string} raw
 * @returns {{ kind: 'agent' | 'shell', text: string }}
 */
export function classifyInput(raw) {
  const original = String(raw ?? '')
  const trimmed = original.trim()
  if (!trimmed) return { kind: 'agent', text: '' }

  if (trimmed.startsWith('?') || trimmed.startsWith('？')) {
    return { kind: 'agent', text: trimmed.slice(1).trim() }
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
