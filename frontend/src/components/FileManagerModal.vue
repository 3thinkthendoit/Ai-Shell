<!--
  文件管理弹窗。

  它服务的是**某一块终端表面**（host+session），不是「本机文件浏览器」：
  默认目录取自那条 shell 上报的 OSC 7，所以用户在哪就在哪 —— 这正是
  「打开就定位到控制台当前目录」的意思。

  为什么不复用 ConsolePanel 的弹窗：那个组件的模板已经很长（终端、审批条、
  归档、若干弹窗挤在一起），再塞进一个带列表/上传/下载的窗口会让它难以阅读。
  这里做成独立组件，靠 store.fileManager 通讯。

  操作范围：**浏览 + 上传 + 下载**，不含删除。删除是不可逆的远端操作，
  让它在一次误点里发生不值得，需要删除时走终端（用户看得见命令、也有历史）。
  所以这里既没有删除按钮，也没有删除确认弹窗 —— 相应地，行操作列只剩一个
  下载图标，行布局因此宽松很多，长文件名不再那么容易被截断。
-->
<template>
  <div v-if="fm.open" class="overlay" @click.self="close">
    <div class="modal fm-modal">
      <div class="fm-head">
        <h3>文件管理</h3>
        <span class="muted mono fm-host">{{ hostLabel }}</span>
      </div>

      <!-- 路径栏：可编辑，用户能直接粘贴一个路径跳过去 -->
      <div class="fm-pathbar">
        <button
          class="sm"
          :disabled="!canGoParent || fm.busy"
          title="上一级目录"
          @click="goParent"
        >↑</button>
        <input
          v-model="pathInput"
          class="fm-path mono"
          spellcheck="false"
          placeholder="远端目录，如 /etc/nginx"
          @keydown.enter.prevent="goPath"
        />
        <button class="sm" :disabled="fm.loading || fm.busy" @click="refresh">刷新</button>
      </div>

      <div v-if="fm.error" class="fm-error">{{ fm.error }}</div>
      <div v-else-if="fm.notice" class="fm-notice">{{ fm.notice }}</div>

      <!-- 列表 -->
      <div class="fm-list">
        <div v-if="fm.loading" class="fm-empty">读取中…</div>
        <div v-else-if="!fm.entries.length" class="fm-empty">这个目录是空的</div>
        <template v-else>
          <div
            v-for="e in sortedEntries"
            :key="e.path"
            class="fm-row"
            :class="{ dir: e.isDir }"
          >
            <button
              class="fm-name"
              :title="e.isDir ? '进入目录' : e.path"
              @click="onRowClick(e)"
            >
              <span class="fm-icon">{{ e.isDir ? '📁' : '📄' }}</span>
              <span class="fm-filename">{{ e.name }}</span>
              <span v-if="e.isLink" class="fm-link">链接</span>
            </button>
            <span class="fm-meta mono">{{ e.isDir ? '—' : humanSize(e.size) }}</span>
            <span class="fm-meta fm-mode mono">{{ e.mode }}</span>
            <!-- 下载用图标而不是文字按钮：一列「下载」两个字占掉约 60px，
                 而这一列对每一行都是同一种操作，文字没有增加信息量。
                 换成图标后名字那一列能宽出不少（长文件名不再那么容易被截断）。
                 图标只对文件有意义：目录要被打包才能传，所以目录位留一个
                 等宽占位，保证各行的图标列仍然对齐。 -->
            <button
              v-if="!e.isDir"
              class="fm-dl"
              :disabled="fm.busy"
              title="下载到本机"
              aria-label="下载到本机"
              @click="download(e)"
            >
              <svg viewBox="0 0 24 24" width="15" height="15" aria-hidden="true">
                <!-- 云 + 下箭头：云表示"远端/云端"，箭头朝下表示"取到本机"。 -->
                <path
                  d="M6.5 17.5a4.5 4.5 0 0 1-.3-8.99A6 6 0 0 1 17.7 9.2a3.9 3.9 0 0 1-.2 8.3"
                  fill="none"
                  stroke="currentColor"
                  stroke-width="1.7"
                  stroke-linecap="round"
                  stroke-linejoin="round"
                />
                <path
                  d="M12 11v6.2m0 0-2.4-2.4M12 17.2l2.4-2.4"
                  fill="none"
                  stroke="currentColor"
                  stroke-width="1.7"
                  stroke-linecap="round"
                  stroke-linejoin="round"
                />
              </svg>
            </button>
            <span v-else class="fm-dl-gap"></span>
          </div>
        </template>
      </div>

      <!-- 传输进度。放在列表与按钮条之间：它是"当前正在发生的事"，
           比列表内容更需要立刻被看到，但又不该盖住列表本身。
           只在有传输时出现（fm.progress 非空），平时不占高度。 -->
      <div v-if="fm.progress" class="fm-progress">
        <div class="fm-progress-head">
          <span class="fm-progress-name mono">{{ fm.progress.name }}</span>
          <span class="fm-progress-pct">{{ progressText }}</span>
          <!-- 取消只对下载有意义：上传的各阶段（读文件/发送）都发生在
               这一侧前端，中断不了的调用里塞一个永远灰着的按钮是噪音。
               id 为空说明第一块还没传完（事件还没来过），先禁着。 -->
          <button
            v-if="fm.progress.phase === 'downloading'"
            class="sm fm-progress-cancel"
            :disabled="!fm.progress.id"
            title="取消下载（半成品文件会被清掉）"
            @click="fmCancelTransfer"
          >取消</button>
        </div>
        <div class="fm-progress-track">
          <!-- 不定进度（发送阶段）用条纹动画：这一段拿不到回调，
               显示一个假百分比会长时间卡在某个数字上，比诚实的不定态更差。 -->
          <div
            class="fm-progress-bar"
            :class="{ indeterminate: !progressKnown }"
            :style="progressKnown ? { width: progressPct + '%' } : null"
          ></div>
        </div>
      </div>

      <div class="modal-actions fm-actions">
        <input
          ref="fileEl"
          type="file"
          class="fm-file"
          @change="onPickFile"
        />
        <button class="sm" :disabled="fm.busy" @click="pickFile">上传到当前目录</button>
        <span class="fm-spacer"></span>
        <span class="modal-hint">上限 {{ limitText }} · 上传/下载支持二进制</span>
        <button class="sm primary" @click="close">关闭</button>
      </div>
    </div>
  </div>
