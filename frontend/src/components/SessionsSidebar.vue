<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import {
  store, push, selectSession, renameSession, deleteSession
} from '../store'

// 侧边栏的会话区（参考 WorkBuddy 的任务列表交互）：
// 列表常驻侧边栏，条目上的「…」按钮悬停才出现，点开才有「重命名 / 删除」。
// 会话属于当前选中的主机 —— 切了主机，这里的列表会跟着换。
const emit = defineEmits(['open'])

const sessions = computed(() => store.currentSessions || [])

// 默认任务没有自己的名字：用「主机名 (user@addr)」顶替，
// 不同节点的默认任务在列表里一眼可分（否则每台机器都叫「默认任务」）。
const hostOf = hostId => store.hosts.find(h => h.id === hostId)

function label(s) {
  let base
  if (s.name) {
    base = s.name
  } else if (s.isDefault) {
    const h = hostOf(store.currentHostId)
    base = h ? `${h.name} (${h.user}@${h.addr})` : '默认任务'
  } else {
    base = '未命名任务'
  }
  return s.turns > 0 ? `${base}（${s.turns} 轮）` : base
}

function open(s) {
  selectSession(s.id)
  emit('open')
}

// 新建会话入口在控制台顶栏（ConsolePanel），这里只管列表与重命名/删除。

// ---- 重命名（应用内弹窗：与新建/删除同形式，交互一致） ----
const renaming = ref(null)
const renameVal = ref('')
const renameEl = ref(null)

function startRename(s) {
  // 双保险：默认任务的名字就是节点标识（主机名 (user@addr)），
  // 菜单入口虽已隐藏，这里仍拦一道，防止其它路径误入。
  if (s.isDefault) return
  menuFor.value = ''
  renaming.value = s
  renameVal.value = s.name || ''
  nextTick(() => { if (renameEl.value) renameEl.value.focus() })
}

async function commitRename() {
  const s = renaming.value
  const name = renameVal.value.trim()
  renaming.value = null
  renameVal.value = ''
  if (!s || !name || name === (s.name || '')) return
  await renameSession(s.id, name)
}

// ---- 「…」菜单 ----
const menuFor = ref('')

function toggleMenu(s) {
  menuFor.value = menuFor.value === s.id ? '' : s.id
}

function onDocMousedown(e) {
  if (!e.target.closest('.ssb-item') && !e.target.closest('.ssb-menu')) {
    menuFor.value = ''
  }
}

// ---- 删除（应用内弹窗：WKWebView 上原生 confirm 不弹） ----
const deleting = ref(null)

function askDelete(s) {
  menuFor.value = ''
  deleting.value = s
}

async function confirmDelete() {
  const s = deleting.value
  deleting.value = null
  if (s) await deleteSession(s.id)
}

// ---- 分组折叠（WorkBuddy 风格：点「会话 (N) ⌄」收起/展开列表） ----
const collapsed = ref(false)

// timeAgo 把会话的更新时间渲染成 WorkBuddy 式的相对时间。
// 阈值： <1min 刚刚 · <1h N分钟前 · <24h N小时前 · <30d N天前 · 更旧显示日期。
function timeAgo(iso) {
  if (!iso) return ''
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return ''
  const diff = Date.now() - t
  if (diff < 60_000) return '刚刚'
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}分钟前`
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}小时前`
  if (diff < 30 * 86_400_000) return `${Math.floor(diff / 86_400_000)}天前`
  const d = new Date(t)
  return `${d.getMonth() + 1}月${d.getDate()}日`
}

onMounted(() => document.addEventListener('mousedown', onDocMousedown))
onBeforeUnmount(() => document.removeEventListener('mousedown', onDocMousedown))
</script>

