import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import type { Account } from '@/types'

const { apply, update, clearError, generate, exchange, showSuccess, showError } = vi.hoisted(() => ({
  apply: vi.fn(), update: vi.fn(), clearError: vi.fn(), generate: vi.fn(), exchange: vi.fn(),
  showSuccess: vi.fn(), showError: vi.fn()
}))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess, showError }) }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key })
}))
vi.mock('@/api/admin', () => ({ adminAPI: {
  accounts: { applyOAuthCredentials: apply, update, clearError },
  antigravity: { generateAuthUrl: generate, exchangeCode: exchange }
} }))

import ReAuthAccountModal from '../ReAuthAccountModal.vue'

const OAuthFlowStub = defineComponent({
  name: 'OAuthAuthorizationFlow',
  emits: ['generate-url'],
  setup(_, { expose }) {
    expose({ authCode: 'synthetic-code', oauthState: 'synthetic-state', inputMethod: 'manual', reset: vi.fn() })
  },
  template: '<button data-test="generate" @click="$emit(\'generate-url\')">Generate</button>'
})
const account = {
  id: 73, name: 'nia', platform: 'antigravity', type: 'oauth', proxy_id: 2,
  credentials: { model_mapping: { 'gemini-4-argon': 'gemini-4-argon-{effort}' } },
  group_ids: [8], extra: { model_rate_limits: { 'gemini-3.8-flash': { rate_limit_reset_at: '2026-10-24T00:00:00Z' } } }
} as unknown as Account

function renderModal() {
  return mount(ReAuthAccountModal, {
    props: { show: true, account },
    global: { stubs: {
      BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' },
      Icon: true, OAuthAuthorizationFlow: OAuthFlowStub
    } }
  })
}

beforeEach(() => {
  vi.clearAllMocks()
  generate.mockResolvedValue({ auth_url: 'https://accounts.google.com/example', session_id: 'session', state: 'synthetic-state' })
  exchange.mockResolvedValue({ access_token: 'synthetic-access', refresh_token: 'synthetic-refresh', token_type: 'Bearer', expires_at: 12345, project_id: 'project' })
  apply.mockResolvedValue(account)
})

describe('Antigravity reauthorization', () => {
  it('applies the grant to the same account without the full update or quota-reset endpoint', async () => {
    const wrapper = renderModal()
    await wrapper.get('[data-test="generate"]').trigger('click')
    await flushPromises()
    const complete = wrapper.findAll('button').find(button => button.text() === 'admin.accounts.oauth.completeAuth')!
    expect(complete.attributes('disabled')).toBeUndefined()
    await complete.trigger('click')
    await flushPromises()
    expect(exchange).toHaveBeenCalledWith({ session_id: 'session', state: 'synthetic-state', code: 'synthetic-code', proxy_id: 2 })
    expect(apply).toHaveBeenCalledOnce()
    expect(apply).toHaveBeenCalledWith(73, { type: 'oauth', credentials: {
      access_token: 'synthetic-access', refresh_token: 'synthetic-refresh', token_type: 'Bearer', expires_at: '12345', project_id: 'project', email: undefined
    } })
    expect(update).not.toHaveBeenCalled()
    expect(clearError).not.toHaveBeenCalled()
    expect(wrapper.emitted('reauthorized')).toEqual([[account]])
    expect(wrapper.emitted('close')).toHaveLength(1)
    expect(showSuccess).toHaveBeenCalledOnce()
  })

  it('keeps the dialog open and reports persistence failure instead of emitting success', async () => {
    apply.mockRejectedValue({ response: { data: { detail: 'Could not save grant' } } })
    const wrapper = renderModal()
    await wrapper.get('[data-test="generate"]').trigger('click')
    await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === 'admin.accounts.oauth.completeAuth')!.trigger('click')
    await flushPromises()
    expect(showError).toHaveBeenCalledWith('Could not save grant')
    expect(showSuccess).not.toHaveBeenCalled()
    expect(wrapper.emitted('reauthorized')).toBeUndefined()
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(clearError).not.toHaveBeenCalled()
  })
})
