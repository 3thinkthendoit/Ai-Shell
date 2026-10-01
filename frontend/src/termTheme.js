// 终端类组件（交互终端、时间线里的内联终端块）共用的 xterm 配色。
//
// 配色跟应用其余部分保持一致（跟随亮/暗主题）：用与界面割裂的终端配色
// 会很突兀 —— 它嵌在界面中间，而不是一个独立窗口。
export const TERM_THEMES = {
  light: {
    background: '#fafaf8',
    foreground: '#2c2c2a',
    cursor: '#2c2c2a',
    selectionBackground: 'rgba(24, 95, 165, 0.25)',
    black: '#2c2c2a',
    red: '#a32d2d',
    green: '#3b6d11',
    yellow: '#854f0b',
    blue: '#185fa5',
    magenta: '#534ab7',
    cyan: '#0f6e56',
    white: '#5f5e5a'
  },
  dark: {
    background: '#0a0c0a',
    foreground: '#d6e2d6',
    cursor: '#00e07f',
    cursorAccent: '#0a0c0a',
    selectionBackground: 'rgba(0, 224, 127, 0.30)',
    black: '#161b16',
    red: '#ff6b60',
    green: '#00e07f',
    yellow: '#f5b04d',
    blue: '#4da6ff',
    magenta: '#a99df5',
    cyan: '#3ddbc4',
    white: '#8fa38f'
  }
}

// currentTermTheme 把 store.theme 映射成 xterm 主题对象，未知主题回退亮色。
export function currentTermTheme(theme) {
  return TERM_THEMES[theme] || TERM_THEMES.light
}
