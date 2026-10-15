// locales/app.js —— App 外壳（侧栏导航/初始化失败/审计告警/加载态）文案。
export const app = {
  zh: {
    'app.brand.sub': 'Linux 智能运维台',
    'app.nav.console': '新建任务',
    'app.nav.hosts': '主机管理',
    'app.fatal': '初始化失败',
    'app.auditWarn.title': '审计日志异常',
    'app.auditWarn.hint': '审计记录可能已缺失，且该缺失是永久的 —— 校验时仍会被发现。',
    'app.auditWarn.close': '知道了',
    'app.loading': '正在初始化…'
  },
  en: {
    'app.brand.sub': 'Linux AI Ops Console',
    'app.nav.console': 'New task',
    'app.nav.hosts': 'Hosts',
    'app.fatal': 'Initialization failed',
    'app.auditWarn.title': 'Audit log error',
    'app.auditWarn.hint': 'Audit records may be missing and the loss is permanent — it will still be caught during verification.',
    'app.auditWarn.close': 'Got it',
    'app.loading': 'Initializing…'
  }
}
