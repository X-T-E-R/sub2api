import { flushPromises, shallowMount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import OpsErrorDetailModal from '../OpsErrorDetailModal.vue'

const mocks = vi.hoisted(() => ({
  getRequestErrorDetail: vi.fn(),
  getUpstreamErrorDetail: vi.fn(),
  listRequestErrorUpstreamErrors: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin/ops', () => ({
  opsAPI: {
    getRequestErrorDetail: mocks.getRequestErrorDetail,
    getUpstreamErrorDetail: mocks.getUpstreamErrorDetail,
    listRequestErrorUpstreamErrors: mocks.listRequestErrorUpstreamErrors
  }
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({ showError: mocks.showError })
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: Error) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}

function telemetryDetail(id: number, limit = `account-${id}`) {
  return {
    id,
    created_at: '2026-09-03T00:00:00Z',
    phase: 'request',
    type: 'upstream_error',
    status_code: 429,
    request_id: `request-${id}`,
    upstream_errors: JSON.stringify([{
      account_id: id,
      upstream_status_code: 429,
      codex_telemetry: {
        v: 1,
        transport: 'http',
        observations: [{ source: 'http_headers', association: 'http_response', active_limit: limit }]
      }
    }])
  }
}

function mountDetail() {
  return shallowMount(OpsErrorDetailModal, {
    props: { show: true, errorId: 1, errorType: 'request' },
    global: { stubs: { BaseDialog: { name: 'BaseDialog', emits: ['close'], template: '<div><slot /></div>' }, Icon: true } }
  })
}

function displayedLimit(wrapper: ReturnType<typeof mountDetail>) {
  return wrapper.findComponent({ name: 'CodexTelemetryPanel' }).props('telemetry').observations[0].active_limit
}

describe('OpsErrorDetailModal', () => {
  beforeEach(() => {
    mocks.getRequestErrorDetail.mockReset()
    mocks.getUpstreamErrorDetail.mockReset()
    mocks.showError.mockReset()
    mocks.listRequestErrorUpstreamErrors.mockReset()
    mocks.listRequestErrorUpstreamErrors.mockResolvedValue({ items: [] })
  })

  it('prioritizes upstream root cause and deduplicates diagnostic payloads', async () => {
    mocks.getRequestErrorDetail.mockResolvedValue({
      id: 1,
      created_at: '2026-08-19T00:00:00Z',
      phase: 'request',
      type: 'upstream_error',
      error_owner: 'provider',
      error_source: 'gateway',
      severity: 'P1',
      status_code: 502,
      upstream_status_code: 429,
      platform: 'openai',
      model: 'gpt-5.6',
      resolved: false,
      request_id: 'rid-1',
      message: 'All available accounts exhausted',
      error_body: '{"error":"same"}',
      upstream_error_message: 'provider rate limit exhausted',
      upstream_error_detail: '{"error":"same"}',
      upstream_errors: '[]',
      account_name: 'account',
      group_name: 'group',
      is_business_limited: false
    })

    const wrapper = shallowMount(OpsErrorDetailModal, {
      props: { show: true, errorId: 1, errorType: 'request' },
      global: {
        stubs: {
          BaseDialog: { template: '<div><slot /></div>' },
          Icon: true
        }
      }
    })
    await flushPromises()

    expect(wrapper.text()).toContain('provider rate limit exhausted')
    expect(wrapper.text()).toContain('admin.ops.errorDetail.upstreamStatus')
    expect(wrapper.text()).toContain('429')
    expect(wrapper.findAll('pre')).toHaveLength(2)
    expect(wrapper.text()).not.toContain('admin.ops.errorDetail.payloads.upstream_detail')
  })

  it('keeps detail and correlated records bound to the latest selected error', async () => {
    const first = deferred<ReturnType<typeof telemetryDetail>>()
    const second = deferred<ReturnType<typeof telemetryDetail>>()
    const relatedFirst = deferred<{ items: Array<{ id: number; message: string }> }>()
    const relatedSecond = deferred<{ items: Array<{ id: number; message: string }> }>()
    mocks.getRequestErrorDetail.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise)
    mocks.listRequestErrorUpstreamErrors.mockReturnValueOnce(relatedFirst.promise).mockReturnValueOnce(relatedSecond.promise)
    const wrapper = mountDetail()
    await wrapper.setProps({ errorId: 2 })
    second.resolve(telemetryDetail(2))
    relatedSecond.resolve({ items: [{ id: 102, message: 'current-correlated-record' }] })
    await flushPromises()
    expect(displayedLimit(wrapper)).toBe('account-2')
    expect(wrapper.text()).toContain('current-correlated-record')

    first.resolve(telemetryDetail(1))
    relatedFirst.resolve({ items: [{ id: 101, message: 'stale-correlated-record' }] })
    await flushPromises()
    expect(displayedLimit(wrapper)).toBe('account-2')
    expect(wrapper.text()).toContain('current-correlated-record')
    expect(wrapper.text()).not.toContain('stale-correlated-record')
    expect(mocks.showError).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('ignores stale failures without ending either current loading state', async () => {
    const first = deferred<ReturnType<typeof telemetryDetail>>()
    const second = deferred<ReturnType<typeof telemetryDetail>>()
    const relatedFirst = deferred<{ items: [] }>()
    const relatedSecond = deferred<{ items: [] }>()
    mocks.getRequestErrorDetail.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise)
    mocks.listRequestErrorUpstreamErrors.mockReturnValueOnce(relatedFirst.promise).mockReturnValueOnce(relatedSecond.promise)
    const wrapper = mountDetail()
    await wrapper.setProps({ errorId: 2 })
    first.reject(new Error('stale detail failure'))
    await flushPromises()
    expect(wrapper.text()).toContain('admin.ops.errorDetail.loading')
    expect(mocks.showError).not.toHaveBeenCalled()

    second.resolve(telemetryDetail(2))
    await flushPromises()
    expect(displayedLimit(wrapper)).toBe('account-2')
    relatedFirst.reject(new Error('stale related failure'))
    await flushPromises()
    expect(wrapper.text()).toContain('common.loading')
    relatedSecond.resolve({ items: [] })
    await flushPromises()
    expect(wrapper.text()).not.toContain('common.loading')
    expect(displayedLimit(wrapper)).toBe('account-2')
    wrapper.unmount()
  })

  it('invalidates pending requests on close and when reopening the same error', async () => {
    const first = deferred<ReturnType<typeof telemetryDetail>>()
    const reopened = deferred<ReturnType<typeof telemetryDetail>>()
    const relatedFirst = deferred<{ items: Array<{ id: number; message: string }> }>()
    mocks.getRequestErrorDetail.mockReturnValueOnce(first.promise).mockReturnValueOnce(reopened.promise)
    mocks.listRequestErrorUpstreamErrors.mockReturnValueOnce(relatedFirst.promise)
    const wrapper = mountDetail()
    wrapper.findComponent({ name: 'BaseDialog' }).vm.$emit('close')
    await flushPromises()
    expect(wrapper.emitted('update:show')).toEqual([[false]])
    expect(wrapper.text()).not.toContain('admin.ops.errorDetail.loading')
    relatedFirst.resolve({ items: [{ id: 101, message: 'closed-correlated-record' }] })
    await flushPromises()
    expect(wrapper.text()).not.toContain('closed-correlated-record')

    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    reopened.resolve(telemetryDetail(1, 'current-open'))
    await flushPromises()
    first.resolve(telemetryDetail(1, 'previous-open'))
    await flushPromises()
    expect(displayedLimit(wrapper)).toBe('current-open')
    expect(mocks.showError).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('reloads the selected endpoint when the error type changes at the same ID', async () => {
    const request = deferred<ReturnType<typeof telemetryDetail>>()
    const upstream = deferred<ReturnType<typeof telemetryDetail>>()
    mocks.getRequestErrorDetail.mockReturnValueOnce(request.promise)
    mocks.getUpstreamErrorDetail.mockReturnValueOnce(upstream.promise)
    const wrapper = mountDetail()
    await wrapper.setProps({ errorType: 'upstream' })
    expect(mocks.getUpstreamErrorDetail).toHaveBeenCalledWith(1)
    upstream.resolve({ ...telemetryDetail(1, 'upstream-selection'), phase: 'upstream' })
    await flushPromises()
    request.resolve(telemetryDetail(1, 'old-request-selection'))
    await flushPromises()
    expect(displayedLimit(wrapper)).toBe('upstream-selection')
    expect(wrapper.text()).not.toContain('admin.ops.errorDetails.upstreamErrors')
    wrapper.unmount()
  })

  it('reports a current request failure and clears its loading state', async () => {
    mocks.getRequestErrorDetail.mockRejectedValueOnce(new Error('current detail failure'))
    const wrapper = mountDetail()
    await flushPromises()
    expect(mocks.showError).toHaveBeenCalledWith('current detail failure')
    expect(wrapper.text()).not.toContain('admin.ops.errorDetail.loading')
    expect(wrapper.findComponent({ name: 'CodexTelemetryPanel' }).exists()).toBe(false)
    wrapper.unmount()
  })
})
