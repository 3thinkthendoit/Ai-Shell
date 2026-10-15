<script setup>
import { computed, onMounted, ref } from 'vue'
import { store, bootstrap, bindEvents, dismissAuditError, initTheme, requestNewTask } from './store'
import { t } from './i18n'
import ConsolePanel from './components/ConsolePanel.vue'
import HostsPanel from './components/HostsPanel.vue'
import SettingsPanel from './components/SettingsPanel.vue'
import PolicyPanel from './components/PolicyPanel.vue'
import AuditPanel from './components/AuditPanel.vue'
import SessionsSidebar from './components/SessionsSidebar.vue'
import SettingsMenu from './components/SettingsMenu.vue'
import logoSrc from './assets/logo.svg'

const tab = ref('console')

// 高频导航只留两页；LLM 设置/安全策略/审计日志收进底部齿轮（SettingsMenu）。
// tabs 用 computed：语言切换时 t() 重新求值，导航文案跟着换。
const tabs = computed(() => [
  { key: 'console', label: t('app.nav.console') },
  { key: 'hosts', label: t('app.nav.hosts') }
])

// 导航图标：内联 SVG（线性风格，stroke 跟随文字颜色），不引第三方图标库。
const icons = {
  // 新建任务：对话气泡 + 加号（对齐 WorkBuddy 主入口）
  console: 'M4 6h11a3 3 0 0 1 3 3v5a3 3 0 0 1-3 3H9l-4 3v-3H7a3 3 0 0 1-3-3V9a3 3 0 0 1 3-3z M19 4v6 M16 7h6',
  hosts: 'M6 4h12v7H6z M6 13h12v7H6z M9 7.5h.01 M9 16.5h.01' // 服务器两台叠放
}

function onNav(key) {
  tab.value = key
  // 「新建任务」主入口：切到对话页并弹出命名框（与 WorkBuddy 语义对齐）。
  if (key === 'console') requestNewTask()
}

onMounted(async () => {
  initTheme()
  bindEvents()
  await bootstrap()
})
</script>

<template>
  <div class="layout">
    <aside class="sidebar">
      <div class="brand">
        <img class="brand-mark" :src="logoSrc" alt="Ai-Shell" />
        <div>
          <div class="brand-title">Ai-Shell</div>
          <div class="brand-sub">{{ t('app.brand.sub') }}</div>
        </div>
      </div>

      <nav>
        <button
          v-for="item in tabs"
          :key="item.key"
          class="nav-btn"
          :class="{ active: tab === item.key }"
          @click="onNav(item.key)"
        >
          <svg class="nav-icon" width="15" height="15" viewBox="0 0 24 24" aria-hidden="true">
            <path :d="icons[item.key]" fill="none" stroke="currentColor"
                  stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" />
          </svg>
          {{ item.label }}
        </button>
      </nav>

      <!-- 会话列表（WorkBuddy 风格）：随当前主机变化；点击会话切到控制台。 -->
      <SessionsSidebar v-if="store.ready" class="ssb-wrap" @open="tab = 'console'" />

      <div class="posture" v-if="store.ready">
        <!-- 偏好与设置类页面的统一入口（主题/语言/终端字体 + 页面跳转）。 -->
        <SettingsMenu :active-page="tab" @page="tab = $event" />
      </div>
    </aside>

    <main class="main">
      <div v-if="store.error" class="fatal">
        <h2>{{ t('app.fatal') }}</h2>
        <pre>{{ store.error }}</pre>
      </div>

      <template v-else-if="store.ready">
        <!-- 审计写入失败的持久告警。刻意不放进对话流：对话会被后续输出冲走，
             而「审计轨迹已不完整」是需要用户明确知晓并确认的事。 -->
        <div v-if="store.auditError" class="audit-warn" role="alert">
          <div>
            <strong>{{ t('app.auditWarn.title') }}</strong>
            <span class="audit-warn-msg">{{ store.auditError }}</span>
            <span class="audit-warn-hint">{{ t('app.auditWarn.hint') }}</span>
          </div>
          <button class="audit-warn-close" @click="dismissAuditError()">{{ t('app.auditWarn.close') }}</button>
        </div>

        <ConsolePanel v-show="tab === 'console'" @page="tab = $event" />
        <HostsPanel v-if="tab === 'hosts'" />
        <SettingsPanel v-if="tab === 'settings'" />
        <PolicyPanel v-if="tab === 'policy'" />
        <AuditPanel v-if="tab === 'audit'" />
      </template>

      <div v-else class="loading">{{ t('app.loading') }}</div>
    </main>
  </div>
</template>

<style scoped>
.layout {
  display: flex;
  height: 100%;
}

.sidebar {
  width: 216px;
  flex-shrink: 0;
  border-right: 1px solid var(--border);
  background: var(--surface-2);
  display: flex;
  flex-direction: column;
  padding: 18px 12px;
  gap: 18px;
}

.brand {
  display: flex;
  align-items: center;
  gap: 9px;
}
/* 图标自带深色底与留白，不跟随主题变色，保证在浅/暗两套主题下都清晰。 */
.brand-mark {
  width: 32px;
  height: 32px;
  border-radius: 8px;
  flex-shrink: 0;
  display: block;
}
.brand-title {
  font-size: 16px;
  font-weight: 600;
  letter-spacing: 0.2px;
}
.brand-sub {
  font-size: 11px;
  color: var(--text-3);
  margin-top: 2px;
}

nav {
  display: flex;
  flex-direction: column;
  gap: 3px;
}

.ssb-wrap {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
}

.nav-btn {
  /* 跟全局 button 一样居中：图标+文案在胶囊里水平居中（「新建任务」CTA） */
  gap: 9px;
  justify-content: center;
  text-align: center;
  width: 100%;
  border: none;
  background: transparent;
  padding: 0 10px;
  border-radius: 8px;
  color: var(--text-2);
}
.nav-icon { flex-shrink: 0; opacity: 0.75; }
.nav-btn.active .nav-icon { opacity: 1; }
.nav-btn:hover { background: var(--hover-overlay); }
.nav-btn.active {
  background: var(--surface);
  color: var(--accent);
  font-weight: 500;
  border: 1px solid var(--border);
}

.posture {
  margin-top: auto;
  border-top: 1px solid var(--border);
  padding-top: 12px;
}
.dot { width: 7px; height: 7px; border-radius: 50%; display: inline-block; }
.dot.ok { background: var(--dot-ok); }
.dot.warn { background: var(--dot-warn); }

.main {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}

.fatal, .loading {
  padding: 40px;
  color: var(--text-2);
}
.fatal h2 { color: var(--danger); font-size: 15px; }
.fatal pre { margin-top: 12px; color: var(--danger); }

.audit-warn {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  margin: 12px 16px 0;
  padding: 10px 14px;
  border: 1px solid var(--danger);
  border-radius: 6px;
  background: var(--danger-bg);
  color: var(--danger);
  font-size: 12px;
}
.audit-warn strong { display: block; margin-bottom: 3px; font-size: 13px; }
.audit-warn-msg { display: block; }
.audit-warn-hint { display: block; margin-top: 3px; opacity: 0.8; }
.audit-warn-close {
  flex-shrink: 0;
  color: var(--danger);
  border-color: var(--danger);
  background: transparent;
}
</style>
