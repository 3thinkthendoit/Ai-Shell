<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import {
  store,
  termState,
  openTerminal,
  writeTerminal,
  resizeTerminal,
  closeTerminal,
  registerTermSink,
  unregisterTermSink,
  textToBase64
} from '../store'

const props = defineProps({
  // active 表示当前是不是「交互终端」这个模式。
  //
  // 组件本身**一直挂着**（父级用 v-show 而不是 v-if），因为 xterm 实例
  // 一被销毁，终端的滚动回放就没了 —— 而「翻回去看刚才那行报错」
  // 恰恰是终端最常见的用法。active 决定的是「要不要去开终端」。
  active: { type: Boolean, default: false }
})

const rootEl = ref(null)
const openedIds = ref([])
const hostEls = new Map() // hostId -> 容器元素
const terms = new Map() // hostId -> { term, fit, sink }
let ro = null

const currentHost = computed(() => store.hosts.find(h => h.id === store.currentHostId))
const st = computed(() => (store.currentHostId ? store.terms[store.currentHostId] : null))

const statusLine = computed(() => {
  const s = st.value
  if (!s) return ''
  if (s.status === 'opening') return '正在打开终端…'
  if (s.status === 'closed') return s.exitReason || '终端已结束'
  if (s.status === 'error') return '打开失败：' + s.error
  return ''
})

const canRetry = computed(() => !!st.value && (st.value.status === 'closed' || st.value.status === 'error'))
const canClose = computed(() => !!st.value && (st.value.status === 'open' || st.value.status === 'opening'))

function setHostEl(id, el) {
  if (el) hostEls.set(id, el)
  else hostEls.delete(id)
}

// 终端配色跟应用其余部分保持一致（跟随亮/暗主题）。
// 用与界面割裂的终端配色会很突兀 —— 它嵌在界面中间，而不是一个独立窗口。
const THEMES = {
  light: {
    background: '#fafaf8',
    foreground: '#2c2c2a',
    cursor: '#2c2c2a',
    selectionBackground: 'rgba(24, 95, 165, 0.25)',
    black: '#2c2c2a',
    red: '#a32d2d',
    green: '#3b6d11',
    yellow: '#854f0b',
    blue: '#185fa5',
    magenta: '#534ab7',
    cyan: '#0f6e56',
    white: '#5f5e5a'
  },
  dark: {
    background: '#0a0c0a',
    foreground: '#d6e2d6',
    cursor: '#00e07f',
    cursorAccent: '#0a0c0a',
    selectionBackground: 'rgba(0, 224, 127, 0.30)',
    black: '#161b16',
    red: '#ff6b60',
    green: '#00e07f',
    yellow: '#f5b04d',
    blue: '#4da6ff',
    magenta: '#a99df5',
    cyan: '#3ddbc4',
    white: '#8fa38f'
  }
}
const currentXtermTheme = () => THEMES[store.theme] || THEMES.light

// 主题切换时同步更新所有已打开的终端实例。
watch(() => store.theme, t => {
  const theme = THEMES[t] || THEMES.light
  for (const { term } of terms.values()) term.options.theme = theme
})

// doFit 只在容器**确实有尺寸**时才测量。
//
// 容器隐藏时（模式切走、或这台主机不是当前主机）量到的是 0x0，
// fit 会算出 0 列 0 行，再把这个荒唐尺寸报给远端 —— 表现是远端程序
// 排版彻底乱掉，而界面上什么异常都看不到。
function doFit(hostId) {
  const t = terms.get(hostId)
  if (!t) return
  const el = hostEls.get(hostId)
  if (!el || el.clientWidth === 0 || el.clientHeight === 0) return
  try {
    t.fit.fit()
  } catch {
    // 容器刚变化、布局还没稳定时会抛。下一次 ResizeObserver 会再试，
    // 所以这里不该把一次瞬时失败变成一条用户可见的报错。
  }
}

function disposeTerm(hostId) {
  const t = terms.get(hostId)
  if (t) {
    unregisterTermSink(hostId, t.sink)
    try {
      t.term.dispose()
    } catch {
      // 已经销毁过就算了
    }
    terms.delete(hostId)
  }
  openedIds.value = openedIds.value.filter(x => x !== hostId)
  hostEls.delete(hostId)
}

