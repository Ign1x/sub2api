<template>
  <div class="flex items-center justify-between gap-4 border-t border-gray-200 pt-4 dark:border-dark-600">
    <div>
      <label class="input-label mb-0">{{ t('admin.accounts.anthropic.nonstreamToStream') }}</label>
      <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.anthropic.nonstreamToStreamDesc') }}</p>
    </div>
    <Toggle v-model="enabled" data-testid="anthropic-nonstream-to-stream" :aria-label="t('admin.accounts.anthropic.nonstreamToStream')" />
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'

const enabled = defineModel<boolean>({ default: false })
const { t } = useI18n()
</script>
