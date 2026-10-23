<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.siwcGate.title')"
    width="wide"
    @close="handleClose"
  >
    <div class="space-y-4">
      <div v-if="loading" class="flex items-center justify-center py-8">
        <svg class="h-6 w-6 animate-spin text-gray-400" fill="none" viewBox="0 0 24 24">
          <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
          <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path>
        </svg>
      </div>

      <template v-else>
        <!-- 账号 + 当前状态 -->
        <div class="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-gray-200 p-4 dark:border-dark-600">
          <div>
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.tempUnschedulable.accountName') }}</p>
            <p class="mt-1 text-sm font-medium text-gray-900 dark:text-gray-100">{{ account?.name || '-' }}</p>
          </div>
          <div class="text-right">
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.siwcGate.stateLabel') }}</p>
            <span :class="['badge mt-1 text-xs', stateBadgeClass]" data-testid="siwc-gate-state">{{ stateText }}</span>
          </div>
        </div>

        <div v-if="gate" class="grid grid-cols-1 gap-3 sm:grid-cols-3">
          <div class="rounded-lg border border-gray-200 p-3 dark:border-dark-600">
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.siwcGate.blockedAt') }}</p>
            <p class="mt-1 text-sm font-medium text-gray-900 dark:text-gray-100">{{ fmt(gate.blocked_at) }}</p>
          </div>
          <div class="rounded-lg border border-gray-200 p-3 dark:border-dark-600">
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.siwcGate.lastCheckAt') }}</p>
            <p class="mt-1 text-sm font-medium text-gray-900 dark:text-gray-100">{{ fmt(gate.last_check_at) }}</p>
          </div>
          <div class="rounded-lg border border-gray-200 p-3 dark:border-dark-600">
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.siwcGate.nextCheckAt') }}</p>
            <p class="mt-1 text-sm font-medium text-gray-900 dark:text-gray-100">{{ fmt(gate.next_check_at) }}</p>
          </div>
          <div class="rounded-lg border border-gray-200 p-3 dark:border-dark-600">
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.siwcGate.consecutive') }}</p>
            <p class="mt-1 text-sm font-medium text-gray-900 dark:text-gray-100">{{ gate.consecutive ?? '-' }}</p>
          </div>
          <div class="rounded-lg border border-gray-200 p-3 dark:border-dark-600">
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.siwcGate.code') }}</p>
            <p class="mt-1 break-all font-mono text-sm text-gray-900 dark:text-gray-100">{{ gate.code || '-' }}</p>
          </div>
          <div class="rounded-lg border border-gray-200 p-3 dark:border-dark-600">
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.siwcGate.requestId') }}</p>
            <p class="mt-1 break-all font-mono text-xs text-gray-900 dark:text-gray-100">{{ gate.request_id || '-' }}</p>
          </div>
          <div v-if="gate.cleared_at" class="rounded-lg border border-gray-200 p-3 dark:border-dark-600 sm:col-span-3">
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.siwcGate.clearedAt') }}</p>
            <p class="mt-1 text-sm font-medium text-gray-900 dark:text-gray-100">{{ fmt(gate.cleared_at) }}</p>
          </div>
        </div>

        <!-- 最近 5 次复检 -->
        <div class="rounded-lg border border-gray-200 dark:border-dark-600">
          <div class="border-b border-gray-200 px-3 py-2 text-xs font-semibold text-gray-600 dark:border-dark-600 dark:text-gray-300">
            {{ t('admin.accounts.siwcGate.recentChecks') }}
          </div>
          <table v-if="recentChecks.length" class="w-full text-xs" data-testid="siwc-gate-checks">
            <thead class="text-left text-gray-500 dark:text-gray-400">
              <tr>
                <th class="px-3 py-1.5 font-medium">{{ t('admin.accounts.siwcGate.checkAt') }}</th>
                <th class="px-3 py-1.5 font-medium">{{ t('admin.accounts.siwcGate.checkStatus') }}</th>
                <th class="px-3 py-1.5 font-medium">{{ t('admin.accounts.siwcGate.checkCode') }}</th>
                <th class="px-3 py-1.5 font-medium">{{ t('admin.accounts.siwcGate.checkRequestId') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
              <tr v-for="(check, index) in recentChecks" :key="`${check.at}-${index}`">
                <td class="px-3 py-1.5 text-gray-700 dark:text-gray-200">{{ fmt(check.at) }}</td>
                <td class="px-3 py-1.5">
                  <span :class="['badge text-[11px]', checkStatusClass(check.status)]">{{ check.status }}</span>
                </td>
                <td class="px-3 py-1.5 font-mono text-gray-700 dark:text-gray-200">{{ check.code || '-' }}</td>
                <td class="max-w-[220px] truncate px-3 py-1.5 font-mono text-gray-500 dark:text-gray-400" :title="check.request_id">{{ check.request_id || '-' }}</td>
              </tr>
            </tbody>
          </table>
          <p v-else class="px-3 py-3 text-xs text-gray-500 dark:text-gray-400">
            {{ t('admin.accounts.siwcGate.noChecks') }}
          </p>
        </div>

        <!-- 指纹明细 -->
        <div class="rounded-lg border border-gray-200 dark:border-dark-600" data-testid="siwc-gate-fingerprint">
          <div class="flex items-center justify-between border-b border-gray-200 px-3 py-2 dark:border-dark-600">
            <span class="text-xs font-semibold text-gray-600 dark:text-gray-300">{{ t('admin.accounts.siwcGate.fingerprintTitle') }}</span>
            <span v-if="fingerprint" :class="['badge text-[11px]', fingerprintBadgeClass]">{{ fingerprintVerdictText }}</span>
          </div>
          <div v-if="fingerprint" class="grid grid-cols-1 gap-3 p-3 text-xs sm:grid-cols-3">
            <div>
              <p class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.siwcGate.fingerprintAt') }}</p>
              <p class="mt-1 font-medium text-gray-900 dark:text-gray-100">{{ fmt(fingerprint.at) }}</p>
            </div>
            <div>
              <p class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.siwcGate.fingerprintAstra') }}</p>
              <p class="mt-1 font-medium text-gray-900 dark:text-gray-100">{{ Math.round(fingerprint.astra_p50 ?? 0) }} ms</p>
              <p class="mt-0.5 font-mono text-[11px] text-gray-500 dark:text-gray-400">{{ sampleText(fingerprint.astra) }}</p>
            </div>
            <div>
              <p class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.siwcGate.fingerprintLuna') }}</p>
              <p class="mt-1 font-medium text-gray-900 dark:text-gray-100">{{ Math.round(fingerprint.luna_p50 ?? 0) }} ms</p>
              <p class="mt-0.5 font-mono text-[11px] text-gray-500 dark:text-gray-400">{{ sampleText(fingerprint.luna) }}</p>
            </div>
          </div>
          <p v-else class="px-3 py-3 text-xs text-gray-500 dark:text-gray-400">
            {{ t('admin.accounts.siwcGate.noFingerprint') }}
          </p>
        </div>

        <div
          v-if="actionError"
          class="rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-600 dark:border-red-700 dark:bg-red-900/30 dark:text-red-400"
          data-testid="siwc-gate-error"
        >
          {{ actionError }}
        </div>
      </template>
    </div>

    <template #footer>
      <div class="flex flex-wrap justify-end gap-3">
        <button type="button" class="btn btn-secondary" @click="handleClose">
          {{ t('common.close') }}
        </button>
        <button
          type="button"
          class="btn btn-secondary"
          :disabled="busy || loading || cooldownRemaining > 0"
          data-testid="siwc-gate-recheck"
          @click="handleRecheck"
        >
          <Icon name="refresh" size="sm" class="mr-1" />
          {{ cooldownRemaining > 0 ? t('admin.accounts.siwcGate.recheckCooldown', { seconds: cooldownRemaining }) : t('admin.accounts.siwcGate.recheck') }}
        </button>
        <button
          type="button"
          class="btn btn-secondary"
          :disabled="busy || loading"
          data-testid="siwc-gate-fingerprint-run"
          @click="handleFingerprint"
        >
          <Icon name="beaker" size="sm" class="mr-1" />
          {{ t('admin.accounts.siwcGate.fingerprintRun') }}
        </button>
        <button
          type="button"
          class="btn btn-primary"
          :disabled="busy || loading || gate?.state !== 'restorable'"
          data-testid="siwc-gate-restore"
          @click="showRestoreConfirm = true"
        >
          <svg v-if="action === 'restore'" class="-ml-1 mr-2 h-4 w-4 animate-spin" fill="none" viewBox="0 0 24 24">
            <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
            <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path>
          </svg>
          {{ t('admin.accounts.siwcGate.restore') }}
        </button>
      </div>
    </template>
  </BaseDialog>

  <ConfirmDialog
    :show="showRestoreConfirm"
    :title="t('admin.accounts.siwcGate.restoreConfirmTitle')"
    :message="t('admin.accounts.siwcGate.restoreConfirmMessage', { name: account?.name || '' })"
    :confirm-text="t('admin.accounts.siwcGate.restore')"
    :cancel-text="t('common.cancel')"
    @confirm="handleRestore"
    @cancel="showRestoreConfirm = false"
  />
</template>

<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import type { Account, SiwcFingerprintResult, SiwcGateState } from '@/types'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import { formatDateTime } from '@/utils/format'
import { extractI18nErrorMessage } from '@/utils/apiError'

type GateAction = 'recheck' | 'fingerprint' | 'restore' | null

const props = defineProps<{
  show: boolean
  account: Account | null
}>()

const emit = defineEmits<{
  close: []
  /** 闸门 / 指纹有更新：父组件据此回填列表里的 extra。 */
  updated: [accountId: number, gate: SiwcGateState | null, fingerprint: SiwcFingerprintResult | null]
  /** 恢复调度成功：账号可调度状态已变，父组件需要重新拉列表。 */
  restored: [accountId: number]
}>()

const { t } = useI18n()
const appStore = useAppStore()
const ERROR_NAMESPACE = 'admin.accounts.siwcGate.errors'

const loading = ref(false)
const action = ref<GateAction>(null)
const busy = computed(() => action.value !== null)
const gate = ref<SiwcGateState | null>(null)
const standaloneFingerprint = ref<SiwcFingerprintResult | null>(null)
const actionError = ref('')
const showRestoreConfirm = ref(false)
// 手动复检冷却倒计时（秒）
const cooldownRemaining = ref(0)
let cooldownTimer: ReturnType<typeof setInterval> | null = null
let requestVersion = 0

const fingerprint = computed(() => standaloneFingerprint.value || gate.value?.fingerprint || null)
const recentChecks = computed(() => (gate.value?.checks || []).slice(-5).reverse())

const stateText = computed(() => {
  switch (gate.value?.state) {
    case 'blocked':
      return t('admin.accounts.siwcGate.stateBlocked')
    case 'restorable':
      return t('admin.accounts.siwcGate.stateRestorable')
    case 'clear':
      return t('admin.accounts.siwcGate.stateClear')
    default:
      return '-'
  }
})

const stateBadgeClass = computed(() => {
  switch (gate.value?.state) {
    case 'blocked':
      return 'badge-danger'
    case 'restorable':
      return 'badge-warning'
    case 'clear':
      return 'badge-success'
    default:
      return 'badge-gray'
  }
})

const fingerprintVerdictText = computed(() => {
  switch (fingerprint.value?.verdict) {
    case 'ok':
      return t('admin.accounts.siwcGate.fingerprintOk')
    case 'suspect':
      return t('admin.accounts.siwcGate.fingerprintSuspect')
    default:
      return t('admin.accounts.siwcGate.fingerprintError')
  }
})

const fingerprintBadgeClass = computed(() => {
  switch (fingerprint.value?.verdict) {
    case 'ok':
      return 'badge-success'
    case 'suspect':
      return 'badge-danger'
    default:
      return 'badge-gray'
  }
})

const fmt = (value?: string) => (value ? formatDateTime(value) : '-')
const sampleText = (samples?: number[]) =>
  Array.isArray(samples) && samples.length ? samples.map((n) => Math.round(n)).join(' / ') : '-'
const checkStatusClass = (status: number) =>
  status >= 200 && status < 300 ? 'badge-success' : status === 0 ? 'badge-gray' : 'badge-danger'

const stopCooldown = () => {
  if (cooldownTimer) {
    clearInterval(cooldownTimer)
    cooldownTimer = null
  }
}

const startCooldown = (seconds: number) => {
  stopCooldown()
  cooldownRemaining.value = Math.max(0, Math.ceil(seconds))
  if (cooldownRemaining.value <= 0) return
  cooldownTimer = setInterval(() => {
    cooldownRemaining.value = Math.max(0, cooldownRemaining.value - 1)
    if (cooldownRemaining.value <= 0) stopCooldown()
  }, 1000)
}

const applyGate = (next: SiwcGateState) => {
  gate.value = next
  if (next.fingerprint) standaloneFingerprint.value = next.fingerprint
  startCooldown(next.manual_cooldown_remaining ?? 0)
}

const loadGate = async () => {
  if (!props.account) return
  const version = ++requestVersion
  gate.value = null
  standaloneFingerprint.value = null
  actionError.value = ''
  loading.value = true
  try {
    const result = await adminAPI.accounts.getSiwcGate(props.account.id)
    if (version === requestVersion) applyGate(result)
  } catch (error: unknown) {
    if (version !== requestVersion) return
    appStore.showError(extractI18nErrorMessage(error, t, ERROR_NAMESPACE, t('admin.accounts.siwcGate.loadFailed')))
    gate.value = null
  } finally {
    if (version === requestVersion) loading.value = false
  }
}

const runAction = async (kind: Exclude<GateAction, null>, run: (id: number) => Promise<void>, successKey: string) => {
  if (!props.account || busy.value) return
  const accountId = props.account.id
  const version = requestVersion
  action.value = kind
  actionError.value = ''
  try {
    await run(accountId)
    if (version !== requestVersion) return
    appStore.showSuccess(t(successKey))
  } catch (error: unknown) {
    if (version !== requestVersion) return
    actionError.value = extractI18nErrorMessage(error, t, ERROR_NAMESPACE, t('admin.accounts.siwcGate.actionFailed'))
    appStore.showError(actionError.value)
  } finally {
    if (version === requestVersion) action.value = null
  }
}

const handleRecheck = () =>
  runAction('recheck', async (id) => {
    const result = await adminAPI.accounts.recheckSiwcGate(id)
    applyGate(result)
    emit('updated', id, result, fingerprint.value)
  }, 'admin.accounts.siwcGate.recheckSuccess')

const handleFingerprint = () =>
  runAction('fingerprint', async (id) => {
    const result = await adminAPI.accounts.runSiwcFingerprint(id)
    standaloneFingerprint.value = result
    emit('updated', id, gate.value, result)
  }, 'admin.accounts.siwcGate.fingerprintSuccess')

const handleRestore = async () => {
  showRestoreConfirm.value = false
  await runAction('restore', async (id) => {
    const result = await adminAPI.accounts.restoreSiwcGate(id)
    applyGate(result)
    emit('updated', id, result, fingerprint.value)
    emit('restored', id)
  }, 'admin.accounts.siwcGate.restoreSuccess')
}

const handleClose = () => {
  showRestoreConfirm.value = false
  emit('close')
}

watch(
  () => [props.show, props.account?.id] as const,
  ([visible], _, onCleanup) => {
    onCleanup(() => { requestVersion++ })
    if (visible && props.account) {
      loadGate()
      return
    }
    gate.value = null
    standaloneFingerprint.value = null
    actionError.value = ''
    stopCooldown()
    cooldownRemaining.value = 0
  }
)

onUnmounted(stopCooldown)
</script>
