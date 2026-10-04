// 文件管理弹窗的**样式契约**检查。
//
// 为什么需要这么一条不寻常的测试：
//
// 本项目所有组件都用 <style scoped>。scoped 的语义是「样式只作用于本组件
// 模板里的元素」，**不会**流到别的组件。于是当 FileManagerModal.vue 只写
// 了 .fm-* 那一堆自己的类、却没写 .overlay / .modal 的基础皮肤时，它渲染
// 出来的遮罩就没有底色、面板就没有背景 —— 终端文字直接透过弹窗和文件列表
// 叠在一起，看上去是一坨乱码。
//
// 这个 bug 有个很坏的性质：**所有其它测试都会通过**。
//   - store 测试只管状态；
//   - 组件测试断言的是"元素在不在、点了有没有反应"；
//   - vitest 默认不加载 CSS（见 vitest.config.js），计算样式根本取不到；
//   - 构建与类型检查更是完全看不见"视觉上透明"这件事。
// 换句话说，它会一路绿灯地发布出去，只有人眼能发现 —— 这正是它需要一条
// 专门的静态守卫的原因。
//
// 同理，"进大目录时弹窗被撑高、底部按钮被顶出屏幕"也属于这一类：
// 逻辑全对、测试全绿，但界面不可用。
//
// 这里不断言具体像素值，只断言"这几条结构性的样式确实存在"。
// 允许改颜色、改尺寸、改比例，但不允许下面这些契约消失。
import { describe, it, expect } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

// vitest 的 cwd 就是 frontend/（见 package.json 的 test 脚本），
// 用 resolve 而不是 import.meta.url：后者的 scheme 在某些 vitest
// 环境下不是 file:，fileURLToPath 会直接抛错。
const src = readFileSync(
  resolve(process.cwd(), 'src/components/FileManagerModal.vue'),
  'utf8'
)

// 取模板：从**最后一个** <template> 开始（文件顶部的注释里提到过这个词，
// indexOf 会先撞上注释，导致后续切片全部错位）。
function template(t) {
  const i = t.lastIndexOf('<template>')
  const j = t.lastIndexOf('</template>')
  return i >= 0 && j > i ? t.slice(i, j) : ''
}

