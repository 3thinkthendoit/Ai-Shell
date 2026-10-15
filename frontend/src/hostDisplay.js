// 主机地址展示：界面上的 IP 中间两段脱敏（a.b.c.d → a.***.***.d）。
// 主机管理页仍用明文；顶栏 / 窗口标题 / 侧栏分组等只读展示走这里。

export function maskHostAddr(addr) {
  if (addr == null) return ''
  const raw = String(addr).trim()
  if (!raw) return ''
  let host = raw
  let port = ''
  const withPort = raw.match(/^(.+):(\d{1,5})$/)
  if (withPort) {
    host = withPort[1]
    port = withPort[2]
  }
  const v4 = host.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/)
  if (v4) {
    const masked = `${v4[1]}.***.***.${v4[4]}`
    return port ? `${masked}:${port}` : masked
  }
  return raw
}

// 顶栏 / 窗口标题用：有名字则「名 · 脱敏 IP」，否则 user@脱敏IP。
export function formatHostLabel(h) {
  if (!h) return ''
  const ip = maskHostAddr(h.addr)
  const name = String(h.name || '').trim()
  if (name && ip) return `${name} · ${ip}`
  if (name) return name
  const user = String(h.user || '').trim()
  if (user && ip) return `${user}@${ip}`
  return ip || user
}
