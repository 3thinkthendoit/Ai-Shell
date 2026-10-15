<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import {
  store, push, selectSession, renameSession, deleteSession, refreshAllSessionLists
} from '../store'
import { t } from '../i18n'

// 侧边栏的会话区（参考 WorkBuddy 的任务列表交互）：
// 列表常驻侧边栏，条目上的「…」按钮悬停才出现，点开才有「重命名 / 删除」。
// 会话属于当前选中的主机 —— 切了主机，这里的列表会跟着换。
const emit = defineEmits(['open'])

// 侧栏：隐藏从未聊过的默认会话；有 turns 的默认会话仍展示（否则首页对话会「失踪」）。
// 具名任务一律展示。
function visibleTasks(list) {
  return (list || []).filter(s => !s.isDefault || s.turns > 0)
}

const sessions = computed(() => visibleTasks(store.currentSessions))

// ---- 按主机分组的任务视图（未选主机时的欢迎态） ----
//
// 没选主机时不再只是干巴巴一句提示，而是把每台主机的任务分组列出来：
// 点组头进入该主机，点组内任务直达那条会话。重命名/删除只对当前主机
// 有意义（store 的会话操作都以 currentHostId 为准），分组视图里不提供。
const groups = computed(() =>
  store.hosts.map(h => ({
    host: h,
    list: visibleTasks(store.sessions[h.id])
  }))
)

// 分组里的任务名用短名：组头已经标明主机；有内容的默认会话用「未命名任务」。
function shortLabel(s) {
  const base = s.name || (s.isDefault ? t('ss.unnamedTask') : t('console.unnamed'))
  return s.turns > 0 ? base + t('ss.turns', { n: s.turns }) : base
}

function enterHost(hostId) {
  store.currentHostId = hostId
  emit('open')
}

async function enterSession(hostId, s) {
  store.pendingSessionId = s.id
  store.currentHostId = hostId
  emit('open')
}

// 分组视图的数据源是全部主机的会话列表。已选主机时当前主机的列表由
// 切换流程维护，其它主机也要拉一次才能显示；主机集合变化（增删）时重拉。
// 只读填充、不碰选中状态（见 refreshAllSessionLists）。
watch(
  () => store.hosts.map(h => h.id).join('\n'),
  () => { refreshAllSessionLists() },
  { immediate: true }
)

function label(s) {
  const base = s.name || (s.isDefault ? t('ss.unnamedTask') : t('console.unnamed'))
  return s.turns > 0 ? base + t('ss.turns', { n: s.turns }) : base
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
      <span>{{ t('ss.head', { n: sessions.length }) }}</span>
      <svg class="ssb-caret" :class="{ open: !collapsed }" width="9" height="6" viewBox="0 0 10 6" aria-hidden="true">
        <path d="M1 1l4 4 4-4" fill="none" stroke="currentColor" stroke-width="1.5"
              stroke-linecap="round" stroke-linejoin="round" />
      </svg>
    </button>

    <template v-if="!collapsed">
      <!-- 未选主机：按主机分组列出全部任务，点组头/任务可直接进入。 -->
      <template v-if="!store.currentHostId">
        <div v-if="!groups.length" class="ssb-empty">{{ t('ss.empty.none') }}</div>
        <div v-for="g in groups" :key="g.host.id" class="ssb-group">
          <button class="ssb-group-head" :title="t('ss.enterHost')" @click="enterHost(g.host.id)">
            <span class="ssb-group-name">{{ g.host.name }}</span>
            <span class="ssb-group-meta mono">{{ g.host.user }}@{{ g.host.addr }}</span>
            <span class="ssb-group-count">{{ g.list.length }}</span>
          </button>
          <div
            v-for="s in g.list"
            :key="s.id"
            class="ssb-item ssb-item-nested"
            @click="enterSession(g.host.id, s)"
          >
            <span class="ssb-name" :title="shortLabel(s)">{{ shortLabel(s) }}</span>
            <span class="ssb-time" :title="s.updatedAt">{{ timeAgo(s.updatedAt) }}</span>
          </div>
        </div>
      </template>
      <template v-else>
      <div v-if="!sessions.length" class="ssb-empty">{{ t('ss.empty.none') }}</div>

      <div
        v-for="s in sessions"
        :key="s.id"
        class="ssb-item"
        :class="{ active: s.id === store.currentSessionId }"
        @click="open(s)"
      >
        <span class="ssb-name" :title="label(s)">{{ label(s) }}</span>
        <span class="ssb-time" :title="s.updatedAt">{{ timeAgo(s.updatedAt) }}</span>
        <!-- 默认任务可删（后端清空内容）；改名仍只给具名任务 -->
        <button
          class="ssb-more"
          :aria-expanded="menuFor === s.id"
          :title="t('ss.more')"
          @click.stop="toggleMenu(s)"
        >⋯</button>

        <div v-if="menuFor === s.id" class="ssb-menu" @click.stop>
          <button v-if="!s.isDefault" @click="startRename(s)">{{ t('ss.rename') }}</button>
          <button
            class="ssb-menu-danger"
            @click="askDelete(s)"
          >{{ t('ss.delete') }}</button>
        </div>
      </div>
      </template>

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
          {{ t('ss.delete.p1') }}<b>{{ label(deleting) }}</b>{{ t('ss.delete.p2') }}
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
  border: none;
  background: transparent;
  text-align: left;
  padding: 5px 10px;
  border-radius: 6px;
  cursor: pointer;
  min-height: 0;
  color: var(--text-2);
}
.ssb-group-head:hover { background: var(--hover-overlay); color: var(--text); }
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
