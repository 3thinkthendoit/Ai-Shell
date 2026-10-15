<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import {
  store, push, selectSession, renameSession, deleteSession, refreshAllSessionLists,
  bindSession, clearFreshTask
} from '../store'
import { t } from '../i18n'
import { maskHostAddr } from '../hostDisplay'

// 侧边栏的会话区（参考 WorkBuddy 的任务列表交互）：
// 始终按主机分组；启动任务后也不切成「单主机列表」。
// 条目上的「…」悬停才出现，点开才有「重命名 / 删除」。
const emit = defineEmits(['open'])

// 侧栏：隐藏从未聊过的默认会话；有 turns 的默认会话仍展示为「未命名任务」。
// 具名任务一律展示。空默认会话的身份由顶栏/窗口标题「未命名任务」承担。
function visibleTasks(list) {
  return (list || []).filter(s => !s.isDefault || s.turns > 0)
}

// 只展示有可见任务的主机分组；空分组不占位、组头不可点。
const groups = computed(() =>
  store.hosts
    .map(h => ({
      host: h,
      list: visibleTasks(store.sessions[h.id])
    }))
    .filter(g => g.list.length > 0)
)

const totalVisible = computed(() =>
  groups.value.reduce((n, g) => n + g.list.length, 0)
)

function shortLabel(s) {
  const base = s.name || (s.isDefault ? t('ss.unnamedTask') : t('console.unnamed'))
  return s.turns > 0 ? base + t('ss.turns', { n: s.turns }) : base
}

// 删除确认框用完整标签（含轮数），与列表文案一致。
function label(s) {
  return shortLabel(s)
}

function isActive(hostId, s) {
  return hostId === store.currentHostId && s.id === store.currentSessionId
}

function enterSession(hostId, s) {
  // 已在该主机：只切会话，避免再走一遍 host watch。
  if (store.currentHostId === hostId) {
    clearFreshTask()
    void selectSession(s.id)
    emit('open')
    return
  }
  clearFreshTask()
  bindSession()
  store.pendingSessionId = s.id
  store.currentHostId = hostId
  emit('open')
}

// 分组数据源是全部主机的会话列表。当前主机由切换/操作里的 refreshSessions
// 维护；其它主机在主机集合变化时拉齐（见 refreshAllSessionLists）。
watch(
  () => store.hosts.map(h => h.id).join('\n'),
  () => { refreshAllSessionLists() },
  { immediate: true }
)

// 新建会话入口在主导航「新建任务」，这里只管列表与重命名/删除。

// ---- 重命名（应用内弹窗：与新建/删除同形式，交互一致） ----
const renaming = ref(null)
const renameVal = ref('')
const renameEl = ref(null)

function startRename(hostId, s) {
  // 默认会话也可改名（身份仍由 ID 认定，名字只是侧栏标签）。
  menuFor.value = ''
  renaming.value = { hostId, session: s }
  renameVal.value = s.name || ''
  nextTick(() => { if (renameEl.value) renameEl.value.focus() })
}

async function commitRename() {
  const ctx = renaming.value
  const name = renameVal.value.trim()
  renaming.value = null
  renameVal.value = ''
  if (!ctx || !name || name === (ctx.session.name || '')) return
  await renameSession(ctx.session.id, name, { hostId: ctx.hostId })
}

// ---- 「…」菜单 ----
const menuFor = ref('')

function menuKey(hostId, s) {
  return `${hostId}\0${s.id}`
}

function toggleMenu(hostId, s) {
  const key = menuKey(hostId, s)
  menuFor.value = menuFor.value === key ? '' : key
}

function onDocMousedown(e) {
  if (!e.target.closest('.ssb-item') && !e.target.closest('.ssb-menu')) {
    menuFor.value = ''
  }
}

// ---- 删除（应用内弹窗：WKWebView 上原生 confirm 不弹） ----
const deleting = ref(null)

function askDelete(hostId, s) {
  menuFor.value = ''
  deleting.value = { hostId, session: s }
}

async function confirmDelete() {
  const ctx = deleting.value
  deleting.value = null
  if (ctx) await deleteSession(ctx.session.id, { hostId: ctx.hostId })
}

// ---- 分组折叠（WorkBuddy 风格：点「会话 (N) ⌄」收起/展开列表） ----
const collapsed = ref(false)

