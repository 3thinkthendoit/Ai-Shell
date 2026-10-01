<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'

// 自绘下拉框：替代原生 <select>。
// 原因：WKWebView（macOS Wails）原生 select 弹层定位/绘制异常，
// 且原生菜单样式无法与界面统一。此组件样式一致，默认向下弹出，
// 靠近窗口底部时自动改为向上弹（如 composer 工具条里的模型选择）。
const props = defineProps({
  modelValue: { type: [String, Number], default: '' },
  options: { type: Array, default: () => [] }, // [{ value, label, disabled?, badge?: { mark, color } }]
  disabled: Boolean,
  placeholder: { type: String, default: '' },
})
const emit = defineEmits(['update:modelValue', 'change'])

const open = ref(false)
const dropUp = ref(false)
const root = ref(null)

const currentOpt = computed(() => props.options.find(o => o.value === props.modelValue) || null)
const current = computed(() => (currentOpt.value ? currentOpt.value.label : props.placeholder))

// toggle 时测量触发器到视口底部的剩余空间：装不下菜单（含 8px 余量）就向上弹。
async function toggle() {
  if (props.disabled) return
  if (open.value) {
    open.value = false
    return
  }
  dropUp.value = false
  open.value = true
  await nextTick()
  if (!root.value) return
  const rect = root.value.getBoundingClientRect()
  const menu = root.value.querySelector('.uis-menu')
  const menuH = menu ? menu.offsetHeight : 240
  if (rect.bottom + menuH + 8 > window.innerHeight) dropUp.value = true
}

function choose(opt) {
  if (opt.disabled) return
  open.value = false
  if (opt.value === props.modelValue) return
  emit('update:modelValue', opt.value)
  emit('change', opt.value)
}

function onDocMousedown(e) {
  if (root.value && !root.value.contains(e.target)) open.value = false
}
function onKeydown(e) {
  if (e.key === 'Escape') open.value = false
}

// 徽标样式：有 logo 时只给 color（SVG 用 currentColor），无 logo 时彩色圆 + 白字。
function badgeStyle(b) {
  return b.logo ? { color: b.color } : { background: b.color, color: '#fff' }
}

onMounted(() => {
  document.addEventListener('mousedown', onDocMousedown)
  document.addEventListener('keydown', onKeydown)
})
onBeforeUnmount(() => {
  document.removeEventListener('mousedown', onDocMousedown)
  document.removeEventListener('keydown', onKeydown)
})
</script>

<template>
  <div ref="root" class="uis" :class="{ open, dropup: dropUp, disabled: props.disabled }">
    <button type="button" class="uis-trigger" :disabled="props.disabled" @click="toggle">
      <span
        v-if="currentOpt && currentOpt.badge"
        class="uis-badge"
        :class="{ 'is-logo': !!currentOpt.badge.logo }"
        :style="badgeStyle(currentOpt.badge)"
        v-html="currentOpt.badge.logo || currentOpt.badge.mark"
      ></span>
      <span class="uis-label" :title="current">{{ current }}</span>
      <svg class="uis-arrow" width="10" height="6" viewBox="0 0 10 6" aria-hidden="true">
        <path d="M1 1l4 4 4-4" fill="none" stroke="currentColor" stroke-width="1.5"
              stroke-linecap="round" stroke-linejoin="round" />
      </svg>
    </button>
    <ul v-if="open" class="uis-menu" role="listbox">
      <li
        v-for="opt in options"
        :key="opt.value"
        class="uis-item"
        role="option"
        :aria-selected="opt.value === modelValue"
        :class="{ selected: opt.value === modelValue, disabled: opt.disabled }"
        @click="choose(opt)"
      >
        <span v-if="opt.badge" class="uis-badge" :class="{ 'is-logo': !!opt.badge.logo }" :style="badgeStyle(opt.badge)" v-html="opt.badge.logo || opt.badge.mark"></span>
        <span class="uis-item-label">{{ opt.label }}</span>
      </li>
    </ul>
  </div>
</template>
