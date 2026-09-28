<script setup>
import { onMounted, ref } from 'vue'
import { store, bootstrap, bindEvents, dismissAuditError } from './store'
import ConsolePanel from './components/ConsolePanel.vue'
import HostsPanel from './components/HostsPanel.vue'
import SettingsPanel from './components/SettingsPanel.vue'
import PolicyPanel from './components/PolicyPanel.vue'
import AuditPanel from './components/AuditPanel.vue'

const tab = ref('console')

const tabs = [
  { key: 'console', label: '控制台' },
  { key: 'hosts', label: '主机管理' },
  { key: 'settings', label: 'LLM 设置' },
  { key: 'policy', label: '安全策略' },
  { key: 'audit', label: '审计日志' }
]

onMounted(async () => {
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
          {{ t.label }}
        </button>
      </nav>

      <div class="posture" v-if="store.ready">
        <div class="posture-row">
          <span class="dot" :class="store.posture.degraded ? 'warn' : 'ok'"></span>
          <span>{{ store.posture.degraded ? '降级保护' : '系统钥匙串' }}</span>
        </div>
        <div class="posture-note">
          {{ store.posture.degraded
            ? '主密钥存于本地文件（0600），安全性低于钥匙串'
            : '主密钥托管在操作系统钥匙串，其他软件无法直接读取' }}
        </div>
        <div class="posture-host">已配置主机 {{ store.hosts.length }} 台</div>
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

nav {
  display: flex;
  flex-direction: column;
  gap: 3px;
}

.nav-btn {
  text-align: left;
  border: none;
  background: transparent;
  padding: 8px 10px;
  border-radius: 8px;
  color: var(--text-2);
}
.nav-btn:hover { background: rgba(0, 0, 0, 0.05); }
.nav-btn.active {
  background: var(--surface);
  color: var(--accent);
  font-weight: 500;
  border: 1px solid var(--border);
}

.posture {
  margin-top: auto;
  font-size: 11px;
  color: var(--text-2);
  border-top: 1px solid var(--border);
  padding-top: 12px;
}
.posture-row { display: flex; align-items: center; gap: 6px; font-weight: 500; }
.posture-note { color: var(--text-3); margin-top: 4px; line-height: 1.5; }
.posture-host { margin-top: 6px; color: var(--text-3); }
.dot { width: 7px; height: 7px; border-radius: 50%; display: inline-block; }
.dot.ok { background: #639922; }
.dot.warn { background: #ba7517; }

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
