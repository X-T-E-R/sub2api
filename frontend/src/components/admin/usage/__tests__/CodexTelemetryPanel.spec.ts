import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import { saveAs } from 'file-saver'
import CodexTelemetryPanel from '../CodexTelemetryPanel.vue'
import { codexTelemetryFromOpsErrors, type CodexTelemetrySnapshot } from '@/types/codexTelemetry'

vi.mock('file-saver', () => ({ saveAs: vi.fn() }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const fixture: CodexTelemetrySnapshot = {
  v: 1, transport: 'websocket', response_id: 'resp-local', connection_reused: true,
  truncated: true, unassociated_events: 2,
  observations: [
    { source: 'responsesapi.websocket_timing', association: 'active_response', engine_ids: ['opaque-engine'] },
    { source: 'error_headers', association: 'upstream_attempt', faster_model: 'retry-hint', primary_used_percent: 100 }
  ]
}

describe('CodexTelemetryPanel', () => {
  it('keeps engine observations, retry hints and inferred attribution separate', async () => {
    const wrapper = mount(CodexTelemetryPanel, { props: { telemetry: fixture } })
    expect(wrapper.text()).toContain('opaque-engine')
    expect(wrapper.text()).toContain('retry-hint')
    expect(wrapper.text()).toContain('usage.codexTelemetry.association.active_response')
    expect(wrapper.text()).toContain('usage.codexTelemetry.association.upstream_attempt')
    expect(wrapper.get('[data-testid="codex-telemetry-inferred"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="codex-telemetry-truncated"]').exists()).toBe(true)
    await wrapper.get('[data-testid="codex-telemetry-download"]').trigger('click')
    expect(saveAs).toHaveBeenCalledWith(expect.any(Blob), 'codex-telemetry.json')
  })

  it('does not invent an inferred label for explicitly matched IDs', () => {
    const wrapper = mount(CodexTelemetryPanel, { props: { telemetry: { ...fixture, truncated: false, observations: [{ source: 'responsesapi.websocket_timing', association: 'response_id', engine_ids: ['explicit-engine'] }] } } })
    expect(wrapper.find('[data-testid="codex-telemetry-inferred"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="codex-telemetry-truncated"]').exists()).toBe(false)
  })

  it('projects only allowlisted Ops fields and leaves malformed metadata unknown', () => {
    const raw = JSON.stringify([{ codex_telemetry: { ...fixture, prompt: 'excluded', observations: [{ ...fixture.observations[0], headers: { authorization: 'excluded' } }] } }])
    const snapshots = codexTelemetryFromOpsErrors(raw)
    expect(snapshots).toHaveLength(1)
    expect(JSON.stringify(snapshots)).not.toContain('excluded')
    for (const bad of ['', '{}', 'not JSON', '[null]', JSON.stringify([{ codex_telemetry: { ...fixture, observations: [{ source: 'error_headers', association: 'upstream_attempt', primary_used_percent: 101 }] } }])]) {
      expect(codexTelemetryFromOpsErrors(bad)).toEqual([])
    }
  })
})
