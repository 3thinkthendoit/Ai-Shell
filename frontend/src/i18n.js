// i18n.js —— 轻量国际化：分域字典 + 响应式语言。
//
// 为什么不用 vue-i18n：文案抽离的工作量两种方案完全一样（都在逐字串改模板），
// 差异只在运行时 —— 本应用文案量 ~300 键，一个框架无关的 t() 足够，还省一个
// 依赖。t() 必须能在纯 JS 模块里用（store.js 的错误推送、termPaint.js 画进
// 终端的文案），不能依赖组件上下文。
//
// 翻译范围只有 UI 文案：注释是开发者面向的，不翻；后端（Go）返回的技术
// 诊断原文展示 —— 且 store.writeTerminal 靠中文子串（没有打开|已关闭）识别
// 后端错误来决定静默，那条匹配不受语言影响，也不许翻。
import { reactive } from 'vue'
import { app } from './locales/app'
import { settingsMenu } from './locales/settingsMenu'
import { console as consoleDomain } from './locales/console'
import { chat as chatDomain } from './locales/chat'
import { timeline } from './locales/timeline'
import { storeMsg } from './locales/storeMsg'
import { panels } from './locales/panels'
import { files } from './locales/files'
import { sessions } from './locales/sessions'

const LOCALE_KEY = 'aishell.locale'

// 分域合并：每个域文件持有自己域的 zh/en 两张表，互不冲突 —— 多人（或多个
// 抽离任务）并行各管各的域，不需要碰同一个文件。
const dicts = {
  zh: {
    ...app.zh, ...settingsMenu.zh, ...consoleDomain.zh, ...chatDomain.zh, ...timeline.zh,
    ...storeMsg.zh, ...panels.zh, ...files.zh, ...sessions.zh
  },
  en: {
    ...app.en, ...settingsMenu.en, ...consoleDomain.en, ...chatDomain.en, ...timeline.en,
    ...storeMsg.en, ...panels.en, ...files.en, ...sessions.en
  }
}

export const i18n = reactive({ locale: 'zh' })

function apply(l) {
  i18n.locale = l
  document.documentElement.lang = l === 'en' ? 'en' : 'zh-CN'
}

// initLocale 在应用挂载前调用（main.js）：恢复上次选择；首次使用跟随系统
// 语言。localStorage 不可用（隐私模式等）时回退默认中文。
export function initLocale() {
  let l = ''
  try { l = localStorage.getItem(LOCALE_KEY) || '' } catch { /* 用默认 */ }
  if (l !== 'zh' && l !== 'en') {
    l = /^en/i.test(String(navigator.language || '')) ? 'en' : 'zh'
  }
  apply(l)
  try { localStorage.setItem(LOCALE_KEY, l) } catch { /* 忽略持久化失败 */ }
}

export function setLocale(l) {
  if (l !== 'zh' && l !== 'en') return
  apply(l)
  try { localStorage.setItem(LOCALE_KEY, l) } catch { /* 忽略持久化失败 */ }
}

// t 取当前语言的文案；缺失时回退中文（开发中漏译不至于露出裸 key），再缺
// 返回 key 本身（测试里一眼能看出来）。{name} 形式的占位符用 params 填充。
export function t(key, params) {
  let s = dicts[i18n.locale][key]
  if (s === undefined) s = dicts.zh[key]
  if (s === undefined) return key
  if (!params) return s
  return s.replace(/\{(\w+)\}/g, (_, k) => (k in params ? String(params[k]) : '{' + k + '}'))
}