</template>

<script setup>
import { computed, ref, watch } from 'vue'
import {
  store, closeFileManager, loadDir, fmEnter, fmGoParent,
  fmUpload, fmDownload, fmCancelTransfer, fmtBytes
} from '../store'

const fm = store.fileManager

// 路径输入框的本地副本：直接双向绑到 fm.cwd 的话，用户每敲一个字符都会
// 触发 store 变更（列表跟着重渲染），而这里的意图只是「敲完再跳过去」。
const pathInput = ref('')
const fileEl = ref(null)

// 目录变化时同步输入框。用 watch 而不是 computed 的反向写：
// 输入框在用户编辑期间不该被外部值覆盖，只有在**目录真的变了**时才刷新。
watch(() => fm.cwd, v => { pathInput.value = v || '' }, { immediate: true })

const canGoParent = computed(() => fm.parent && fm.parent !== fm.cwd)

// 上限文案取自后端下发的值，不写死 —— 见 store.maxTransferBytes 的说明。
const limitText = computed(() => fmtBytes(store.maxTransferBytes))

// 进度的三个派生值。之所以要分成三个 computed 而不是在模板里直接算：
// 它们都要处理"total 未知/为 0"的情况，散在模板里会重复三遍同样的判断，
// 而且模板里的除零只会安静地产生 NaN%，不容易在 code review 里被看见。

// progressKnown 表示"能给出一个有意义的百分比"。
// 读阶段（上传）与下载阶段都可以（有文件大小），发送阶段不行
// （拿不到回调）—— 后者宁可显示不定进度，也不显示一个骗人的数字。
const progressKnown = computed(() => {
  const p = fm.progress
  if (!p) return false
  if (p.phase === 'reading' || p.phase === 'downloading') return p.total > 0
  return false
})

const progressPct = computed(() => {
  const p = fm.progress
  if (!p || !p.total) return 0
  const pct = Math.round((p.loaded / p.total) * 100)
  // 夹在 0..100：某些浏览器在最后一块上会多报一点（读到 total+1），
  // 不夹的话进度条会超出容器。
  return Math.max(0, Math.min(100, pct))
})