// timeAgo 把会话的更新时间渲染成 WorkBuddy 式的相对时间。
// 阈值： <1min 刚刚 · <1h N分钟前 · <24h N小时前 · <30d N天前 · 更旧显示日期。
function timeAgo(iso) {
  if (!iso) return ''
  // 局部变量不能叫 t —— 它会遮蔽 i18n 的翻译函数 t()。
  const ts = new Date(iso).getTime()
  if (Number.isNaN(ts)) return ''
  const diff = Date.now() - ts
  if (diff < 60_000) return t('ss.time.justNow')
  if (diff < 3_600_000) return t('ss.time.min', { n: Math.floor(diff / 60_000) })
  if (diff < 86_400_000) return t('ss.time.hour', { n: Math.floor(diff / 3_600_000) })
  if (diff < 30 * 86_400_000) return t('ss.time.day', { n: Math.floor(diff / 86_400_000) })
  const d = new Date(ts)
  return t('ss.time.date', { m: d.getMonth() + 1, d: d.getDate() })
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
      <span>{{ t('ss.head', { n: totalVisible }) }}</span>
      <svg class="ssb-caret" :class="{ open: !collapsed }" width="9" height="6" viewBox="0 0 10 6" aria-hidden="true">
        <path d="M1 1l4 4 4-4" fill="none" stroke="currentColor" stroke-width="1.5"
              stroke-linecap="round" stroke-linejoin="round" />
      </svg>
    </button>

    <template v-if="!collapsed">
      <!-- 始终按主机分组（启动任务后也不切换成单主机列表）。 -->
      <div v-if="!groups.length" class="ssb-empty">{{ t('ss.empty.none') }}</div>
      <div v-for="g in groups" :key="g.host.id" class="ssb-group">
        <div class="ssb-group-head">
          <span class="ssb-group-name">{{ g.host.name }}</span>
          <span class="ssb-group-meta mono">{{ g.host.user }}@{{ maskHostAddr(g.host.addr) }}</span>
          <span class="ssb-group-count">{{ g.list.length }}</span>
        </div>
        <div
          v-for="s in g.list"
          :key="s.id"
          class="ssb-item ssb-item-nested"
          :class="{ active: isActive(g.host.id, s) }"
          @click="enterSession(g.host.id, s)"
        >
          <span class="ssb-name" :title="shortLabel(s)">{{ shortLabel(s) }}</span>
          <span class="ssb-time" :title="s.updatedAt">{{ timeAgo(s.updatedAt) }}</span>
          <button
            class="ssb-more"
            :aria-expanded="menuFor === menuKey(g.host.id, s)"
            :title="t('ss.more')"
            @click.stop="toggleMenu(g.host.id, s)"
          >⋯</button>

          <div v-if="menuFor === menuKey(g.host.id, s)" class="ssb-menu" @click.stop>
            <button @click="startRename(g.host.id, s)">{{ t('ss.rename') }}</button>
            <button
              class="ssb-menu-danger"
              @click="askDelete(g.host.id, s)"
            >{{ t('ss.delete') }}</button>
          </div>
        </div>
      </div>

      <!-- 重命名弹窗（应用内，与新建/删除同形式） -->
      <div v-if="renaming" class="overlay" @click.self="renaming = null">
        <div class="modal">
          <h3>{{ t('ss.renameTitle') }}</h3>
          <input
            ref="renameEl"
            v-model="renameVal"
            :placeholder="t('ss.namePh')"
            @keydown.enter.prevent="commitRename"
            @keydown.esc.prevent="renaming = null"
          />
          <div class="modal-hint">{{ t('ss.enterEsc') }}</div>
          <div class="modal-actions">
            <button class="sm" @click="renaming = null">{{ t('ss.cancel') }}</button>
            <button class="sm primary" @click="commitRename">{{ t('ss.confirm') }}</button>
          </div>
        </div>
      </div>
    </template>

    <div v-if="deleting" class="overlay" @click.self="deleting = null">
      <div class="modal">
        <h3>{{ t('ss.deleteTitle') }}</h3>
        <p>
          {{ t('ss.delete.p1') }}<b>{{ label(deleting.session) }}</b>{{ t('ss.delete.p2') }}
        </p>
        <div class="modal-actions">
          <button class="sm" @click="deleting = null">{{ t('ss.cancel') }}</button>
          <button class="sm danger" @click="confirmDelete">{{ t('ss.deleteBtn') }}</button>
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

/* 分组视图（未选主机）：主机名当组头，组内任务缩进一级。 */
.ssb-group {
  display: flex;
  flex-direction: column;
  gap: 2px;
  margin-bottom: 6px;
}
.ssb-group-head {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 5px 10px;
  min-height: 0;
  color: var(--text-2);
  pointer-events: none;
  user-select: none;
}
.ssb-group-name {
  font-size: 12px;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.ssb-group-meta {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 10.5px;
  color: var(--text-3);
}
.ssb-group-count {
  flex-shrink: 0;
  font-size: 10.5px;
  color: var(--text-3);
  background: var(--surface-2);
  border-radius: 8px;
  padding: 0 6px;
  line-height: 16px;
}
.ssb-item-nested { margin-left: 12px; }

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
