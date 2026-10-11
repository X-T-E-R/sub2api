import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  accountTestStreamModeLabelKey,
  consumeAccountTestStream,
  mergeAccountTestMetadata,
  metadataText,
  metadataVerbatim
} from '@/utils/accountTestStream'
import type { AccountTestStreamEvent } from '@/types/accountTestStream'

const encoder = new TextEncoder()

/** Reader over fixed chunks; each `read()` consumes one chunk. */
function chunkedReader(chunks: string[]) {
  const encoded = chunks.map((chunk) => encoder.encode(chunk))
  let index = 0
  return {
    read: vi.fn(async () =>
      index < encoded.length
        ? { done: false, value: encoded[index++] }
        : { done: true, value: undefined }
    )
  }
}

function responseWith(chunks: string[]): Response {
  return { ok: true, body: { getReader: () => chunkedReader(chunks) } } as unknown as Response
}

function consume(chunks: string[], onEvent: (event: AccountTestStreamEvent) => void) {
  global.fetch = vi.fn().mockResolvedValue(responseWith(chunks)) as any
  return consumeAccountTestStream({
    url: '/api/v1/admin/accounts/7/test',
    body: { model_id: 'm', prompt: '' },
    signal: new AbortController().signal,
    onEvent
  })
}

describe('consumeAccountTestStream', () => {
  beforeEach(() => {
    localStorage.setItem('auth_token', 'test-token')
  })

  afterEach(() => {
    vi.restoreAllMocks()
    localStorage.clear()
  })

  it('reports each event as soon as its chunk arrives', async () => {
    const events: string[] = []
    let releaseStream!: () => void
    let calls = 0
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      body: {
        getReader: () => ({
          read: () => {
            calls += 1
            if (calls === 1) {
              return Promise.resolve({
                done: false,
                value: encoder.encode('data: {"type":"content","text":"first"}\n\n')
              })
            }
            return new Promise((resolve) => {
              releaseStream = () => resolve({ done: true, value: undefined })
            })
          }
        })
      }
    } as any)

    const pending = consumeAccountTestStream({
      url: '/api/v1/admin/accounts/7/test',
      body: { model_id: 'm', prompt: '' },
      signal: new AbortController().signal,
      onEvent: (event) => events.push(event.text ?? event.type)
    })

    // The first delta must already be visible while the stream stays open.
    await vi.waitFor(() => expect(events).toEqual(['first']))
    releaseStream()
    await pending
    expect(events).toEqual(['first'])
  })

  it('posts the model test body with JSON headers', async () => {
    global.fetch = vi.fn().mockResolvedValue(responseWith([])) as any
    await consumeAccountTestStream({
      url: '/api/v1/admin/accounts/7/test',
      body: { model_id: 'gemini-3.6-flash', prompt: '', reasoning_effort: 'high' },
      signal: new AbortController().signal,
      onEvent: () => {}
    })

    const [url, init] = (global.fetch as any).mock.calls[0]
    expect(url).toBe('/api/v1/admin/accounts/7/test')
    expect(init.method).toBe('POST')
    expect(init.headers.Authorization).toBe('Bearer test-token')
    expect(init.headers['X-Admin-UI-Request']).toBe('1')
    expect(JSON.parse(init.body)).toEqual({
      model_id: 'gemini-3.6-flash',
      prompt: '',
      reasoning_effort: 'high'
    })
  })

  it('reassembles an event split across chunks and tolerates CRLF and missing spaces', async () => {
    const events: string[] = []
    await consume(
      [
        'data: {"type":"test_metadata","requested_model":"gemini-3.6-flash",',
        '"stream_mode":"stream"}\r\n',
        'data:{"type":"content","text":"ok"}\n'
      ],
      (event) => events.push(event.type)
    )

    expect(events).toEqual(['test_metadata', 'content'])
  })

  it('delivers a final event that arrives without a trailing newline', async () => {
    const events: AccountTestStreamEvent[] = []
    await consume(['data: {"type":"test_complete","success":true}'], (event) => events.push(event))

    expect(events).toHaveLength(1)
    expect(events[0].type).toBe('test_complete')
    expect(events[0].success).toBe(true)
  })

  it('ignores comments, blank payloads and malformed JSON', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    const events: string[] = []
    await consume(
      [': keep-alive\n\n', 'data: \n', 'data: {"type":\n', 'data: {"type":"status","text":"ok"}\n'],
      (event) => events.push(event.type)
    )

    expect(events).toEqual(['status'])
    expect(errorSpy).toHaveBeenCalled()
  })

  it('fails loudly on a non-OK response', async () => {
    global.fetch = vi.fn().mockResolvedValue({ ok: false, status: 502 } as Response) as any
    await expect(
      consumeAccountTestStream({
        url: '/api/v1/admin/accounts/7/test',
        body: {},
        signal: new AbortController().signal,
        onEvent: () => {}
      })
    ).rejects.toThrow('HTTP error! status: 502')
  })

  it('reports a missing response body with the caller message', async () => {
    global.fetch = vi.fn().mockResolvedValue({ ok: true, body: null } as unknown as Response) as any
    await expect(
      consumeAccountTestStream({
        url: '/api/v1/admin/accounts/7/test',
        body: {},
        signal: new AbortController().signal,
        noResponseBodyMessage: 'No response body',
        onEvent: () => {}
      })
    ).rejects.toThrow('No response body')
  })
})

