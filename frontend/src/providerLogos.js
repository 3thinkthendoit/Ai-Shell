// 厂商识别 + 模型徽标。
//
// 约束：LLMProfile 没有厂商字段，baseUrl 又常是「兼容 OpenAI」的自定义/代理地址，
// 无法可靠映射官方商标。所以这里用「关键字识别厂商 → 优先内联 SVG logo，
// 识别到厂商但没有对应 logo（或完全认不出）→ 降级为彩色圆 + 文字」。
//
// logo 用简单几何图形手绘（非官方精确商标），仅作视觉区分；颜色为近似品牌色。

const PROVIDERS = [
  { key: 'deepseek', re: /deepseek/i, color: '#4d6bfe', mark: 'DS' },
  { key: 'openai', re: /(openai|chatgpt|gpt-|\bo[1-4]\b)/i, color: '#10a37f', mark: 'AI' },
  { key: 'anthropic', re: /(claude|anthropic)/i, color: '#d97757', mark: 'C' },
  { key: 'qwen', re: /(qwen|tongyi|通义|千问)/i, color: '#615ced', mark: 'Q' },
  { key: 'glm', re: /(glm|zhipu|chatglm|智谱)/i, color: '#3370ff', mark: 'Z' },
  { key: 'kimi', re: /(kimi|moonshot|月之)/i, color: '#16162a', mark: 'K' },
  { key: 'gemini', re: /(gemini|google)/i, color: '#4285f4', mark: 'G' },
  { key: 'llama', re: /(llama|\bmeta\b)/i, color: '#0866ff', mark: 'L' },
  { key: 'mistral', re: /mistral/i, color: '#ff7000', mark: 'M' },
  { key: 'ernie', re: /(ernie|wenxin|文心)/i, color: '#2932e1', mark: 'W' },
  { key: 'doubao', re: /(doubao|豆包|volc)/i, color: '#ff5a2b', mark: '豆' }
]

// 24×24 视口、fill=currentColor（颜色由外层 span 的 color 决定），18px 显示。
const wrap = inner =>
  `<svg viewBox="0 0 24 24" width="18" height="18" fill="currentColor" aria-hidden="true">${inner}</svg>`

// 手绘近似 logo：只用能可靠画出的简单几何（花瓣 / 光芒 / 四角星 / 斜条），
// 认不准的厂商宁可用降级圆标，也不放一个画歪的假 logo。
const LOGOS = {
  // OpenAI：六瓣「花结」近似。
  openai: wrap(
    `<g>${[0, 60, 120, 180, 240, 300]
      .map(a => `<ellipse cx="12" cy="6.6" rx="2.3" ry="4.8" transform="rotate(${a} 12 12)"/>`)
      .join('')}</g>`
  ),
  // Anthropic / Claude：八向光芒。
  anthropic: wrap(
    `<g>${[0, 45, 90, 135, 180, 225, 270, 315]
      .map(a => `<rect x="11.1" y="1.6" width="1.8" height="7.6" rx="0.9" transform="rotate(${a} 12 12)"/>`)
      .join('')}</g>`
  ),
  // Google Gemini：四角星。
  gemini: wrap(`<polygon points="12,1 14.2,9.8 23,12 14.2,14.2 12,23 9.8,14.2 1,12 9.8,9.8"/>`),
  // Mistral：三条斜杠。
  mistral: wrap(
    `<g transform="skewX(-12)"><rect x="4" y="6" width="3.4" height="12"/><rect x="10.3" y="6" width="3.4" height="12"/><rect x="16.6" y="6" width="3.4" height="12"/></g>`
  )
}

// providerBadge 返回 { logo?, mark?, color }：有 logo 用 logo，否则用彩色圆 + mark。
export function providerBadge(p) {
  const hay = `${p.model || ''} ${p.baseUrl || ''} ${p.name || ''}`
  const hit = PROVIDERS.find(x => x.re.test(hay))
  if (hit) {
    if (LOGOS[hit.key]) return { logo: LOGOS[hit.key], color: hit.color }
    return { mark: hit.mark, color: hit.color }
  }
  // 完全认不出：回退方案名首字（CJK 取首字，ASCII 取首字母大写），中性灰圆标。
  const nm = String(p.name || p.model || '?').trim()
  const ch = /[\u4e00-\u9fa5]/.test(nm[0]) ? nm[0] : nm[0].toUpperCase()
  return { mark: ch, color: 'var(--text-3)' }
}
