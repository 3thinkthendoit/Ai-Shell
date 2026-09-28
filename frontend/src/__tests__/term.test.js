import { describe, it, expect } from 'vitest'
import { wrapCommand, parseBareCd, extractCwd, CWD_MARKER, promptParts } from '../term.js'

describe('term cwd helpers', () => {
  it('wrapCommand 包一层 cd 跟踪目录', () => {
    expect(wrapCommand('~', 'uname -a')).toBe('cd ~ && uname -a')
    expect(wrapCommand('/etc', 'ls')).toBe("cd '/etc' && ls")
  })

  it('纯 cd 追加哨兵', () => {
    const w = wrapCommand('~', 'cd /var')
    expect(w).toContain("cd '/var'")
    expect(w).toContain(CWD_MARKER)
  })

  it('parseBareCd 识别纯 cd', () => {
    expect(parseBareCd('cd')).toBe('')
    expect(parseBareCd('cd /tmp')).toBe('/tmp')
    expect(parseBareCd('cd /tmp; rm -rf /')).toBeNull()
    expect(parseBareCd('ls')).toBeNull()
  })

  it('extractCwd 抽出哨兵并清洗 stdout', () => {
    const { stdout, cwd } = extractCwd(`${CWD_MARKER}/etc\n`)
    expect(cwd).toBe('/etc')
    expect(stdout).not.toContain(CWD_MARKER)
  })

  it('promptParts root 用 #', () => {
    const p = promptParts({ user: 'root', addr: '10.0.0.1:22' }, '/etc')
    expect(p.who).toBe('root@10.0.0.1')
    expect(p.sym).toBe('#')
    expect(p.path).toBe('/etc')
  })
})
