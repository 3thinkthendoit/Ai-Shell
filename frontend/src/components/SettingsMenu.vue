<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { t } from '../i18n'

// 设置弹层：侧栏底部齿轮。只做低频页面入口（基本配置 / 主机 / LLM / 策略 / 审计）。
// 弹层从齿轮向上展开，点外部或 Esc 关闭。
const props = defineProps({
  // 当前激活页面 key，用于齿轮高亮。
  activePage: { type: String, default: '' }
})
const emit = defineEmits(['page'])

const open = ref(false)
const root = ref(null)

const gearActive = computed(() =>
  ['prefs', 'hosts', 'settings', 'policy', 'audit'].includes(props.activePage)
)

const GEAR_PATH = 'M12 8.5a3.5 3.5 0 1 0 0 7 3.5 3.5 0 0 0 0-7z M12 3v2.5 M12 18.5V21 M4.2 7.5l2.2 1.3 M17.6 15.2l2.2 1.3 M4.2 16.5l2.2-1.3 M17.6 8.8l2.2-1.3'

const PAGE_ICONS = {
  prefs: 'M4 6h16v2H4z M4 11h10v2H4z M4 16h13v2H4z', // 偏好列表
  hosts: 'M6 4h12v7H6z M6 13h12v7H6z M9 7.5h.01 M9 16.5h.01',
  settings: 'M12 8.5a3.5 3.5 0 1 0 0 7 3.5 3.5 0 0 0 0-7z M12 3v2.5 M12 18.5V21 M4.2 7.5l2.2 1.3 M17.6 15.2l2.2 1.3 M4.2 16.5l2.2-1.3 M17.6 8.8l2.2-1.3',
  policy: 'M12 3l7 3v5.5c0 4.3-3 7.6-7 9-4-1.4-7-4.7-7-9V6z M9.5 12l1.8 1.8 3.4-3.6',
  audit: 'M6 3h9l4 4v14H6z M14 3v5h5 M9.5 12h5 M9.5 16h5'
}

const pages = computed(() => [
  { key: 'prefs', label: t('menu.page.prefs'), icon: PAGE_ICONS.prefs },
  { key: 'hosts', label: t('menu.page.hosts'), icon: PAGE_ICONS.hosts },
  { key: 'settings', label: t('menu.page.settings'), icon: PAGE_ICONS.settings },
  { key: 'policy', label: t('menu.page.policy'), icon: PAGE_ICONS.policy },
  { key: 'audit', label: t('menu.page.audit'), icon: PAGE_ICONS.audit }
])

function toggle() {
  open.value = !open.value
}
function close() {
  open.value = false
}
function goPage(key) {
  close()
  emit('page', key)
}
function onDocMousedown(e) {
  if (root.value && !root.value.contains(e.target)) close()
}
function onKeydown(e) {
  if (e.key === 'Escape') close()
}
onMounted(() => {
  document.addEventListener('mousedown', onDocMousedown)
  document.addEventListener('keydown', onKeydown)
})
onBeforeUnmount(() => {
  document.removeEventListener('mousedown', onDocMousedown)
  document.removeEventListener('keydown', onKeydown)
})
</script>

<template>
  <div ref="root" class="sm-root">
    <button class="sm-trigger" :class="{ active: gearActive, open }" :title="t('menu.gear')" @click="toggle">
      <svg width="15" height="15" viewBox="0 0 24 24" aria-hidden="true">
        <path :d="GEAR_PATH" fill="none" stroke="currentColor"
              stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" />
      </svg>
      <span>{{ t('menu.gear') }}</span>
    </button>

    <div v-if="open" class="sm-pop" role="menu">
      <button
        v-for="p in pages"
        :key="p.key"
        class="sm-page"
        :class="{ on: activePage === p.key }"
        @click="goPage(p.key)"
      >
        <span class="sm-page-label">
          <svg width="13" height="13" viewBox="0 0 24 24" aria-hidden="true">
            <path :d="p.icon" fill="none" stroke="currentColor"
                  stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" />
          </svg>
          <span>{{ p.label }}</span>
        </span>
        <span class="sm-arrow">›</span>
      </button>
    </div>
  </div>
</template>

<style scoped>
.sm-root {
  position: relative;
  margin-top: 10px;
}
.sm-trigger {
  width: 100%;
  display: flex;
  align-items: center;
  justify-content: flex-start;
  gap: 9px;
  font-size: 11.5px;
  font-weight: 600;
  min-height: 28px;
  text-align: left;
}
.sm-trigger.open {
  background: var(--surface);
  border: 1px solid var(--border);
}
.sm-trigger.active {
  color: var(--accent);
  font-weight: 600;
}

.sm-pop {
  position: absolute;
  bottom: calc(100% + 8px);
  left: 0;
  right: 0;
  z-index: 60;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: 10px;
  box-shadow: 0 8px 24px rgba(0, 0, 0, 0.18);
  padding: 8px;
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.sm-page {
  width: 100%;
  display: flex;
  align-items: center;
  justify-content: space-between;
  text-align: left;
  font-size: 12.5px;
  color: var(--text-2);
  border: none;
  background: transparent;
  padding: 9px 10px;
  border-radius: 8px;
  min-height: 36px;
  gap: 8px;
}
.sm-page-label {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.sm-page-label svg {
  flex: none;
  opacity: 0.75;
}
.sm-page:hover {
  background: var(--hover-overlay);
}
.sm-page.on {
  color: var(--accent);
  background: var(--surface-2);
}
.sm-arrow {
  opacity: 0.5;
}
</style>
