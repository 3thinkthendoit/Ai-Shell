import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { mount } from '@vue/test-utils'
import PrefsPanel from '../components/PrefsPanel.vue'
import { store, setTheme, DEFAULT_TERM_FONT } from '../store.js'
import { i18n, setLocale } from '../i18n.js'

let wrapper

beforeEach(() => {
  setTheme('light')
  setLocale('zh')
  store.termFont = { ...DEFAULT_TERM_FONT }
  wrapper = mount(PrefsPanel)
})

afterEach(() => {
  if (wrapper) wrapper.unmount()
  wrapper = null
})

describe('PrefsPanel 基本配置', () => {
  it('渲染标题、分区与终端预览', () => {
    expect(wrapper.text()).toContain('基本配置')
    expect(wrapper.text()).toContain('外观')
    expect(wrapper.text()).toContain('终端')
    expect(wrapper.find('.term-preview').exists()).toBe(true)
    expect(wrapper.findAll('.theme-pick')).toHaveLength(2)
  })

  it('切换主题立即生效', async () => {
    const darkBtn = wrapper.findAll('.theme-pick').find(b => b.text().includes('暗黑'))
    await darkBtn.trigger('click')
    expect(store.theme).toBe('dark')
    expect(darkBtn.classes()).toContain('on')
  })

  it('切换语言立即生效', async () => {
    const enBtn = wrapper.findAll('.pill-seg button').find(b => b.text() === 'English')
    await enBtn.trigger('click')
    expect(i18n.locale).toBe('en')
    expect(wrapper.text()).toContain('Preferences')
  })
})
