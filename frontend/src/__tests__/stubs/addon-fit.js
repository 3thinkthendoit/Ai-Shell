// @xterm/addon-fit 的测试替身。
//
// 真 FitAddon 靠测量容器的像素尺寸来算行列，jsdom 没有布局，量出来恒为 0。
// 这里模拟成「算出一个固定尺寸」，让「fit → onResize → 上报后端」这条链路
// 能被真实地走一遍。
export class FitAddon {
  constructor() {
    this.terminal = null
    this.fitCount = 0
    this.cols = 100
    this.rows = 30
  }

  fit() {
    this.fitCount++
    if (this.terminal) {
      this.terminal.cols = this.cols
      this.terminal.rows = this.rows
      this.terminal.emitResize(this.cols, this.rows)
    }
  }
}
