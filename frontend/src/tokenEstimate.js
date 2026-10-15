// 与 internal/agent/tokenbudget.go EstimateTokens 对齐的前端估算：
// 英文约 4 字符/token，宽字符（CJK 等）按更碎的切分计。

// 与 Go requestOverhead / perMessageOverhead 同口径，空闲时也能估「下一轮大概多大」。
const PER_MESSAGE_OVERHEAD = 4
const REQUEST_OVERHEAD = 1800
// 与 Go estimateRequestTokens 里 image_url 的保守估对齐（偏高，避免圆环低估）。
const IMAGE_TOKEN_BASE = 800
const IMAGE_URL_CHARS_PER_TOKEN = 100

/** 单张待发图的保守 token 估（mime + base64 data，可无 data URL 前缀）。 */
export function estimateImageTokens(img) {
  if (!img) return IMAGE_TOKEN_BASE
  const mime = String(img.mime || '')
  const data = String(img.data || '')
  const urlLen = data.startsWith('data:')
    ? data.length
    : (`data:${mime};base64,`.length + data.length)
  return IMAGE_TOKEN_BASE + Math.floor(urlLen / IMAGE_URL_CHARS_PER_TOKEN)
}

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

/**
 * 根据本地时间线估算「下一轮请求大概多大」。
 * 不含服务端摘要/完整系统提示全文，但比「发送前永远 0」有用得多。
 * pendingImages：作曲箱里尚未发出的附图，按 Go 同口径计入。
 */
export function estimateSessionContext(entries, pendingImages = []) {
  let tokens = REQUEST_OVERHEAD
  let messages = 0
  for (const e of entries || []) {
    if (!e) continue
    if (e.kind === 'user' || e.kind === 'assistant') {
      tokens += PER_MESSAGE_OVERHEAD + estimateTokens(`${e.reasoning || ''}${e.content || ''}`)
      messages++
      continue
    }
    if (e.kind === 'result') {
      tokens += PER_MESSAGE_OVERHEAD + estimateTokens(e.content || '')
      messages++
      continue
    }
    if (e.kind === 'tool' && e.tool) {
      tokens += PER_MESSAGE_OVERHEAD +
        estimateTokens(e.tool.name || '') +
        estimateTokens(e.tool.command || e.tool.args || '')
      messages++
    }
  }
  const imgs = Array.isArray(pendingImages) ? pendingImages.filter(Boolean) : []
  if (imgs.length) {
    // 附图会进下一轮 user 消息（即使文案还空），按即将多一条消息计。
    tokens += PER_MESSAGE_OVERHEAD
    messages++
    for (const img of imgs) {
      tokens += estimateImageTokens(img)
    }
  }
  return { tokens: messages ? tokens : 0, messages }
}

/**
 * 粗匹配模型窗口（与 Go tokenbudget.tier 对齐的常用档）。
 * 方案里若配置了 contextWindow，应优先用配置值，不要走这里。
 */
export function contextWindowForModel(model) {
  const low = String(model || '').toLowerCase()
  if (!low) return 0
  if (low.includes('gemini') || low.includes('qwen-long')) return 1000000
  if (low.includes('claude')) return 200000
  if (low.includes('gpt-4o') || low.includes('gpt-4.1') || low.includes('gpt-4-turbo')) return 128000
  if (low.includes('qwen') || low.includes('glm-4') || low.includes('moonshot') || low.includes('kimi')) return 131072
  // Go 内置表：deepseek → 64K（不是 128K）；线路更大请在 LLM 方案里手填。
  if (low.includes('deepseek')) return 65536
  if (low.includes('gpt-3.5')) return 16385
  if (low.includes('gpt-4')) return 8192
  return 0
}

/** 方案配置优先，否则按模型名推断。 */
export function resolveContextWindow(profile) {
  const configured = Number(profile && profile.contextWindow)
  if (Number.isFinite(configured) && configured > 0) return Math.floor(configured)
  return contextWindowForModel(profile && profile.model)
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
