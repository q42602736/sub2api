<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.oauth.openai.autoReauthBatchTitle')"
    width="extra-wide"
    :z-index="60"
    @close="emit('close')"
  >
    <div v-if="!job" class="py-8 text-center text-sm text-gray-500 dark:text-gray-400">
      {{ t('admin.accounts.oauth.openai.autoReauthing') }}
    </div>
    <div v-if="job" class="space-y-4">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div class="min-w-0">
          <p class="truncate text-sm text-gray-500 dark:text-gray-400">
            {{ t('admin.accounts.oauth.openai.autoReauthBatchJobId') }}: {{ job.job_id }}
          </p>
          <p class="mt-1 text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.accounts.oauth.openai.autoReauthBatchProgress', { completed: job.completed, total: job.total }) }}
          </p>
        </div>
        <span
          :class="[
            'rounded-full px-2.5 py-1 text-xs font-medium',
            job.status === 'succeeded'
              ? 'bg-green-100 text-green-700 dark:bg-green-950/40 dark:text-green-300'
              : job.status === 'failed'
                ? 'bg-red-100 text-red-700 dark:bg-red-950/40 dark:text-red-300'
                : 'bg-blue-100 text-blue-700 dark:bg-blue-950/40 dark:text-blue-300'
          ]"
        >
          {{ batchStatusLabel }}
        </span>
      </div>

      <div class="space-y-2">
        <div
          role="progressbar"
          :aria-valuenow="progressPercent"
          aria-valuemin="0"
          aria-valuemax="100"
          class="h-2 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-700"
        >
          <div
            class="h-full rounded-full bg-primary-500 transition-[width] duration-300"
            :style="{ width: `${progressPercent}%` }"
          ></div>
        </div>
        <div class="flex flex-wrap gap-x-4 gap-y-1 text-xs text-gray-500 dark:text-gray-400">
          <span>{{ t('admin.accounts.oauth.openai.autoReauthBatchSucceededCount', { count: job.succeeded_count }) }}</span>
          <span>{{ t('admin.accounts.oauth.openai.autoReauthBatchFailedCount', { count: job.failed_count }) }}</span>
        </div>
      </div>

      <p v-if="currentAccount" class="text-sm text-gray-600 dark:text-gray-300">
        {{ t('admin.accounts.oauth.openai.autoReauthBatchCurrent', { name: accountLabel(currentAccount) }) }}
      </p>

      <div class="max-h-[32rem] space-y-2 overflow-y-auto pr-1">
        <details
          v-for="result in job.results"
          :key="result.account_id"
          :open="shouldExpand(result)"
          class="rounded-lg border border-gray-200 bg-white dark:border-dark-600 dark:bg-dark-800"
        >
          <summary class="flex cursor-pointer list-none items-center justify-between gap-3 px-3 py-3">
            <span class="min-w-0 truncate text-sm font-medium text-gray-900 dark:text-white">
              {{ accountLabel(result) }}
            </span>
            <span
              :class="[
                'shrink-0 rounded-full px-2 py-0.5 text-xs font-medium',
                result.status === 'succeeded'
                  ? 'bg-green-100 text-green-700 dark:bg-green-950/40 dark:text-green-300'
                  : result.status === 'failed'
                    ? 'bg-red-100 text-red-700 dark:bg-red-950/40 dark:text-red-300'
                    : result.status === 'running'
                      ? 'bg-blue-100 text-blue-700 dark:bg-blue-950/40 dark:text-blue-300'
                      : 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
              ]"
            >
              {{ accountStatusLabel(result.status) }}
            </span>
          </summary>
          <div class="border-t border-gray-200 px-3 py-3 dark:border-dark-600">
            <ol v-if="result.logs.length > 0" class="space-y-2">
              <li
                v-for="(log, index) in result.logs"
                :key="`${log.at}-${index}`"
                class="flex items-start gap-3 text-sm"
              >
                <span class="shrink-0 font-mono text-xs text-gray-400 dark:text-gray-500">
                  {{ formatLogTime(log.at) }}
                </span>
                <span class="min-w-0 whitespace-pre-line text-gray-700 dark:text-gray-200">{{ log.message }}</span>
              </li>
            </ol>
            <p v-else class="text-sm text-gray-500 dark:text-gray-400">
              {{ t('admin.accounts.oauth.openai.autoReauthBatchNoLogs') }}
            </p>
            <p v-if="result.error" class="mt-3 whitespace-pre-line text-sm text-red-600 dark:text-red-400">
              {{ result.error }}
            </p>
          </div>
        </details>
      </div>
    </div>

    <template #footer>
      <div class="flex justify-end">
        <button type="button" class="btn btn-secondary" @click="emit('close')">
          {{ t('common.close') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

import BaseDialog from '@/components/common/BaseDialog.vue'
import type {
  AutoReauthorizeBatchAccountResult,
  AutoReauthorizeBatchJob
} from '@/api/admin/accounts'

const emit = defineEmits<{
  close: []
}>()

const props = defineProps<{
  show: boolean
  job: AutoReauthorizeBatchJob | null
}>()

const { t } = useI18n()

const progressPercent = computed(() => {
  if (!props.job || props.job.total <= 0) return 0
  return Math.min(100, Math.max(0, Math.round((props.job.completed / props.job.total) * 100)))
})

const batchStatusLabel = computed(() => {
  if (!props.job) return ''
  if (props.job.status === 'succeeded') return t('admin.accounts.oauth.openai.autoReauthBatchSucceeded')
  if (props.job.status === 'failed') return t('admin.accounts.oauth.openai.autoReauthBatchFailed')
  return t('admin.accounts.oauth.openai.autoReauthBatchRunning')
})

const currentAccount = computed(() => {
  if (!props.job?.current_account_id) return null
  return props.job.results.find((result) => result.account_id === props.job?.current_account_id) || null
})

const accountLabel = (result: Pick<AutoReauthorizeBatchAccountResult, 'account_id' | 'account_name'>) =>
  result.account_name?.trim() || `#${result.account_id}`

const accountStatusLabel = (status: AutoReauthorizeBatchAccountResult['status']) => {
  if (status === 'succeeded') return t('admin.accounts.oauth.openai.autoReauthBatchAccountSucceeded')
  if (status === 'failed') return t('admin.accounts.oauth.openai.autoReauthBatchAccountFailed')
  if (status === 'running') return t('admin.accounts.oauth.openai.autoReauthBatchAccountRunning')
  return t('admin.accounts.oauth.openai.autoReauthBatchAccountPending')
}

const shouldExpand = (result: AutoReauthorizeBatchAccountResult) =>
  result.status === 'running' || result.status === 'failed' || result.account_id === props.job?.current_account_id

const formatLogTime = (value: string) => {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleTimeString()
}
</script>
