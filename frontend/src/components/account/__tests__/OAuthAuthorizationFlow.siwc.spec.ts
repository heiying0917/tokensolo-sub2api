import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import OAuthAuthorizationFlow from '../OAuthAuthorizationFlow.vue'

const mocks = vi.hoisted(() => ({
  list: vi.fn(),
  getCapabilities: vi.fn(),
  copyToClipboard: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: { list: mocks.list },
    grok: { getCapabilities: mocks.getCapabilities }
  }
}))
vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copied: { value: false }, copyToClipboard: mocks.copyToClipboard })
}))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

enableAutoUnmount(afterEach)

const STATE = 'state_123'
const CALLBACK = `http://127.0.0.1:1455/auth/callback?code=ac_1&state=${STATE}&client_id=oaiapp_abc`

beforeEach(() => {
  vi.clearAllMocks()
  mocks.list.mockResolvedValue({
    items: [
      { id: 1, name: 'codex-main', platform: 'openai', type: 'oauth', credentials: { email: 'a@b.c' }, parent_account_id: null },
      { id: 2, name: 'siwc-01', platform: 'openai', type: 'oauth', credentials: { auth_mode: 'siwc' }, parent_account_id: null },
      { id: 3, name: 'pat', platform: 'openai', type: 'oauth', credentials: { auth_mode: 'personalAccessToken' }, parent_account_id: null }
    ]
  })
})

function mountFlow(extraProps: Record<string, unknown> = {}) {
  return mount(OAuthAuthorizationFlow, {
    props: {
      addMethod: 'oauth',
      platform: 'openai',
      showManualOption: true,
      showCookieOption: false,
      showSiwcOption: true,
      showSiwcJsonOption: true,
      initialInputMethod: 'manual',
      ...extraProps
    }
  })
}

