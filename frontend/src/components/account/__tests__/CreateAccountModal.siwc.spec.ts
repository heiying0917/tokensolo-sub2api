import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

const source = readFileSync(
  resolve(process.cwd(), 'src/components/account/CreateAccountModal.vue'),
  'utf8'
)

describe('CreateAccountModal SIWC wiring', () => {
  it('hides Codex-only toggles and strips their extra keys for SIWC', () => {
    expect(source).toContain('data-testid="create-openai-ws-mode"')
    expect(source).toMatch(/!isOpenAISiwcFamily && \(accountCategory === 'oauth-based' \|\| accountCategory === 'apikey'\)"\s+data-testid="create-openai-ws-mode"/)
    const builder = source.slice(source.indexOf('const buildOpenAIExtra ='), source.indexOf('const buildOpenAICodexImportExtra ='))
    expect(builder).toContain('if (isOpenAISiwcFamily.value)')
    for (const key of ['openai_passthrough', 'codex_cli_only', 'codex_fingerprint_mode', 'openai_compact_mode']) {
      expect(builder).toContain(`'${key}'`)
    }
  })


  it('picks the Codex / SIWC family in step 1 and passes only that family to step 2', () => {
    expect(source).toContain('data-testid="openai-auth-family"')
    expect(source).toContain("openaiAuthFamily.value === 'siwc'")
    expect(source).toContain(':show-siwc-option="isOpenAISiwcFamily"')
    expect(source).toContain(':show-siwc-json-option="isOpenAISiwcFamily"')
    expect(source).toContain(':show-codex-pat-option="isOpenAICodexFamily"')
    expect(source).toContain(':show-manual-option="!isOpenAISiwcFamily"')
    expect(source).toContain(":initial-input-method=\"isOpenAISiwcFamily ? 'siwc' : 'manual'\"")
    // 重置时回到 Codex
    expect(source).toContain("openaiAuthFamily.value = 'codex'")
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
