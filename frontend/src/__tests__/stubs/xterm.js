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
    // 真实 xterm 的缓冲区服务：type 区分主屏/备用屏（全屏程序接管），
    // cursorX 是当前列。本地行编辑靠它做让路判定与擦除定位；
    // 测试里直接改这两个字段就能模拟「进了 vim」或「提示符占了几列」。
    this.buffer = { active: { type: 'normal', cursorX: 0, cursorY: 0 } }
    // 选区服务（对应真 xterm 的 getSelection/onSelectionChange/clearSelection）。
    // 组件用它做框选复制：真 xterm 在 Canvas 下也有这套模型，所以替身必须模拟出
    // 「选区变了会通知、能取到选中文本」这两个行为，否则复制接线无从断言。
    this.selection = ''
    this.selectionHandlers = []
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

  focus() {
    this.focused = true
  }

  onData(fn) {
    this.dataHandlers.push(fn)
    return { dispose() {} }
  }

  onResize(fn) {
    this.resizeHandlers.push(fn)
    return { dispose() {} }
  }

  attachCustomKeyEventHandler(fn) {
    this.customKeyHandler = fn
  }

  onSelectionChange(fn) {
    this.selectionHandlers.push(fn)
    return { dispose() {} }
  }

  getSelection() {
    return this.selection
  }

  clearSelection() {
    this.selection = ''
    for (const fn of this.selectionHandlers) fn()
  }

  dispose() {
    this.disposed = true
  }

  // ---- 以下是替身给测试用的入口，真 xterm 没有 ----

  // emitData 模拟用户敲键。
  emitData(s) {
    for (const fn of this.dataHandlers) fn(s)
  }

  // emitKey 模拟用户按下按键交给自定义处理器。
  // 返回 handler 的判定值（true=放行给 xterm，false=组件自己接管了）。
  // 没有注册 handler 时返回 true，与真实 xterm 的默认行为一致。
  emitKey(e) {
    if (!this.customKeyHandler) return true
    return this.customKeyHandler(e)
  }

  // emitResize 模拟终端尺寸变化。
  emitResize(cols, rows) {
    this.cols = cols
    this.rows = rows
    for (const fn of this.resizeHandlers) fn({ cols, rows })
  }

  // emitSelection 模拟用户拖选/取消选中：改选区文本并通知所有监听者。
  emitSelection(text) {
    this.selection = text
    for (const fn of this.selectionHandlers) fn()
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