describe('OAuthAuthorizationFlow SIWC section', () => {
  it('separates Codex and SIWC methods into two tabs', async () => {
    const w = mountFlow()
    // 默认 Codex tab：只有 Codex 系列方式
    expect(w.find('[data-testid="oauth-family-tabs"]').exists()).toBe(true)
    expect(w.find('[data-testid="oauth-method-siwc"]').exists()).toBe(false)
    expect(w.find('[data-testid="oauth-method-siwc-json"]').exists()).toBe(false)
    expect(w.find('input[value="manual"]').exists()).toBe(true)

    // 切到 SIWC tab：只剩两种 SIWC 方式，默认选中 SIWC 授权
    await w.get('[data-testid="oauth-family-siwc"]').trigger('click')
    expect(w.find('[data-testid="oauth-method-siwc"]').exists()).toBe(true)
    expect(w.find('[data-testid="oauth-method-siwc-json"]').exists()).toBe(true)
    expect(w.find('input[value="manual"]').exists()).toBe(false)
    expect(w.emitted('update:inputMethod')?.at(-1)).toEqual(['siwc'])

    // 切回 Codex tab：回到手动授权
    await w.get('[data-testid="oauth-family-codex"]').trigger('click')
    expect(w.emitted('update:inputMethod')?.at(-1)).toEqual(['manual'])
    expect(w.find('[data-testid="oauth-method-siwc"]').exists()).toBe(false)
  })

  it('opens on the SIWC tab when the initial method is a SIWC one', () => {
    const w = mountFlow({ initialInputMethod: 'siwc_json' })
    expect(w.find('[data-testid="oauth-method-siwc-json"]').exists()).toBe(true)
    expect(w.find('input[value="manual"]').exists()).toBe(false)
  })

  it('hides the tab bar when SIWC is not enabled', () => {
    const disabled = mountFlow({ showSiwcOption: false, showSiwcJsonOption: false })
    expect(disabled.find('[data-testid="oauth-family-tabs"]').exists()).toBe(false)
    expect(disabled.find('[data-testid="oauth-method-siwc"]').exists()).toBe(false)
  })

  it('emits generate-siwc-url with the trimmed agent name', async () => {
    const w = mountFlow({ initialInputMethod: 'siwc' })
    await flushPromises()
    await w.get('[data-testid="siwc-agent-name"]').setValue('  cpa-01  ')
    await w.get('[data-testid="siwc-generate-url"]').trigger('click')
    expect(w.emitted('generate-siwc-url')).toEqual([['cpa-01']])
  })

  it('loads management credential options excluding SIWC / PAT accounts', async () => {
    const w = mountFlow({ initialInputMethod: 'siwc' })
    await flushPromises()
    expect(mocks.list).toHaveBeenCalledWith(1, 200, { platform: 'openai', type: 'oauth', lite: '1' })
    const options = w.get('[data-testid="siwc-management-select"]').findAll('option')
    // 第一个是"不关联"，其后只剩普通 Codex OAuth 账号
    expect(options).toHaveLength(2)
    expect(options[1].text()).toContain('codex-main')
    expect(options[1].text()).toContain('a@b.c')
  })

  it('validates the pasted callback live and gates the submit button', async () => {
    const w = mountFlow({
      initialInputMethod: 'siwc',
      siwcAuthUrl: 'https://auth.openai.com/oauth/authorize?state=' + STATE,
      siwcState: STATE,
      siwcRemainingSeconds: 1795,
      siwcPhase: 'waiting'
    })
    await flushPromises()
    expect(w.get('[data-testid="siwc-countdown"]').text()).toContain('admin.accounts.oauth.openai.siwcExpiresIn')
    const submit = w.get('[data-testid="siwc-submit"]')
    expect(submit.attributes('disabled')).toBeDefined()

    const input = w.get('[data-testid="siwc-callback-input"]')
    await input.setValue('http://127.0.0.1:1455/auth/callback?code=ac_1&state=other&client_id=oaiapp_abc')
    expect(w.get('[data-testid="siwc-callback-error"]').text()).toBe('admin.accounts.oauth.openai.siwcCallbackInvalid.state_mismatch')
    expect(submit.attributes('disabled')).toBeDefined()

    await input.setValue(`http://127.0.0.1:1455/auth/callback?error=access_denied&state=${STATE}`)
    expect(w.get('[data-testid="siwc-callback-error"]').text()).toBe('admin.accounts.oauth.openai.siwcCallbackInvalid.upstream_error')

    await input.setValue(CALLBACK)
    expect(w.find('[data-testid="siwc-callback-error"]').exists()).toBe(false)
    expect(w.get('[data-testid="siwc-callback-ok"]').exists()).toBe(true)
    expect(submit.attributes('disabled')).toBeUndefined()
  })

  it('emits create-siwc-account with callback and the selected management account', async () => {
    const w = mountFlow({
      initialInputMethod: 'siwc',
      siwcAuthUrl: 'https://auth.openai.com/oauth/authorize',
      siwcState: STATE,
      siwcPhase: 'waiting'
    })
    await flushPromises()
    await w.get('[data-testid="siwc-callback-input"]').setValue(`  ${CALLBACK}  `)
    await w.get('[data-testid="siwc-management-select"]').setValue('1')
    await w.get('[data-testid="siwc-submit"]').trigger('click')
    expect(w.emitted('create-siwc-account')).toEqual([[{ callbackUrl: CALLBACK, managementAccountId: 1 }]])
  })

  it('blocks submit and shows the expired hint once the session expires', async () => {
    const w = mountFlow({
      initialInputMethod: 'siwc',
      siwcAuthUrl: 'https://auth.openai.com/oauth/authorize',
      siwcState: STATE,
      siwcPhase: 'expired'
    })
    await flushPromises()
    await w.get('[data-testid="siwc-callback-input"]').setValue(CALLBACK)
    expect(w.get('[data-testid="siwc-expired"]').exists()).toBe(true)
    expect(w.get('[data-testid="siwc-submit"]').attributes('disabled')).toBeDefined()
    expect(w.emitted('create-siwc-account')).toBeUndefined()
  })

  it('prechecks SIWC JSON and emits import-siwc-json only for valid content', async () => {
    const w = mountFlow({ initialInputMethod: 'siwc_json' })
    await flushPromises()
    const input = w.get('[data-testid="siwc-json-input"]')
    const button = w.get('[data-testid="siwc-json-import"]')

    await input.setValue('{"client_id":"app_codex","refresh_token":"rt","email":"e"}')
    expect(w.get('[data-testid="siwc-json-error"]').text()).toBe('admin.accounts.oauth.openai.siwcJsonInvalid.not_siwc')
    expect(button.attributes('disabled')).toBeDefined()

    const entry = JSON.stringify({ type: 'chatgpt_siwc', client_id: 'oaiapp_x', refresh_token: 'rt', email: 'a@b.c' })
    await input.setValue(`${entry}\n${entry}`)
    expect(w.find('[data-testid="siwc-json-error"]').exists()).toBe(false)
    expect(button.attributes('disabled')).toBeUndefined()
    await button.trigger('click')
    expect(w.emitted('import-siwc-json')).toEqual([[`${entry}\n${entry}`]])
  })

  it('renders import results with the created_with_error warning row', async () => {
    const w = mountFlow({
      initialInputMethod: 'siwc_json',
      siwcImportResult: {
        total: 2,
        created: 2,
        updated: 0,
        skipped: 0,
        failed: 0,
        items: [
          { index: 1, name: 'a', action: 'created', account_id: 10, auth_mode: 'siwc' },
          { index: 2, name: 'b', action: 'created_with_error', account_id: 11, message: 'refresh failed', auth_mode: 'siwc' }
        ]
      }
    })
    await flushPromises()
    const rows = w.get('[data-testid="siwc-json-result"]').findAll('li')
    expect(rows).toHaveLength(2)
    expect(rows[1].text()).toContain('admin.accounts.oauth.openai.siwcJsonAction.created_with_error')
    expect(rows[1].text()).toContain('refresh failed')
    expect(rows[1].find('span').classes().join(' ')).toContain('amber')
  })
})
