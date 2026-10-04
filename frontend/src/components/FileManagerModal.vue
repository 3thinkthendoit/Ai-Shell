<!--
  文件管理弹窗。

  它服务的是**某一块终端表面**（host+session），不是「本机文件浏览器」：
  默认目录取自那条 shell 上报的 OSC 7，所以用户在哪就在哪 —— 这正是
  「打开就定位到控制台当前目录」的意思。

  为什么不复用 ConsolePanel 的弹窗：那个组件的模板已经很长（终端、审批条、
  归档、若干弹窗挤在一起），再塞进一个带列表/上传/下载/删除的窗口会让
  它难以阅读。这里做成独立组件，靠 store.fileManager 通讯。
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
            <!-- 目录不提供下载（要打包才能传），上传/下载都只对文件有意义。 -->
            <button
              v-if="!e.isDir"
              class="sm fm-act"
              :disabled="fm.busy"
              title="下载到本机"
              @click="download(e)"
            >下载</button>
            <span v-else class="fm-act-gap"></span>
            <button
              class="sm danger fm-act"
              :disabled="fm.busy"
              title="删除（会二次确认）"
              @click="askDelete(e)"
            >删除</button>
          </div>
        </template>
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

      <!-- 删除确认：删除不可逆，必须让用户看清自己删的是什么 -->
      <div v-if="pendingDelete" class="overlay fm-confirm" @click.self="pendingDelete = null">
        <div class="modal">
          <h3>确认删除</h3>
          <p class="fm-confirm-text mono">{{ pendingDelete.path }}</p>
          <p class="modal-hint">
            {{ pendingDelete.isDir ? '这是一个目录，其中的全部内容都会一并删除。' : '此操作不可恢复。' }}
          </p>
          <div class="modal-actions">
            <button class="sm" @click="pendingDelete = null">取消</button>
            <button class="sm danger" @click="confirmDelete">删除</button>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { computed, ref, watch } from 'vue'
import {
  store, closeFileManager, loadDir, fmEnter, fmGoParent,
  fmDelete, fmUpload, fmDownload, fmtBytes
} from '../store'

const fm = store.fileManager

// 路径输入框的本地副本：直接双向绑到 fm.cwd 的话，用户每敲一个字符都会
// 触发 store 变更（列表跟着重渲染），而这里的意图只是「敲完再跳过去」。
const pathInput = ref('')
const pendingDelete = ref(null)
const fileEl = ref(null)

// 目录变化时同步输入框。用 watch 而不是 computed 的反向写：
// 输入框在用户编辑期间不该被外部值覆盖，只有在**目录真的变了**时才刷新。
watch(() => fm.cwd, v => { pathInput.value = v || '' }, { immediate: true })

const canGoParent = computed(() => fm.parent && fm.parent !== fm.cwd)

// 上限文案取自后端下发的值，不写死 —— 见 store.maxTransferBytes 的说明。
const limitText = computed(() => fmtBytes(store.maxTransferBytes))

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
  pendingDelete.value = null
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

function askDelete(e) {
  pendingDelete.value = e
}

async function confirmDelete() {
  const target = pendingDelete.value
  pendingDelete.value = null
  if (target) await fmDelete(target.path)
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
/* 宽弹窗：列表要能放下「名字 + 大小 + 权限 + 两个按钮」。 */
.fm-modal {
  width: min(760px, calc(100% - 60px));
  display: flex;
  flex-direction: column;
  max-height: calc(100vh - 100px);
}
.fm-head { display: flex; align-items: baseline; gap: 10px; }
.fm-host { font-size: 11px; }

.fm-pathbar { display: flex; gap: 6px; margin-top: 10px; }
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
}
.fm-notice {
  margin-top: 8px;
  font-size: 12px;
  color: var(--ok, #16a34a);
  background: rgba(22, 163, 74, 0.08);
  border-radius: 6px;
  padding: 6px 9px;
}

/* 列表自己滚：弹窗高度有上限，目录里几百个文件时不能让整页跟着长。 */
.fm-list {
  margin-top: 10px;
  overflow: auto;
  flex: 1;
  min-height: 120px;
  border: 1px solid var(--border);
  border-radius: 7px;
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
.fm-act { flex-shrink: 0; }
.fm-act-gap { width: 44px; flex-shrink: 0; }

.fm-actions { align-items: center; }
.fm-spacer { flex: 1; }
/* 原生 file input 必须存在才能触发选择框，但界面不该看到它。 */
.fm-file { display: none; }

.fm-confirm-text {
  margin: 0;
  font-size: 12px;
  word-break: break-all;
  background: var(--inset-bg);
  border-radius: 6px;
  padding: 8px 10px;
}
</style>
