import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import SettingsMenu from '../components/SettingsMenu.vue'
import { setLocale } from '../i18n.js'

let wrapper

beforeEach(() => {
  setLocale('zh')
  wrapper = mount(SettingsMenu, { props: { activePage: 'console' }, attachTo: document.body })
})

afterEach(() => {
  if (wrapper) wrapper.unmount()
  wrapper = null
})

describe('SettingsMenu', () => {
  it('齿轮打开后只列页面入口，含基本配置', async () => {
    await wrapper.find('.sm-trigger').trigger('click')
    await nextTick()
    const labels = wrapper.findAll('.sm-page').map(b => b.text())
    expect(labels.some(t => t.includes('基本配置'))).toBe(true)
    expect(labels.some(t => t.includes('主机管理'))).toBe(true)
    expect(labels.some(t => t.includes('LLM'))).toBe(true)
    expect(wrapper.find('.sm-seg').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('字号')
  })

  it('点基本配置 → emit page prefs 并关闭弹层', async () => {
    await wrapper.find('.sm-trigger').trigger('click')
    await nextTick()
    const prefs = wrapper.findAll('.sm-page').find(b => b.text().includes('基本配置'))
    await prefs.trigger('click')
    expect(wrapper.emitted('page')?.[0]).toEqual(['prefs'])
    expect(wrapper.find('.sm-pop').exists()).toBe(false)
  })
})
