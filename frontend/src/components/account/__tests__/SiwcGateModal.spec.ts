import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import SiwcGateModal from '../SiwcGateModal.vue'
import type { Account, SiwcGateState } from '@/types'

const mocks = vi.hoisted(() => ({
  getSiwcGate: vi.fn(),
  recheckSiwcGate: vi.fn(),
  restoreSiwcGate: vi.fn(),
  runSiwcFingerprint: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn()
}))

vi.mock('@/api/admin', () => ({ adminAPI: { accounts: mocks } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => mocks }))
vi.mock('@/utils/format', () => ({ formatDateTime: (value: string) => `date(${value})` }))
// 错误码命名空间返回带前缀的译文，模拟真实 i18n 里"有对应翻译"的情况（原样返回会被当作缺键回退）
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => (key.includes('.errors.') ? `tr:${key}` : key) }) }))

enableAutoUnmount(afterEach)
beforeEach(() => vi.clearAllMocks())

const fingerprint = { at: '2026-10-23T10:00:00Z', astra: [120, 130, 125], luna: [300, 310, 305], astra_p50: 125, luna_p50: 305, verdict: 'ok' as const }

const gateOf = (state: SiwcGateState['state'], extra: Partial<SiwcGateState> = {}): SiwcGateState => ({
  state,
  code: 'no_subscription_sharing',
  request_id: 'req_1',
  blocked_at: '2026-10-22T10:00:00Z',
  last_check_at: '2026-10-23T09:00:00Z',
  next_check_at: '2026-10-23T12:00:00Z',
  consecutive: 2,
  checks: [1, 2, 3, 4, 5, 6].map((i) => ({ at: `2026-10-23T0${i}:00:00Z`, status: i % 2 ? 403 : 200, code: `c${i}`, request_id: `r${i}` })),
  fingerprint,
  ...extra
})

const stubs = {
  BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' },
  ConfirmDialog: {
    props: ['show'],
    emits: ['confirm', 'cancel'],
    template: '<div v-if="show" data-testid="confirm"><button data-testid="confirm-yes" @click="$emit(\'confirm\')">yes</button></div>'
  }
}

async function open(account: Partial<Account> = { id: 1, name: 'siwc-01' }) {
  const w = mount(SiwcGateModal, {
    props: { show: false, account: account as Account },
    global: { stubs }
  })
  await w.setProps({ show: true })
  await flushPromises()
  return w
}

