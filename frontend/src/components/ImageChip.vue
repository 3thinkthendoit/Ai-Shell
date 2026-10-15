<script setup>
/**
 * 附图芯片：默认只显示文件名；悬停才弹出图片预览。
 * removable 时左侧显示 ×（作曲箱待发图）。
 */
defineProps({
  name: { type: String, default: 'image.png' },
  preview: { type: String, default: '' },
  removable: { type: Boolean, default: false },
  disabled: { type: Boolean, default: false }
})

defineEmits(['remove'])
</script>

<template>
  <span class="image-chip" :title="name">
    <button
      v-if="removable"
      type="button"
      class="image-chip-x"
      :disabled="disabled"
      :aria-label="'remove ' + name"
      @click.stop="$emit('remove')"
    >×</button>
    <span class="image-chip-name">{{ name }}</span>
    <span v-if="preview" class="image-chip-preview" aria-hidden="true">
      <img :src="preview" :alt="name" />
    </span>
  </span>
</template>

<style scoped>
.image-chip {
  position: relative;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  max-width: 220px;
  padding: 3px 10px 3px 6px;
  border-radius: 8px;
  border: 1px solid var(--border, #e5e7eb);
  background: var(--surface-2, #f3f4f6);
  color: var(--text, #111827);
  font-size: 13px;
  line-height: 1.35;
  vertical-align: middle;
  cursor: default;
}
.image-chip-x {
  flex: 0 0 auto;
  width: 18px;
  height: 18px;
  margin: 0;
  padding: 0;
  border: 0;
  border-radius: 4px;
  background: transparent;
  color: var(--muted, #6b7280);
  font-size: 14px;
  line-height: 1;
  cursor: pointer;
}
.image-chip-x:hover:not(:disabled) {
  color: var(--text, #111827);
  background: rgba(0, 0, 0, 0.06);
}
.image-chip-x:disabled {
  opacity: 0.4;
  cursor: default;
}
.image-chip-name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  min-width: 0;
}
.image-chip-preview {
  display: none;
  position: absolute;
  bottom: calc(100% + 8px);
  left: 0;
  z-index: 50;
  padding: 6px;
  border-radius: 10px;
  border: 1px solid var(--border, #e5e7eb);
  background: var(--surface, #fff);
  box-shadow: 0 10px 28px rgba(0, 0, 0, 0.14);
  pointer-events: none;
}
.image-chip:hover .image-chip-preview {
  display: block;
}
.image-chip-preview img {
  display: block;
  max-width: min(320px, 70vw);
  max-height: 220px;
  border-radius: 6px;
  object-fit: contain;
}
</style>

<style>
/* 非 scoped：父组件传入的 preview-end 挂在芯片根上，用于右对齐气泡。 */
.image-chip.preview-end .image-chip-preview {
  left: auto;
  right: 0;
}
</style>
