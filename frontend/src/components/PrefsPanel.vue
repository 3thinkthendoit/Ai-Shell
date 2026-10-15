<script setup>
import { computed } from 'vue'
import { store, setTheme, setTermFont, DEFAULT_TERM_FONT } from '../store'
import { i18n, setLocale, t } from '../i18n'
import UiSelect from './UiSelect.vue'

// 基本配置：主题 / 语言 / 终端字体字号。

const fontOptions = computed(() => [
  { value: DEFAULT_TERM_FONT.family, label: t('menu.font.system') },
  { value: 'Consolas, "Courier New", monospace', label: 'Consolas' },
  { value: '"Cascadia Code", Consolas, monospace', label: 'Cascadia Code' },
  { value: '"JetBrains Mono", Consolas, monospace', label: 'JetBrains Mono' },
  { value: '"Source Code Pro", Consolas, monospace', label: 'Source Code Pro' },
  { value: '"Sarasa Mono SC", "Microsoft YaHei", monospace', label: 'Sarasa Mono SC' }
])

const previewStyle = computed(() => ({
  fontFamily: store.termFont.family,
  fontSize: `${store.termFont.size}px`
}))

function onFontChange(v) {
  setTermFont({ family: v })
}
function bumpSize(delta) {
  setTermFont({ size: store.termFont.size + delta })
}
</script>

<template>
  <div class="panel">
    <header class="panel-head">
      <h2>{{ t('prefs.title') }}</h2>
      <p class="muted">{{ t('prefs.headHint') }}</p>
    </header>

    <section class="block">
      <div class="block-label">{{ t('menu.appearance') }}</div>
      <div class="stack">
        <div class="field">
          <div class="field-meta">
            <div class="field-title">{{ t('menu.theme') }}</div>
            <div class="field-desc">{{ t('prefs.theme.hint') }}</div>
          </div>
          <div class="theme-picks" role="group" :aria-label="t('menu.theme')">
            <button
              type="button"
              class="theme-pick"
              :class="{ on: store.theme === 'light' }"
              @click="setTheme('light')"
            >
              <span class="theme-swatch light" aria-hidden="true">
                <span class="sw-bar"></span>
                <span class="sw-line"></span>
                <span class="sw-line short"></span>
              </span>
              <span class="theme-name">{{ t('menu.theme.light') }}</span>
            </button>
            <button
              type="button"
              class="theme-pick"
              :class="{ on: store.theme === 'dark' }"
              @click="setTheme('dark')"
            >
              <span class="theme-swatch dark" aria-hidden="true">
                <span class="sw-bar"></span>
                <span class="sw-line"></span>
                <span class="sw-line short"></span>
              </span>
              <span class="theme-name">{{ t('menu.theme.dark') }}</span>
            </button>
          </div>
        </div>

        <div class="field">
          <div class="field-meta">
            <div class="field-title">{{ t('menu.language') }}</div>
            <div class="field-desc">{{ t('prefs.lang.hint') }}</div>
          </div>
          <div class="pill-seg" role="group" :aria-label="t('menu.language')">
            <button type="button" :class="{ on: i18n.locale === 'zh' }" @click="setLocale('zh')">中文</button>
            <button type="button" :class="{ on: i18n.locale === 'en' }" @click="setLocale('en')">English</button>
          </div>
        </div>
      </div>
    </section>

    <section class="block">
      <div class="block-label">{{ t('menu.terminal') }}</div>
      <div class="stack">
        <div class="field">
          <div class="field-meta">
            <div class="field-title">{{ t('menu.font') }}</div>
            <div class="field-desc">{{ t('prefs.font.hint') }}</div>
          </div>
          <UiSelect
            class="font-select"
            :model-value="store.termFont.family"
            :options="fontOptions"
            @update:model-value="onFontChange"
          />
        </div>

        <div class="field">
          <div class="field-meta">
            <div class="field-title">{{ t('menu.fontSize') }}</div>
            <div class="field-desc">{{ t('prefs.fontSize.hint') }}</div>
          </div>
          <div class="stepper">
            <button type="button" class="step-btn" :aria-label="t('prefs.fontSize.dec')" @click="bumpSize(-0.5)">−</button>
            <span class="step-val">{{ store.termFont.size }}</span>
            <button type="button" class="step-btn" :aria-label="t('prefs.fontSize.inc')" @click="bumpSize(0.5)">+</button>
          </div>
        </div>

        <div class="term-preview" :style="previewStyle" aria-hidden="true">
          <span class="preview-prompt">root@host</span>
          <span class="preview-path">:~$</span>
          <span class="preview-cmd"> ls -la /var/log</span>
        </div>
      </div>
    </section>
  </div>
</template>

<style scoped>
.panel {
  padding: 28px 32px 48px;
  overflow-y: auto;
  height: 100%;
  background:
    radial-gradient(900px 420px at 0% -10%, color-mix(in srgb, var(--accent) 10%, transparent), transparent 60%),
    var(--bg);
}
.panel > * { max-width: 560px; }

