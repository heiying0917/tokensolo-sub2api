<template>
  <BaseDialog :show="show" :title="t('admin.accounts.siwcAgents.title')" width="wide" @close="handleClose">
    <div class="space-y-4" data-testid="siwc-agents-modal">
      <div v-if="loading && !group" class="flex items-center justify-center py-8">
        <svg class="h-6 w-6 animate-spin text-gray-400" fill="none" viewBox="0 0 24 24">
          <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
          <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"></path>
        </svg>
      </div>

      <template v-else-if="group">
        <!-- ChatGPT 账号 -->
        <div class="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-gray-200 p-4 dark:border-dark-600">
          <div class="min-w-0">
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.siwcAgents.chatgptAccount') }}</p>
            <p class="mt-1 truncate text-sm font-medium text-gray-900 dark:text-gray-100">{{ group.email }}</p>
          </div>
          <div class="text-right">
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.siwcAgents.management') }}</p>
            <p class="mt-1 text-sm text-gray-900 dark:text-gray-100" data-testid="siwc-agents-management">
              <template v-if="group.management_account_id">#{{ group.management_account_id }} · {{ group.management_account_name }}</template>
              <span v-else class="text-amber-600 dark:text-amber-400">{{ t('admin.accounts.siwcAgents.managementMissing') }}</span>
            </p>
          </div>
        </div>
        <p
          v-if="group.paused"
          class="rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800 dark:border-amber-800/40 dark:bg-amber-900/20 dark:text-amber-200"
          data-testid="siwc-agents-paused"
        >
          {{ t('admin.accounts.siwcAgents.pausedHint') }}
        </p>
        <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.siwcAgents.hint') }}</p>

        <!-- Agent 列表 -->
        <div class="overflow-x-auto rounded-lg border border-gray-200 dark:border-dark-600">
          <table class="min-w-full text-sm">
            <thead class="bg-gray-50 text-xs text-gray-500 dark:bg-dark-700 dark:text-gray-400">
              <tr>
                <th class="px-3 py-2 text-left">{{ t('admin.accounts.siwcAgents.colAgent') }}</th>
                <th class="px-3 py-2 text-left">{{ t('admin.accounts.siwcAgents.colRole') }}</th>
                <th class="px-3 py-2 text-left">{{ t('admin.accounts.siwcAgents.colStatus') }}</th>
                <th class="px-3 py-2 text-left">{{ t('admin.accounts.siwcAgents.colLastUsed') }}</th>
                <th class="px-3 py-2 text-right">{{ t('admin.accounts.siwcAgents.colActions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
              <tr v-for="agent in group.agents" :key="agent.id" :data-testid="`siwc-agent-row-${agent.id}`">
                <td class="px-3 py-2">
                  <div class="font-medium text-gray-900 dark:text-gray-100">
                    #{{ agent.id }} · {{ agent.name }}
                    <span
                      v-if="agent.id === group.primary_account_id"
                      class="badge badge-gray ml-1 text-[10px]"
                      :data-testid="`siwc-agent-primary-${agent.id}`"
                    >{{ t('admin.accounts.siwcAgents.primary') }}</span>
                  </div>
                  <div v-if="agent.agent_name" class="text-xs text-gray-500 dark:text-gray-400">{{ agent.agent_name }}</div>
                </td>
                <td class="px-3 py-2">
                  <span
                    :class="['badge text-xs', isActive(agent) ? 'badge-success' : 'badge-gray']"
                    :data-testid="`siwc-agent-role-${agent.id}`"
                  >
                    {{ isActive(agent) ? t('admin.accounts.siwcAgents.roleActive') : t('admin.accounts.siwcAgents.roleStandby') }}
                  </span>
                </td>
                <td class="px-3 py-2">
                  <span v-if="agent.usable" class="text-xs text-emerald-600 dark:text-emerald-400">{{ t('admin.accounts.siwcAgents.statusOk') }}</span>
                  <span v-else class="text-xs text-red-600 dark:text-red-400" :title="agent.error_message || ''">
                    {{ t('admin.accounts.siwcAgents.statusBroken') }}
                  </span>
                </td>
                <td class="px-3 py-2 text-xs text-gray-500 dark:text-gray-400">{{ agent.last_used_at ? formatRelativeTime(agent.last_used_at) : '-' }}</td>
                <td class="whitespace-nowrap px-3 py-2 text-right">
                  <button
                    v-if="!isActive(agent) && agent.usable"
                    type="button"
                    class="btn btn-secondary btn-sm"
                    :disabled="busy"
                    :data-testid="`siwc-agent-activate-${agent.id}`"
                    @click="handleActivate(agent)"
                  >
                    {{ t('admin.accounts.siwcAgents.activate') }}
                  </button>
                  <button
                    type="button"
                    class="btn btn-secondary btn-sm ml-2"
                    :disabled="busy"
                    :data-testid="`siwc-agent-reauth-${agent.id}`"
                    @click="startAuth('reauth', agent.id)"
                  >
                    {{ t('admin.accounts.siwcAgents.reauth') }}
                  </button>
                  <button
                    v-if="group.agents.length > 1"
                    type="button"
                    class="btn btn-sm ml-2 text-red-600 hover:bg-red-50 dark:text-red-400 dark:hover:bg-red-900/20"
                    :disabled="busy"
                    :data-testid="`siwc-agent-delete-${agent.id}`"
                    @click="deleteTarget = agent"
                  >
                    {{ t('common.delete') }}
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <!-- 授权面板：添加 Agent / 重新授权，流程与首次添加 SIWC 账号一致 -->
        <div v-if="authMode" class="space-y-3 rounded-lg border border-blue-200 bg-blue-50 p-4 dark:border-blue-800/50 dark:bg-blue-900/20" data-testid="siwc-agents-auth">
          <p class="text-sm font-medium text-blue-900 dark:text-blue-200">
            {{ authMode === 'add'
              ? t('admin.accounts.siwcAgents.addTitle')
              : t('admin.accounts.siwcAgents.reauthTitle', { id: authTargetId }) }}
          </p>
          <p class="text-xs text-blue-700 dark:text-blue-300">{{ t('admin.accounts.siwcAgents.sameEmailHint', { email: group.email }) }}</p>

          <div v-if="!siwc.authUrl.value || siwc.isExpired.value" class="flex items-center gap-2">
            <input
              v-if="authMode === 'add'"
              v-model="agentName"
              type="text"
              class="input flex-1 text-sm"
              :placeholder="t('admin.accounts.oauth.openai.siwcAgentNamePlaceholder')"
            />
            <button type="button" class="btn btn-primary btn-sm" :disabled="siwc.loading.value" data-testid="siwc-agents-generate" @click="generate">
              <svg v-if="siwc.loading.value" class="-ml-1 mr-2 inline h-4 w-4 animate-spin" fill="none" viewBox="0 0 24 24" data-testid="siwc-agents-generate-spinner">
                <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
                <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"></path>
              </svg>
              {{ siwc.isExpired.value ? t('admin.accounts.oauth.openai.siwcRegenerate') : t('admin.accounts.oauth.openai.siwcGenerateUrl') }}
            </button>
          </div>

          <template v-else>
            <div class="flex items-center gap-2">
              <input :value="siwc.authUrl.value" readonly class="input flex-1 font-mono text-xs" />
              <button type="button" class="btn btn-secondary btn-sm" @click="copyToClipboard(siwc.authUrl.value)">
                {{ t('admin.accounts.oauth.openai.siwcCopyUrl') }}
              </button>
              <a :href="siwc.authUrl.value" target="_blank" rel="noopener noreferrer" class="btn btn-secondary btn-sm">
                {{ t('admin.accounts.oauth.openai.siwcOpenInNewWindow') }}
              </a>
            </div>
            <p class="text-xs text-blue-700 dark:text-blue-300">
              {{ t('admin.accounts.oauth.openai.siwcExpiresIn', { time: remainingText }) }}
            </p>
            <textarea
              v-model="callbackUrl"
              rows="2"
              class="input w-full font-mono text-xs"
              :placeholder="t('admin.accounts.oauth.openai.siwcCallbackPlaceholder')"
              data-testid="siwc-agents-callback"
            ></textarea>
            <p v-if="callbackUrl.trim()" :class="['text-xs', callbackCheck.ok ? 'text-emerald-600 dark:text-emerald-400' : 'text-red-600 dark:text-red-400']">
              {{ callbackMessage }}
            </p>
            <p v-else class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.oauth.openai.siwcCallbackHint') }}</p>
          </template>

          <div class="flex justify-end gap-2">
            <button type="button" class="btn btn-secondary btn-sm" :disabled="siwc.loading.value" @click="cancelAuth">{{ t('common.cancel') }}</button>
            <button
              type="button"
              class="btn btn-primary btn-sm"
              :disabled="!callbackCheck.ok || siwc.loading.value || siwc.isExpired.value"
              data-testid="siwc-agents-submit"
              @click="submitAuth"
            >
              <svg v-if="siwc.loading.value" class="-ml-1 mr-2 inline h-4 w-4 animate-spin" fill="none" viewBox="0 0 24 24" data-testid="siwc-agents-submit-spinner">
                <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
                <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"></path>
              </svg>
              {{ siwc.loading.value
                ? t('admin.accounts.siwcAgents.submitting')
                : authMode === 'add' ? t('admin.accounts.siwcAgents.addSubmit') : t('admin.accounts.siwcAgents.reauthSubmit') }}
            </button>
          </div>
        </div>
      </template>
    </div>

    <template #footer>
      <div class="flex w-full items-center justify-between">
        <button
          type="button"
          class="btn btn-secondary"
          :disabled="!group || busy || authMode !== null"
          data-testid="siwc-agents-add"
          @click="startAuth('add', null)"
        >
          + {{ t('admin.accounts.siwcAgents.add') }}
        </button>
        <button type="button" class="btn btn-secondary" @click="handleClose">{{ t('common.close') }}</button>
      </div>
    </template>
  </BaseDialog>

  <ConfirmDialog
    :show="deleteTarget !== null"
    :title="t('admin.accounts.siwcAgents.deleteTitle')"
    :message="t('admin.accounts.siwcAgents.deleteMessage', { name: deleteTarget?.name || '' })"
    danger
    @confirm="confirmDelete"
    @cancel="deleteTarget = null"
  />
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import type { Account, SiwcAgentGroup, SiwcAgentView } from '@/types'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import { useOpenAISiwcOAuth } from '@/composables/useOpenAISiwcOAuth'
import { useClipboard } from '@/composables/useClipboard'
import { validateSiwcCallback } from '@/utils/siwcCallback'
import { formatRelativeTime } from '@/utils/format'
import { extractI18nErrorMessage } from '@/utils/apiError'

const props = defineProps<{
  show: boolean
  account: Account | null
  // 打开时直接进入该 Agent 的重新授权（账号菜单「重新授权」入口）
  reauthAccountId?: number | null
}>()

const emit = defineEmits<{
  close: []
  changed: []
}>()

const { t } = useI18n()
const appStore = useAppStore()
const siwc = useOpenAISiwcOAuth()
const { copyToClipboard } = useClipboard()

const group = ref<SiwcAgentGroup | null>(null)
const loading = ref(false)
const busy = ref(false)
const authMode = ref<'add' | 'reauth' | null>(null)
const authTargetId = ref<number | null>(null)
const agentName = ref('')
const callbackUrl = ref('')
const deleteTarget = ref<SiwcAgentView | null>(null)
// 弹窗锚点账号：加载列表、添加 Agent 的继承来源；锚点被删除时换到剩余的第一个 Agent
const anchorId = ref<number | null>(null)

const isActive = (agent: SiwcAgentView) => agent.id === group.value?.active_account_id

const callbackCheck = computed(() => validateSiwcCallback(callbackUrl.value, siwc.oauthState.value))
const callbackMessage = computed(() => {
  const result = callbackCheck.value
  if (result.ok) return t('admin.accounts.oauth.openai.siwcCallbackValid')
  if (result.reason === 'upstream_error') {
    return t('admin.accounts.oauth.openai.siwcCallbackInvalid.upstream_error', { error: result.upstreamError || '' })
  }
  return t(`admin.accounts.oauth.openai.siwcCallbackInvalid.${result.reason}`)
})
const remainingText = computed(() => {
  const total = Math.max(0, siwc.remainingSeconds.value)
  const m = Math.floor(total / 60)
  const s = total % 60
  return `${m}:${String(s).padStart(2, '0')}`
})

const showError = (err: unknown, fallbackKey: string) => {
  appStore.showError(extractI18nErrorMessage(err, t, 'admin.accounts.oauth.openai.errors', t(fallbackKey)))
}

const load = async () => {
  if (!anchorId.value) return
  loading.value = true
  try {
    group.value = await adminAPI.accounts.getSiwcAgents(anchorId.value)
  } catch (err) {
    showError(err, 'admin.accounts.siwcAgents.loadFailed')
  } finally {
    loading.value = false
  }
}

const resetAuth = () => {
  authMode.value = null
  authTargetId.value = null
  agentName.value = ''
  callbackUrl.value = ''
  siwc.resetState()
}

watch(
  () => [props.show, props.account?.id] as const,
  async ([show]) => {
    if (!show) return
    group.value = null
    anchorId.value = props.account?.id ?? null
    resetAuth()
    await load()
    if (props.reauthAccountId) {
      startAuth('reauth', props.reauthAccountId)
    }
  },
  { immediate: true }
)

const startAuth = (mode: 'add' | 'reauth', targetId: number | null) => {
  resetAuth()
  authMode.value = mode
  authTargetId.value = targetId
}

const cancelAuth = () => resetAuth()

const generate = async () => {
  const source = props.account
  await siwc.generateAuthUrl({
    proxy_id: source?.proxy_id ?? undefined,
    agent_name: authMode.value === 'add' ? agentName.value : undefined
  })
}

const submitAuth = async () => {
  if (!anchorId.value || !callbackCheck.value.ok) return
  const url = callbackUrl.value.trim()
  let ok = false
  if (authMode.value === 'add') {
    // 继承来源取当前打开的 Agent：分组、代理、优先级、模型白名单、管理凭据都随之复制
    ok = !!(await siwc.createFromCallback({ callback_url: url, inherit_from_account_id: anchorId.value }))
    if (ok) appStore.showSuccess(t('admin.accounts.siwcAgents.addSuccess'))
  } else if (authTargetId.value) {
    ok = !!(await siwc.reauthFromCallback(authTargetId.value, url))
    if (ok) appStore.showSuccess(t('admin.accounts.siwcAgents.reauthSuccess'))
  }
  if (!ok) return
  resetAuth()
  emit('changed')
  await load()
}

const handleActivate = async (agent: SiwcAgentView) => {
  busy.value = true
  try {
    group.value = await adminAPI.accounts.activateSiwcAgent(agent.id)
    appStore.showSuccess(t('admin.accounts.siwcAgents.activateSuccess', { name: agent.name }))
    emit('changed')
  } catch (err) {
    showError(err, 'admin.accounts.siwcAgents.activateFailed')
  } finally {
    busy.value = false
  }
}

const confirmDelete = async () => {
  const target = deleteTarget.value
  deleteTarget.value = null
  if (!target) return
  busy.value = true
  try {
    // 后端负责：删主行时下一个 Agent 升为主行、删活跃 Agent 时切到下一个可用 Agent
    group.value = await adminAPI.accounts.removeSiwcAgent(target.id)
    anchorId.value = group.value.primary_account_id || group.value.agents[0]?.id || null
    appStore.showSuccess(t('admin.accounts.siwcAgents.deleteSuccess'))
    emit('changed')
  } catch (err) {
    showError(err, 'admin.accounts.siwcAgents.deleteFailed')
  } finally {
    busy.value = false
  }
}

const handleClose = () => {
  resetAuth()
  emit('close')
}
</script>