describe('SiwcGateModal', () => {
  it('loads the gate on open and renders state, last 5 checks and fingerprint', async () => {
    mocks.getSiwcGate.mockResolvedValueOnce(gateOf('blocked'))
    const w = await open()
    expect(mocks.getSiwcGate).toHaveBeenCalledWith(1)
    expect(w.get('[data-testid="siwc-gate-state"]').text()).toBe('admin.accounts.siwcGate.stateBlocked')
    const rows = w.get('[data-testid="siwc-gate-checks"]').findAll('tbody tr')
    expect(rows).toHaveLength(5)
    // 最新的在最上面
    expect(rows[0].text()).toContain('c6')
    expect(w.get('[data-testid="siwc-gate-fingerprint"]').text()).toContain('125 ms')
    expect(w.get('[data-testid="siwc-gate-fingerprint"]').text()).toContain('admin.accounts.siwcGate.fingerprintOk')
  })

  it('only enables restore when the gate is restorable', async () => {
    mocks.getSiwcGate.mockResolvedValueOnce(gateOf('blocked'))
    const blocked = await open()
    expect(blocked.get('[data-testid="siwc-gate-restore"]').attributes('disabled')).toBeDefined()

    mocks.getSiwcGate.mockResolvedValueOnce(gateOf('restorable'))
    const restorable = await open({ id: 2, name: 'siwc-02' })
    expect(restorable.get('[data-testid="siwc-gate-restore"]').attributes('disabled')).toBeUndefined()
  })

  it('disables recheck while the manual cooldown is running', async () => {
    mocks.getSiwcGate.mockResolvedValueOnce(gateOf('blocked', { manual_cooldown_remaining: 45 }))
    const w = await open()
    const button = w.get('[data-testid="siwc-gate-recheck"]')
    expect(button.attributes('disabled')).toBeDefined()
    expect(button.text()).toContain('admin.accounts.siwcGate.recheckCooldown')
  })

  it('rechecks and emits the updated gate', async () => {
    mocks.getSiwcGate.mockResolvedValueOnce(gateOf('blocked'))
    const next = gateOf('restorable')
    mocks.recheckSiwcGate.mockResolvedValueOnce(next)
    const w = await open()
    await w.get('[data-testid="siwc-gate-recheck"]').trigger('click')
    await flushPromises()
    expect(mocks.recheckSiwcGate).toHaveBeenCalledWith(1)
    expect(w.get('[data-testid="siwc-gate-state"]').text()).toBe('admin.accounts.siwcGate.stateRestorable')
    expect(w.emitted('updated')?.[0]).toEqual([1, next, fingerprint])
    expect(mocks.showSuccess).toHaveBeenCalledWith('admin.accounts.siwcGate.recheckSuccess')
  })

  it('asks for confirmation before restoring, then emits restored', async () => {
    mocks.getSiwcGate.mockResolvedValueOnce(gateOf('restorable'))
    mocks.restoreSiwcGate.mockResolvedValueOnce(gateOf('clear', { cleared_at: '2026-10-23T12:30:00Z' }))
    const w = await open()
    await w.get('[data-testid="siwc-gate-restore"]').trigger('click')
    expect(mocks.restoreSiwcGate).not.toHaveBeenCalled()
    expect(w.find('[data-testid="confirm"]').exists()).toBe(true)
    await w.get('[data-testid="confirm-yes"]').trigger('click')
    await flushPromises()
    expect(mocks.restoreSiwcGate).toHaveBeenCalledWith(1)
    expect(w.emitted('restored')).toEqual([[1]])
    expect(w.get('[data-testid="siwc-gate-state"]').text()).toBe('admin.accounts.siwcGate.stateClear')
  })

  it('surfaces the suspect fingerprint rejection from the backend', async () => {
    mocks.getSiwcGate.mockResolvedValueOnce(gateOf('restorable'))
    mocks.restoreSiwcGate.mockRejectedValueOnce({ status: 409, reason: 'SIWC_GATE_FINGERPRINT_SUSPECT', message: 'suspect' })
    const w = await open()
    await w.get('[data-testid="siwc-gate-restore"]').trigger('click')
    await w.get('[data-testid="confirm-yes"]').trigger('click')
    await flushPromises()
    expect(w.get('[data-testid="siwc-gate-error"]').text()).toBe('tr:admin.accounts.siwcGate.errors.SIWC_GATE_FINGERPRINT_SUSPECT')
    expect(w.emitted('restored')).toBeUndefined()
  })

  it('runs the fingerprint check and emits it without touching the gate', async () => {
    mocks.getSiwcGate.mockResolvedValueOnce(gateOf('blocked', { fingerprint: undefined }))
    const suspect = { ...fingerprint, verdict: 'suspect' as const }
    mocks.runSiwcFingerprint.mockResolvedValueOnce(suspect)
    const w = await open()
    await w.get('[data-testid="siwc-gate-fingerprint-run"]').trigger('click')
    await flushPromises()
    expect(mocks.runSiwcFingerprint).toHaveBeenCalledWith(1)
    expect(w.get('[data-testid="siwc-gate-fingerprint"]').text()).toContain('admin.accounts.siwcGate.fingerprintSuspect')
    const [id, gate, fp] = w.emitted('updated')![0]
    expect(id).toBe(1)
    expect((gate as SiwcGateState).state).toBe('blocked')
    expect(fp).toEqual(suspect)
  })

  it('ignores a late response after switching accounts', async () => {
    let resolveOld!: (value: SiwcGateState) => void
    mocks.getSiwcGate
      .mockReturnValueOnce(new Promise<SiwcGateState>((resolve) => { resolveOld = resolve }))
      .mockResolvedValueOnce(gateOf('restorable'))
    const w = await open()
    await w.setProps({ account: { id: 2, name: 'second' } as Account })
    await flushPromises()
    resolveOld(gateOf('blocked'))
    await flushPromises()
    expect(w.get('[data-testid="siwc-gate-state"]').text()).toBe('admin.accounts.siwcGate.stateRestorable')
  })
})
