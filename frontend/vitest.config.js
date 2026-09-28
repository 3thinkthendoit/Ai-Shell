import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

const stub = name => fileURLToPath(new URL(`./src/__tests__/stubs/${name}`, import.meta.url))

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: [
      // xterm 需要真实布局与 canvas，jsdom 都没有。换成替身，
      // 只保留被测的那层「接线」—— 生产构建走的是真货（vite.config.js 里没有这些 alias）。
      //
      // 用正则精确匹配而不是字符串前缀：字符串别名会把
      // "@xterm/xterm/css/xterm.css" 也一起吃掉，于是样式导入解析失败，
      // 整个测试文件连收集都过不去。
      { find: /^@xterm\/xterm$/, replacement: stub('xterm.js') },
      { find: /^@xterm\/addon-fit$/, replacement: stub('addon-fit.js') }
    ]
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.js'],
    // 这些测试全部是纯逻辑/状态机测试，不依赖 Wails 运行时。
    // window.go / window.runtime 由测试自己注入。
    restoreMocks: true
  }
})
