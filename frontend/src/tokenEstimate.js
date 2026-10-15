// 与 internal/agent/tokenbudget.go EstimateTokens 对齐的前端估算：
// 英文约 4 字符/token，宽字符（CJK 等）按更碎的切分计。

export function estimateTokens(s) {
  if (!s) return 0
  let ascii = 0
  let other = 0
  for (const ch of String(s)) {
    const code = ch.codePointAt(0)
    if (code < 128) {
      ascii++
    } else if (code >= 0x800) {
      // UTF-8 三字节及以上（CJK 等），与 Go utf8.RuneLen>=3 对齐
      other += 2
    } else {
      other++
    }
  }
  return Math.floor(ascii / 4) + Math.floor((other * 2) / 3)
}

export function formatTokenCount(n) {
  const v = Number(n) || 0
  if (v < 1000) return String(v)
  if (v < 10000) {
    const t = (v / 1000).toFixed(1)
    return `${t.replace(/\.0$/, '')}K`
  }
  return `${Math.round(v / 1000)}K`
}

/** 助手气泡：输出 token（正文+思考）；有上下文估算时一并带上。 */
export function assistantTokenStats(it) {
  if (!it) return null
  let out = it.tokensOut
  if (out == null || out === 0) {
    out = estimateTokens(`${it.reasoning || ''}${it.content || ''}`)
  }
  if (!out) return null
  const inn = it.tokensIn || 0
  return { out, in: inn }
}