.panel-head { margin-bottom: 28px; }
.panel-head h2 {
  font-size: 20px;
  font-weight: 650;
  letter-spacing: -0.02em;
  margin: 0 0 8px;
}
.panel-head p {
  margin: 0;
  font-size: 13px;
  line-height: 1.65;
  color: var(--text-3);
  max-width: 420px;
}

.block { margin-bottom: 28px; }
.block-label {
  font-size: 11px;
  font-weight: 650;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--text-3);
  margin-bottom: 10px;
  padding-left: 2px;
}

.stack {
  border: 1px solid var(--border);
  border-radius: 12px;
  overflow: hidden;
  background: var(--surface);
}
.field {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 20px;
  padding: 16px 18px;
  min-height: 72px;
}
.field + .field {
  border-top: 1px solid var(--border);
}

.field-meta { min-width: 0; flex: 1; }
.field-title {
  font-size: 13.5px;
  font-weight: 600;
  color: var(--text);
  margin-bottom: 3px;
}
.field-desc {
  font-size: 12px;
  color: var(--text-3);
  line-height: 1.45;
}

.theme-picks {
  display: flex;
  gap: 8px;
  flex-shrink: 0;
}
.theme-pick {
  display: flex;
  flex-direction: column;
  align-items: stretch;
  gap: 6px;
  padding: 6px;
  min-height: 0;
  border: 1.5px solid var(--border);
  border-radius: 10px;
  background: transparent;
  color: var(--text-2);
  font-weight: 500;
  transition: border-color 0.12s, background 0.12s, color 0.12s;
}
.theme-pick:hover:not(:disabled) {
  background: var(--hover-overlay);
  border-color: var(--border-strong);
}
.theme-pick.on {
  border-color: var(--accent);
  background: var(--accent-bg);
  color: var(--accent);
}
.theme-swatch {
  width: 72px;
  height: 46px;
  border-radius: 7px;
  padding: 8px 8px 6px;
  display: flex;
  flex-direction: column;
  gap: 5px;
  border: 1px solid transparent;
}
.theme-swatch.light {
  background: #f7f6f2;
  border-color: rgba(0, 0, 0, 0.08);
}
.theme-swatch.dark {
  background: #101410;
  border-color: rgba(0, 224, 127, 0.18);
}
.sw-bar {
  display: block;
  height: 5px;
  width: 38%;
  border-radius: 2px;
}
.theme-swatch.light .sw-bar { background: #185fa5; }
.theme-swatch.dark .sw-bar { background: #00e07f; }
.sw-line {
  display: block;
  height: 3px;
  width: 78%;
  border-radius: 2px;
}
.sw-line.short { width: 52%; }
.theme-swatch.light .sw-line { background: rgba(0, 0, 0, 0.14); }
.theme-swatch.dark .sw-line { background: rgba(214, 226, 214, 0.22); }
.theme-name {
  font-size: 11.5px;
  text-align: center;
  line-height: 1.2;
}

.pill-seg {
  display: flex;
  gap: 4px;
  padding: 3px;
  background: var(--surface-2);
  border-radius: 9px;
  flex-shrink: 0;
}
.pill-seg button {
  border: none;
  background: transparent;
  color: var(--text-2);
  font-size: 12.5px;
  font-weight: 550;
  padding: 0 14px;
  min-height: 30px;
  border-radius: 7px;
}
.pill-seg button:hover:not(:disabled) {
  background: var(--hover-overlay);
  border-color: transparent;
}
.pill-seg button.on {
  background: var(--surface);
  color: var(--text);
  box-shadow: 0 1px 2px rgba(0, 0, 0, 0.06), 0 0 0 1px var(--border);
}

.font-select {
  width: 200px;
  flex-shrink: 0;
}

.stepper {
  display: flex;
  align-items: center;
  gap: 2px;
  padding: 3px;
  background: var(--surface-2);
  border-radius: 9px;
  flex-shrink: 0;
}
.step-btn {
  width: 30px;
  min-height: 30px;
  padding: 0;
  border: none;
  border-radius: 7px;
  background: transparent;
  color: var(--text-2);
  font-size: 15px;
  font-weight: 500;
}
.step-btn:hover:not(:disabled) {
  background: var(--surface);
  border-color: transparent;
  color: var(--text);
}
.step-val {
  min-width: 40px;
  text-align: center;
  font-size: 13px;
  font-weight: 600;
  font-variant-numeric: tabular-nums;
  color: var(--text);
}

.term-preview {
  padding: 14px 18px;
  border-top: 1px solid var(--border);
  background: #0e120e;
  color: #b8cbb8;
  line-height: 1.5;
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
}
.preview-prompt { color: #00e07f; }
.preview-path { color: #7a9480; }
.preview-cmd { color: #d6e2d6; }
</style>