// progressText 是右侧那行文字：能算百分比就算，不能就报阶段 + 已传字节。
// 下载阶段额外带速度 —— 后端按累计平均算好的（抖动小），这里只管渲染。
const progressText = computed(() => {
  const p = fm.progress
  if (!p) return ''
  const size = p.total ? fmtBytes(p.total) : ''
  if (progressKnown.value) {
    const base = `${progressPct.value}% · ${fmtBytes(p.loaded)} / ${size}`
    if (p.phase === 'downloading' && p.bps > 0) {
      return `${base} · ${fmtBytes(Math.round(p.bps))}/s`
    }
    return base
  }
  if (p.phase === 'downloading') return p.total ? `准备中 · ${size}` : '准备中…'
  if (p.phase === 'sending') return `发送中 · ${size}`
  if (p.phase === 'done') return '完成'
  return size
})

const hostLabel = computed(() => {
  const h = store.hosts.find(x => x.id === fm.hostId)
  return h ? `${h.name} · ${h.user}@${h.addr}` : ''
})

// 目录在前、文件在后，各自按名字排。与文件管理器的通行习惯一致，
// 也让用户找目录时不必在一堆文件里翻。
const sortedEntries = computed(() => {
  const list = [...(fm.entries || [])]
  list.sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1
    return a.name.localeCompare(b.name)
  })
  return list
})

function close() {
  closeFileManager()
}

function goParent() {
  fmGoParent()
}

function goPath() {
  const p = pathInput.value.trim()
  if (p) loadDir(p)
}

function refresh() {
  loadDir(fm.cwd)
}

function onRowClick(e) {
  if (e.isDir) fmEnter(e.path)
}

async function download(e) {
  await fmDownload(e.path, e.name)
}

function pickFile() {
  if (fileEl.value) fileEl.value.click()
}

async function onPickFile(ev) {
  const files = ev.target.files
  if (files && files.length) await fmUpload(files[0])
  // 清空 input：否则连续上传同一个文件时 change 不会再触发
  // （值没变），用户看到的是「第二次选了没反应」。
  if (fileEl.value) fileEl.value.value = ''
}

