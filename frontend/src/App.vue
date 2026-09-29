<script setup>
import { onMounted, ref } from 'vue'
import { store, bootstrap, bindEvents, dismissAuditError, initTheme, setTheme } from './store'
import ConsolePanel from './components/ConsolePanel.vue'
import HostsPanel from './components/HostsPanel.vue'
import SettingsPanel from './components/SettingsPanel.vue'
import PolicyPanel from './components/PolicyPanel.vue'
import AuditPanel from './components/AuditPanel.vue'
import SessionsSidebar from './components/SessionsSidebar.vue'

const tab = ref('console')

const tabs = [
  { key: 'console', label: '控制台' },
  { key: 'hosts', label: '主机管理' },
  { key: 'settings', label: 'LLM 设置' },
  { key: 'policy', label: '安全策略' },
  { key: 'audit', label: '审计日志' }
]

// 导航图标：内联 SVG（线性风格，stroke 跟随文字颜色），不引第三方图标库。
const icons = {
  console: 'M4 5h16v11H4z M8 20h8 M12 16v4',            // 终端窗口
  hosts: 'M6 4h12v7H6z M6 13h12v7H6z M9 7.5h.01 M9 16.5h.01', // 服务器两台叠放
  settings: 'M12 8.5a3.5 3.5 0 1 0 0 7 3.5 3.5 0 0 0 0-7z M12 3v2.5 M12 18.5V21 M4.2 7.5l2.2 1.3 M17.6 15.2l2.2 1.3 M4.2 16.5l2.2-1.3 M17.6 8.8l2.2-1.3', // 齿轮
  policy: 'M12 3l7 3v5.5c0 4.3-3 7.6-7 9-4-1.4-7-4.7-7-9V6z M9.5 12l1.8 1.8 3.4-3.6', // 盾牌+对勾
  audit: 'M6 3h9l4 4v14H6z M14 3v5h5 M9.5 12h5 M9.5 16h5' // 文档+行
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
        <div class="brand-title">Ai-Shell</div>
        <div class="brand-sub">Linux 智能运维台</div>
      </div>

      <nav>
        <button
          v-for="t in tabs"
          :key="t.key"
          class="nav-btn"
          :class="{ active: tab === t.key }"
          @click="tab = t.key"
        >
          <svg class="nav-icon" width="15" height="15" viewBox="0 0 24 24" aria-hidden="true">
            <path :d="icons[t.key]" fill="none" stroke="currentColor"
                  stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" />
          </svg>
          {{ t.label }}
        </button>
      </nav>

      <!-- 会话列表（WorkBuddy 风格）：随当前主机变化；点击会话切到控制台。 -->
      <SessionsSidebar v-if="store.ready" class="ssb-wrap" @open="tab = 'console'" />

      <div class="posture" v-if="store.ready">
        <button
          class="theme-btn"
          :title="store.theme === 'dark' ? '切换到浅色主题' : '切换到暗黑主题'"
          @click="setTheme(store.theme === 'dark' ? 'light' : 'dark')"
        >
          {{ store.theme === 'dark' ? '☀ 浅色' : '🌙 暗黑' }}
        </button>
      </div>
    </aside>

    <main class="main">
      <div v-if="store.error" class="fatal">
        <h2>初始化失败</h2>
        <pre>{{ store.error }}</pre>
      </div>

      <template v-else-if="store.ready">
        <!-- 审计写入失败的持久告警。刻意不放进对话流：对话会被后续输出冲走，
             而「审计轨迹已不完整」是需要用户明确知晓并确认的事。 -->
        <div v-if="store.auditError" class="audit-warn" role="alert">
          <div>
            <strong>审计日志异常</strong>
            <span class="audit-warn-msg">{{ store.auditError }}</span>
            <span class="audit-warn-hint">审计记录可能已缺失，且该缺失是永久的 —— 校验时仍会被发现。</span>
          </div>
          <button class="audit-warn-close" @click="dismissAuditError()">知道了</button>
        </div>

        <ConsolePanel v-show="tab === 'console'" />
        <HostsPanel v-if="tab === 'hosts'" />
        <SettingsPanel v-if="tab === 'settings'" />
        <PolicyPanel v-if="tab === 'policy'" />
        <AuditPanel v-if="tab === 'audit'" />
      </template>

      <div v-else class="loading">正在初始化…</div>
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

.theme-btn {
  margin-top: 10px;
  width: 100%;
  font-size: 11.5px;
  min-height: 28px;
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
  gap: 9px;
  text-align: left;
  justify-content: flex-start;
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