<template>
  <div class="ssb">
    <button
      class="ssb-head"
      :aria-expanded="!collapsed"
      @click="collapsed = !collapsed"
    >
      <span>任务 ({{ sessions.length }})</span>
      <svg class="ssb-caret" :class="{ open: !collapsed }" width="9" height="6" viewBox="0 0 10 6" aria-hidden="true">
        <path d="M1 1l4 4 4-4" fill="none" stroke="currentColor" stroke-width="1.5"
              stroke-linecap="round" stroke-linejoin="round" />
      </svg>
    </button>

    <template v-if="!collapsed">
      <div v-if="!store.currentHostId" class="ssb-empty">选择主机后在这里管理任务。</div>
      <div v-else-if="!sessions.length" class="ssb-empty">暂无任务。</div>

      <div
        v-for="s in sessions"
        :key="s.id"
        class="ssb-item"
        :class="{ active: s.id === store.currentSessionId }"
        @click="open(s)"
      >
        <span class="ssb-name" :title="label(s)">{{ label(s) }}</span>
        <span class="ssb-time" :title="s.updatedAt">{{ timeAgo(s.updatedAt) }}</span>
        <!-- 默认任务没有可用操作（不可删、不可改名），「⋯」干脆不给 -->
        <button
          v-if="!s.isDefault"
          class="ssb-more"
          :aria-expanded="menuFor === s.id"
          title="更多操作"
          @click.stop="toggleMenu(s)"
        >⋯</button>

        <div v-if="menuFor === s.id" class="ssb-menu" @click.stop>
          <button @click="startRename(s)">重命名</button>
          <button
            class="ssb-menu-danger"
            @click="askDelete(s)"
          >删除</button>
        </div>
      </div>

      <!-- 重命名弹窗（应用内，与新建/删除同形式） -->
      <div v-if="renaming" class="overlay" @click.self="renaming = null">
        <div class="modal">
          <h3>重命名任务</h3>
          <input
            ref="renameEl"
            v-model="renameVal"
            placeholder="任务名称"
            @keydown.enter.prevent="commitRename"
            @keydown.esc.prevent="renaming = null"
          />
          <div class="modal-hint">Enter 确定 · Esc 取消</div>
          <div class="modal-actions">
            <button class="sm" @click="renaming = null">取消</button>
            <button class="sm primary" @click="commitRename">确定</button>
          </div>
        </div>
      </div>
    </template>

    <div v-if="deleting" class="overlay" @click.self="deleting = null">
      <div class="modal">
        <h3>删除任务</h3>
        <p>
          确认删除「<b>{{ label(deleting) }}</b>」？
          该任务的上下文会被永久丢弃，此操作不可撤销。
        </p>
        <div class="modal-actions">
          <button class="sm" @click="deleting = null">取消</button>
          <button class="sm danger" @click="confirmDelete">确认删除</button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.ssb {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-height: 0;
}

.ssb-head {
  display: flex;
  align-items: center;
  gap: 5px;
  border: none;
  background: transparent;
  font-size: 11.5px;
  font-weight: 600;
  color: var(--text-2);
  padding: 4px 10px;
  letter-spacing: 0.3px;
  cursor: pointer;
  min-height: 0;
  justify-content: flex-start;
}
.ssb-caret { color: var(--text-3); transition: transform 0.15s; }
.ssb-caret.open { transform: rotate(180deg); }

.ssb-empty {
  font-size: 11.5px;
  color: var(--text-3);
  padding: 6px 10px;
  line-height: 1.6;
}

.ssb-item {
  position: relative;
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 8px 10px;
  border-radius: 7px;
  cursor: pointer;
  background: var(--surface-2);
  color: var(--text);
  min-height: 0;
}
.ssb-item:hover { background: var(--hover-overlay); }
.ssb-item.active {
  background: var(--accent-bg);
  color: var(--accent);
  font-weight: 600;
}
.ssb-name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 12.5px;
}
.ssb-time {
  flex-shrink: 0;
  font-size: 11px;
  color: var(--text-3);
}

/* 「…」只在悬停或菜单打开时可见；绝对定位不参与布局 ——
   否则 display:none ↔ inline-flex 切换会把条目撑高（按钮比时间文字高）。
   时间此时已隐藏，不会与按钮重叠。 */
.ssb-more {
  position: absolute;
  right: 8px;
  top: 50%;
  transform: translateY(-50%);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border: none;
  background: var(--surface-2);
  color: var(--text-3);
  padding: 0 4px;
  font-size: 15px;
  line-height: 1;
  min-height: 20px;
  height: 20px;
  border-radius: 6px;
  opacity: 0;
  flex-shrink: 0;
}
.ssb-item:hover .ssb-more,
.ssb-more[aria-expanded='true'] { opacity: 1; }
.ssb-item:hover .ssb-time,
.ssb-item:has(.ssb-more[aria-expanded='true']) .ssb-time { display: none; }
.ssb-more:hover { background: var(--hover-overlay); color: var(--text); }

.ssb-menu {
  position: absolute;
  right: 6px;
  top: calc(100% + 2px);
  z-index: 60;
  display: flex;
  flex-direction: column;
  min-width: 96px;
  background: var(--surface);
  border: 1px solid var(--border-strong);
  border-radius: 8px;
  box-shadow: var(--shadow);
  padding: 3px;
}
.ssb-menu button {
  border: none;
  background: transparent;
  text-align: left;
  justify-content: flex-start;
  padding: 0 10px;
  font-size: 12.5px;
  font-weight: 400;
  min-height: 30px;
  border-radius: 6px;
  color: var(--text);
}
.ssb-menu button:hover { background: var(--surface-2); }
.ssb-menu-danger { color: var(--danger) !important; }
.ssb-menu-danger:hover { background: var(--danger-bg) !important; }

.modal-hint { font-size: 11px; color: var(--text-3); margin-top: 6px; }

.overlay {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.35);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 200;
}
.modal {
  background: var(--surface);
  border: 1px solid var(--border-strong);
  border-radius: var(--radius);
  padding: 18px 20px;
  width: min(380px, calc(100% - 40px));
  box-shadow: var(--shadow);
}
.modal h3 { margin: 0 0 10px; font-size: 14px; }
.modal p { margin: 0 0 14px; font-size: 12.5px; line-height: 1.7; color: var(--text-2); }
.modal-actions { display: flex; justify-content: flex-end; gap: 8px; }
</style>
