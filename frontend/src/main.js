import { createApp } from 'vue'
import App from './App.vue'
import './style.css'
import { initLocale } from './i18n'
import { initTermFont } from './store'
import { installExternalLinkGuard } from './openExternal'

// 偏好恢复放在 mount 前：语言决定首帧文案、字体决定终端外观，放 onMounted
// 会先闪一帧默认值（主题同理，见 store.initTheme 的注释）。
initLocale()
initTermFont()
// 对话框 Markdown 外链：禁止 WebView 内跳转，改开系统浏览器。
installExternalLinkGuard()

createApp(App).mount('#app')
