import { enableAutoUnmount, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import SiwcGateBadge from '../SiwcGateBadge.vue'
import type { Account } from '@/types'

vi.mock('@/utils/format', () => ({ formatDateTime: (value: string) => `date(${value})` }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, params?: Record<string, unknown>) => (params ? `${key}:${JSON.stringify(params)}` : key) }) }))

enableAutoUnmount(afterEach)

const siwc = (extra: Record<string, unknown> = {}): Account => ({
  id: 1,
  name: 'siwc',
  platform: 'openai',
  type: 'oauth',
  credentials: { auth_mode: 'siwc' },
  extra
} as unknown as Account)

describe('SiwcGateBadge', () => {
  it('renders nothing for non-SIWC accounts', () => {
    const w = mount(SiwcGateBadge, { props: { account: { ...siwc(), credentials: {} } as Account } })
    expect(w.find('[data-testid="siwc-gate-badge"]').exists()).toBe(false)
  })

  it('shows the blocked badge with next check time and emits show-gate on click', async () => {
    const w = mount(SiwcGateBadge, {
      props: { account: siwc({ siwc_gate: { state: 'blocked', next_check_at: '2026-10-23T12:00:00Z' } }) }
    })
    expect(w.get('[data-testid="siwc-gate-blocked"]').classes()).toContain('badge-danger')
    expect(w.get('[data-testid="siwc-gate-next-check"]').text()).toContain('date(2026-10-23T12:00:00Z)')
    await w.get('[data-testid="siwc-gate-blocked"]').trigger('click')
    expect(w.emitted('show-gate')?.[0]?.[0]).toMatchObject({ id: 1 })
  })

  it('shows the restorable badge and the fingerprint verdict with tooltip', () => {
    const w = mount(SiwcGateBadge, {
      props: {
        account: siwc({
          siwc_gate: { state: 'restorable' },
          siwc_fingerprint: { at: '2026-10-23T10:00:00Z', astra: [1], luna: [2], astra_p50: 120.4, luna_p50: 300.6, verdict: 'suspect' },
          siwc_management_account_id: 42
        })
      }
    })
    expect(w.get('[data-testid="siwc-gate-restorable"]').classes()).toContain('badge-warning')
    const fp = w.get('[data-testid="siwc-fingerprint"]')
    expect(fp.text()).toBe('admin.accounts.siwcGate.fingerprintSuspect')
    expect(fp.classes().join(' ')).toContain('text-red')
    expect(fp.attributes('title')).toContain('"astra":120')
    expect(fp.attributes('title')).toContain('"luna":301')
    expect(w.get('[data-testid="siwc-management-account"]').text()).toContain('"id":42')
  })

  it('falls back to the fingerprint embedded in the gate and greys out errors', () => {
    const w = mount(SiwcGateBadge, {
      props: { account: siwc({ siwc_gate: { state: 'clear', fingerprint: { at: '', astra: [], luna: [], astra_p50: 0, luna_p50: 0, verdict: 'error' } } }) }
    })
    expect(w.find('[data-testid="siwc-gate-blocked"]').exists()).toBe(false)
    expect(w.find('[data-testid="siwc-gate-restorable"]').exists()).toBe(false)
    const fp = w.get('[data-testid="siwc-fingerprint"]')
    expect(fp.text()).toBe('admin.accounts.siwcGate.fingerprintError')
    expect(fp.classes().join(' ')).toContain('text-gray')
  })
})
