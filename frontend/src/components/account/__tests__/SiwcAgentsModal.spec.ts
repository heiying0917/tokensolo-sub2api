import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'

const mocks = vi.hoisted(() => ({
  getSiwcAgents: vi.fn(),
  activateSiwcAgent: vi.fn(),
  reauthSiwcFromOAuth: vi.fn(),
  createOpenAISiwc: vi.fn(),
  generateSiwcAuthUrl: vi.fn(),
  deleteAccount: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getSiwcAgents: mocks.getSiwcAgents,
      activateSiwcAgent: mocks.activateSiwcAgent,
      reauthSiwcFromOAuth: mocks.reauthSiwcFromOAuth,
      createOpenAISiwc: mocks.createOpenAISiwc,
      generateSiwcAuthUrl: mocks.generateSiwcAuthUrl,
      delete: mocks.deleteAccount
    }
  }
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess: mocks.showSuccess, showError: mocks.showError, showInfo: vi.fn() })
}))
vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copied: { value: false }, copyToClipboard: vi.fn() })
}))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import SiwcAgentsModal from '../SiwcAgentsModal.vue'

enableAutoUnmount(afterEach)

const BaseDialogStub = defineComponent({
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const STATE = 'state_abc'
const CALLBACK = `http://127.0.0.1:1455/auth/callback?code=ac_1&state=${STATE}&client_id=oaiapp_new`

function group(activeId = 2) {
  return {
    email: 'a@b.c',
    active_account_id: activeId,
    management_account_id: 1,
    management_account_name: 'codex',
    agents: [
      { id: 2, name: 'a@b.c (siwc)', role: activeId === 2 ? 'active' : 'standby', status: 'active', schedulable: activeId === 2, usable: true, created_at: '' },
      { id: 7, name: 'a@b.c Agent #2', role: activeId === 7 ? 'active' : 'standby', status: 'active', schedulable: activeId === 7, usable: true, created_at: '' },
      { id: 8, name: 'a@b.c Agent #3', role: 'standby', status: 'error', schedulable: false, usable: false, error_message: 'invalid_grant', created_at: '' }
    ]
  }
}

function mountModal(props: Record<string, unknown> = {}) {
  return mount(SiwcAgentsModal, {
    props: { show: true, account: { id: 2, proxy_id: null } as any, ...props },
    global: { stubs: { BaseDialog: BaseDialogStub, ConfirmDialog: true } }
  })
}

beforeEach(() => {
  vi.clearAllMocks()
  mocks.getSiwcAgents.mockResolvedValue(group())
  mocks.generateSiwcAuthUrl.mockResolvedValue({ auth_url: `https://auth.openai.com/x?state=${STATE}`, session_id: 'sess_1', state: STATE, expires_in: 600 })
})

describe('SiwcAgentsModal', () => {
  it('lists agents with roles and only offers activate for usable standby agents', async () => {
    const w = mountModal()
    await flushPromises()
    expect(mocks.getSiwcAgents).toHaveBeenCalledWith(2)
    expect(w.get('[data-testid="siwc-agent-role-2"]').text()).toBe('admin.accounts.siwcAgents.roleActive')
    expect(w.get('[data-testid="siwc-agent-role-7"]').text()).toBe('admin.accounts.siwcAgents.roleStandby')
    expect(w.find('[data-testid="siwc-agent-activate-2"]').exists()).toBe(false)
    expect(w.find('[data-testid="siwc-agent-activate-7"]').exists()).toBe(true)
    expect(w.find('[data-testid="siwc-agent-activate-8"]').exists()).toBe(false)
  })

  it('switches the active agent', async () => {
    mocks.activateSiwcAgent.mockResolvedValue(group(7))
    const w = mountModal()
    await flushPromises()
    await w.get('[data-testid="siwc-agent-activate-7"]').trigger('click')
    await flushPromises()
    expect(mocks.activateSiwcAgent).toHaveBeenCalledWith(7)
    expect(w.get('[data-testid="siwc-agent-role-7"]').text()).toBe('admin.accounts.siwcAgents.roleActive')
    expect(w.emitted('changed')).toBeTruthy()
  })

  it('adds an agent through the SIWC flow inheriting from the current account', async () => {
    mocks.createOpenAISiwc.mockResolvedValue({ id: 9 })
    const w = mountModal()
    await flushPromises()
    await w.get('[data-testid="siwc-agents-add"]').trigger('click')
    await w.get('[data-testid="siwc-agents-generate"]').trigger('click')
    await flushPromises()
    await w.get('[data-testid="siwc-agents-callback"]').setValue(CALLBACK)
    await w.get('[data-testid="siwc-agents-submit"]').trigger('click')
    await flushPromises()
    expect(mocks.createOpenAISiwc).toHaveBeenCalledWith(expect.objectContaining({
      session_id: 'sess_1',
      callback_url: CALLBACK,
      inherit_from_account_id: 2
    }))
    expect(mocks.getSiwcAgents).toHaveBeenCalledTimes(2)
  })

  it('opens directly in re-auth mode for the given agent and replaces its credential', async () => {
    mocks.reauthSiwcFromOAuth.mockResolvedValue({ id: 7 })
    const w = mountModal({ reauthAccountId: 7 })
    await flushPromises()
    expect(w.get('[data-testid="siwc-agents-auth"]').text()).toContain('admin.accounts.siwcAgents.reauthTitle')
    await w.get('[data-testid="siwc-agents-generate"]').trigger('click')
    await flushPromises()
    // state 不一致时不能提交
    await w.get('[data-testid="siwc-agents-callback"]').setValue(CALLBACK.replace(STATE, 'other'))
    expect(w.get('[data-testid="siwc-agents-submit"]').attributes('disabled')).toBeDefined()
    await w.get('[data-testid="siwc-agents-callback"]').setValue(CALLBACK)
    await w.get('[data-testid="siwc-agents-submit"]').trigger('click')
    await flushPromises()
    expect(mocks.reauthSiwcFromOAuth).toHaveBeenCalledWith(7, { session_id: 'sess_1', callback_url: CALLBACK })
  })
})
