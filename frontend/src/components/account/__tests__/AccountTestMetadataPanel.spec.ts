import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AccountTestMetadataPanel from '../AccountTestMetadataPanel.vue'

const MESSAGES: Record<string, string> = {
  'admin.accounts.testMetadata.title': 'Request and response metadata',
  'admin.accounts.testMetadata.subtitle': 'Reported by the test run as it progresses.',
  'admin.accounts.testMetadata.requestedModel': 'Requested model',
  'admin.accounts.testMetadata.upstreamSentModel': 'Upstream sent model',
  'admin.accounts.testMetadata.upstreamResponseModel': 'Upstream response model',
  'admin.accounts.testMetadata.responseConflictHint': 'upstream model mismatch',
  'admin.accounts.testMetadata.effectiveEffort': 'Reasoning effort',
  'admin.accounts.testMetadata.effortFromSuffix': 'from model ID suffix',
  'admin.accounts.testMetadata.effectivePrompt': 'Effective prompt',
  'admin.accounts.testMetadata.promptReported': 'reported by the test run',
  'admin.accounts.testMetadata.promptCustom': 'custom input',
  'admin.accounts.testMetadata.promptGlobal': 'global setting',
  'admin.accounts.testMetadata.promptBuiltin': 'built-in default',
  'admin.accounts.testMetadata.streamModeLabel': 'Response mode',
  'admin.accounts.testMetadata.streamMode.stream': 'stream',
  'admin.accounts.testMetadata.streamMode.native_non_stream': 'native non-stream',
  'admin.accounts.testMetadata.notProvided': 'not provided',
  'common.loading': 'Loading'
}

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => MESSAGES[key] ?? key
    })
  }
})

type PanelProps = {
  metadata?: Record<string, unknown>
  requestedModel?: string
  effortFallback?: string
  effortSource?: 'model' | 'selection' | 'default'
  promptFallback?: string
  promptSource?: 'custom' | 'global' | 'builtin'
  promptPending?: boolean
}

function mountPanel(props: PanelProps = {}) {
  return mount(AccountTestMetadataPanel, {
    props: {
      metadata: {},
      ...props
    } as any,
    global: { stubs: { Icon: true } }
  })
}

function cell(wrapper: ReturnType<typeof mountPanel>, key: string): string {
  return wrapper.find(`[data-testid="account-test-metadata-${key}"]`).text()
}

describe('AccountTestMetadataPanel', () => {
  it('shows every field a run did not report as "not provided"', () => {
    const wrapper = mountPanel()

    for (const key of [
      'requested-model',
      'upstream-sent-model',
      'upstream-response-model',
      'effective-effort',
      'effective-prompt',
      'stream-mode'
    ]) {
      expect(cell(wrapper, key)).toContain('not provided')
    }
    expect(wrapper.find('[data-testid="account-test-metadata-conflict"]').exists()).toBe(false)
  })

  it('renders the models the run requested, sent and received', () => {
    const wrapper = mountPanel({
      metadata: {
        requested_model: 'gemini-3.6-flash',
        upstream_sent_model: 'gemini-3.6-flash-high',
        upstream_response_model: 'gemini-3.6-flash-high',
        effective_effort: 'high',
        effective_prompt: 'ping',
        stream_mode: 'stream'
      }
    })

    expect(cell(wrapper, 'requested-model')).toContain('gemini-3.6-flash')
    expect(cell(wrapper, 'upstream-sent-model')).toContain('gemini-3.6-flash-high')
    expect(cell(wrapper, 'upstream-response-model')).toContain('gemini-3.6-flash-high')
    expect(cell(wrapper, 'effective-effort')).toContain('high')
    expect(cell(wrapper, 'effective-prompt')).toContain('ping')
    expect(cell(wrapper, 'stream-mode')).toContain('stream')
    expect(wrapper.text()).toContain('reported by the test run')
  })

  it('shows the prompt exactly as reported, whitespace included', () => {
    const wrapper = mountPanel({
      metadata: { effective_prompt: '  first line\n  second line  ' }
    })

    // The prompt is the request itself, so its whitespace is never normalized.
    // `text()` trims, so the raw text node is what proves verbatim rendering.
    const raw =
      wrapper.find('[data-testid="account-test-metadata-effective-prompt"]').element.textContent ?? ''
    expect(raw.startsWith('  first line\n  second line  ')).toBe(true)
    // A whitespace-only prompt still reads as absent.
    expect(
      cell(mountPanel({ metadata: { effective_prompt: '   \n ' } }), 'effective-prompt')
    ).toContain('not provided')
  })

  it('falls back to the legacy model field for the sent model', () => {
    const wrapper = mountPanel({ metadata: { model: 'claude-sonnet-4-6' } })

    expect(cell(wrapper, 'upstream-sent-model')).toContain('claude-sonnet-4-6')
  })

  it('keeps both models visible and warns when the response model conflicts', () => {
    const wrapper = mountPanel({
      metadata: {
        upstream_sent_model: 'gemini-3.6-flash-medium',
        upstream_response_model: 'gemini-3.6-flash-high',
        upstream_response_model_conflict: true
      }
    })

    expect(cell(wrapper, 'upstream-sent-model')).toContain('gemini-3.6-flash-medium')
    expect(cell(wrapper, 'upstream-response-model')).toContain('gemini-3.6-flash-high')
    expect(wrapper.find('[data-testid="account-test-metadata-conflict"]').text()).toContain(
      'upstream model mismatch'
    )
  })

  it('shows the expected effort and prompt before the run reports them', () => {
    const wrapper = mountPanel({
      requestedModel: 'gemini-3.6-flash-high',
      effortFallback: 'high',
      effortSource: 'model',
      promptFallback: 'Reply with the single word: hi',
      promptSource: 'global'
    })

    expect(cell(wrapper, 'requested-model')).toContain('gemini-3.6-flash-high')
    expect(cell(wrapper, 'effective-effort')).toContain('high')
    expect(cell(wrapper, 'effective-effort')).toContain('from model ID suffix')
    expect(cell(wrapper, 'effective-prompt')).toContain('Reply with the single word: hi')
    expect(cell(wrapper, 'effective-prompt')).toContain('global setting')
    expect(cell(wrapper, 'stream-mode')).toContain('not provided')
  })

  it('shows a loading placeholder instead of "not provided" while the global prompt loads', () => {
    const wrapper = mountPanel({ promptPending: true })

    expect(cell(wrapper, 'effective-prompt')).toContain('Loading')
    expect(cell(wrapper, 'effective-prompt')).not.toContain('not provided')
  })

  it('labels a mode the contract does not define verbatim', () => {
    const wrapper = mountPanel({ metadata: { stream_mode: 'chunked' } })

    expect(cell(wrapper, 'stream-mode')).toContain('chunked')
  })
})
