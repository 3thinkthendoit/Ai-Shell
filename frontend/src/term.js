// 交互式会话里跟踪「当前工作目录」的纯逻辑。
// 远端 Exec 无状态，cd 必须由本机包一层；哨兵行把 pwd 结果带回前端。

export const CWD_MARKER = '__AISHELL_CWD__'

export function shellQuote(s) {
  return "'" + String(s).replace(/'/g, `'\\''`) + "'"
}

/** 把用户命令包成「先切到跟踪目录再执行」；纯 cd 时追加哨兵打印新目录。 */
export function wrapCommand(cwd, cmd) {
  const c = String(cmd || '').trim()
  const dir = cwd && cwd !== '~' ? shellQuote(cwd) : '~'
  const bare = parseBareCd(c)
  if (bare !== null) {
    const target = bare === '' || bare === '~' ? '~' : shellQuote(bare)
    return `cd ${dir} && cd ${target} && printf '%s\\n' ${CWD_MARKER}$(pwd -P)`
  }
  return `cd ${dir} && ${c}`
}

/** 识别「整行只是一次 cd」。返回目标路径；不是纯 cd 则 null。 */
export function parseBareCd(cmd) {
  const m = String(cmd || '').trim().match(/^cd(?:\s+(.+))?$/)
  if (!m) return null
  if (!m[1]) return ''
  const arg = m[1].trim()
  // 拒绝带 shell 元字符的复杂形式，避免包装出错
  if (/[;&|<>()$`]/.test(arg)) return null
  return arg.replace(/^['"]|['"]$/g, '')
}

/** 从 stdout 抽出哨兵目录，并去掉哨兵行。 */
export function extractCwd(stdout) {
  const text = String(stdout || '')
  const re = new RegExp(`(?:^|\\n)${CWD_MARKER}([^\\n]*)`)
  const m = text.match(re)
  if (!m) return { stdout: text, cwd: null }
  const cwd = m[1]
  const cleaned = text.replace(re, (match, _p, offset) => (offset === 0 ? '' : '\n')).replace(/^\n/, '')
  return { stdout: cleaned, cwd }
}

export function promptParts(host, cwd) {
  const user = (host && host.user) || 'user'
  const addr = host && host.addr ? String(host.addr).replace(/:22$/, '') : 'host'
  const path = cwd || '~'
  const sym = user === 'root' ? '#' : '$'
  return { who: `${user}@${addr}`, path, sym }
}
