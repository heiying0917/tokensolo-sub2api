import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

const source = readFileSync(
  resolve(process.cwd(), 'src/components/account/CreateAccountModal.vue'),
  'utf8'
)

describe('CreateAccountModal SIWC wiring', () => {
  it('enables the SIWC and SIWC JSON options for the OpenAI platform only', () => {
    expect(source).toContain(':show-siwc-option="form.platform === \'openai\'"')
    expect(source).toContain(':show-siwc-json-option="form.platform === \'openai\'"')
    expect(source).toContain(':siwc-auth-url="siwcAuthUrl"')
    expect(source).toContain(':siwc-state="siwcState"')
    expect(source).toContain(':siwc-remaining-seconds="siwcRemainingSeconds"')
    expect(source).toContain(':siwc-phase="siwcPhase"')
    expect(source).toContain(':siwc-import-result="siwcImportResult"')
  })

  it('routes the flow events to dedicated handlers', () => {
    expect(source).toContain('@generate-siwc-url="handleOpenAIGenerateSiwcUrl"')
    expect(source).toContain('@create-siwc-account="handleOpenAICreateSiwc"')
    expect(source).toContain('@import-siwc-json="handleOpenAIImportSiwcJson"')
  })

  it('creates SIWC accounts through the composable with the shared codex fields', () => {
    expect(source).toContain("import { useOpenAISiwcOAuth } from '@/composables/useOpenAISiwcOAuth'")
    expect(source).toContain('const openaiSiwcOAuth = useOpenAISiwcOAuth()')
    const handler = source.slice(source.indexOf('const handleOpenAICreateSiwc'), source.indexOf('const handleOpenAIImportSiwcJson'))
    expect(handler).toContain('openaiSiwcOAuth.createFromCallback({')
    expect(handler).toContain('callback_url: callbackUrl')
    expect(handler).toContain('management_account_id: payload.managementAccountId ?? undefined')
    expect(handler).toContain('buildOpenAICodexImportCredentialExtras()')
    expect(handler).toContain('extra: withUpstreamRequestIdHeader(extra)')
    expect(handler).toContain('auto_pause_on_expired: autoPauseOnExpired.value')
    expect(handler).toContain("emit('created')")
  })

  it('imports SIWC JSON through the codex-session endpoint and keeps created_with_error visible', () => {
    const handler = source.slice(source.indexOf('const handleOpenAIImportSiwcJson'), source.indexOf('// OpenAI RT 批量验证和创建'))
    expect(handler).toContain('adminAPI.accounts.importCodexSession({')
    expect(handler).toContain('update_existing: true')
    expect(handler).toContain('siwcImportResult.value = result')
    expect(handler).toContain("item.action === 'created_with_error'")
    expect(handler).toContain('siwcJsonCreatedWithError')
  })

  it('resets SIWC state together with the other OAuth composables', () => {
    expect(source.match(/openaiSiwcOAuth\.resetState\(\)/g)?.length).toBeGreaterThanOrEqual(2)
    expect(source.match(/siwcImportResult\.value = null/g)?.length).toBeGreaterThanOrEqual(2)
  })
})