describe('mergeAccountTestMetadata', () => {
  it('keeps known values when a later event omits or blanks them', () => {
    const merged = mergeAccountTestMetadata(
      { requested_model: 'base', upstream_sent_model: 'base-high', stream_mode: 'stream' },
      { requested_model: '  ', upstream_response_model: 'base-high' }
    )

    expect(merged).toEqual({
      requested_model: 'base',
      upstream_sent_model: 'base-high',
      stream_mode: 'stream',
      upstream_response_model: 'base-high'
    })
  })

  it('keeps a reported prompt verbatim, whitespace included', () => {
    const merged = mergeAccountTestMetadata({}, { effective_prompt: '  line one\n\tline two  ' })

    expect(merged.effective_prompt).toBe('  line one\n\tline two  ')

    // A whitespace-only prompt is still absent and never overwrites the known one.
    const kept = mergeAccountTestMetadata(
      { effective_prompt: 'hi' },
      { effective_prompt: '   \n ' }
    )
    expect(kept.effective_prompt).toBe('hi')
  })

  it('carries the legacy model field and updates the conflict flag both ways', () => {
    const withConflict = mergeAccountTestMetadata({}, {
      model: 'legacy-model',
      upstream_response_model_conflict: true
    })
    expect(withConflict.model).toBe('legacy-model')
    expect(withConflict.upstream_response_model_conflict).toBe(true)

    const cleared = mergeAccountTestMetadata(withConflict, {
      upstream_response_model_conflict: false
    })
    expect(cleared.upstream_response_model_conflict).toBe(false)
    expect(cleared.model).toBe('legacy-model')
  })
})

describe('metadataText', () => {
  it('treats blank and non-strings as absent', () => {
    expect(metadataText('  high ')).toBe('high')
    expect(metadataText('   ')).toBe('')
    expect(metadataText(undefined)).toBe('')
    expect(metadataText(7)).toBe('')
  })
})

describe('metadataVerbatim', () => {
  it('keeps non-blank text exactly as reported', () => {
    expect(metadataVerbatim('  say hi\n twice  ')).toBe('  say hi\n twice  ')
    expect(metadataVerbatim('hi')).toBe('hi')
  })

  it('still treats whitespace-only and non-strings as absent', () => {
    expect(metadataVerbatim('   \n\t')).toBe('')
    expect(metadataVerbatim('')).toBe('')
    expect(metadataVerbatim(undefined)).toBe('')
    expect(metadataVerbatim(7)).toBe('')
  })
})

describe('accountTestStreamModeLabelKey', () => {
  it('maps known modes to i18n keys and leaves unknown ones verbatim', () => {
    expect(accountTestStreamModeLabelKey('stream')).toContain('streamMode.stream')
    expect(accountTestStreamModeLabelKey('native_non_stream')).toContain(
      'streamMode.native_non_stream'
    )
    expect(accountTestStreamModeLabelKey('chunked')).toBe('')
  })
})
