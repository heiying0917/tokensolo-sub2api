/**
 * SIWC（Sign in with ChatGPT）回调 URL 解析与校验。
 *
 * 授权完成后浏览器会跳到本机回调地址 http://127.0.0.1:1455/auth/callback?code=...&state=...&client_id=...，
 * 该页面通常打不开（本机没有监听），管理员把地址栏整条 URL 粘贴回来即可。
 * 这里只做形态校验，不改写输入；真正的 code 兑换由后端完成。
 */

/** 允许的回调 host（宽松匹配：允许 127.0.0.1 与 localhost）。 */
const ALLOWED_CALLBACK_HOSTS = ['127.0.0.1:1455', 'localhost:1455']
const CALLBACK_PATH = '/auth/callback'
/** SIWC 使用的 OAuth client_id 前缀。 */
export const SIWC_CLIENT_ID_PREFIX = 'oaiapp_'

export interface SiwcCallbackParams {
  code: string
  state: string
  clientId: string
  error: string
  errorDescription: string
}

export type SiwcCallbackInvalidReason =
  | 'empty'
  | 'malformed'
  | 'wrong_redirect'
  | 'duplicate_param'
  | 'upstream_error'
  | 'missing_code'
  | 'missing_client_id'
  | 'invalid_client_id'
  | 'missing_state'
  | 'state_mismatch'

export interface SiwcCallbackValidation {
  ok: boolean
  /** 校验失败原因；ok 为 true 时为空。 */
  reason?: SiwcCallbackInvalidReason
  /** 上游返回的 error / error_description（reason 为 upstream_error 时填充）。 */
  upstreamError?: string
  params?: SiwcCallbackParams
}

const TRACKED_PARAMS = ['code', 'state', 'client_id', 'error', 'error_description'] as const

/** 把裸 query（`code=...&state=...` 或以 `?` 开头）或整条 URL 统一解析为 URL 对象。 */
function toUrl(raw: string): { url: URL; bare: boolean } | null {
  const trimmed = raw.trim()
  if (!trimmed) return null
  if (/^https?:\/\//i.test(trimmed)) {
    try {
      return { url: new URL(trimmed), bare: false }
    } catch {
      return null
    }
  }
  // 裸 query：不校验 host/path，仅解析参数
  const query = trimmed.replace(/^\?/, '')
  if (!query.includes('=')) return null
  try {
    return { url: new URL(`http://127.0.0.1:1455${CALLBACK_PATH}?${query}`), bare: true }
  } catch {
    return null
  }
}

/** 解析回调 URL 或裸 query，拿不到的字段返回空串；解析失败全部为空串。 */
export function parseSiwcCallback(raw: string): SiwcCallbackParams {
  const empty: SiwcCallbackParams = { code: '', state: '', clientId: '', error: '', errorDescription: '' }
  const parsed = toUrl(raw)
  if (!parsed) return empty
  const params = parsed.url.searchParams
  const pick = (key: string) => (params.get(key) ?? '').trim()
  return {
    code: pick('code'),
    state: pick('state'),
    clientId: pick('client_id'),
    error: pick('error'),
    errorDescription: pick('error_description')
  }
}

/**
 * 校验回调 URL：
 * 1. 整条 URL 时 host/path 必须是 127.0.0.1:1455 或 localhost:1455 的 /auth/callback；裸 query 跳过该项；
 * 2. 每个关键参数最多出现一次；
 * 3. 带 error= 时直接视为上游错误；
 * 4. code 与 client_id 必须都有，client_id 以 oaiapp_ 开头；
 * 5. state 必须与会话一致。
 */
