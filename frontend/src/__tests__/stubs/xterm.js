// @xterm/xterm 的测试替身。
//
// 真实的 xterm 需要真实布局和 canvas，jsdom 两样都没有 —— 直接跑会刷出一堆
// "Not implemented: HTMLCanvasElement.prototype.getContext"。
//
// 这里要测的是**接线**：实例什么时候建、按键怎么送出去、尺寸变化怎么上报、
// 收到的字节有没有原样写进去。这些都不需要真的画出来。
// 由 vitest.config.js 的 alias 挂上，生产构建用的是真货。

// instances 按创建顺序记录所有实例，测试用它断言「建了几个、是不是同一个」。
export const instances = []

export function resetInstances() {
  instances.length = 0
}

export class Terminal {
  constructor(options = {}) {
    this.options = options
    this.cols = 80
    this.rows = 24
    this.written = [] // 收到的原始数据（字符串或 Uint8Array），按顺序
    this.disposed = false
    this.openedIn = null
    this.dataHandlers = []
    this.resizeHandlers = []
    instances.push(this)
  }

  loadAddon(addon) {
    this.addon = addon
    addon.terminal = this
  }

  open(el) {
    this.openedIn = el
  }

  write(data) {
    if (this.disposed) throw new Error('对已销毁的终端写入')
    this.written.push(data)
  }

  onData(fn) {
    this.dataHandlers.push(fn)
    return { dispose() {} }
  }

  onResize(fn) {
    this.resizeHandlers.push(fn)
    return { dispose() {} }
  }

  dispose() {
    this.disposed = true
  }

  // ---- 以下是替身给测试用的入口，真 xterm 没有 ----

  // emitData 模拟用户敲键。
  emitData(s) {
    for (const fn of this.dataHandlers) fn(s)
  }

  // emitResize 模拟终端尺寸变化。
  emitResize(cols, rows) {
    this.cols = cols
    this.rows = rows
    for (const fn of this.resizeHandlers) fn({ cols, rows })
  }

  // text 把收到的内容拼成字符串，便于断言「用户看到了什么」。
  //
  // 必须**流式**解码：真实 xterm 的 UTF-8 解码器本来就是跨块工作的，
  // 替身也得如此。逐块独立解码的话，「一个汉字被切成两块」这种用例会假失败 ——
  // 明明接线是对的，替身却解出三个替换字符。
  text() {
    const dec = new TextDecoder()
    let out = ''
    for (const w of this.written) {
      out += typeof w === 'string' ? w : dec.decode(w, { stream: true })
    }
    return out
  }
}
