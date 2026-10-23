<template>
  <div v-if="isSiwc" class="flex flex-col items-start gap-0.5" data-testid="siwc-gate-badge">
    <!-- 资格闸门：blocked 红 / restorable 黄；点击打开详情弹窗 -->
    <button
      v-if="gate?.state === 'blocked'"
      type="button"
      class="badge badge-danger cursor-pointer text-xs"
      :title="t('admin.accounts.siwcGate.badgeBlockedTitle')"
      data-testid="siwc-gate-blocked"
      @click="emit('show-gate', account)"
    >
      {{ t('admin.accounts.status.siwcBlocked') }}
    </button>
    <button
      v-else-if="gate?.state === 'restorable'"
      type="button"
      class="badge badge-warning cursor-pointer text-xs"
      :title="t('admin.accounts.siwcGate.badgeRestorableTitle')"
      data-testid="siwc-gate-restorable"
      @click="emit('show-gate', account)"
    >
      {{ t('admin.accounts.status.siwcRestorable') }}
    </button>
    <span
      v-if="gate?.state === 'blocked' && nextCheckText"
      class="text-[11px] leading-4 text-gray-500 dark:text-gray-400"
      data-testid="siwc-gate-next-check"
    >
      {{ t('admin.accounts.siwcGate.badgeNextCheck', { time: nextCheckText }) }}
    </span>

    <!-- 指纹：ok 绿点 / suspect 红字 / error 灰字，悬停显示中位数与时间 -->
    <span
      v-if="fingerprint"
      :class="['inline-flex items-center gap-1 text-[11px] leading-4', fingerprintClass]"
      :title="fingerprintTitle"
      data-testid="siwc-fingerprint"
    >
      <span :class="['h-1.5 w-1.5 rounded-full', fingerprintDotClass]" />
      <span>{{ fingerprintLabel }}</span>
    </span>

    <!-- 关联的管理凭据 -->
    <span
      v-if="managementAccountId"
      class="text-[11px] leading-4 text-gray-400 dark:text-gray-500"
      data-testid="siwc-management-account"
    >
      {{ t('admin.accounts.siwcGate.managementAccount', { id: managementAccountId }) }}
    </span>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Account, SiwcFingerprintResult, SiwcGateState } from '@/types'
import { formatDateTime } from '@/utils/format'
import { isSiwcAccount } from '@/utils/siwcAccount'

const props = defineProps<{ account: Account }>()
const emit = defineEmits<{ 'show-gate': [account: Account] }>()
const { t } = useI18n()

const isSiwc = computed(() => isSiwcAccount(props.account))

const gate = computed<SiwcGateState | undefined>(() => {
  const value = props.account.extra?.siwc_gate
  return value && typeof value === 'object' ? (value as SiwcGateState) : undefined
})

// 指纹优先取 extra.siwc_fingerprint，其次取闸门对象里的快照
const fingerprint = computed<SiwcFingerprintResult | undefined>(() => {
  const direct = props.account.extra?.siwc_fingerprint
  if (direct && typeof direct === 'object') return direct as SiwcFingerprintResult
  return gate.value?.fingerprint
})

const managementAccountId = computed(() => {
  const value = props.account.extra?.siwc_management_account_id
  return typeof value === 'number' && value > 0 ? value : null
})

const nextCheckText = computed(() => {
  if (!gate.value?.next_check_at) return ''
  return formatDateTime(gate.value.next_check_at)
})

const fingerprintLabel = computed(() => {
  switch (fingerprint.value?.verdict) {
    case 'ok':
      return t('admin.accounts.siwcGate.fingerprintOk')
    case 'suspect':
      return t('admin.accounts.siwcGate.fingerprintSuspect')
    default:
      return t('admin.accounts.siwcGate.fingerprintError')
  }
})

const fingerprintClass = computed(() => {
  switch (fingerprint.value?.verdict) {
    case 'ok':
      return 'text-green-600 dark:text-green-400'
    case 'suspect':
      return 'text-red-600 dark:text-red-400'
    default:
      return 'text-gray-400 dark:text-gray-500'
  }
})

const fingerprintDotClass = computed(() => {
  switch (fingerprint.value?.verdict) {
    case 'ok':
      return 'bg-green-500'
    case 'suspect':
      return 'bg-red-500'
    default:
      return 'bg-gray-400'
  }
})

const fingerprintTitle = computed(() => {
  if (!fingerprint.value) return ''
  return t('admin.accounts.siwcGate.fingerprintTooltip', {
    astra: Math.round(fingerprint.value.astra_p50 ?? 0),
    luna: Math.round(fingerprint.value.luna_p50 ?? 0),
    time: fingerprint.value.at ? formatDateTime(fingerprint.value.at) : '-'
  })
})
</script>
