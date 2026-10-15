<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { store, setTheme, setTermFont, DEFAULT_TERM_FONT } from '../store'
import { i18n, setLocale, t } from '../i18n'
import UiSelect from './UiSelect.vue'

// 设置弹层：侧栏底部齿轮。把散落的偏好（主题按钮原先独占一个位置）与新增
// 偏好（语言、终端字体）收进一个入口；LLM 设置/安全策略/审计日志三个低频
// 页面也从主导航挪到这里 —— 高频切换（控制台↔主机）保持一键直达。
//
// 弹层从齿轮向上展开（齿轮贴着窗口底部），点外部或 Esc 关闭。
const props = defineProps({
  // 当前激活页面 key（console/hosts/settings/policy/audit），
  // 用于给齿轮一个「正停在设置类页面」的高亮。
  activePage: { type: String, default: '' }
})
const emit = defineEmits(['page'])

const open = ref(false)
const root = ref(null)

const gearActive = computed(() => ['settings', 'policy', 'audit'].includes(props.activePage))

const GEAR_PATH = 'M12 8.5a3.5 3.5 0 1 0 0 7 3.5 3.5 0 0 0 0-7z M12 3v2.5 M12 18.5V21 M4.2 7.5l2.2 1.3 M17.6 15.2l2.2 1.3 M4.2 16.5l2.2-1.3 M17.6 8.8l2.2-1.3'

// 等宽字体候选：只列跨平台常见的。第一项是与历史默认完全相同的系统栈，
// 保证升级后老用户观感不变；value 直接喂给 xterm options.fontFamily。
const fontOptions = computed(() => [
  { value: DEFAULT_TERM_FONT.family, label: t('menu.font.system') },
  { value: 'Consolas, "Courier New", monospace', label: 'Consolas' },
  { value: '"Cascadia Code", Consolas, monospace', label: 'Cascadia Code' },
  { value: '"JetBrains Mono", Consolas, monospace', label: 'JetBrains Mono' },
  { value: '"Source Code Pro", Consolas, monospace', label: 'Source Code Pro' },
  { value: '"Sarasa Mono SC", "Microsoft YaHei", monospace', label: 'Sarasa Mono SC' }
])

// 页面入口沿用原侧栏导航的图标（收敛进弹层时视觉延续），线性风格 stroke
// 跟随文字颜色，不引第三方图标库。
const PAGE_ICONS = {
  settings: 'M12 8.5a3.5 3.5 0 1 0 0 7 3.5 3.5 0 0 0 0-7z M12 3v2.5 M12 18.5V21 M4.2 7.5l2.2 1.3 M17.6 15.2l2.2 1.3 M4.2 16.5l2.2-1.3 M17.6 8.8l2.2-1.3', // 齿轮
  policy: 'M12 3l7 3v5.5c0 4.3-3 7.6-7 9-4-1.4-7-4.7-7-9V6z M9.5 12l1.8 1.8 3.4-3.6', // 盾牌+对勾
  audit: 'M6 3h9l4 4v14H6z M14 3v5h5 M9.5 12h5 M9.5 16h5' // 文档+行
}

const pages = computed(() => [
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
function onFontChange(v) {
  setTermFont({ family: v })
}
// 字号步进（0.5 一档）：夹紧（8~28）在 store.setTermFont 里兜底。
function bumpSize(delta) {
  setTermFont({ size: store.termFont.size + delta })
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
      <div class="sm-sec">
        <div class="sm-sec-title">{{ t('menu.appearance') }}</div>
        <div class="sm-row">
          <span class="sm-label">{{ t('menu.theme') }}</span>
          <div class="sm-seg">
            <button :class="{ on: store.theme === 'light' }" @click="setTheme('light')">{{ t('menu.theme.light') }}</button>
            <button :class="{ on: store.theme === 'dark' }" @click="setTheme('dark')">{{ t('menu.theme.dark') }}</button>
          </div>
        </div>
        <div class="sm-row">
          <span class="sm-label">{{ t('menu.language') }}</span>
          <div class="sm-seg">
            <button :class="{ on: i18n.locale === 'zh' }" @click="setLocale('zh')">中文</button>
            <button :class="{ on: i18n.locale === 'en' }" @click="setLocale('en')">English</button>
          </div>
        </div>
      </div>

      <div class="sm-sec">
        <div class="sm-sec-title">{{ t('menu.terminal') }}</div>
        <div class="sm-row">
          <span class="sm-label">{{ t('menu.font') }}</span>
          <UiSelect class="sm-select" :model-value="store.termFont.family" :options="fontOptions" @update:model-value="onFontChange" />
        </div>
        <div class="sm-row">
          <span class="sm-label">{{ t('menu.fontSize') }}</span>
          <div class="sm-step">
            <button type="button" @click="bumpSize(-0.5)">−</button>
            <span class="sm-size">{{ store.termFont.size }}</span>
            <button type="button" @click="bumpSize(0.5)">+</button>
          </div>
        </div>
      </div>

      <div class="sm-sec">
        <div class="sm-sec-title">{{ t('menu.pages') }}</div>
        <button v-for="p in pages" :key="p.key" class="sm-page" :class="{ on: activePage === p.key }" @click="goPage(p.key)">
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
  gap: 9px;
  font-size: 11.5px;
  min-height: 28px;
}
.sm-trigger.open {
  background: var(--surface);
  border: 1px solid var(--border);
}
.sm-trigger.active {
  color: var(--accent);
  font-weight: 500;
}

/* 弹层：锚在齿轮正上方向上展开，压过会话列表。 */
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
  padding: 6px;
}
.sm-sec + .sm-sec {
  border-top: 1px solid var(--border);
  margin-top: 5px;
  padding-top: 7px;
}
.sm-sec-title {
  font-size: 10.5px;
  color: var(--text-3);
  padding: 3px 8px 5px;
  letter-spacing: 0.3px;
}
.sm-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding: 3px 8px;
  min-height: 30px;
}
.sm-label {
  font-size: 12px;
  color: var(--text-2);
}

/* 二选一分段控件（主题/语言）。 */
.sm-seg {
  display: flex;
  border: 1px solid var(--border);
  border-radius: 7px;
  overflow: hidden;
}
.sm-seg button {
  border: none;
  background: transparent;
  color: var(--text-2);
  font-size: 11.5px;
  padding: 3px 10px;
  min-height: 24px;
}
.sm-seg button + button {
  border-left: 1px solid var(--border);
}
.sm-seg button.on {
  background: var(--accent);
  color: #fff;
}

.sm-select {
  flex: 1;
  max-width: 170px;
}

/* 字号步进器。 */
.sm-step {
  display: flex;
  align-items: center;
  gap: 4px;
}
.sm-step button {
  width: 24px;
  min-height: 24px;
  padding: 0;
  font-size: 13px;
}
.sm-size {
  min-width: 34px;
  text-align: center;
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}

/* 页面入口：低频页面（LLM 设置/安全策略/审计日志）从主导航收编到这里。 */
.sm-page {
  width: 100%;
  display: flex;
  align-items: center;
  justify-content: space-between;
  text-align: left;
  font-size: 12px;
  color: var(--text-2);
  border: none;
  background: transparent;
  padding: 5px 8px;
  border-radius: 7px;
  min-height: 28px;
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