async function ensureTerm(hostId) {
  if (!hostId || !props.active) return
  const state = termState(hostId)

  if (terms.has(hostId)) {
    if (state.status === 'closed' || state.status === 'error') {
      // 上一次的会话已经结束：把旧实例换掉。留着的话新会话的画面会接在
      // 上一段后面，看起来像「重连了但没清屏」。
      disposeTerm(hostId)
    } else {
      await nextTick()
      doFit(hostId)
      return
    }
  }

  // 先把容器渲染出来再 open：xterm 在 open 的那一刻就要测量容器尺寸、
  // 建立渲染层。容器还不存在时量到 0x0，画布会按错的尺寸建，
  // 之后再 fit 也修不回来 —— 表现是终端一片空白或只画了左上角一小块。
  if (!openedIds.value.includes(hostId)) openedIds.value.push(hostId)
  await nextTick()

  const el = hostEls.get(hostId)
  if (!el) return

  const term = new Terminal({
    cursorBlink: true,
    // 回放行数：给足但不无限。终端的价值一半在「翻回去看」，
    // 而每一行都要占内存，5000 行足够覆盖一次排查。
    scrollback: 5000,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
    fontSize: 12.5,
    theme: currentXtermTheme()
  })
  const fit = new FitAddon()
  term.loadAddon(fit)
  term.open(el)

  // 本地新建了渲染实例，但远端的 shell 可能还活着（比如切走视图再回来）。
  // 直接说明，而不是留一片空白 —— 空白会让人以为终端坏了。
  if (state.status === 'open') {
    term.write('\x1b[2m[本地重新连接到已存在的会话，之前的画面不再显示]\x1b[0m\r\n')
  }

  const sink = bytes => term.write(bytes)
  // 顺序不能反：必须先登记落点，再调 OpenTerminal。
  // 远端 shell 一启动就打印提示符，登记晚了那一段就落进虚空了。
  registerTermSink(hostId, sink)
  terms.set(hostId, { term, fit, sink })

  // 用户按键 → 后端。不做本地回显：远端行规程（ECHO=1）会把输入回显回来，
  // 本地再画一遍就会看到每个字符都重复。
  term.onData(d => {
    writeTerminal(hostId, textToBase64(d))
  })

  // 尺寸变化 → 后端。少了这一步，远端程序会一直按初始尺寸排版。
  term.onResize(({ cols, rows }) => {
    resizeTerminal(hostId, cols, rows)
  })

  doFit(hostId)
  await openTerminal(hostId, term.cols, term.rows)
}

async function onClose() {
  const hostId = store.currentHostId
  if (!hostId) return
  await closeTerminal(hostId)
  disposeTerm(hostId)
}

function onRetry() {
  ensureTerm(store.currentHostId)
}

watch(
  () => store.currentHostId,
  () => {
    ensureTerm(store.currentHostId)
  }
)

watch(
  () => props.active,
  on => {
    if (on) ensureTerm(store.currentHostId)
  }
)

onMounted(() => {
  // jsdom 里没有 ResizeObserver，测试环境不该因为这一点炸掉。
  if (typeof ResizeObserver !== 'undefined') {
    ro = new ResizeObserver(() => doFit(store.currentHostId))
    if (rootEl.value) ro.observe(rootEl.value)
  }
  if (props.active) ensureTerm(store.currentHostId)
})

onBeforeUnmount(() => {
  if (ro) {
    ro.disconnect()
    ro = null
  }
  // 卸载时销毁本地实例（它的 DOM 已经没了，留着就是一个坏对象），
  // 但**不关远端的终端**：卸载往往只是换了个视图，而用户希望切回来时
  // shell 还在原来的目录里。真正的清理走「关闭终端」按钮；
  // 应用退出时后端 shutdown 会关掉所有终端。
  for (const hostId of [...terms.keys()]) disposeTerm(hostId)
})
</script>

<template>
  <div class="iterm" ref="rootEl">
    <!-- 策略绕过必须写在最显眼的位置：交互终端里的每条命令都直接以登录用户
         身份执行，不经过任何裁决，这和「Agent会话」（经 LLM + 策略引擎）是完全不同的风险等级。 -->
    <div class="iterm-banner">
      <span>
        交互终端<span v-if="currentHost"> · <b>{{ currentHost.user }}@{{ currentHost.addr }}</b></span>
        ：命令以登录用户身份直接执行，<b>不经过 LLM 与策略引擎</b>，也没有命令审计。
      </span>
      <button v-if="canClose" class="sm" @click="onClose">关闭终端</button>
    </div>

    <div v-if="!store.currentHostId" class="iterm-empty">请先选择一台主机。</div>

    <template v-else>
      <div v-if="statusLine" class="iterm-status" :class="{ bad: st.status === 'error' }">
        <span>{{ statusLine }}</span>
        <button v-if="canRetry" class="sm" @click="onRetry">重新打开</button>
      </div>

      <div class="iterm-body">
        <div
          v-for="id in openedIds"
          :key="id"
          class="iterm-host"
          :class="{ hidden: id !== store.currentHostId }"
          :ref="el => setHostEl(id, el)"
        ></div>
      </div>
    </template>
  </div>
</template>

<style scoped>
.iterm {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-height: 0;
}

.iterm-banner {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 8px 14px;
  background: var(--warn-bg);
  color: var(--warn);
  font-size: 12.5px;
  line-height: 1.6;
}

.iterm-status {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 6px 14px;
  font-size: 12.5px;
  color: var(--text-2);
  background: var(--surface-2);
}

.iterm-status.bad {
  color: var(--danger);
  background: var(--danger-bg);
}

.iterm-empty {
  padding: 24px 16px;
  color: var(--text-3);
  font-size: 13px;
}

.iterm-body {
  position: relative;
  flex: 1;
  min-height: 0;
  background: var(--bg);
}

/* 每个主机一个容器，全部叠在同一处，只显示当前主机那个。
   容器必须一直留在 DOM 里：xterm 实例绑在它的元素上，元素被移除
   实例就废了，滚动回放也跟着没。 */
.iterm-host {
  position: absolute;
  inset: 8px;
}

.iterm-host.hidden {
  display: none;
}

.iterm-host :deep(.xterm) {
  height: 100%;
}
</style>
