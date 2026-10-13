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
  const hit = matchProvider(p)
  if (hit) {
    if (LOGOS[hit.key]) return { logo: LOGOS[hit.key], color: hit.color }
    return { mark: hit.mark, color: hit.color }
  }
  // 完全认不出：回退方案名首字（CJK 取首字，ASCII 取首字母大写），中性灰圆标。
  const nm = String(p.name || p.model || '?').trim()
  const ch = /[\u4e00-\u9fa5]/.test(nm[0]) ? nm[0] : nm[0].toUpperCase()
  return { mark: ch, color: 'var(--text-3)' }
}

function matchProvider(p) {
  const hay = `${p.model || ''} ${p.baseUrl || ''} ${p.name || ''}`
  return PROVIDERS.find(x => x.re.test(hay))
}

// 终端场景的图标/颜色：终端里画不了 SVG，只能用**一个字形 + ANSI 颜色**近似。
// 每个厂商配一个简短 ASCII/CJK 标（`mark` 已有）与一个十六进制色（ANSI 真彩）。
//
// 为什么不用 Nerd Font 字形（如 logos 私有区）：那要求用户装了对应字体，
// 没装就是方框豆腐块，反而更糟。用厂商的通用字母标（DS / AI / C / Q …）在任何
// 等宽字体下都稳定可读，配合品牌色一眼可辨 —— 与界面里的降级圆标同一套心智。
const TERM_ICON = {
  deepseek: 'DS',
  openai: 'AI',
  anthropic: 'C',
  qwen: 'Q',
  glm: 'Z',
  kimi: 'K',
  gemini: 'G',
  llama: 'L',
  mistral: 'M',
  ernie: 'W',
  doubao: '豆'
}

// providerTermBadge 返回终端可用的徽标信息：
//   { mark, color, name } —— mark 是一两个字符的图标，color 是十六进制品牌色，
//   name 是人可读的展示名（优先用方案名，其次厂商名，最后回退模型串）。
// 与 providerBadge 共用同一套厂商识别（matchProvider），避免两处规则漂移。
export function providerTermBadge(p) {
  const profile = p || {}
  const hit = matchProvider(profile)
  // 展示名：方案名最能对上用户在下拉框里看到的东西；没有就用厂商名，再没有就模型串。
  const rawName = String(profile.name || '').trim()
  const name = rawName || (hit ? PROVIDER_LABEL[hit.key] : '') || String(profile.model || '').trim()
  if (hit) {
    return { mark: TERM_ICON[hit.key] || hit.mark, color: ansiHex(hit.color), name }
  }
  // 完全认不出：方案名/模型串首字 + 中性灰。
  const nm = String(name || profile.model || '?').trim()
  const ch = /[\u4e00-\u9fa5]/.test(nm[0]) ? nm[0] : (nm[0] || '?').toUpperCase()
  return { mark: ch, color: '#8a8a86', name }
}

// 厂商的中文/展示名，供终端抬头在方案名为空时兜底。
const PROVIDER_LABEL = {
  deepseek: 'DeepSeek',
  openai: 'OpenAI',
  anthropic: 'Claude',
  qwen: '通义千问',
  glm: '智谱 GLM',
  kimi: 'Kimi',
  gemini: 'Gemini',
  llama: 'Llama',
  mistral: 'Mistral',
  ernie: '文心一言',
  doubao: '豆包'
}

// ansiHex 把一个 #RRGGBB 品牌色原样传出（xterm 支持 truecolor，
// termPaint 会用 \x1b[38;2;r;g;bm 直接落到终端）。这里只做「是不是合法 hex」的
// 兜底：品牌色表里都是 hex，认不出时给中性灰。
function ansiHex(hex) {
  return /^#[0-9a-f]{6}$/i.test(hex || '') ? hex : '#8a8a86'
}
