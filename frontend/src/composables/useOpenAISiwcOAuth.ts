import { computed, onScopeDispose, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import { extractI18nErrorMessage } from '@/utils/apiError'
import type { Account, OpenAISiwcAuthUrlRequest, OpenAISiwcCreateRequest } from '@/types'

/**
 * SIWC（Sign in with ChatGPT）授权流程状态。
 * 与 useOpenAIOAuth 同形：生成链接 → 管理员在浏览器完成授权 → 粘贴回调 URL → 一步建号。
 *
 * phase:
 *   idle        尚未生成链接
 *   generating  正在请求授权链接
 *   waiting     链接已生成，等待粘贴回调
 *   submitting  正在用回调 URL 建号
 *   expired     会话已过期，需要重新生成
 */
export type SiwcOAuthPhase = 'idle' | 'generating' | 'waiting' | 'submitting' | 'expired'

export function useOpenAISiwcOAuth() {
  const appStore = useAppStore()
  const { t } = useI18n()

  const authUrl = ref('')
  const sessionId = ref('')
  const oauthState = ref('')
  // 会话过期时间（毫秒时间戳），0 表示无会话
  const expiresAt = ref(0)
  const phase = ref<SiwcOAuthPhase>('idle')
  const loading = ref(false)
  const error = ref('')
  // 倒计时用的"当前时间"，每秒刷新一次
  const now = ref(Date.now())
  let ticker: ReturnType<typeof setInterval> | null = null

  const stopTicker = () => {
    if (ticker) {
      clearInterval(ticker)
      ticker = null
    }
  }

  const startTicker = () => {
    stopTicker()
    now.value = Date.now()
    ticker = setInterval(() => {
      now.value = Date.now()
      if (expiresAt.value && now.value >= expiresAt.value && phase.value === 'waiting') {
        phase.value = 'expired'
        stopTicker()
      }
    }, 1000)
  }

  onScopeDispose(stopTicker)

  /** 剩余秒数（向下取整，不小于 0）。 */
  const remainingSeconds = computed(() => {
    if (!expiresAt.value) return 0
    return Math.max(0, Math.floor((expiresAt.value - now.value) / 1000))
  })

  const isExpired = computed(() => phase.value === 'expired')

  const resetState = () => {
    stopTicker()
    authUrl.value = ''
    sessionId.value = ''
    oauthState.value = ''
    expiresAt.value = 0
    phase.value = 'idle'
    loading.value = false
    error.value = ''
  }

  /** 生成授权链接。 */
  const generateAuthUrl = async (input: OpenAISiwcAuthUrlRequest = {}): Promise<boolean> => {
    resetState()
    loading.value = true
    phase.value = 'generating'
    try {
      const payload: OpenAISiwcAuthUrlRequest = {}
      if (input.proxy_id) payload.proxy_id = input.proxy_id
      if (input.redirect_uri) payload.redirect_uri = input.redirect_uri
      if (input.agent_name && input.agent_name.trim()) payload.agent_name = input.agent_name.trim()

      const response = await adminAPI.accounts.generateSiwcAuthUrl(payload)
      authUrl.value = response.auth_url
      sessionId.value = response.session_id
      oauthState.value = response.state || ''
      if (!oauthState.value) {
        // 兜底：从链接里解析 state
        try {
          oauthState.value = new URL(response.auth_url).searchParams.get('state') || ''
        } catch {
          oauthState.value = ''
        }
      }
      const ttlSeconds = Number(response.expires_in) > 0 ? Number(response.expires_in) : 1800
      expiresAt.value = Date.now() + ttlSeconds * 1000
      phase.value = 'waiting'
      startTicker()
      return true
    } catch (err: unknown) {
      error.value = extractI18nErrorMessage(
        err,
        t,
        'admin.accounts.oauth.openai.errors',
        t('admin.accounts.oauth.openai.siwcFailedToGenerateUrl')
      )
      phase.value = 'idle'
      appStore.showError(error.value)
      return false
    } finally {
      loading.value = false
    }
  }

  /** 用回调 URL 一步建号；成功返回 Account，失败返回 null 并把错误写入 error。 */
  const createFromCallback = async (
    payload: Omit<OpenAISiwcCreateRequest, 'session_id'> & { session_id?: string }
  ): Promise<Account | null> => {
    const currentSessionId = payload.session_id || sessionId.value
    if (!currentSessionId) {
      error.value = t('admin.accounts.oauth.openai.siwcSessionMissing')
      return null
    }
    if (phase.value === 'expired') {
      error.value = t('admin.accounts.oauth.openai.siwcSessionExpired')
      return null
    }

    loading.value = true
    error.value = ''
    const previousPhase = phase.value
    phase.value = 'submitting'
    try {
      const account = await adminAPI.accounts.createOpenAISiwc({
        ...payload,
        session_id: currentSessionId
      })
      stopTicker()
      phase.value = 'idle'
      return account
    } catch (err: unknown) {
      error.value = extractI18nErrorMessage(
        err,
        t,
        'admin.accounts.oauth.openai.errors',
        t('admin.accounts.oauth.openai.siwcCreateFailed')
      )
      phase.value = previousPhase
      appStore.showError(error.value)
      return null
    } finally {
      loading.value = false
    }
  }

  /** 用回调 URL 重新授权已有 Agent（替换凭据，保留角色与设置）；失败返回 null。 */
  const reauthFromCallback = async (accountId: number, callbackUrl: string): Promise<Account | null> => {
    if (!sessionId.value) {
      error.value = t('admin.accounts.oauth.openai.siwcSessionMissing')
      return null
    }
    if (phase.value === 'expired') {
      error.value = t('admin.accounts.oauth.openai.siwcSessionExpired')
      return null
    }
    loading.value = true
    error.value = ''
    const previousPhase = phase.value
    phase.value = 'submitting'
    try {
      const account = await adminAPI.accounts.reauthSiwcFromOAuth(accountId, {
        session_id: sessionId.value,
        callback_url: callbackUrl
      })
      stopTicker()
      phase.value = 'idle'
      return account
    } catch (err: unknown) {
      error.value = extractI18nErrorMessage(
        err,
        t,
        'admin.accounts.oauth.openai.errors',
        t('admin.accounts.siwcAgents.reauthFailed')
      )
      phase.value = previousPhase
      appStore.showError(error.value)
      return null
    } finally {
      loading.value = false
    }
  }

  return {
    reauthFromCallback,
    authUrl,
    sessionId,
    oauthState,
    expiresAt,
    remainingSeconds,
    isExpired,
    phase,
    loading,
    error,
    resetState,
    generateAuthUrl,
    createFromCallback
  }
}