// 取样式块并**剥掉 CSS 注释**。
// 必须剥：注释里出现的 `}` 会把「取到下一个 } 为止」的正则提前截断，
// 于是断言读到半截规则、失败得莫名其妙（写这条测试时就踩过一次）。
// 注释本身也不是要校验的东西。
const css = (() => {
  const t = src
  const i = t.lastIndexOf('<style')
  const j = t.lastIndexOf('</style>')
  if (i < 0 || j < 0) return ''
  const body = t.slice(t.indexOf('>', i) + 1, j)
  return body.replace(/\/\*[\s\S]*?\*\//g, '')
})()

// ruleBody 取出某个选择器的声明块内容。
// 要求选择器前面是换行：避免 `.modal` 误匹配到 `\n.fm-modal`。
function ruleBody(sel) {
  const esc = sel.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const m = css.match(new RegExp(`(?:^|\n)${esc}\\s*\\{([^}]*)\\}`))
  return m ? m[1] : null
}

// expectRule 取规则并在缺失时给出可读的失败信息。
function expectRule(sel) {
  const body = ruleBody(sel)
  expect(body, `${sel} 必须在本组件里定义（不能依赖其它组件的 scoped 样式）`).toBeTruthy()
  return body
}

describe('文件管理弹窗 样式自足性', () => {
  it('组件里有 style 块', () => {
    expect(css.length).toBeGreaterThan(200)
  })

  // 这几条是「弹窗能盖住下面内容」的最小集合。缺任意一条都会让终端透上来。
  it.each(['.overlay', '.modal'])('自己定义了 %s 的基础样式', sel => {
    expectRule(sel)
  })

  it('遮罩是 fixed 全屏、带不透明底色、且自己不滚', () => {
    const body = expectRule('.overlay')
    expect(body, '遮罩必须 position:fixed 才能盖住整屏').toMatch(/position:\s*fixed/)
    // 底色可以走变量，但必须有兜底，否则某个主题下变量缺失就退化成透明。
    expect(body, '遮罩必须有 background，且带兜底色').toMatch(/background:[^;]*rgba/)
    // 滚的应该是列表，不是遮罩：遮罩滚起来会把头部和底部按钮一起推走。
    expect(body, '遮罩本身不该滚').toMatch(/overflow:\s*hidden/)
  })

  it('面板有不透明背景（这是"不穿透"的关键一条）', () => {
    const body = expectRule('.modal')
    // var(--surface, #fff) 这种带兜底的写法：变量没定义时用实色，
    // 绝不会退化成 transparent。
    expect(body, '面板背景必须带兜底实色').toMatch(/background:[^;]*var\([^)]*,\s*#[0-9a-fA-F]{3,8}\s*\)/)
  })

  it('遮罩 z-index 高于终端与常规弹窗', () => {
    const z = expectRule('.overlay').match(/z-index:\s*(\d+)/)
    expect(z, '.overlay 需要显式 z-index').toBeTruthy()
    expect(Number(z[1])).toBeGreaterThanOrEqual(200)
  })

  it('文件列表也有实色底（行 hover 需要确定的地板）', () => {
    const body = expectRule('.fm-list')
    expect(body, '列表背景必须带兜底实色').toMatch(/background:[^;]*var\([^)]*,\s*#[0-9a-fA-F]{3,8}\s*\)/)
  })
})

// 这一组守的是「进大目录时弹窗不散架」。
//
// 症状是：进入一个几百项的目录（/usr/lib 这种），整块面板跟着长高，
// 底部那排「上传到当前目录 / 关闭」被顶到屏幕外——**目录越大越够不着按钮**，
// 而这恰恰是最需要刷新/离开的时候。
//
// 修法是锁定面板高度、只让列表内部滚。锁定的前提有四条，缺任一条都会退回去：
//   1. .fm-modal 有确定的 height（不是只有 max-height）；
//   2. .fm-modal 自己不滚（否则滚的是整块面板，头部跟着走）；
//   3. .fm-list 有 min-height:0（不然 flex 子项拒绝收缩，flex:1 形同虚设）；
//   4. 头部/底部 flex-shrink:0（收缩压力全给列表，不压扁按钮条）。
describe('文件管理弹窗 固定高度', () => {
  it('.fm-modal 有确定的 height，而不只是 max-height', () => {
    const body = expectRule('.fm-modal')
    // 必须是 height/min-height 这类能"撑住"面板的属性。
    expect(body, '面板需要确定的 height 才能不随内容长高').toMatch(/(^|;|\s)height:\s*min\(/)
    // 用 dvh 而不是 vh：移动端地址栏会吃掉 vh 的一部分。
    expect(body, '高度单位应是 dvh（移动端可视高度）').toMatch(/dvh/)
  })

  it('.fm-modal 自己不滚动（滚动必须留给列表）', () => {
    expect(expectRule('.fm-modal'), '面板本体不能滚，否则头部会跟着滚走')
      .toMatch(/overflow:\s*hidden/)
  })

  it('.fm-list 有 min-height:0，flex:1 才真的能收缩', () => {
    const body = expectRule('.fm-list')
    expect(body, 'flex 子项默认 min-height:auto，必须显式归零').toMatch(/min-height:\s*0\b/)
    expect(body, '列表要能滚').toMatch(/overflow:\s*auto/)
  })

  it('.fm-actions 是固定不缩的那一条', () => {
    expect(expectRule('.fm-actions'), '按钮条必须 flex-shrink:0，否则会被列表挤扁')
      .toMatch(/flex-shrink:\s*0/)
  })

  it('头部与路径栏不参与压缩', () => {
    for (const sel of ['.fm-head', '.fm-pathbar']) {
      expect(expectRule(sel), `${sel} 需要 flex-shrink:0`).toMatch(/flex-shrink:\s*0/)
    }
  })

  // 行操作列只有一个下载图标。放宽行距的关键就在这里：
  // 原先「下载」+「删除」两个文字按钮各占约 60px，名字列被挤得很窄，
  // 稍长的文件名就被截断成 "very-long-file-n…"。
  it('下载是图标按钮，且与目录占位等宽（保证图标列对齐）', () => {
    const dl = expectRule('.fm-dl')
    // 图标按钮要固定尺寸：它是 flex 行里的一项，宽度由内容决定的话，
    // 各行的图标位置会随 SVG 渲染宽度浮动。
    expect(dl, '.fm-dl 需要固定宽度').toMatch(/width:\s*\d+px/)
    expect(dl, '.fm-dl 需要固定高度').toMatch(/height:\s*\d+px/)

    const gap = expectRule('.fm-dl-gap')
    const w = dl.match(/width:\s*(\d+)px/)
    const gw = gap.match(/width:\s*(\d+)px/)
    expect(gw, '.fm-dl-gap 需要固定宽度').toBeTruthy()
    // 两者必须同宽：目录行放的是占位 span，宽度不一致会让文件行和目录行的
    // 图标列左右错开，看上去像排版抖动。
    expect(gw[1], '目录占位必须与下载按钮等宽').toBe(w[1])
  })

  it('下载按钮有可见的 hover / focus 反馈', () => {
    // 无边框图标按钮若没有 hover 反馈，用户不知道它可点。
    expect(css, '缺少 :hover 规则').toMatch(/\.fm-dl:hover/)
    // 键盘用户同样需要一个可见的落点。
    expect(css, '缺少 :focus-visible 规则').toMatch(/\.fm-dl:focus-visible/)
  })
})

// 上传进度条。它坏掉时界面**看起来仍然正常** —— 会显示"上传中"、最后也成功，
// 只是百分比是 NaN%、停在 0%，或者传完不消失。所以要把它钉住。
describe('文件管理弹窗 上传进度', () => {
  it('进度条不该被列表挤没', () => {
    expect(expectRule('.fm-progress'), '进度条必须 flex-shrink:0')
      .toMatch(/flex-shrink:\s*0/)
  })

  it('下载的取消按钮也不参与压缩（否则长文件名会把它挤变形）', () => {
    expect(expectRule('.fm-progress-cancel'), '取消按钮必须 flex-shrink:0')
      .toMatch(/flex-shrink:\s*0/)
  })

  it('进度轨道裁剪填充条（否则圆角会被盖住）', () => {
    expect(expectRule('.fm-progress-track'), '轨道需要 overflow:hidden')
      .toMatch(/overflow:\s*hidden/)
  })

  it('有不定进度的样式（发送阶段用）', () => {
    // 发送阶段拿不到回调，只能显示不定进度。少了这条，那一段会是
    // 一条宽 0 的空条 —— 用户以为卡死了。
    expect(css, '缺少 .indeterminate 规则').toMatch(/\.fm-progress-bar\.indeterminate/)
    expect(css, '不定进度需要动画').toMatch(/@keyframes\s+fm-indeterminate/)
  })

  it('尊重「减少动态效果」偏好', () => {
    // 持续的位移动画对部分用户会造成不适；这条偏好必须被尊重，
    // 且退化成"看得见但不动的条"，而不是直接消失。
    const m = css.match(/@media\s*\(\s*prefers-reduced-motion:\s*reduce\s*\)\s*\{([\s\S]*?)\n\}/)
    expect(m, '缺少 prefers-reduced-motion 媒体查询').toBeTruthy()
    expect(m[1], '该查询里要关掉不定进度的动画').toMatch(/fm-progress-bar\.indeterminate/)
    expect(m[1], '并且要保留一条可见的条（不能直接 width:0）').toMatch(/animation:\s*none/)
  })

  it('进度条带动画（百分比跳变看起来应是流动而不是闪）', () => {
    expect(expectRule('.fm-progress-bar'), '缺少 transition')
      .toMatch(/transition:\s*width/)
  })
})

// 弹窗不再提供删除。这不是"少写了一个按钮"——它是一条产品决定：
// 远端删除不可逆，要做就得走终端（看得见命令、留得下历史、能被审批闸门拦）。
// 做成列表里的按钮，在目录与文件混排时误点代价太高。
describe('文件管理弹窗 不含删除入口', () => {
  it('模板里没有删除按钮、也没有删除确认框', () => {
    const tpl = template(src)
    expect(tpl, '不该再有删除确认框').not.toMatch(/fm-confirm/)
    // 「删除」这两个字不该出现在按钮文案里。注释里提到删除是允许的
    // （上面那段说明），所以只检查模板而不是整个文件。
    expect(tpl, '不该再有删除按钮文案').not.toMatch(/>\s*删除\s*</)
  })

  it('组件不再 import fmDelete，也不调用删函数', () => {
    const script = src.slice(src.lastIndexOf('<script setup>'), src.indexOf('</script>'))
    expect(script, '不该再 import fmDelete').not.toMatch(/fmDelete/)
    // 不留悬挂的确认状态：删掉按钮后 pendingDelete/askDelete/confirmDelete
    // 若还留着，就是死代码，说明删得不干净。
    expect(script, '不该再有 pendingDelete 状态').not.toMatch(/pendingDelete/)
    expect(script, '不该再有 askDelete').not.toMatch(/askDelete/)
    expect(script, '不该再有 confirmDelete').not.toMatch(/confirmDelete/)
  })
})