export function validateSiwcCallback(raw: string, expectedState: string): SiwcCallbackValidation {
  const trimmed = raw.trim()
  if (!trimmed) return { ok: false, reason: 'empty' }

  const parsed = toUrl(trimmed)
  if (!parsed) return { ok: false, reason: 'malformed' }
  const { url, bare } = parsed

  if (!bare) {
    const host = url.host.toLowerCase()
    const pathOk = url.pathname.replace(/\/+$/, '') === CALLBACK_PATH
    if (!ALLOWED_CALLBACK_HOSTS.includes(host) || !pathOk || url.hash) {
      return { ok: false, reason: 'wrong_redirect' }
    }
  }

  const params = url.searchParams
  for (const key of TRACKED_PARAMS) {
    if (params.getAll(key).length > 1) {
      return { ok: false, reason: 'duplicate_param' }
    }
  }

  const values = parseSiwcCallback(trimmed)
  if (values.error) {
    const upstreamError = values.errorDescription
      ? `${values.error}: ${values.errorDescription}`
      : values.error
    return { ok: false, reason: 'upstream_error', upstreamError, params: values }
  }
  if (!values.code) return { ok: false, reason: 'missing_code', params: values }
  if (!values.clientId) return { ok: false, reason: 'missing_client_id', params: values }
  if (!values.clientId.startsWith(SIWC_CLIENT_ID_PREFIX)) {
    return { ok: false, reason: 'invalid_client_id', params: values }
  }
  if (!values.state) return { ok: false, reason: 'missing_state', params: values }
  if (!expectedState || values.state !== expectedState) {
    return { ok: false, reason: 'state_mismatch', params: values }
  }
  return { ok: true, params: values }
}

/**
 * cpa 工具包产出的 SIWC 凭据 JSON 形态预检。
 * 支持：单个对象 / JSON 数组 / 每行一个 JSON 对象。
 * 判定条件：含 refresh_token 与 email，且 type === "chatgpt_siwc" 或 client_id 以 oaiapp_ 开头。
 */
export type SiwcJsonPrecheckReason = 'empty' | 'invalid_json' | 'not_siwc' | 'missing_fields'

export interface SiwcJsonPrecheck {
  ok: boolean
  /** 识别出的条目数。 */
  count: number
  reason?: SiwcJsonPrecheckReason
  /** 首个不合格条目的序号（从 1 开始）。 */
  index?: number
}

function isSiwcRecord(value: unknown): { siwc: boolean; complete: boolean } {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    return { siwc: false, complete: false }
  }
  const record = value as Record<string, unknown>
  const type = typeof record.type === 'string' ? record.type.trim().toLowerCase() : ''
  const clientId = typeof record.client_id === 'string' ? record.client_id.trim() : ''
  const siwc = type === 'chatgpt_siwc' || clientId.startsWith(SIWC_CLIENT_ID_PREFIX)
  const complete =
    typeof record.refresh_token === 'string' && record.refresh_token.trim() !== '' &&
    typeof record.email === 'string' && record.email.trim() !== ''
  return { siwc, complete }
}

function parseSiwcJsonEntries(content: string): unknown[] | null {
  const trimmed = content.trim()
  if (!trimmed) return []
  try {
    const parsed = JSON.parse(trimmed)
    return Array.isArray(parsed) ? parsed : [parsed]
  } catch {
    // 回退为每行一个 JSON 对象
    const lines = trimmed.split('\n').map((line) => line.trim()).filter(Boolean)
    const entries: unknown[] = []
    for (const line of lines) {
      try {
        entries.push(JSON.parse(line))
      } catch {
        return null
      }
    }
    return entries
  }
}

export function precheckSiwcJson(content: string): SiwcJsonPrecheck {
  const entries = parseSiwcJsonEntries(content)
  if (entries === null) return { ok: false, count: 0, reason: 'invalid_json' }
  if (entries.length === 0) return { ok: false, count: 0, reason: 'empty' }
  for (let i = 0; i < entries.length; i++) {
    const { siwc, complete } = isSiwcRecord(entries[i])
    if (!siwc) return { ok: false, count: entries.length, reason: 'not_siwc', index: i + 1 }
    if (!complete) return { ok: false, count: entries.length, reason: 'missing_fields', index: i + 1 }
  }
  return { ok: true, count: entries.length }
}
