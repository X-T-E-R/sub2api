import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import OpsSettingsDialog from '../OpsSettingsDialog.vue'

const api = vi.hoisted(() => ({
  getAlertRuntimeSettings: vi.fn(), getEmailNotificationConfig: vi.fn(), getAdvancedSettings: vi.fn(), getMetricThresholds: vi.fn(),
  updateAlertRuntimeSettings: vi.fn(), updateEmailNotificationConfig: vi.fn(), updateAdvancedSettings: vi.fn(), updateMetricThresholds: vi.fn()
}))
vi.mock('@/api/admin/ops', () => ({ opsAPI: api }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('Ops Codex telemetry setting', () => {
  it('defaults an older payload to off and saves explicit enable/disable through the existing settings endpoint', async () => {
    api.getAlertRuntimeSettings.mockResolvedValue({ evaluation_interval_seconds: 60, distributed_lock: { enabled: false, key: '', ttl_seconds: 30 }, silencing: { enabled: false, entries: [] }, thresholds: {} })
    api.getEmailNotificationConfig.mockResolvedValue({ alert: { enabled: false, recipients: [] }, report: { enabled: false, recipients: [], daily_summary_enabled: false, weekly_summary_enabled: false } })
    api.getAdvancedSettings.mockResolvedValue({ data_retention: { cleanup_enabled: false, error_log_retention_days: 30, minute_metrics_retention_days: 30, hourly_metrics_retention_days: 30 }, aggregation: { aggregation_enabled: false }, openai_account_quota_auto_pause: { default_threshold_5h: 0, default_threshold_7d: 0 }, ignore_count_tokens_errors: true, ignore_context_canceled: true, ignore_no_available_accounts: false, ignore_insufficient_balance_errors: false, auto_refresh_enabled: false, display_alert_events: true, display_openai_token_stats: false })
    api.getMetricThresholds.mockResolvedValue({})
    const wrapper = mount(OpsSettingsDialog, { props: { show: false }, global: { stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }, Select: true } } })
    await wrapper.setProps({ show: true })
    await flushPromises()
    const toggle = wrapper.get('[data-testid="codex-telemetry-toggle"]')
    expect(toggle.attributes('aria-checked')).toBe('false')
    await toggle.trigger('click')
    expect(toggle.attributes('aria-checked')).toBe('true')
    const save = wrapper.findAll('button').find(button => button.text().includes('common.save'))
    expect(save).toBeDefined()
    await save!.trigger('click')
    await flushPromises()
    expect(api.updateAdvancedSettings).toHaveBeenLastCalledWith(expect.objectContaining({ codex_telemetry_enabled: true }))
    await toggle.trigger('click')
    await save!.trigger('click')
    await flushPromises()
    expect(api.updateAdvancedSettings).toHaveBeenLastCalledWith(expect.objectContaining({ codex_telemetry_enabled: false }))
  })
})