// humanSize 把字节数变成人能读的大小。目录不显示大小（显示 —）。
function humanSize(n) {
  if (!n && n !== 0) return ''
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} K`
  if (n < 1024 * 1024 * 1024) return `${(n / 1048576).toFixed(1)} M`
  return `${(n / 1073741824).toFixed(2)} G`
}
</script>

<style scoped>
/* 这几条是**本组件自己**的遮罩与面板皮肤，必须在这里定义。
 *
 * 不能指望 ConsolePanel.vue 里那套同名类：那个组件的 <style scoped> 只作用于
 * 它自己的模板，样式不会流到本组件的元素上。先前这里漏了这几条，结果遮罩没有
 * 底色、面板没有背景 —— 终端文字直接透过来和文件列表叠在一起（那种"垃圾界面"）。
 *
 * 背景用不透明的实色而不是半透明：半透明能看出"底下的东西"，一旦某个
 * 颜色变量在某主题下没定义，退化成 transparent 就又变成叠字。
 * 实色 + 明确兜底色，任何主题下都不会穿透。
 */
.overlay {
  position: fixed;
  inset: 0;
  /* 兜底色放前面：变量没定义时用 rgba 而不是透明 */
  background: rgba(0, 0, 0, 0.55);
  background: var(--overlay-bg, rgba(0, 0, 0, 0.55));
  display: flex;
  align-items: center;
  justify-content: center;
  /* 遮罩自己不许滚：滚的应该是弹窗内部的列表。留在这里会变成
     「整页跟着列表一起滚」，头部路径栏和底部按钮都被推出屏幕。 */
  overflow: hidden;
  z-index: 300;
}

.modal {
  /* 实色面板：这是"不穿透"的关键一条 */
  background: var(--surface, #ffffff);
  border: 1px solid var(--border-strong, #d4d4d8);
  border-radius: var(--radius, 10px);
  padding: 18px 20px;
  box-shadow: var(--shadow, 0 18px 48px rgba(0, 0, 0, 0.35));
  /* 面板自身也要挡住下方内容：有些浏览器在合成层会漏出一点 */
  isolation: isolate;
}

.modal h3 { margin: 0 0 10px; font-size: 14px; color: var(--text, #18181b); }
.modal-hint { font-size: 11px; color: var(--text-3, #71717a); }
.modal-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 14px; }

/* 宽弹窗：列表要能放下「名字 + 大小 + 权限 + 两个按钮」。
 *
 * 高度策略：**固定住整块面板**，只让中间的列表滚。
 *
 * 之前写的是 max-height + .fm-list{flex:1}，看着对，实际不管用：
 * 进入一个大目录（比如 /usr/lib 几百项）时整块面板会一起长高，
 * 把底部那排「上传到当前目录 / 关闭」顶到屏幕外面去 —— 面板越高越够不着，
 * 而用户恰恰在这种时候最需要那排按钮。
 *
 * 两个原因：
 *   1. flex 子项的 min-height 默认是 auto —— 内容多高它就要多高，
 *      不肯收缩，于是 flex:1 拿不到"被压缩"的通知；
 *   2. overlay 是 align-items:center，面板高度由内容撑，max-height 只在
 *      超过时才截断，截断掉的正是底部（因为它是最后一块）。
 *
 * 所以这里把面板高度**锁死**成一个固定值（用 dvh 而不是 vh：移动端浏览器
 * 的地址栏会吃掉 vh 的一部分，dvh 才是真实可视高度），并给列表 min-height:0
 * 让它可以收缩。这样面板尺寸恒等于锁定的值，头部和底部按钮永远在原位，
 * 目录里有多少项都只影响列表内部的滚动位置。 */
.fm-modal {
  width: min(760px, calc(100% - 60px));
  display: flex;
  flex-direction: column;
  /* 固定高度：min 保证小屏不会超出视口，72vh 是大屏的舒适比例。 */
  height: min(72vh, calc(100dvh - 80px));
  max-height: calc(100dvh - 80px);
  /* 面板自己不滚：滚动交给 .fm-list。 */
  overflow: hidden;
}

/* flex-shrink:0 让头部/路径栏/底部按钮在面板被压时**不被压缩**：
   收缩的压力应该全部由 .fm-list 承担（它才是那个该滚的东西）。
   不加的话列了一项目录后会发现路径栏被压扁了几个像素。 */
.fm-head { display: flex; align-items: baseline; gap: 10px; flex-shrink: 0; }
.fm-host { font-size: 11px; }

.fm-pathbar { display: flex; gap: 6px; margin-top: 10px; flex-shrink: 0; }
.fm-path {
  flex: 1;
  min-width: 0;
  font-size: 12px;
  padding: 5px 8px;
  background: var(--inset-bg);
  border: 1px solid var(--border);
  border-radius: 6px;
  color: var(--text);
}
.fm-path:focus { outline: none; border-color: var(--border-strong); }

.fm-error {
  margin-top: 8px;
  font-size: 12px;
  color: var(--danger);
  background: var(--danger-bg, rgba(220, 38, 38, 0.08));
  border-radius: 6px;
  padding: 6px 9px;
  /* 错误/提示条也固定高度：它们出现时压缩的应该是列表，不是这条文字
     （被压扁的错误信息等于没显示）。 */
  flex-shrink: 0;
}
.fm-notice {
  margin-top: 8px;
  font-size: 12px;
  color: var(--ok, #16a34a);
  background: rgba(22, 163, 74, 0.08);
  border-radius: 6px;
  padding: 6px 9px;
  flex-shrink: 0;
}

/* 列表自己滚：面板高度是固定的，目录里几百个文件时只让这块内部滚动，
   头部路径栏与底部按钮都不动。 */
.fm-list {
  margin-top: 10px;
  overflow: auto;
  flex: 1;
  /* min-height 必须显式写 0：flex 子项默认 min-height:auto，
     意思是"内容多高我就多高"，于是 flex:1 的收缩根本不会发生 ——
     列表会顶破面板，把底部按钮挤出去。这一条是"固定高度"能否生效的
     关键，少了它上面那些 height 都是白写。 */
  min-height: 0;
  border: 1px solid var(--border);
  border-radius: 7px;
  /* 列表自己也有实色底：不能只靠外层 .modal 的面板 —— 行与行之间的
     hover/选中反馈需要一块确定的地板，否则在半透明主题下会显得"浮"在
     终端上。兜底色与 .modal 同源。 */
  background: var(--surface, #ffffff);
}
.fm-empty { padding: 20px; text-align: center; font-size: 12px; color: var(--text-3); }

.fm-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 3px 8px;
  border-bottom: 1px solid var(--border);
  font-size: 12px;
}
.fm-row:last-child { border-bottom: none; }
.fm-row:hover { background: var(--inset-bg); }

/* 名字占满剩余宽度，长文件名截断而不是把按钮挤出去。 */
.fm-name {
  flex: 1;
  min-width: 0;
  display: flex;
  align-items: center;
  gap: 6px;
  background: none;
  border: none;
  padding: 3px 0;
  text-align: left;
  color: var(--text);
  font-size: 12px;
  cursor: default;
}
.fm-row.dir .fm-name { cursor: pointer; color: var(--accent); }
/* 图标占固定宽度：不同 emoji 的渲染宽度不一致，不固定的话
   每行的文件名起始位置会参差，看起来像排版坏了。 */
.fm-icon { flex-shrink: 0; width: 16px; text-align: center; }
.fm-filename { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.fm-link {
  font-size: 10px;
  color: var(--text-3);
  border: 1px solid var(--border);
  border-radius: 4px;
  padding: 0 4px;
  flex-shrink: 0;
}

.fm-meta { color: var(--text-3); font-size: 11px; flex-shrink: 0; }
.fm-mode { width: 84px; }

/* 下载图标按钮：方形、无边框，hover 才浮出底色。
   图标默认低调（--text-3），鼠标移到行上或悬停时变亮 —— 一行有几十个
   高饱和按钮会非常吵，而下载是个低频动作，不该盖过文件名本身。 */
.fm-dl {
  flex-shrink: 0;
  width: 26px;
  height: 24px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  padding: 0;
  background: none;
  border: 1px solid transparent;
  border-radius: 6px;
  color: var(--text-3, #71717a);
  cursor: pointer;
}
.fm-dl:hover:not(:disabled) {
  color: var(--accent);
  background: var(--inset-bg);
  border-color: var(--border);
}
.fm-dl:disabled { opacity: 0.45; cursor: default; }
/* 焦点可见性：键盘 Tab 到这行时要看得见落在哪个图标上。 */
.fm-dl:focus-visible {
  outline: 2px solid var(--accent);
  outline-offset: 1px;
}
/* 目录位占位，宽度与 .fm-dl 一致，保证各行图标列对齐。 */
.fm-dl-gap { width: 26px; flex-shrink: 0; }

/* 底部按钮条：固定在面板底部，不随列表长短移动、也不参与压缩。
   加一条分隔线 + 一点上边距，让"按钮区"和"内容区"分开 —— 否则列表
   的最后一行会和按钮挨在一起，看起来像按钮是那一行的一部分。 */
.fm-actions {
  align-items: center;
  flex-shrink: 0;
  margin-top: 12px;
  padding-top: 12px;
  border-top: 1px solid var(--border);
}
.fm-spacer { flex: 1; }
/* 原生 file input 必须存在才能触发选择框，但界面不该看到它。 */
.fm-file { display: none; }

/* 传输进度。flex-shrink:0：它是"正在发生的事"，不该被列表挤没。 */
.fm-progress {
  margin-top: 10px;
  flex-shrink: 0;
}
.fm-progress-head {
  display: flex;
  align-items: baseline;
  gap: 8px;
  font-size: 11px;
  margin-bottom: 4px;
}
.fm-progress-name {
  flex: 1;
  min-width: 0;
  color: var(--text);
  /* 长文件名截断而不是换行：换行会让进度条位置跳来跳去。 */
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.fm-progress-pct { color: var(--text-3); flex-shrink: 0; }
/* 取消按钮贴在百分比旁边：头部一行被 flex 摊开，名字吃掉剩余空间，
   取消必须 flex-shrink:0 才不会被长文件名挤压。 */
.fm-progress-cancel {
  flex-shrink: 0;
  font-size: 11px;
  padding: 1px 8px;
}

.fm-progress-track {
  height: 6px;
  border-radius: 3px;
  background: var(--inset-bg, rgba(0, 0, 0, 0.08));
  /* 裁剪：圆角容器里放一个方形填充，不裁的话进度条的角会盖住容器圆角。 */
  overflow: hidden;
}
.fm-progress-bar {
  height: 100%;
  border-radius: 3px;
  background: var(--accent, #2563eb);
  /* width 由内联样式驱动。加过渡让百分比跳动看起来是"流动"而不是"闪"。 */
  transition: width 0.15s linear;
}
/* 不定进度：宽度交给动画，表示"在动但不知道到哪了"。 */
.fm-progress-bar.indeterminate {
  width: 35%;
  animation: fm-indeterminate 1.1s ease-in-out infinite;
}
@keyframes fm-indeterminate {
  0%   { transform: translateX(-100%); }
  100% { transform: translateX(340%); }
}
/* 尊重"减少动态效果"偏好：有些用户对持续的位移动画不适，
   退化成一条不动的实心条，仍然能看出"正在进行"。 */
@media (prefers-reduced-motion: reduce) {
  .fm-progress-bar.indeterminate {
    animation: none;
    transform: none;
    width: 100%;
    opacity: 0.55;
  }
}
</style>
